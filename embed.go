package main

import (
	"crypto/sha256"
	"encoding/hex"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// initBinary is the statically linked guest pid-1 shim (cmd/vxv-init), built
// with CGO_ENABLED=0 by the Makefile. libkrun's init can only exec a static
// binary as the first guest process, so we hand it this rather than the shell
// directly. See cmd/vxv-init for what it does once running.
//
//go:embed internal/initbin/vxv-init
var initBinary []byte

// materializeInit writes the embedded shim to a stable, content-addressed path
// on the host and returns it. Because the guest root IS the host root, any host
// path under --root is visible (read-only) to the guest and usable as the exec
// path. The file is content-hashed so upgrades don't collide and repeated runs
// reuse the same file instead of littering new ones.
func materializeInit() (string, error) {
	if len(initBinary) == 0 {
		return "", fmt.Errorf("embedded vxv-init is empty; build with `make` (it compiles the static guest shim first)")
	}
	sum := sha256.Sum256(initBinary)
	name := "vxv-init-" + hex.EncodeToString(sum[:8])

	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "vxv")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create shim cache dir: %w", err)
	}
	path := filepath.Join(dir, name)

	// Reuse an intact copy if it's already there.
	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(initBinary)) {
		return path, nil
	}

	// Write atomically: a temp file in the same dir, then rename into place.
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return "", fmt.Errorf("create shim temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(initBinary); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write shim: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", fmt.Errorf("chmod shim: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close shim: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("install shim: %w", err)
	}
	return path, nil
}
