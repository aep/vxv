a microvm boundary for clankers

spawns a microvm and mounts the real hosts / as read-only
with some tmpfs overlays so your tools keep working, but actually any writes are thrown away when you exit the shell.

then it mounts $PWD as read-write so you can work inside that normally as if it was your host. doesn't really do any protection beyond that, like https://github.com/jingkaihe/matchlock is probably a safer tool but i dont like how complicated it is.
