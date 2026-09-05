package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// nsEnv carries the fixture directory into the re-executed test.
const nsEnv = "VXV_TEST_FIXTURE"

// TestGuestRootView builds a real guest view and checks what a guest could do
// with it. Assembling one needs CAP_SYS_ADMIN and a mount namespace to throw
// away afterwards, so the test re-runs itself under `unshare -Ur --mount`: the
// mount plumbing is the same there as it is under the CAP_SYS_ADMIN vxv
// actually requires.
func TestGuestRootView(t *testing.T) {
	fixture := os.Getenv(nsEnv)
	if fixture == "" {
		checkGuestRootView(t, buildFixture(t))
		return
	}
	verifyGuestRootView(t, fixture)
}

// buildFixture lays out a miniature host to serve as the guest's root: a working
// directory holding a secret and a protected file, a directory standing in for
// /etc, the mountpoint vxv needs for its scratch tmpfs, and a guestfs tree to
// inject.
func buildFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"root", "root/work", "root/etc", "root/run", "inject", "inject/etc"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"root/work/secret.env":  "TOKEN=hunter2",
		"root/work/config.yaml": "keep: me",
		"inject/etc/vxv.conf":   "injected",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// checkGuestRootView re-runs this one test inside a namespace it is allowed to
// mount in, then checks the host it was supposed to leave alone.
func checkGuestRootView(t *testing.T, fixture string) {
	t.Helper()
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare(1) not available")
	}
	// The test binary's own path: /proc/self/exe would resolve to unshare(1) by
	// the time it execs.
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	cmd := exec.Command("unshare", "-Ur", "--mount", "--",
		self, "-test.run=TestGuestRootView", "-test.v")
	cmd.Env = append(os.Environ(), nsEnv+"="+fixture)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("namespaced run failed: %v\n%s", err, out)
	}
	if testing.Verbose() {
		t.Log("\n" + string(out))
	}

	// Back on the host: the session wrote where it was allowed to and nowhere
	// else. Both halves matter — a view that protected everything equally would
	// pass the guest-side checks too.
	if _, err := os.Stat(filepath.Join(fixture, "root", "work", "output")); err != nil {
		t.Errorf("working-directory writes did not reach the host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "escape")); err == nil {
		t.Error("a write escaped the read-only tree")
	}
	if _, err := os.Stat(filepath.Join(fixture, "root", "etc", "vxv.conf")); err == nil {
		t.Error("injected config was written to the host, not to the overlay")
	}
	if data, _ := os.ReadFile(filepath.Join(fixture, "root", "work", "secret.env")); string(data) != "TOKEN=hunter2" {
		t.Errorf("hidden file was altered on the host: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(fixture, "root", "work", "config.yaml")); string(data) != "keep: me" {
		t.Errorf("readonly file was altered on the host: %q", data)
	}
}

// verifyGuestRootView is the half that runs inside the namespace.
func verifyGuestRootView(t *testing.T, fixture string) {
	root := filepath.Join(fixture, "root")
	pwd := filepath.Join(root, "work")
	etc := filepath.Join(root, "etc")
	secret := filepath.Join(pwd, "secret.env")
	protected := filepath.Join(pwd, "config.yaml")

	// The stand-in for /etc gets its own tmpfs first. An unprivileged user
	// namespace refuses to overlay a filesystem it does not own, which the
	// CAP_SYS_ADMIN vxv runs with does not care about — handing the lower layer
	// to this namespace is what lets the test exercise the overlay path at all.
	if err := unix.Mount("tmpfs", etc, "tmpfs", 0, "mode=0755"); err != nil {
		t.Fatalf("mount fixture tmpfs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(etc, "motd"), []byte("host copy"), 0o644); err != nil {
		t.Fatal(err)
	}

	o := &options{
		root:      root,
		pwd:       pwd,
		overlay:   []string{etc},
		injectDir: filepath.Join(fixture, "inject"),
		hide:      []string{secret},
		readonly:  []string{protected},
	}
	shim, err := o.buildGuestRoot()
	if err != nil {
		t.Fatalf("buildGuestRoot: %v", err)
	}

	// The tree outside the working directory is read-only.
	if err := os.WriteFile(filepath.Join(fixture, "escape"), []byte("x"), 0o644); err == nil {
		t.Error("wrote outside the working directory; the tree is not read-only")
	}

	// The working directory is not.
	if err := os.WriteFile(filepath.Join(pwd, "output"), []byte("x"), 0o644); err != nil {
		t.Errorf("working directory is not writable: %v", err)
	}

	// A hidden file is empty, not merely unreadable: there is nothing behind it
	// to leak, whatever credentials the reader has.
	if data, err := os.ReadFile(secret); err == nil && len(data) != 0 {
		t.Errorf("hidden file still readable: %q", data)
	}

	// A protected file keeps its contents and refuses writes.
	if data, err := os.ReadFile(protected); err != nil || string(data) != "keep: me" {
		t.Errorf("readonly file should stay readable, got %q (%v)", data, err)
	}
	if err := os.WriteFile(protected, []byte("clobbered"), 0o644); err == nil {
		t.Error("readonly file was writable")
	}

	// An overlaid directory takes writes even though its lower layer is now
	// read-only: they copy up onto the ephemeral upper layer.
	motd := filepath.Join(etc, "motd")
	if err := os.WriteFile(motd, []byte("guest copy"), 0o644); err != nil {
		t.Errorf("overlay is not writable: %v", err)
	} else if data, _ := os.ReadFile(motd); string(data) != "guest copy" {
		t.Errorf("overlay write did not take: %q", data)
	}

	// The guestfs tree landed on that same ephemeral layer.
	if data, err := os.ReadFile(filepath.Join(etc, "vxv.conf")); err != nil || string(data) != "injected" {
		t.Errorf("guestfs tree was not injected: %q (%v)", data, err)
	}

	// The entrypoint is where the guest expects to exec it from, and the store
	// backing all of the above is not browsable.
	if fi, err := os.Stat(shim); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("guest shim %s is not executable: %v", shim, err)
	}
	if guest := o.guestPath(shim); guest != "/run/"+shimName {
		t.Errorf("guest would exec %s, not /run/%s", guest, shimName)
	}
	entries, err := os.ReadDir(filepath.Join(root, "run", storeName))
	if err == nil && len(entries) != 0 {
		t.Errorf("scratch store is visible to the guest: %v", entries)
	}
}
