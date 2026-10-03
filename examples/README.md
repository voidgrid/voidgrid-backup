# Examples

Generic `docker-compose.yaml` and `.env.example` pairs for both binaries.
Copy the one you need, fill in `.env`, `docker compose up -d`. Nothing here
is homelab-specific — placeholders throughout.

| Directory | Runs | Docs |
|---|---|---|
| [server/](server/) | `voidgrid-backup-server`, once | [docs/server.md](../docs/server.md) |
| [agent/](agent/) | `voidgrid-backup-agent`, on every host with something to back up | [docs/agent.md](../docs/agent.md) |

There's no compose example for `voidgrid-backup-recover`: it's a one-shot CLI
you run by hand when you need it, not a long-running service. See
[docs/recovery.md](../docs/recovery.md).
