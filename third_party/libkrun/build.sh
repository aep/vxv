#!/usr/bin/env bash
# Reproducibly build the patched libkrun.so.1 that vxv bundles.
#
# Why this exists: vxv makes directories like /home writable inside the guest via
# an ephemeral overlayfs (see cmd/vxv-init). overlayfs copy-up clones the lower
# inode's attribute flags with the FS_IOC_GETFLAGS ioctl. Stock libkrun's
# virtio-fs answers every unhandled ioctl with EOPNOTSUPP, and the guest kernel's
# overlayfs treats EOPNOTSUPP as fatal (it only tolerates ENOTTY/EINVAL) — so
# WITHOUT this patch every write to an overlaid dir fails with "Operation not
# supported". The patch changes that one default arm to return ENOTTY, the
# semantically correct errno for an unsupported ioctl, which overlayfs tolerates.
#
# Pin matches go-microvm@v0.0.40's versions.env (LIBKRUN_VERSION=v1.19.4). The
# build links against the system libkrunfw (v5.5.0), which must be installed.
#
# Usage: ./build.sh   (writes libkrun.so.1 next to this script)
set -euo pipefail

LIBKRUN_VERSION="${LIBKRUN_VERSION:-v1.19.4}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PATCH="$HERE/0001-virtiofs-ioctl-ENOTTY-for-overlayfs.patch"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

command -v cargo >/dev/null || { echo "error: cargo (Rust) is required" >&2; exit 1; }
[ -e /usr/lib/libkrunfw.so.5 ] || [ -e /usr/local/lib/libkrunfw.so.5 ] \
  || echo "warning: libkrunfw.so.5 not found in /usr/lib or /usr/local/lib; the link step may fail" >&2

echo ">> cloning libkrun $LIBKRUN_VERSION"
git clone --depth 1 --branch "$LIBKRUN_VERSION" https://github.com/containers/libkrun "$WORK/libkrun"

echo ">> applying $(basename "$PATCH")"
git -C "$WORK/libkrun" apply "$PATCH"

echo ">> building (default features: virtio-fs + vsock/TSI, which is all vxv uses)"
( cd "$WORK/libkrun" && cargo build --release )

cp -f "$WORK/libkrun/target/release/libkrun.so" "$HERE/libkrun.so.1"
chmod 755 "$HERE/libkrun.so.1"
echo ">> wrote $HERE/libkrun.so.1"
objdump -p "$HERE/libkrun.so.1" | grep -i soname || true
