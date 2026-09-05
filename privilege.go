package main

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// vxv needs CAP_SYS_ADMIN to unshare a mount namespace and assemble the guest's
// filesystem in it, and must not keep it afterwards: the virtio-fs server that
// answers the guest runs in this same process, so whatever it can reach, the
// guest can reach. Privileges are therefore dropped to the calling user in the
// window between "view built" and "VM booted".

// buildCaps is what assembling the guest view takes: CAP_SYS_ADMIN to mount,
// CAP_DAC_OVERRIDE to set up overlays over root-owned dirs and lay the guestfs
// tree into them, CAP_CHOWN so an overlaid dir keeps its real owner. All of it
// is gone before the VM boots.
const buildCaps = "cap_sys_admin,cap_dac_override,cap_chown"

// caller is who the session belongs to: the user vxv drops back to, and the
// identity the guest shell runs as. Under sudo that is not this process's own
// uid, so it is taken from SUDO_UID and the passwd database rather than from
// the environment, which sudo has already rewritten to root's.
type caller struct {
	uid, gid int
	groups   []int
	home     string
	shell    string
}

// hasCap reports whether the capability is in this process's effective set.
// Being root is not the same thing on paper but is on every path vxv cares
// about, so it counts.
func hasCap(cap uintptr) bool {
	if os.Geteuid() == 0 {
		return true
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return false
	}
	return data[0].Effective&(1<<cap) != 0
}

// hasSysAdmin reports whether this process can create a mount namespace, either
// as root or through a file capability (setcap cap_sys_admin+ep).
func hasSysAdmin() bool {
	return hasCap(unix.CAP_SYS_ADMIN)
}

// warnBuildCaps flags the capabilities that are not needed to *start* but that
// the namespace build needs to come out right. Without them a mount fails with a
// bare EACCES, or worse, quietly succeeds with the wrong ownership — so name
// them up front instead.
func warnBuildCaps() {
	missing := []string{}
	for _, c := range []struct {
		bit  uintptr
		name string
	}{
		{unix.CAP_DAC_OVERRIDE, "cap_dac_override"},
		{unix.CAP_CHOWN, "cap_chown"},
	} {
		if !hasCap(c.bit) {
			missing = append(missing, c.name)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "vxv: warning: running without %s: overlays over "+
		"root-owned dirs may fail to mount, guestfs injection may be skipped, and "+
		"overlaid dirs can end up owned by uid %d instead of their real owner\n",
		strings.Join(missing, " and "), os.Getuid())
}

// requireSysAdmin fails the launch with the two ways to fix it.
func requireSysAdmin() error {
	if hasSysAdmin() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		self = "$(command -v vxv)"
	}
	return fmt.Errorf("vxv needs CAP_SYS_ADMIN to build the guest filesystem view; "+
		"run it under sudo, or grant the binary the capabilities once:\n"+
		"    sudo setcap %s+ep %s", buildCaps, self)
}

// resolveCaller determines who the session belongs to. Running under sudo, the
// real user is SUDO_UID and their home and shell come from passwd, since sudo
// has replaced HOME and SHELL with root's. Running with a file capability (or
// as root outright), this process already carries the right identity.
func resolveCaller() *caller {
	c := &caller{
		uid:   os.Getuid(),
		gid:   os.Getgid(),
		home:  os.Getenv("HOME"),
		shell: os.Getenv("SHELL"),
	}
	if g, err := os.Getgroups(); err == nil {
		c.groups = g
	}

	uid, ok := sudoID("SUDO_UID")
	if os.Geteuid() != 0 || !ok {
		return c
	}
	c.uid = uid
	if gid, ok := sudoID("SUDO_GID"); ok {
		c.gid = gid
	}
	c.groups = nil
	c.home, c.shell = "", ""

	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return c
	}
	c.home = u.HomeDir
	if ids, err := u.GroupIds(); err == nil {
		for _, s := range ids {
			if n, err := strconv.Atoi(s); err == nil {
				c.groups = append(c.groups, n)
			}
		}
	}
	c.shell = loginShellOf(u.Username)
	return c
}

// drop hands the process to the calling user for good: after this it holds no
// capability the caller doesn't, which is what keeps the guest inside the
// caller's own reach. Root drops uid and gid (which clears its capabilities with
// them); a file-capability run keeps its uid and drops the capability itself.
func (c *caller) drop() error {
	if os.Geteuid() != 0 {
		return dropCaps()
	}
	if c.uid == 0 {
		// Launched from a root shell rather than via sudo: there is no
		// unprivileged caller to become.
		return nil
	}
	if len(c.groups) > 0 {
		if err := syscall.Setgroups(c.groups); err != nil {
			return fmt.Errorf("set groups: %w", err)
		}
	}
	if err := syscall.Setgid(c.gid); err != nil {
		return fmt.Errorf("set gid %d: %w", c.gid, err)
	}
	if err := syscall.Setuid(c.uid); err != nil {
		return fmt.Errorf("set uid %d: %w", c.uid, err)
	}
	return nil
}

// dropCaps empties the capability sets of a binary that was granted them by
// setcap. The bounding set is left alone: with nothing permitted there is
// nothing left to raise from.
func dropCaps() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("drop capabilities: %w", err)
	}
	return nil
}

// sudoID reads one of sudo's identity variables.
func sudoID(key string) (int, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// loginShellOf returns the login shell recorded in passwd for a user. os/user
// does not expose it, so read the field directly; an empty result just means
// the usual fallbacks apply.
func loginShellOf(username string) string {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 7 && fields[0] == username {
			return fields[6]
		}
	}
	return ""
}

// raiseNoFile lifts the soft fd limit to the hard one. libkrun's virtio-fs
// serves the guest from this process and pins one host fd per inode the guest
// has looked up, released only on FORGET; a build over a large tree exhausts a
// 1024-4096 soft limit within seconds and surfaces in the guest as EMFILE.
func raiseNoFile() {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil || lim.Cur >= lim.Max {
		return
	}
	lim.Cur = lim.Max
	_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &lim)
}
