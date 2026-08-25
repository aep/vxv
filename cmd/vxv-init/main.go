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
//  7. relay bytes between libkrun's console (our stdio) and the pty master.
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
	setupScratch()
	setupOverlays()
	setupInject()
	pwd := setupWorkdir()

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
	if err := cmd.Start(); err != nil {
		slave.Close()
		return fmt.Errorf("start shell %s: %w", shell, err)
	}
	slave.Close() // the child holds its own copy

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

	err = cmd.Wait()
	// The shell has exited; once its last slave fd is gone the master read ends.
	// Wait for the output copy to drain so we don't truncate the final bytes.
	master.Close()
	<-drained

	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			os.Exit(ws.ExitStatus())
		}
		os.Exit(1)
	}
	return err
}

// setupHostname adopts the host's hostname so the guest isn't just "localhost".
// Runs as root here, before privileges are dropped.
func setupHostname() {
	if h := os.Getenv("VXV_HOSTNAME"); h != "" {
		_ = syscall.Sethostname([]byte(h))
	}
}

// setupScratch mounts ephemeral tmpfs over the standard scratch directories.
// These live in guest memory only and never touch the host.
func setupScratch() {
	if os.Getenv("VXV_TMPFS") == "0" {
		return
	}
	for _, d := range []string{"/tmp", "/var/tmp", "/run", "/dev/shm"} {
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

		opts := "lowerdir=" + target + ",upperdir=" + upper + ",workdir=" + work
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

// setWinsize applies the host terminal size (passed by vxv) to the pty.
func setWinsize(f *os.File) {
	cols := atoiOr(os.Getenv("VXV_COLS"), 80)
	rows := atoiOr(os.Getenv("VXV_ROWS"), 24)
	_ = unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows), Col: uint16(cols),
	})
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
