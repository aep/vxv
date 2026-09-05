a microvm boundary for clankers

spawns a microvm whose root filesystem is the real host's, assembled on the host
side in a private mount namespace: everything read-only, some tmpfs overlays so
your tools keep working (writes go to RAM and are thrown away when you exit the
shell), and `$PWD` bound back in read-write so you can work in it normally as if
it was your host.

nothing is enforced inside the vm. the virtio-fs server *is* the vxv process, so
a guest that gets root can remount whatever it likes and still reach nothing new
— vxv itself can no longer reach it. beyond that it doesn't do much, like
https://github.com/jingkaihe/matchlock is probably a safer tool but i dont like
how complicated it is.

## running it

assembling the namespace needs privileges vxv drops again (back to your uid,
groups and all) before the vm boots — so the file server answering the guest has
exactly your access and no more. `cap_sys_admin` to mount, `cap_dac_override` to
overlay root-owned dirs and inject into them, `cap_chown` so an overlaid dir
keeps its real owner. either:

```sh
sudo vxv                                                        # works anywhere
sudo setcap cap_sys_admin,cap_dac_override,cap_chown+ep $(which vxv)
```

the second one is a one-off; after it, plain `vxv` works. note that setcap
applies to the inode, so it has to be redone after every `make`.

`make install` does the setcap for you. note it only applies to the installed
binary: ld.so ignores `$ORIGIN` runpaths on a binary with capabilities, so a
setcap'd `./vxv` in the repo won't find the bundled libkrun. use sudo there.

needs linux 5.12+ (`mount_setattr`) and /dev/kvm.

## per-project policy (`.vxv.yaml`)

on launch, vxv looks for a `.vxv.yaml` in the working directory and every parent
up to `/`. every one it finds is merged. it has two independent controls:

- **`hide`** — the guest can neither read nor write the path. an empty,
  mode-0000 placeholder is bind-mounted over it, so there is nothing behind it
  to leak even to a guest running as root.
- **`readonly`** — the guest can read the path but not write it. bind-mounted
  over itself read-only, so its contents stay visible while writes fail.

```yaml
# .vxv.yaml
hide:
  - secrets.env      # relative paths resolve against this file's directory
  - /home/me/.aws    # absolute host paths work too (no ~ expansion)
  - /etc/shadow
readonly:
  - config.yaml      # readable inside the guest, but immutable
  - Makefile
```

aliases: `hide` also accepts `deny` / `block` / `files`; `readonly` also accepts
`no-write` / `protect`. a bare top-level yaml list is shorthand for `hide`. both
lists are applied last — after the overlays and the writable `$PWD` — so they
always win. everything happens in vxv's own mount namespace, so the real host
files are never touched. a path is only affected if it exists on the host;
missing entries are skipped, and a path in both lists is treated as `hide`.

note: a *write-only* file (writable but unreadable) isn't offered — it can't be
done without modifying the host file itself.
