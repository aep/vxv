// Command vxv isolates a Claude agent (or any process) inside a libkrun microVM.
//
// The filesystem the guest boots on is assembled on the HOST, inside a private
// mount namespace: the whole host tree is made read-only, the dirs in --overlay
// get an ephemeral overlayfs whose upper layer lives in host RAM and is
// DISCARDED on exit, the working directory is bound back in writable, and the
// .vxv.yaml policy (hide, readonly) goes on last. That view — and nothing else —
// is handed to the VM as its root over virtio-fs.
//
// Doing it there rather than in the guest is the point: the virtio-fs server is
// this process, so a guest that gets root cannot reach anything vxv itself
// cannot reach, and there is no in-guest mount for it to undo. Assembling the
// namespace needs CAP_SYS_ADMIN (sudo, or setcap cap_sys_admin+ep); vxv drops
// back to the calling user before the VM boots, so the file server answering the
// guest runs with exactly the caller's access.
//
// Networking uses libkrun's TSI (Transparent Socket Impersonation): every guest
// TCP/UDP flow is proxied through the host network stack over vsock, which is
// what lets us bolt host-side network policy on later.
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
