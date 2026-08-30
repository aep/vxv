// Command vxv-init is the guest pid-1 shim executed inside the microVM.
//
// It exists because libkrun's own init cannot dynamically link the *first*
// process it execs when the root is the host filesystem — a statically linked
// entrypoint is required. Built with CGO_ENABLED=0, embedded into the vxv
// binary, and materialized to disk at launch.
//
// Responsibilities, in order:
//  1. mount ephemeral tmpfs over the standard scratch dirs (guest memory only,
//     so the read-only-host guarantee holds while the guest stays usable);
//  2. overlay the dirs in VXV_OVERLAY (e.g. /home, /etc) with a tmpfs-backed
//     upper layer, making them writable inside the guest but discarded on exit;
//  3. lay the VXV_INJECT host tree into the guest fs (config dropped in guest-only);
//  4. mount the writable PWD virtiofs share over its real host path;
//  5. mount a fresh devpts and allocate a pseudo-terminal;
//  6. start the shell on the pty slave as a new session leader, dropping to the
//     caller's uid/gid, in the working directory;
//  7. relay bytes between libkrun's console (our stdio) and the pty master, and
//     mirror the console's window size onto the pty whenever the host terminal
//     is resized.
//
// Because libkrun's init execs this shim in place, we run as guest PID 1 and
// carry init's reaping duty: while relaying, we wait() for every child the guest
// orphans onto us, so zombies (and any fds they still pin) can't accumulate over
// the life of the VM.
//
// The pty is what gives the guest shell a real controlling terminal (line
// editing, job control, a working `tty`) even though libkrun hands us a plain
// virtio-serial port. Dropping privileges here — rather than running the shell
// as root — is enforced by the kernel at exec time via Credential.
//
// All configuration arrives through VXV_* environment variables set by the host
// side. It refuses to run unless VXV_INIT=1 is present, so it can never
// accidentally mount over a real host's directories if run by mistake.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func main() {
	if os.Getenv("VXV_INIT") != "1" {
		fmt.Fprintln(os.Stderr, "vxv-init: refusing to run outside a vxv guest (VXV_INIT!=1)")
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "vxv-init:", err)
		os.Exit(1)
	}
}

func run() error {
	setupHostname()
	setupRunTmpfs() // /run first: it backs the overlay upper/work dirs
	setupOverlays()
	setupScratch() // remaining scratch dirs, on top of overlays so a nested one (e.g. /var/tmp under the /var overlay) isn't shadowed
	setupInject()
	pwd := setupWorkdir()
	setupBlocks()

	shell := envOr("VXV_SHELL", "/bin/sh")
	argv := []string{shell}
	if login := os.Getenv("VXV_LOGIN"); login != "" {
		argv = append(argv, strings.Fields(login)...)
	}

	cred, err := credential()
	if err != nil {
		return err
	}

	// Allocate the pty the shell will run on.
	master, slavePath, err := openPTY()
	if err != nil {
		// Fall back to a bare exec without a pty rather than failing outright.
		fmt.Fprintln(os.Stderr, "vxv-init: no pty ("+err.Error()+"); shell will lack a controlling terminal")
		return execNoPTY(shell, argv, cred)
	}
	defer master.Close()
	masterFd := int(master.Fd())

	// Hand ownership of the slave to the target user (what grantpt does), so the
	// unprivileged shell can read and write its own terminal.
	if cred != nil {
		_ = os.Chown(slavePath, int(cred.Uid), int(cred.Gid))
	}
	_ = os.Chmod(slavePath, 0o620)

	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fmt.Errorf("open pty slave: %w", err)
	}
	setWinsize(slave)

	cmd := exec.Command(shell, argv[1:]...)
	cmd.Path = shell
	cmd.Args = argv
	cmd.Env = os.Environ()
	cmd.Dir = pwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true, // new session
		Setctty:    true, // make the slave our controlling terminal
		Ctty:       0,    // ...the fd at index 0 (stdin => slave)
		Credential: cred, // drop to the caller's uid/gid (nil => stay root)
	}
	// Register for child-death notifications BEFORE starting the shell, so its
	// exit (or that of an orphan that dies during boot) can't slip through the
	// gap before we begin reaping.
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)

	if err := cmd.Start(); err != nil {
		slave.Close()
		return fmt.Errorf("start shell %s: %w", shell, err)
	}
	slave.Close() // the child holds its own copy

	// Keep the shell's terminal in step with the host's. A host SIGWINCH makes
	// libkrun push a virtio-console resize into the guest; the kernel applies it
	// to our console (hvc0) and raises SIGWINCH on that console's foreground
	// process group, which we are in (libkrun's init made hvc0 the controlling
	// terminal and then execed us — that exec replaced it, so we ARE pid 1 and
	// inherited hvc0's controlling terminal and foreground process group).
	// What's left is passing the new size through to the inner pty — setting it
	// on the master is what makes the kernel signal the shell in turn.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			syncWinsize(masterFd)
		}
	}()

	// Put the guest console (our stdin) into raw mode when it's a tty. libkrun
	// hands us a cooked console tty; without this, a Ctrl-C byte would make the
	// guest console's line discipline raise SIGINT against this shim instead of
	// flowing through to the shell's pty. In raw mode it's just a byte we relay
	// to the inner pts, where the shell's own line discipline turns it into the
	// signal — the correct target. Ignoring the job-control signals guards the
	// relay in case anything still slips through.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP)
	if term.IsTerminal(0) {
		_, _ = term.MakeRaw(0)
	}

	// Relay: guest console (our stdio) <-> pty master. libkrun already keeps the
	// host terminal raw, so we just shuffle raw bytes both ways.
	hostIn := os.NewFile(0, "vxv-console-in")
	hostOut := os.NewFile(1, "vxv-console-out")
	drained := make(chan struct{})
	go func() {
		io.Copy(hostOut, master) // shell output -> host, until the pty master EOFs
		close(drained)
	}()
	go io.Copy(master, hostIn) // host keystrokes -> shell

	// As PID 1 we are the reaper for the entire guest: any process whose parent
	// exits reparents to us, and one we never wait() for lingers as a zombie.
	// Reap every dead child on each SIGCHLD until the shell itself exits, then
	// carry its status out as our own.
	status := reapUntilShellExits(sigchld, cmd.Process.Pid)

	// The shell has exited; once its last slave fd is gone the master read ends.
	// Wait for the output copy to drain so we don't truncate the final bytes.
	master.Close()
	<-drained

	os.Exit(status)
	return nil
}

// reapUntilShellExits performs PID 1's reaping duty until the interactive shell
// terminates. Because this shim is pid 1 in the guest, every orphaned process
// reparents to us; without a wait() they accumulate as zombies (and any that
// were still holding pipes or ptys keep those fds pinned until reaped). On each
// SIGCHLD we drain all currently-dead children with a non-blocking Wait4(-1);
// when the reaped pid is the shell's we return its exit code for run to exit with.
//
// We reap the shell here via Wait4 rather than cmd.Wait() on purpose: a
// Wait4(-1) reaper and cmd.Wait() would race for the shell's zombie, and
// whichever lost would fail with "no child processes" and drop the exit status.
func reapUntilShellExits(sigchld <-chan os.Signal, shellPid int) int {
	for range sigchld {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				continue
			}
			if pid <= 0 {
				// pid 0: children remain but none are dead right now — wait for
				// the next SIGCHLD. pid <0 (ECHILD): no children left at all,
				// which shouldn't happen while the shell lives; treat as done.
				break
			}
			if pid == shellPid {
				return waitStatusToExitCode(ws)
			}
			// Any other pid was an orphan we've just reaped; keep draining.
		}
	}
	return 0
}

// waitStatusToExitCode renders a child's wait status as a process exit code,
// following the shell convention of 128+signal for a child killed by a signal.
func waitStatusToExitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// setupHostname adopts the host's hostname so the guest isn't just "localhost".
// Runs as root here, before privileges are dropped.
func setupHostname() {
	if h := os.Getenv("VXV_HOSTNAME"); h != "" {
		_ = syscall.Sethostname([]byte(h))
	}
}

// setupRunTmpfs mounts the ephemeral tmpfs on /run. It runs BEFORE setupOverlays
// because the overlay upper/work dirs live under /run (see overlayBase); mounting
// /run after the overlays would shadow those dirs and break every overlay.
func setupRunTmpfs() {
	if os.Getenv("VXV_TMPFS") == "0" {
		return
	}
	_ = syscall.Mount("tmpfs", "/run", "tmpfs", 0, "")
}

// setupScratch mounts ephemeral tmpfs over the remaining scratch directories.
// These live in guest memory only and never touch the host. It runs AFTER
// setupOverlays so a scratch dir nested under an overlay target — notably
// /var/tmp, under the /var overlay — is mounted on top of that overlay instead
// of being shadowed by it. /run is handled earlier by setupRunTmpfs.
func setupScratch() {
	if os.Getenv("VXV_TMPFS") == "0" {
		return
	}
	for _, d := range []string{"/tmp", "/var/tmp", "/dev/shm"} {
		_ = syscall.Mount("tmpfs", d, "tmpfs", 0, "")
	}
}

// overlayBase is the tmpfs-backed directory that holds every overlay's upper and
// work dirs. It sits under /run (guest memory), so all overlay writes — and thus
// the whole "writable but ephemeral" layer — live in RAM and vanish on shutdown.
const overlayBase = "/run/vxv-overlay"

// setupOverlays makes the directories listed in VXV_OVERLAY writable without
// touching the host: each is remounted as an overlayfs whose lower layer is the
// read-only host directory itself and whose upper/work layers live on an
// in-guest tmpfs. Writes land in that tmpfs (copy-up on first modification) and
// are discarded when the VM exits, so the host root stays pristine.
//
// This runs after setupScratch (so /run is already tmpfs when tmpfs is enabled)
// and before setupWorkdir (so an explicit writable PWD mounts on top and wins).
func setupOverlays() {
	list := strings.TrimSpace(os.Getenv("VXV_OVERLAY"))
	if list == "" {
		return
	}

	// Backing store for every overlay's upper/work dirs. If /run isn't writable
	// (tmpfs disabled), give it its own ephemeral tmpfs — the overlay layer is
	// discarded regardless of the --tmpfs setting, so this stays true to intent.
	if err := os.MkdirAll(overlayBase, 0o755); err != nil {
		if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, ""); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot back overlays (no writable /run):", err)
			return
		}
		if err := os.MkdirAll(overlayBase, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot create overlay store:", err)
			return
		}
	}

	for i, target := range strings.Split(list, ",") {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
			// Not a directory on this host; nothing to overlay.
			continue
		}

		// A unique upper/work pair per target, both on the same tmpfs (an
		// overlayfs requirement). Overlaying a directory onto itself — lowerdir
		// and mountpoint both `target` — is resolved by the kernel before the
		// new mount shadows it, the standard writable-overlay-in-place idiom.
		slot := filepath.Join(overlayBase, strconv.Itoa(i))
		upper := filepath.Join(slot, "upper")
		work := filepath.Join(slot, "work")
		if err := os.MkdirAll(upper, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: overlay skip "+target+":", err)
			continue
		}
		if err := os.MkdirAll(work, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: overlay skip "+target+":", err)
			continue
		}

		// metacopy/redirect_dir need trusted.overlay.* xattrs on the layers, which
		// the virtiofs host-root lower cannot serve (getxattr returns EACCES —
		// "overlayfs: failed to get metacopy (-13)"), and metadata copy-up then
		// fails. We need neither feature for an ephemeral upper, so turn both off.
		opts := "lowerdir=" + target + ",upperdir=" + upper + ",workdir=" + work +
			",redirect_dir=off,metacopy=off"
		if err := syscall.Mount("overlay", target, "overlay", 0, opts); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: "+target+" stays read-only (overlay mount failed):", err)
		}
	}
}

// setupInject lays a host directory tree (VXV_INJECT) into the guest filesystem,
// mirroring each entry's relative path under / — so a host file at
// <dir>/etc/foo/bar.json is written to /etc/foo/bar.json in the guest. The source
// is visible read-only through the host-root share; the destinations land on the
// ephemeral overlay/tmpfs layers, so injected files are guest-only and discarded
// on shutdown. It runs after setupOverlays (destinations are writable) and while
// still root (can write /etc and create any parents). Failures are per-entry
// warnings, never fatal — a read-only destination just means that file is skipped.
func setupInject() {
	dir := os.Getenv("VXV_INJECT")
	if dir == "" {
		return
	}
	root := os.DirFS(dir)
	err := fs.WalkDir(root, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || rel == "." {
			return nil
		}
		dst := filepath.Join("/", rel)
		src := filepath.Join(dir, rel)
		info, e := d.Info()
		if e != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: inject "+dst+":", e)
			return nil
		}
		switch {
		case d.IsDir():
			// Only create dirs the guest lacks — and mirror ownership/mode only on
			// those. Never re-own a dir that already exists (e.g. /etc), or we'd
			// hand a system directory to the source tree's owner.
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
					fmt.Fprintln(os.Stderr, "vxv-init: warning: inject "+dst+":", err)
					return nil
				}
				chownLike(dst, info)
				_ = os.Chmod(dst, info.Mode().Perm())
			}
		case d.Type()&os.ModeSymlink != 0:
			if target, e := os.Readlink(src); e == nil {
				_ = os.Remove(dst)
				if e := os.Symlink(target, dst); e != nil {
					fmt.Fprintln(os.Stderr, "vxv-init: warning: inject "+dst+":", e)
				} else {
					chownLike(dst, info) // Lchown: owns the link, not its target
				}
			}
		case d.Type().IsRegular():
			if e := copyInto(src, dst, info); e != nil {
				fmt.Fprintln(os.Stderr, "vxv-init: warning: inject "+dst+":", e)
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vxv-init: warning: inject "+dir+":", err)
	}
}

// copyInto writes src's contents to dst, mirroring the source file's mode and
// (uid, gid). The guest shares the host's uid namespace, so the numeric owner
// carried over virtiofs is the right one — a guestfs file owned by the user
// appears user-owned in the guest, a root-owned one stays root-owned.
func copyInto(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		out.Close()
		return err
	}
	chownLike(dst, info)
	return out.Close()
}

// chownLike sets dst's owner to match the source's uid/gid. It uses Lchown so a
// symlink itself is re-owned rather than its target, and stays silent on failure
// (best-effort; the file is already in place and usable).
func chownLike(dst string, info fs.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(dst, int(st.Uid), int(st.Gid))
	}
}

// setupWorkdir mounts the writable PWD share over its real host path and returns
// the path (or "/" if none/failed) for use as the shell's working directory.
func setupWorkdir() string {
	pwd := os.Getenv("VXV_PWD")
	if pwd == "" {
		return "/"
	}
	tag := envOr("VXV_TAG", "vxvpwd")
	if err := syscall.Mount(tag, pwd, "virtiofs", 0, ""); err != nil {
		fmt.Fprintln(os.Stderr, "vxv-init: warning: "+pwd+" is read-only inside the guest:", err)
	}
	if _, err := os.Stat(pwd); err != nil {
		return "/"
	}
	return pwd
}

// blockBase holds the inaccessible placeholder inodes that mask hidden paths.
// It sits under /run (guest memory), so nothing here touches the host.
const blockBase = "/run/vxv-blocked"

// setupBlocks enforces the two .vxv.yaml controls, passed as comma-separated
// path lists:
//
//	VXV_HIDE   files the guest may neither read nor write. Each is shadowed by
//	           bind-mounting a mode-0000 root-owned placeholder over it, so the
//	           unprivileged shell sees an empty, inaccessible node in its place.
//	VXV_RONLY  files the guest may read but not write. Each is bind-mounted over
//	           itself and remounted read-only, so its real contents stay visible
//	           while writes fail with EROFS.
//
// This runs LAST — after the overlays, the injected tree, and the writable PWD
// share — so these masks win no matter which layer the real file lives on. A
// hidden file is masked with an empty file and a directory with an empty
// directory, since a bind mount requires matching inode types.
//
// Everything happens only in the guest mount namespace over in-guest memory;
// the host files are never touched. A path that doesn't exist on this host is
// silently skipped, and any single failure is a warning, never fatal.
func setupBlocks() {
	hide := splitList(os.Getenv("VXV_HIDE"))
	ronly := splitList(os.Getenv("VXV_RONLY"))
	if len(hide) == 0 && len(ronly) == 0 {
		return
	}

	applyReadonly(ronly)
	applyHide(hide)
}

// applyReadonly bind-mounts each target over itself and remounts it read-only,
// leaving contents readable but blocking writes.
func applyReadonly(targets []string) {
	for _, target := range targets {
		if _, err := os.Stat(target); err != nil {
			continue // not present on this host
		}
		if err := syscall.Mount(target, target, "", syscall.MS_BIND, ""); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot protect "+target+":", err)
			continue
		}
		if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: "+target+" stays writable (read-only remount failed):", err)
		}
	}
}

// applyHide shadows each target with an inaccessible placeholder so the guest
// can neither read nor write it.
func applyHide(targets []string) {
	if len(targets) == 0 {
		return
	}

	// Backing store for the placeholder inodes. Fall back to giving /run its own
	// tmpfs if it isn't already writable (e.g. --tmpfs disabled), so the masks
	// stay guest-only regardless.
	if err := os.MkdirAll(blockBase, 0o755); err != nil {
		if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, ""); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot hide files (no writable /run):", err)
			return
		}
		if err := os.MkdirAll(blockBase, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot create hide store:", err)
			return
		}
	}

	// One empty file and one empty directory, both mode 0000 and root-owned, so
	// the dropped-privilege shell has no access through them.
	emptyFile := filepath.Join(blockBase, "file")
	emptyDir := filepath.Join(blockBase, "dir")
	if f, err := os.OpenFile(emptyFile, os.O_CREATE|os.O_WRONLY, 0o000); err == nil {
		f.Close()
	} else {
		fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot create hide placeholder:", err)
		return
	}
	if err := os.Mkdir(emptyDir, 0o000); err != nil && !os.IsExist(err) {
		fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot create hide placeholder dir:", err)
		return
	}
	_ = os.Chmod(emptyFile, 0o000)
	_ = os.Chmod(emptyDir, 0o000)

	for _, target := range targets {
		fi, err := os.Stat(target)
		if err != nil {
			continue // not present on this host
		}
		src := emptyFile
		if fi.IsDir() {
			src = emptyDir
		}
		if err := syscall.Mount(src, target, "", syscall.MS_BIND, ""); err != nil {
			fmt.Fprintln(os.Stderr, "vxv-init: warning: cannot hide "+target+":", err)
			continue
		}
		// Re-assert read-only on the bind so the mask can't be written through
		// even if the placeholder's mode were somehow bypassed.
		_ = syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
	}
}

// splitList parses a comma-separated path list, trimming blanks.
func splitList(list string) []string {
	var out []string
	for _, p := range strings.Split(strings.TrimSpace(list), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// openPTY mounts devpts and allocates a pseudo-terminal, returning the master
// file and the slave's path.
func openPTY() (*os.File, string, error) {
	// A fresh devpts instance; harmless if one is already mounted.
	_ = syscall.Mount("devpts", "/dev/pts", "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "mode=0620,ptmxmode=0666,gid=5")

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Unlock and resolve the slave.
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlockpt: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, "", fmt.Errorf("ptsname: %w", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n), nil
}

// consoleFd is our stdin: the virtio console libkrun gave the guest, and the one
// device that learns the host terminal's real size.
const consoleFd = 0

// setWinsize gives the pty its starting size. The console already carries the
// host's dimensions (libkrun sends them when the port comes up), so prefer those
// and fall back to what vxv measured on the host, in case the resize message has
// not landed yet this early in boot.
func setWinsize(f *os.File) {
	ws, err := unix.IoctlGetWinsize(consoleFd, unix.TIOCGWINSZ)
	if err != nil || (ws.Col == 0 && ws.Row == 0) {
		ws = &unix.Winsize{
			Col: uint16(atoiOr(os.Getenv("VXV_COLS"), 80)),
			Row: uint16(atoiOr(os.Getenv("VXV_ROWS"), 24)),
		}
	}
	_ = unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, ws)
}

// syncWinsize copies the console's current window size onto the pty. A zero size
// means the console has not been told one; leave the pty as it is rather than
// blanking the shell's idea of its terminal.
func syncWinsize(ptyFd int) {
	ws, err := unix.IoctlGetWinsize(consoleFd, unix.TIOCGWINSZ)
	if err != nil || (ws.Col == 0 && ws.Row == 0) {
		return
	}
	_ = unix.IoctlSetWinsize(ptyFd, unix.TIOCSWINSZ, ws)
}

// credential parses the target uid/gid/groups. Returns nil to keep root (uid 0),
// which is also what an unset/zero uid means.
func credential() (*syscall.Credential, error) {
	uidStr := os.Getenv("VXV_UID")
	if uidStr == "" {
		return nil, nil
	}
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return nil, fmt.Errorf("bad VXV_UID %q: %w", uidStr, err)
	}
	if uid == 0 {
		return nil, nil
	}
	gid := atoiOr(os.Getenv("VXV_GID"), uid)

	var groups []uint32
	for _, g := range strings.Split(os.Getenv("VXV_GROUPS"), ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		n, err := strconv.Atoi(g)
		if err != nil {
			return nil, fmt.Errorf("bad group %q in VXV_GROUPS: %w", g, err)
		}
		groups = append(groups, uint32(n))
	}
	return &syscall.Credential{
		Uid:    uint32(uid),
		Gid:    uint32(gid),
		Groups: groups,
	}, nil
}

// execNoPTY is the degraded path: exec the shell directly on our stdio without a
// controlling terminal. Privilege drop still applies.
func execNoPTY(shell string, argv []string, cred *syscall.Credential) error {
	if cred == nil {
		return syscall.Exec(shell, argv, os.Environ())
	}
	cmd := exec.Command(shell, argv[1:]...)
	cmd.Path, cmd.Args = shell, argv
	cmd.Env = os.Environ()
	cmd.Dir = envOr("VXV_PWD", "/")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
