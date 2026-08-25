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

// libkrun virtiofs tags.
const (
	// rootTag is libkrun's reserved tag for the guest root filesystem
	// (KRUN_FS_ROOT_TAG in libkrun.h). Adding it explicitly via AddVirtioFS3 is
	// the documented way to obtain a read-only root.
	rootTag = "/dev/root"
	// pwdTag identifies the writable working-directory share; the guest
	// bootstrap mounts it over the PWD's real path.
	pwdTag = "vxvpwd"
)

type options struct {
	cpus    uint8
	memMiB  uint32
	root    string
	pwd     string
	shell   string
	env     []string // extra KEY=VALUE pairs from -e
	tmpfs   bool
	overlay []string // dirs made writable via ephemeral overlayfs
	inject  string   // host dir whose tree is laid into the guest fs at boot
	debug   bool
}

// defaultInjectDir is the name of the guest-file tree looked for next to the vxv
// binary when --inject is not given. Ship files under it mirroring their guest
// paths, e.g. guestfs/etc/claude-code/managed-settings.d/10-auto.json lands at
// /etc/claude-code/managed-settings.d/10-auto.json inside the VM.
const defaultInjectDir = "guestfs"

func newRootCmd() *cobra.Command {
	o := &options{}
	var cpus, mem uint

	cmd := &cobra.Command{
		Use:   "vxv [flags]",
		Short: "Open an isolated login shell in a libkrun microVM",
		Long: "vxv isolates a Claude agent (or any interactive session) inside a libkrun microVM.\n\n" +
			"The host filesystem is mounted READ-ONLY as the guest root via virtiofs; only the\n" +
			"working directory is writable (a separate virtiofs share mounted over its real path).\n" +
			"Directories like /home and /etc are made writable inside the guest via an ephemeral\n" +
			"overlayfs (--overlay) whose changes live in guest memory and are DISCARDED on exit,\n" +
			"so the host stays pristine. An interactive login shell is opened with a fresh\n" +
			"environment. Networking uses\n" +
			"TSI, proxying all guest traffic through the host network stack so policy can be\n" +
			"enforced host-side.",
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
	f.UintVar(&mem, "mem", 2048, "guest RAM in MiB")
	f.StringVar(&o.root, "root", "/", "host directory exposed READ-ONLY as guest root")
	f.StringVar(&o.pwd, "pwd", "", "directory exposed READ-WRITE (default: current directory)")
	f.StringVar(&o.shell, "shell", "", "login shell to run (default: $SHELL, else /bin/bash, else /bin/sh)")
	f.StringArrayVarP(&o.env, "env", "e", nil, "extra environment variable KEY=VALUE (repeatable)")
	f.BoolVar(&o.tmpfs, "tmpfs", true, "mount ephemeral in-guest tmpfs on /tmp, /var/tmp, /run, /dev/shm")
	f.StringSliceVar(&o.overlay, "overlay",
		[]string{"/home", "/root", "/etc", "/opt", "/srv", "/usr", "/var"},
		"dirs made writable via ephemeral overlayfs; writes are discarded on exit (empty disables)")
	f.StringVar(&o.inject, "inject", "",
		"host dir whose tree is laid into the guest fs at boot (default: '"+defaultInjectDir+"' next to the vxv binary if present); files land on the ephemeral layer, discarded on exit")
	f.BoolVar(&o.debug, "debug", false, "enable verbose libkrun logging")
	return cmd
}

func (o *options) execute() error {
	if err := o.resolve(); err != nil {
		return err
	}

	// Preflight: the environment must be able to run a microVM at all.
	if !krun.IsAvailable() {
		return fmt.Errorf("libkrun reports the platform is unavailable (is libkrun installed and KVM accessible?)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not accessible: %w", err)
	}

	if o.debug {
		_ = krun.SetLogLevel(krun.LogLevelDebug)
	} else {
		_ = krun.SetLogLevel(krun.LogLevelError)
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

	// Root filesystem: the host's `/` (or --root), exposed READ-ONLY. shmSize 0
	// disables the DAX window; the final argument enforces read-only in virtiofs.
	if err := ctx.AddVirtioFS3(rootTag, o.root, 0, true); err != nil {
		return fmt.Errorf("add read-only root virtiofs (%s): %w", o.root, err)
	}

	// Working directory: a second, WRITABLE virtiofs share. The guest bootstrap
	// mounts it over o.pwd so writes land back on the host at that path.
	if err := ctx.AddVirtioFS(pwdTag, o.pwd); err != nil {
		return fmt.Errorf("add writable pwd virtiofs (%s): %w", o.pwd, err)
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

	// The guest entrypoint is our statically linked shim (libkrun can only exec
	// a static binary as the first guest process). It sets up the mounts and
	// then execs the interactive login shell. It lives on the host and, since
	// the guest root IS the host root, is visible read-only inside the guest.
	shim, err := materializeInit()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(shim, ensureTrailingSlash(o.root)) && o.root != "/" {
		return fmt.Errorf("guest shim %s is not under --root %s, so the guest cannot exec it; use --root / or a root that contains %s", shim, o.root, shim)
	}
	if err := ctx.SetExec(shim, []string{shim}, envp); err != nil {
		return fmt.Errorf("set exec: %w", err)
	}

	o.printBanner()

	// Hands control to the VMM. On success this exits the process with the
	// guest's exit code; it only returns here if boot configuration failed.
	if err := ctx.StartEnter(); err != nil {
		return fmt.Errorf("start microVM: %w", err)
	}
	return nil
}

// resolve normalizes paths and derives defaults.
func (o *options) resolve() error {
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

	if rootAbs, err := filepath.Abs(o.root); err == nil {
		o.root = rootAbs
	}

	if o.shell == "" {
		o.shell = defaultShell()
	}

	o.overlay = normalizeOverlays(o.overlay)

	if err := o.resolveInject(); err != nil {
		return err
	}
	return nil
}

// resolveInject locates the guest-file tree. An explicit --inject must exist; the
// implicit default (a "guestfs" dir next to the binary) is used only if present.
// Either way the directory must live under --root, since the guest reaches it by
// reading its host path through the read-only root share.
func (o *options) resolveInject() error {
	explicit := o.inject != ""
	if o.inject == "" {
		self, err := os.Executable()
		if err != nil {
			return nil // can't locate the binary; no default injection
		}
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		o.inject = filepath.Join(filepath.Dir(self), defaultInjectDir)
	}

	abs, err := filepath.Abs(o.inject)
	if err != nil {
		return fmt.Errorf("resolve --inject: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	o.inject = abs

	fi, err := os.Stat(o.inject)
	if err != nil || !fi.IsDir() {
		if explicit {
			return fmt.Errorf("--inject %q is not a directory", o.inject)
		}
		o.inject = "" // no default tree present; nothing to inject
		return nil
	}
	if o.root != "/" && !strings.HasPrefix(ensureTrailingSlash(o.inject), ensureTrailingSlash(o.root)) {
		if explicit {
			return fmt.Errorf("--inject %s is not under --root %s, so the guest cannot read it", o.inject, o.root)
		}
		o.inject = ""
	}
	return nil
}

// normalizeOverlays cleans the overlay dir list into absolute, deduplicated,
// comma-free paths. Entries that aren't absolute or contain a comma are dropped:
// the list is joined with commas into VXV_OVERLAY, and the guest splits on both
// commas and the overlayfs option separator, so neither can appear in a path.
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

// defaultShell picks the user's shell, falling back to common ones. Because the
// guest root IS the host root, any host shell path is valid inside the guest.
func defaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		if _, err := os.Stat(s); err == nil {
			return s
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
// The identity variables (VXV_UID/GID/GROUPS) are taken from this process's own
// credentials — the real caller — and are appended LAST so a -e cannot shadow
// them. Combined with rejecting any -e VXV_* key, this guarantees a user running
// vxv cannot make the guest drop to a different user's uid.
func (o *options) buildEnv() ([]string, error) {
	tmpfs := "0"
	if o.tmpfs {
		tmpfs = "1"
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=" + orDefault(os.Getenv("TERM"), "xterm-256color"),
		"HOME=" + orDefault(os.Getenv("HOME"), "/root"),
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
		"VXV_PWD="+o.pwd,
		"VXV_TAG="+pwdTag,
		"VXV_SHELL="+o.shell,
		"VXV_LOGIN="+loginArgs(o.shell),
		"VXV_TMPFS="+tmpfs,
		"VXV_OVERLAY="+strings.Join(o.overlay, ","),
		"VXV_INJECT="+o.inject,
		"VXV_UID="+strconv.Itoa(os.Getuid()),
		"VXV_GID="+strconv.Itoa(os.Getgid()),
		"VXV_GROUPS="+groupList(),
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
func groupList() string {
	gids, err := os.Getgroups()
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(gids))
	for _, g := range gids {
		parts = append(parts, strconv.Itoa(g))
	}
	return strings.Join(parts, ",")
}

// terminalSize returns the host terminal's dimensions, or a sane default when
// stdout is not a terminal.
func terminalSize() (cols, rows int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
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
	overlay := "(none)"
	if len(o.overlay) > 0 {
		overlay = strings.Join(o.overlay, " ")
	}
	inject := "(none)"
	if o.inject != "" {
		inject = o.inject
	}
	fmt.Fprintf(os.Stderr,
		"┌─ vxv microVM ────────────────────────────────────\n"+
			"│ root (ro)   : %s\n"+
			"│ pwd  (rw)   : %s\n"+
			"│ overlay (∅) : %s\n"+
			"│ inject      : %s\n"+
			"│ resources   : %d vCPU, %d MiB\n"+
			"│ network     : TSI (host-proxied, policy-ready)\n"+
			"│ shell       : %s (interactive login)\n"+
			"└──────────────────────────────────────────────────\n",
		o.root, o.pwd, overlay, inject, o.cpus, o.memMiB, o.shell)
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
