package main

import (
	_ "embed"
	"fmt"
	"os"
)

// initBinary is the statically linked guest pid-1 shim (cmd/vxv-init), built
// with CGO_ENABLED=0 by the Makefile. libkrun's init can only exec a static
// binary as the first guest process, so we hand it this rather than the shell
// directly. See cmd/vxv-init for what it does once running.
//
//go:embed internal/initbin/vxv-init
var initBinary []byte

// writeShim drops the embedded shim into the namespace's scratch tmpfs, where
// the guest can exec it through the root share. It is written there rather than
// anywhere on the host because that tmpfs is ours: the file exists in RAM, only
// inside this mount namespace, and disappears with the VM.
func writeShim(path string) error {
	if len(initBinary) == 0 {
		return fmt.Errorf("embedded vxv-init is empty; build with `make` (it compiles the static guest shim first)")
	}
	if err := os.WriteFile(path, initBinary, 0o755); err != nil {
		return fmt.Errorf("write guest shim: %w", err)
	}
	// WriteFile honours umask on create; the shim must be executable by the
	// guest's init regardless of the umask vxv inherited.
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("chmod guest shim: %w", err)
	}
	return nil
}
