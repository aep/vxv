# vxv — build the static guest shim first, then embed it into the host binary.
#
# The order matters: `vxv` embeds internal/initbin/vxv-init via //go:embed, so
# that file must exist before the main build runs. `make` (the default target)
# does both steps.

GO ?= go
PREFIX ?= /usr/local

SHIM := internal/initbin/vxv-init
BIN  := vxv

# vxv bundles a patched libkrun.so.1 (see third_party/libkrun): it makes the
# guest's virtio-fs answer unsupported ioctls with ENOTTY instead of EOPNOTSUPP,
# without which overlayfs copy-up — and therefore every write to a writable
# overlaid dir like /home — fails. We do NOT overwrite the system libkrun; we
# ship our copy and point the binary at it with an $ORIGIN-relative RUNPATH,
# which ld.so searches before the default /usr/lib. The two entries cover both
# layouts: running ./vxv from the repo (lib under third_party/libkrun) and an
# installed tree ($PREFIX/bin/vxv with the lib in $PREFIX/lib).
LIBKRUN_SO := third_party/libkrun/libkrun.so.1
RPATH      := -Wl,-rpath,\$$ORIGIN/third_party/libkrun -Wl,-rpath,\$$ORIGIN/../lib

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
$(BIN): $(SHIM) main.go root.go embed.go $(LIBKRUN_SO)
	CGO_ENABLED=1 $(GO) build -trimpath \
		-ldflags "-extldflags '$(RPATH)'" -o $(BIN) .

.PHONY: install
install: $(BIN)
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/bin/$(BIN)
	install -Dm755 $(LIBKRUN_SO) $(DESTDIR)$(PREFIX)/lib/libkrun.so.1
	# guestfs is looked up next to the binary; ship it there so injected
	# defaults (e.g. Claude auto mode) apply to the installed vxv too.
	rm -rf $(DESTDIR)$(PREFIX)/bin/guestfs
	cp -a guestfs $(DESTDIR)$(PREFIX)/bin/guestfs

.PHONY: clean
clean:
	rm -f $(BIN) $(SHIM)

.PHONY: test
test: $(BIN)
	$(GO) vet ./...
