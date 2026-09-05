package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// The filesystem the guest sees is assembled here, on the host, inside a private
// mount namespace, and handed to libkrun as a single virtio-fs share. Nothing is
// left for the guest to enforce: read-only means the host kernel answers EROFS,
// hidden means the host serves an empty inode. A guest that gets root can remount
// whatever it likes and still reach nothing new, because its virtio-fs server —
// this process — cannot reach it either.
//
// Building the namespace needs CAP_SYS_ADMIN; serving files out of it must not
// have it. So vxv runs in two stages: this process unshares the namespace,
// builds the view, then drops to the calling user before libkrun boots.

// stageEnv marks the re-executed child that owns the private mount namespace.
// Only its presence matters; the value is never read.
const stageEnv = "VXV_MOUNTNS"

// storeName is the directory, inside the scratch tmpfs, holding everything the
// namespace needs to exist but the guest has no business seeing: overlay upper
// layers, the working-directory stash, the placeholders that mask hidden files.
// It is covered by an empty tmpfs once the view is built.
const storeName = "vxv"

// shimName is the guest entrypoint, written into the scratch tmpfs so it lives
// in guest memory rather than on the host disk.
const shimName = "vxv-init"

// reexec re-runs this binary in a fresh mount namespace and waits for it. The
// unshare has to happen between fork and exec: unshare(CLONE_NEWNS) moves only
// the calling thread, while a child that unshares before exec puts the whole
// process — including every thread libkrun starts later, virtio-fs workers among
// them — in the new namespace. Go additionally marks / MS_REC|MS_PRIVATE there,
// so nothing mounted below escapes back to the host.
//
// It re-execs by resolved path, NOT by /proc/self/exe, which would quietly cost
// us the capability we are about to need. That magic link resolves to the
// vfsmount recorded at the original exec — a mount belonging to the namespace we
// just left — and the kernel ignores file capabilities on a file whose mount is
// not in the caller's own mount namespace (mnt_may_suid -> check_mnt). The exec
// still succeeds, so a setcap'd vxv would arrive in its new namespace with
// nothing to mount with. The same path resolves fine in the new namespace, which
// is a copy of the old one.
func reexec() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the vxv binary to re-exec: %w", err)
	}
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Env = append(os.Environ(), stageEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}

	// No Setpgid: the child stays in the terminal's foreground process group, so
	// libkrun keeps receiving SIGWINCH and the keyboard signals directly. This
	// wrapper catches those same signals only to survive them — catching rather
	// than ignoring, because SIG_IGN would be inherited through the exec and
	// leave the child unable to be interrupted at all. Everything the terminal
	// did not broadcast is passed on, so killing the wrapper kills the VM: the
	// namespace lives only as long as the child, but the child would happily
	// keep running without a parent.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP,
		syscall.SIGTERM, syscall.SIGHUP)

	// libkrun puts the terminal in raw mode down there. Snapshot the settings so
	// a child that dies without restoring them doesn't leave a mangled shell.
	state, _ := term.GetState(int(os.Stdin.Fd()))

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("re-exec into mount namespace: %w", err)
	}
	go func() {
		for sig := range sigs {
			switch sig {
			case syscall.SIGTERM, syscall.SIGHUP:
				_ = cmd.Process.Signal(sig)
			}
		}
	}()
	err = cmd.Wait()
	signal.Stop(sigs)
	close(sigs)
	if state != nil {
		_ = term.Restore(int(os.Stdin.Fd()), state)
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			os.Exit(128 + int(ws.Signal()))
		}
		os.Exit(exit.ExitCode())
	}
	if err != nil {
		return fmt.Errorf("re-exec into mount namespace: %w", err)
	}
	os.Exit(0)
	return nil
}

// buildGuestRoot assembles the guest's filesystem inside our private mount
// namespace and returns the host path of the guest entrypoint shim. Order is the
// whole design:
//
//  1. the entire tree goes read-only, so anything not named below is immutable;
//  2. a scratch tmpfs supplies the only writable storage we own;
//  3. the working directory is stashed aside before an overlay can shadow it;
//  4. the --overlay dirs get a tmpfs-backed upper layer (writable, discarded);
//  5. the working directory is bound back on top, so it stays the real host dir;
//  6. the guestfs tree is laid into the overlays while we are still privileged;
//  7. readonly, then hide, go on last so no later layer can undo them;
//  8. the store is covered, and the shim written where the guest can exec it.
func (o *options) buildGuestRoot() (string, error) {
	// 1. Everything read-only, in one atomic pass over every mount in the tree.
	// Later steps punch the writable holes back through. Device nodes are
	// unaffected — a read-only mount blocks writes to the filesystem, not opens
	// of a character device — so /dev/kvm still works after this.
	if err := unix.MountSetattr(unix.AT_FDCWD, "/", unix.AT_RECURSIVE,
		&unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
		return "", fmt.Errorf("make the host tree read-only (needs Linux 5.12+): %w", err)
	}

	// 2. Scratch space. A tmpfs over <root>/run needs no writable host directory
	// to land on, which matters when vxv runs with CAP_SYS_ADMIN alone rather
	// than as root. It also gives the guest the empty /run an ephemeral machine
	// wants; everything below it lives in host RAM and dies with this process.
	store := filepath.Join(o.root, "run")
	if err := unix.Mount("tmpfs", store, "tmpfs", 0, "mode=0755"); err != nil {
		return "", fmt.Errorf("mount scratch tmpfs on %s (--root must contain a run directory): %w", store, err)
	}
	base := filepath.Join(store, storeName)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("create scratch store: %w", err)
	}

	// 3. Stash the working directory. It is bound aside *before* the overlays go
	// up, because a $PWD under an overlaid dir (~/src under /home) would
	// otherwise be shadowed by that overlay and every write to it would end up
	// ephemeral. The stash keeps a handle on the real host directory.
	stash := filepath.Join(base, "pwd")
	if err := os.Mkdir(stash, 0o755); err != nil {
		return "", fmt.Errorf("create pwd stash: %w", err)
	}
	if err := bindWritable(o.pwd, stash); err != nil {
		return "", fmt.Errorf("stash %s: %w", o.pwd, err)
	}

	// 4. Ephemeral writable dirs.
	o.overlay = o.mountOverlays(base)

	// 5. The real working directory, back on top of whatever now covers it.
	if err := bindWritable(stash, o.pwd); err != nil {
		return "", fmt.Errorf("make %s writable: %w", o.pwd, err)
	}

	// 6. Guest-only config, written while we still have the privileges for it.
	o.inject()

	// 7. Policy. Last, so an overlay or the writable $PWD cannot outrank it.
	for _, path := range o.readonly {
		if _, err := os.Stat(path); err != nil {
			continue // not on this host; nothing to protect
		}
		if err := bindReadOnly(path, path); err != nil {
			fmt.Fprintf(os.Stderr, "vxv: warning: %s stays writable: %v\n", path, err)
		}
	}
	if err := o.hidePaths(base); err != nil {
		return "", err
	}

	// 8. Cover the store: the guest has no use for the overlay upper layers or
	// the mask placeholders, and the mounts above hold their own references, so
	// hiding the directory they live in costs nothing.
	if err := unix.Mount("tmpfs", base, "tmpfs", unix.MS_RDONLY, "mode=0000"); err != nil {
		fmt.Fprintf(os.Stderr, "vxv: warning: cannot hide %s from the guest: %v\n", base, err)
	}

	// 9. The entrypoint, in the scratch tmpfs so no host directory is written.
	shim := filepath.Join(store, shimName)
	if err := writeShim(shim); err != nil {
		return "", err
	}
	return shim, nil
}

// mountOverlays makes each --overlay directory writable without touching the
// host: an overlayfs whose lower layer is the (now read-only) host directory and
// whose upper and work layers live on the scratch tmpfs. Writes copy up into
// host RAM and are discarded when this process exits. A directory that isn't
// there, or that refuses to overlay, is left read-only with a warning — the
// session is still usable. It returns the ones that took, which is what the
// banner reports: a dir listed as writable that silently isn't would be worse
// than the warning.
func (o *options) mountOverlays(base string) []string {
	store := filepath.Dir(base)
	var mounted []string
	for i, target := range o.overlay {
		fi, err := os.Stat(target)
		if err != nil || !fi.IsDir() {
			continue
		}
		if target == store {
			// Overlaying the scratch tmpfs would bury the upper layers we are
			// about to point at it.
			fmt.Fprintf(os.Stderr, "vxv: warning: not overlaying %s: vxv keeps its own scratch tmpfs there\n", target)
			continue
		}
		slot := filepath.Join(base, "overlay", strconv.Itoa(i))
		upper := filepath.Join(slot, "upper")
		work := filepath.Join(slot, "work")
		if err := os.MkdirAll(upper, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "vxv: warning: overlay skip %s: %v\n", target, err)
			continue
		}
		if err := os.MkdirAll(work, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "vxv: warning: overlay skip %s: %v\n", target, err)
			continue
		}
		// The merged directory takes its owner and mode from the upper layer, so
		// mirror the lower's: an overlaid /etc must still look like root's /etc,
		// or the guest would find it writable by whoever launched vxv.
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(upper, int(st.Uid), int(st.Gid))
		}
		_ = os.Chmod(upper, fi.Mode().Perm())

		// Kernel defaults for the rest: the lower layer is an ordinary host
		// filesystem now, so overlayfs needs none of the workarounds it took to
		// stack this on virtio-fs from inside the guest.
		//
		// xino is asked for explicitly because this overlay is re-exported over
		// virtio-fs. Without it a file that has been copied up reports its upper
		// (tmpfs) inode number while an untouched one reports its lower number,
		// under one st_dev — so the two can collide, and the guest's file server
		// keys its inode table on exactly that pair. Filesystems that cannot
		// spare the bits refuse the option outright, hence the retry.
		opts := "lowerdir=" + target + ",upperdir=" + upper + ",workdir=" + work
		err = unix.Mount("overlay", target, "overlay", 0, opts+",xino=on")
		if err != nil {
			err = unix.Mount("overlay", target, "overlay", 0, opts)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "vxv: warning: %s stays read-only (overlay mount failed): %v\n", target, err)
			continue
		}
		mounted = append(mounted, target)
	}
	return mounted
}

// hidePaths masks each hidden path with an empty, inaccessible placeholder from
// the scratch tmpfs: a file for a file, a directory for a directory, since a
// bind mount needs matching inode types. The mask is empty rather than merely
// unreadable, so it gives up nothing even to a guest running as root — the
// contents are not behind a permission check, they are not there.
func (o *options) hidePaths(base string) error {
	if len(o.hide) == 0 {
		return nil
	}
	dir := filepath.Join(base, "mask")
	if err := os.Mkdir(dir, 0o755); err != nil {
		return fmt.Errorf("create mask store: %w", err)
	}
	emptyFile := filepath.Join(dir, "file")
	emptyDir := filepath.Join(dir, "dir")
	f, err := os.OpenFile(emptyFile, os.O_CREATE|os.O_WRONLY, 0o000)
	if err != nil {
		return fmt.Errorf("create mask file: %w", err)
	}
	f.Close()
	if err := os.Mkdir(emptyDir, 0o000); err != nil {
		return fmt.Errorf("create mask dir: %w", err)
	}

	for _, path := range o.hide {
		fi, err := os.Stat(path)
		if err != nil {
			continue // not on this host; nothing to hide
		}
		src := emptyFile
		if fi.IsDir() {
			src = emptyDir
		}
		if err := bindReadOnly(src, path); err != nil {
			fmt.Fprintf(os.Stderr, "vxv: warning: cannot hide %s: %v\n", path, err)
		}
	}
	return nil
}

// inject lays the guestfs tree into the guest filesystem, mirroring each entry's
// relative path under / — a host file at <dir>/etc/foo/bar.json is written to
// /etc/foo/bar.json. Destinations are the overlays mounted above, so injected
// files exist only for this session and the host copies are never touched.
// Failures are per-entry warnings: a destination outside any overlay is
// read-only, which means that one file is skipped, not that the launch fails.
func (o *options) inject() {
	if o.injectDir == "" {
		return
	}
	root := os.DirFS(o.injectDir)
	err := fs.WalkDir(root, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || rel == "." {
			return nil
		}
		dst := filepath.Join(o.root, rel)
		src := filepath.Join(o.injectDir, rel)
		info, e := d.Info()
		if e != nil {
			fmt.Fprintf(os.Stderr, "vxv: warning: inject %s: %v\n", dst, e)
			return nil
		}
		switch {
		case d.IsDir():
			// Only create dirs the guest lacks — and mirror ownership and mode
			// only on those. Never re-own one that already exists (/etc), or a
			// system directory would end up belonging to the guestfs tree's owner.
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
					// Skip the subtree rather than reporting the same failure
					// once per file underneath it.
					fmt.Fprintf(os.Stderr, "vxv: warning: inject %s: %v\n", dst, err)
					return fs.SkipDir
				}
				chownLike(dst, info)
				_ = os.Chmod(dst, info.Mode().Perm())
			}
		case d.Type()&os.ModeSymlink != 0:
			if target, e := os.Readlink(src); e == nil {
				_ = os.Remove(dst)
				if e := os.Symlink(target, dst); e != nil {
					fmt.Fprintf(os.Stderr, "vxv: warning: inject %s: %v\n", dst, e)
				} else {
					chownLike(dst, info) // Lchown: the link, not its target
				}
			}
		case d.Type().IsRegular():
			if e := copyInto(src, dst, info); e != nil {
				fmt.Fprintf(os.Stderr, "vxv: warning: inject %s: %v\n", dst, e)
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vxv: warning: inject %s: %v\n", o.injectDir, err)
	}
}

// copyInto writes src's contents to dst, mirroring the source file's mode and
// (uid, gid). The guest shares the host's uid space, so a guestfs file owned by
// the user appears user-owned in the guest and a root-owned one stays root's.
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

// chownLike sets dst's owner to match the source's. Lchown so a symlink itself
// is re-owned rather than its target; best-effort, since the file is already in
// place and usable either way.
func chownLike(dst string, info fs.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(dst, int(st.Uid), int(st.Gid))
	}
}

// bindWritable binds src onto dst and clears the read-only flag the clone
// inherited from step 1. MS_REMOUNT|MS_BIND replaces the mount's flags with
// exactly what is passed, so an empty flag set is a writable mount.
func bindWritable(src, dst string) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	return unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT, "")
}

// bindReadOnly binds src onto dst and remounts it read-only. Binding a file over
// itself is how a single file is made immutable while the directory around it
// stays writable.
func bindReadOnly(src, dst string) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	return unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
}
