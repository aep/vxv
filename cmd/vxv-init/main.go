// Command vxv-init is the guest pid-1 shim executed inside the microVM.
//
// It exists because libkrun's own init cannot dynamically link the *first*
// process it execs when the root is the host filesystem — a statically linked
// entrypoint is required. Built with CGO_ENABLED=0, embedded into the vxv
// binary, and written into the host mount namespace's scratch tmpfs at launch.
//
// It does only what the guest kernel alone can do. The filesystem the guest sees
// was already assembled on the host, in a private mount namespace, and handed
// over as the root virtio-fs share: read-only tree, ephemeral overlays, writable
// working directory, and the .vxv.yaml policy are all in place before the VM
// boots, and none of it can be undone from in here. What is left:
//
//  1. mount ephemeral tmpfs over the scratch dirs — guest memory is simply a
//     better home for /tmp than a virtio-fs round trip, and /dev/shm needs to be
//     a real in-guest tmpfs for shared mappings to work;
//  2. mount a fresh devpts and allocate a pseudo-terminal;
//  3. start the shell on the pty slave as a new session leader, dropping to the
//     caller's uid/gid, in the working directory;
//  4. relay bytes between libkrun's console (our stdio) and the pty master, and
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
	"os"
	"os/exec"
	"os/signal"
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
	pwd := workdir()

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

// setupScratch mounts ephemeral tmpfs over the scratch directories. These live
// in guest memory only and never reach the host: /tmp and /var/tmp because a
// tmpfs beats a virtio-fs round trip for throwaway files, /run because the host
// side keeps its own bookkeeping there, and /dev/shm because shared memory has
// to be a real in-guest filesystem.
func setupScratch() {
	if os.Getenv("VXV_TMPFS") == "0" {
		return
	}
	for _, d := range []string{"/run", "/tmp", "/var/tmp", "/dev/shm"} {
		_ = syscall.Mount("tmpfs", d, "tmpfs", 0, "")
	}
}

// workdir returns the directory the shell starts in. The host namespace already
// bound the real working directory there, writable, on top of every overlay.
func workdir() string {
	pwd := os.Getenv("VXV_PWD")
	if pwd == "" {
		return "/"
	}
	if _, err := os.Stat(pwd); err != nil {
		fmt.Fprintln(os.Stderr, "vxv-init: warning: "+pwd+" is not reachable in the guest:", err)
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
