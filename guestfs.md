# guestfs — injecting files into the VM

`vxv` lays a host directory tree into the guest filesystem at boot, mirroring
each path: `guestfs/etc/foo/bar` → `/etc/foo/bar` inside the VM. Files land on
the ephemeral overlay/tmpfs layer, so they exist **only inside the VM** and are
discarded on exit — the host copies of those paths are never touched.

- The tree is `guestfs/` next to the `vxv` binary by default (used only if it
  exists). Override with `--inject DIR`.
- The whole tree is mirrored, so put *only* files you want in the guest under it
  (don't drop a README in there — it would become `/README.md`).
- Ownership and mode are copied from the host source: a file owned by your user
  in `guestfs/` appears user-owned in the guest, a root-owned one stays root. New
  directories are created with the source's owner/mode; pre-existing system dirs
  (e.g. `/etc`) keep their own ownership. The guest shares the host's uid space,
  so numeric owners carry over as-is.
- Destinations must be writable in the guest, i.e. under an `--overlay` dir
  (`/etc`, `/home`, … by default). Injecting outside those is skipped with a
  warning.

## What ships here

`guestfs/etc/claude-code/managed-settings.d/10-vxv-auto.json` sets Claude Code's
default permission mode to `auto` inside the VM — a *managed* setting (highest
precedence) in the `.d` drop-in form, so it doesn't clobber other Claude config.
`auto` is used rather than `bypassPermissions` because the latter is refused
under root and shows a one-time acceptance dialog that the VM's ephemeral home
would re-prompt every boot. Delete the file to turn it off, or drop in your own.
