package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stacklok/go-microvm/krun"
	"golang.org/x/sys/unix"
)

// rootTag is libkrun's reserved tag for the guest root filesystem
// (KRUN_FS_ROOT_TAG in libkrun.h). vxv serves exactly one share under it: the
// filesystem view assembled in our private mount namespace, writable at the
// virtio-fs level because what may be written is decided by the host kernel's
// mount flags, not by the file server.
const rootTag = "/dev/root"

type options struct {
	cpus      uint8
	memMiB    uint32
	root      string
	pwd       string
	shell     string
	env       []string // extra KEY=VALUE pairs from -e
	tmpfs     bool
	overlay   []string // dirs made writable via ephemeral overlayfs
	injectDir string   // guestfs tree laid into the guest fs ("" if absent)
	hide      []string // paths the guest cannot read or write (from .vxv.yaml)
	readonly  []string // paths the guest can read but not write (from .vxv.yaml)
	caller    *caller  // who the session belongs to
	debug     bool
}

// injectDirName is the guest-file tree looked for next to the vxv binary. Ship
// files under it mirroring their guest paths, e.g.
// guestfs/etc/claude-code/managed-settings.d/10-auto.json lands at
// /etc/claude-code/managed-settings.d/10-auto.json inside the VM.
const injectDirName = "guestfs"

func newRootCmd() *cobra.Command {
	o := &options{}
	var cpus, mem uint

	cmd := &cobra.Command{
		Use:   "vxv [flags]",
		Short: "Open an isolated login shell in a libkrun microVM",
		Long: "vxv isolates a Claude agent (or any interactive session) inside a libkrun microVM.\n\n" +
			"The guest's filesystem is assembled on the host, in a private mount namespace:\n" +
			"the whole tree is made read-only, the --overlay dirs get an ephemeral overlayfs\n" +
			"whose writes live in RAM and are DISCARDED on exit, the working directory is bound\n" +
			"back in writable, and any .vxv.yaml policy is applied last. That view is handed to\n" +
			"the VM as its root over virtio-fs, so nothing is left for the guest to enforce: it\n" +
			"cannot remount its way to anything, because vxv itself can no longer reach it.\n" +
			"An interactive login shell is opened with a fresh environment. Networking uses TSI,\n" +
			"proxying all guest traffic through the host network stack so policy can be\n" +
			"enforced host-side.\n\n" +
			"Building the namespace needs CAP_SYS_ADMIN: run vxv under sudo, or grant it once\n" +
			"with `setcap cap_sys_admin+ep`. It is dropped again before the VM boots.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if cpus < 1 || cpus > 255 {
				return fmt.Errorf("--cpus must be between 1 and 255")
			}
			if mem < 128 {
				return fmt.Errorf("--mem must be at least 128 MiB")
			}
			o.cpus, o.memMiB = uint8(cpus), uint32(mem)
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.execute()
		},
	}

	f := cmd.Flags()
	f.UintVar(&cpus, "cpus", 2, "number of vCPUs")
	f.UintVar(&mem, "mem", 16384, "guest RAM in MiB")
	f.StringVar(&o.root, "root", "/", "host directory the guest sees as its root")
	f.StringVar(&o.pwd, "pwd", "", "directory exposed READ-WRITE (default: current directory)")
	f.StringVar(&o.shell, "shell", "", "login shell to run (default: $SHELL, else /bin/bash, else /bin/sh)")
	f.StringArrayVarP(&o.env, "env", "e", nil, "extra environment variable KEY=VALUE (repeatable)")
	f.BoolVar(&o.tmpfs, "tmpfs", true, "mount ephemeral in-guest tmpfs on /tmp, /var/tmp, /run, /dev/shm")
	f.StringSliceVar(&o.overlay, "overlay",
		[]string{"/home", "/root", "/etc", "/opt", "/srv", "/usr", "/var"},
		"dirs made writable via ephemeral overlayfs; writes are discarded on exit (empty disables)")
	f.BoolVar(&o.debug, "debug", false, "enable verbose libkrun logging")
	return cmd
}

// execute runs in two stages. The first is this process as launched: it
// validates everything it can while errors are still cheap, then re-execs into
// a private mount namespace. The second is that child, which builds the guest's
// filesystem, drops back to the calling user, and boots the VM.
func (o *options) execute() error {
	stage2 := os.Getenv(stageEnv) != ""
	if err := o.resolve(stage2); err != nil {
		return err
	}

	if !stage2 {
		if err := requireSysAdmin(); err != nil {
			return err
		}
		if err := preflight(); err != nil {
			return err
		}
		return reexec()
	}

	// Re-checked on this side of the re-exec: the namespace exists, but a
	// capability that failed to survive the exec would otherwise surface as an
	// unexplained EPERM from the first mount.
	if err := requireSysAdmin(); err != nil {
		return err
	}
	warnBuildCaps()

	shim, err := o.buildGuestRoot()
	if err != nil {
		return err
	}
	o.printBanner()

	// From here on this process holds nothing the caller doesn't. Everything the
	// guest will ever be allowed to touch was decided above.
	if err := o.caller.drop(); err != nil {
		return err
	}
	raiseNoFile()
	return o.boot(shim)
}

// preflight checks that the environment can run a microVM at all.
func preflight() error {
	if !krun.IsAvailable() {
		return fmt.Errorf("libkrun reports the platform is unavailable (is libkrun installed and KVM accessible?)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not accessible: %w", err)
	}
	return nil
}

// boot configures libkrun and hands it the assembled root. The guest entrypoint
// is our statically linked shim (libkrun can only exec a static binary as the
// first guest process); it lives in the namespace's scratch tmpfs, which the
// guest reaches through the same share.
func (o *options) boot(shim string) error {
	if o.debug {
		_ = krun.SetLogLevel(krun.LogLevelDebug)
	} else {
		_ = krun.SetLogLevel(krun.LogLevelError)
	}

	// Re-checked here, after the privilege drop: /dev/kvm was reachable while we
	// were building the namespace, but the caller is who has to open it.
	if err := unix.Access("/dev/kvm", unix.R_OK|unix.W_OK); err != nil {
		return fmt.Errorf("/dev/kvm is not accessible to uid %d (is that user in the kvm group?): %w", os.Getuid(), err)
	}

	ctx, err := krun.CreateContext()
	if err != nil {
		return fmt.Errorf("create libkrun context: %w", err)
	}
	// StartEnter never returns on success (it exit()s with the guest's status),
	// so Free only matters on the pre-boot error paths below.
	defer ctx.Free()

	if err := ctx.SetVMConfig(o.cpus, o.memMiB); err != nil {
		return fmt.Errorf("set vm config: %w", err)
	}

	// The one share: our namespace's view of o.root. shmSize 0 disables the DAX
	// window; the final argument would make virtio-fs itself refuse writes, which
	// we do not want — read-only is the host kernel's job here, and the parts of
	// the tree that are writable (the working directory, the overlays) have to
	// stay writable through this same share.
	if err := ctx.AddVirtioFS3(rootTag, o.root, 0, false); err != nil {
		return fmt.Errorf("add root virtiofs (%s): %w", o.root, err)
	}

	// Networking is intentionally left implicit: libkrun creates a vsock device
	// with TSI hijacking enabled, giving the guest full outbound internet routed
	// through the host stack. Host-side policy hooks attach there later.

	envp, err := o.buildEnv()
	if err != nil {
		return err
	}
	if err := ctx.SetEnv(envp); err != nil {
		return fmt.Errorf("set env: %w", err)
	}

	guestShim := o.guestPath(shim)
	if err := ctx.SetExec(guestShim, []string{guestShim}, envp); err != nil {
		return fmt.Errorf("set exec: %w", err)
	}

	// Hands control to the VMM. On success this exits the process with the
	// guest's exit code; it only returns here if boot configuration failed.
	if err := ctx.StartEnter(); err != nil {
		return fmt.Errorf("start microVM: %w", err)
	}
	return nil
}

// resolve normalizes paths and derives defaults. The policy and guestfs lookups
// only matter to the stage that builds the namespace, and are skipped in the
// first one so their warnings are not printed twice.
func (o *options) resolve(withPolicy bool) error {
	o.caller = resolveCaller()

	if rootAbs, err := filepath.Abs(o.root); err == nil {
		o.root = filepath.Clean(rootAbs)
	}

	if o.pwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("determine current directory: %w", err)
		}
		o.pwd = wd
	}
	abs, err := filepath.Abs(o.pwd)
	if err != nil {
		return fmt.Errorf("resolve --pwd: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("--pwd %q is not a directory", abs)
	}
	o.pwd = abs
	if o.pwd == o.root {
		return fmt.Errorf("--pwd %q is the guest root itself; give a directory inside it", o.pwd)
	}
	if !strings.HasPrefix(ensureTrailingSlash(o.pwd), ensureTrailingSlash(o.root)) {
		return fmt.Errorf("--pwd %q is outside --root %q, so the guest could not see it", o.pwd, o.root)
	}
	if run := filepath.Join(o.root, "run"); strings.HasPrefix(ensureTrailingSlash(o.pwd), ensureTrailingSlash(run)) {
		return fmt.Errorf("--pwd %q is under %s, where vxv mounts the guest's own tmpfs", o.pwd, run)
	}

	if o.shell == "" {
		o.shell = defaultShell(o.caller.shell)
	}

	o.overlay = normalizeOverlays(o.overlay)

	if withPolicy {
		o.resolveInject()
		o.resolvePolicy()
	}
	return nil
}

// resolveInject locates the guest-file tree: a "guestfs" dir next to the vxv
// binary, used only if present. It must live under --root, since the guest
// reaches it by way of that share. Anything missing just means nothing is
// injected, never an error.
func (o *options) resolveInject() {
	self, err := os.Executable()
	if err != nil {
		return // can't locate the binary; nothing to inject
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	dir := filepath.Join(filepath.Dir(self), injectDirName)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return
	}
	o.injectDir = dir
}

// guestPath translates a host path in our namespace into the path the guest
// will see it at. They are the same string for the usual --root /.
func (o *options) guestPath(host string) string {
	if o.root == "/" {
		return host
	}
	rel, err := filepath.Rel(o.root, host)
	if err != nil || strings.HasPrefix(rel, "..") {
		return host
	}
	return "/" + rel
}

// normalizeOverlays cleans the overlay dir list into absolute, deduplicated,
// comma-free paths. Entries that aren't absolute or contain a comma are dropped:
// the paths go into an overlayfs option string, where a comma would split them.
func normalizeOverlays(dirs []string) []string {
	seen := make(map[string]bool, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" || strings.Contains(d, ",") || !filepath.IsAbs(d) {
			continue
		}
		d = filepath.Clean(d)
		if d == "/" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// defaultShell picks the caller's shell, falling back to common ones. Because
// the guest root IS the host root, any host shell path is valid inside the guest.
func defaultShell(preferred string) string {
	if preferred != "" {
		if _, err := os.Stat(preferred); err == nil {
			return preferred
		}
	}
	for _, s := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	return "/bin/sh"
}

// loginArgs returns the flags that make the chosen shell an interactive login
// shell. bash/zsh take -l; plain sh (dash) only understands -i.
func loginArgs(shell string) string {
	switch filepath.Base(shell) {
	case "bash", "zsh":
		return "-l -i"
	default:
		return "-i"
	}
}

// buildEnv assembles a fresh, minimal guest environment plus the control
// variables consumed by the shim. Host env is NOT inherited; callers add what
// they need with -e. Every entry is validated to be single-line ASCII, since
// libkrun folds the environment into the guest kernel command line.
//
// The identity variables (VXV_UID/GID/GROUPS) are the caller's — the real user,
// not whatever sudo made this process — and are appended LAST so a -e cannot
// shadow them. Combined with rejecting any -e VXV_* key, this guarantees a user
// running vxv cannot make the guest drop to a different user's uid.
func (o *options) buildEnv() ([]string, error) {
	tmpfs := "0"
	if o.tmpfs {
		tmpfs = "1"
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=" + orDefault(os.Getenv("TERM"), "xterm-256color"),
		"HOME=" + orDefault(o.caller.home, "/root"),
	}

	// User-supplied variables, before the trusted control block.
	for _, e := range o.env {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			return nil, fmt.Errorf("-e %q must be in KEY=VALUE form", e)
		}
		if strings.HasPrefix(key, "VXV_") || key == "VXV" {
			return nil, fmt.Errorf("-e %s is reserved (VXV_* variables are set by vxv and cannot be overridden)", key)
		}
		env = append(env, e)
	}

	cols, rows := terminalSize()
	// Trusted control block — appended last so it always wins.
	env = append(env,
		"VXV=1",
		"VXV_INIT=1",
		"VXV_PWD="+o.guestPath(o.pwd),
		"VXV_SHELL="+o.shell,
		"VXV_LOGIN="+loginArgs(o.shell),
		"VXV_TMPFS="+tmpfs,
		"VXV_UID="+strconv.Itoa(o.caller.uid),
		"VXV_GID="+strconv.Itoa(o.caller.gid),
		"VXV_GROUPS="+groupList(o.caller.groups),
		"VXV_COLS="+strconv.Itoa(cols),
		"VXV_ROWS="+strconv.Itoa(rows),
		"VXV_HOSTNAME="+hostname(),
	)

	for _, e := range env {
		if !isCmdlineSafe(e) {
			return nil, fmt.Errorf("environment entry %q contains non-ASCII or newline characters, which libkrun cannot pass to the guest", e)
		}
	}
	return env, nil
}

// hostname returns the host's hostname so the guest can adopt it instead of the
// kernel default ("localhost"). Empty on error, which the shim treats as "leave
// as-is".
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// groupList renders the caller's supplementary groups as a comma-separated list.
func groupList(gids []int) string {
	parts := make([]string, 0, len(gids))
	for _, g := range gids {
		parts = append(parts, strconv.Itoa(g))
	}
	return strings.Join(parts, ",")
}

// terminalSize returns the host terminal's dimensions, or a sane default when
// none of our standard streams is a terminal. The fds are tried in the same
// order libkrun picks the one it takes the guest console's size from, so the
// boot-time size we hand the guest matches what the console reports later.
func terminalSize() (cols, rows int) {
	for _, fd := range []int{int(os.Stdin.Fd()), int(os.Stdout.Fd()), int(os.Stderr.Fd())} {
		ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		if err == nil && ws.Col != 0 {
			return int(ws.Col), int(ws.Row)
		}
	}
	return 80, 24
}

// isCmdlineSafe reports whether s is printable single-line ASCII, the subset
// libkrun can safely fold into the guest kernel command line.
func isCmdlineSafe(s string) bool {
	for _, r := range s {
		if r > 0x7e || r < 0x20 {
			return false
		}
	}
	return true
}

func (o *options) printBanner() {
	fmt.Fprintf(os.Stderr,
		"┌─ vxv microVM ────────────────────────────────────\n"+
			"│ root (ro)   : %s\n"+
			"│ pwd  (rw)   : %s\n"+
			"│ overlay (∅) : %s\n"+
			"│ inject      : %s\n"+
			"│ hidden      : %s\n"+
			"│ readonly    : %s\n"+
			"│ resources   : %d vCPU, %d MiB\n"+
			"│ network     : TSI (host-proxied, policy-ready)\n"+
			"│ shell       : %s (interactive login, uid %d)\n"+
			"└──────────────────────────────────────────────────\n",
		o.root, o.pwd, orNone(o.overlay), orDefault(o.injectDir, "(none)"),
		orNone(o.hide), orNone(o.readonly),
		o.cpus, o.memMiB, o.shell, o.caller.uid)
}

func ensureTrailingSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "(none)"
	}
	return strings.Join(list, " ")
}
