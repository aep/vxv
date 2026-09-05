# vxv — build the static guest shim first, then embed it into the host binary.
#
# The order matters: `vxv` embeds internal/initbin/vxv-init via //go:embed, so
# that file must exist before the main build runs. `make` (the default target)
# does both steps.

GO ?= go
PREFIX ?= /usr/local

SHIM := internal/initbin/vxv-init
BIN  := vxv

# vxv bundles a patched libkrun.so.1 (see third_party/libkrun): it answers
# unsupported virtio-fs ioctls with ENOTTY instead of EOPNOTSUPP. That was
# required back when the guest stacked overlayfs on top of virtio-fs; the
# overlays now live in the host mount namespace, so nothing in the guest issues
# that ioctl any more and stock libkrun may well do — the bundle stays until
# someone confirms that end to end. We do NOT overwrite the system libkrun: we
# ship our copy and point the binary at it with a RUNPATH, which ld.so searches
# before the default /usr/lib.
#
# The repo binary finds it $ORIGIN-relative. The INSTALLED one cannot: it carries
# CAP_SYS_ADMIN, and ld.so refuses to expand $ORIGIN for a binary with file
# capabilities, so that build gets an absolute RUNPATH instead.
LIBKRUN_SO    := third_party/libkrun/libkrun.so.1
RPATH         := -Wl,-rpath,\$$ORIGIN/third_party/libkrun
INSTALL_RPATH := -Wl,-rpath,$(PREFIX)/lib

.PHONY: all
all: $(BIN)

# The guest shim MUST be statically linked (CGO_ENABLED=0): libkrun can only
# exec a static binary as the first process inside the guest.
$(SHIM): cmd/vxv-init/main.go
	mkdir -p $(dir $(SHIM))
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o $(SHIM) ./cmd/vxv-init

# Rebuild the patched libkrun from source (needs Rust + system libkrunfw). The
# prebuilt .so is committed, so this is only needed to regenerate it.
.PHONY: libkrun
libkrun:
	third_party/libkrun/build.sh

$(LIBKRUN_SO):
	third_party/libkrun/build.sh

# The host binary links libkrun, so it needs cgo. The RUNPATH makes it load the
# bundled patched libkrun.so.1 rather than the system one.
$(BIN): $(SHIM) $(wildcard *.go) $(LIBKRUN_SO)
	CGO_ENABLED=1 $(GO) build -trimpath \
		-ldflags "-extldflags '$(RPATH)'" -o $(BIN) .

.PHONY: install
install: $(SHIM) $(LIBKRUN_SO)
	CGO_ENABLED=1 $(GO) build -trimpath \
		-ldflags "-extldflags '$(INSTALL_RPATH)'" -o $(BIN).install .
	install -Dm755 $(BIN).install $(DESTDIR)$(PREFIX)/bin/$(BIN)
	rm -f $(BIN).install
	install -Dm755 $(LIBKRUN_SO) $(DESTDIR)$(PREFIX)/lib/libkrun.so.1
	# vxv assembles the guest's filesystem in a mount namespace, which needs
	# CAP_SYS_ADMIN; it drops back to the calling user before the VM boots.
	# Granting it here is what lets users run vxv without sudo.
	setcap cap_sys_admin,cap_dac_override,cap_chown+ep $(DESTDIR)$(PREFIX)/bin/$(BIN)
	# guestfs is looked up next to the binary; ship it there so injected
	# defaults (e.g. Claude auto mode) apply to the installed vxv too.
	rm -rf $(DESTDIR)$(PREFIX)/bin/guestfs
	cp -a guestfs $(DESTDIR)$(PREFIX)/bin/guestfs

.PHONY: clean
clean:
	rm -f $(BIN) $(BIN).install $(SHIM)

.PHONY: test
test: $(BIN)
	$(GO) vet ./...
	$(GO) test ./...
