a microvm boundary for clankers

spawns a microvm and mounts the real hosts / as read-only
with some tmpfs overlays so your tools keep working, but actually any writes are thrown away when you exit the shell.

then it mounts $PWD as read-write so you can work inside that normally as if it was your host. doesn't really do any protection beyond that, like https://github.com/jingkaihe/matchlock is probably a safer tool but i dont like how complicated it is.

## per-project policy (`.vxv.yaml`)

on launch, vxv looks for a `.vxv.yaml` in the working directory and every parent
up to `/`. every one it finds is merged. it has two independent controls:

- **`hide`** — the guest can neither read nor write the file. it's shadowed by an
  inaccessible (mode-0000, root-owned) placeholder bind-mounted over it, so the
  real contents are invisible inside the vm.
- **`readonly`** — the guest can read the file but not write it. it's bind-mounted
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
lists are applied last — after the writable $PWD and the overlays — so they always
win, and everything happens only in the guest's mount namespace, so the real host
files are never touched. a path is only affected if it exists on the host;
missing entries are skipped, and a path in both lists is treated as `hide`.

note: a *write-only* file (writable but unreadable) isn't offered — it can't be
done without modifying the host file itself.
