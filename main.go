// Command vxv isolates a Claude agent (or any process) inside a libkrun microVM.
//
// The guest boots with the host's filesystem mounted READ-ONLY as its root via
// virtiofs, so the agent can read anything on the machine but write nothing —
// except the current working directory, which is exposed as a separate
// read-write virtiofs share mounted over its real path. A shell is opened in
// that directory. Networking uses libkrun's TSI (Transparent Socket
// Impersonation): every guest TCP/UDP flow is proxied through the host network
// stack over vsock, which is what lets us bolt host-side network policy on
// later.
//
// Directories like /home and /etc (see --overlay) are made writable inside the
// guest with an ephemeral overlayfs: an in-guest tmpfs supplies the upper layer
// over the read-only host dir, so writes land in guest memory and are DISCARDED
// on shutdown while the host stays pristine. This requires the patched libkrun
// bundled in third_party/libkrun — stock libkrun's virtio-fs returns EOPNOTSUPP
// for the FS_IOC_GETFLAGS ioctl that overlayfs copy-up needs, which the guest
// kernel treats as fatal; the patch returns ENOTTY instead (which overlayfs
// tolerates). The Makefile points the binary at the bundled copy via RUNPATH.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "vxv: %v\n", err)
		os.Exit(1)
	}
}
