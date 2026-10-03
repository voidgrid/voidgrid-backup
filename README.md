# voidgrid-backup

I wrote this using Claude as I couldn't find a single pane of glass that covered my homelab which consists of docker-compose stacks, vitrual machines, and various arbitrary paths. I didn't want to spin up and manage multiple stacks for something that a single app should cover. I noticed most of the solutions out there not only didn't cover what I wanted to do but also had a bunch of enterprise features that my homelab simply doesn't need and only add complexity. I designed this for the itch I wanted to scratch and decided it might be useful for others as well so here it is. I don't ever intend to even try and monetize hence the MIT license. \

I also don't want to just use Claude and generate code. I am reading the code and trying to learn Go as I work on this. I don't trust purely vibe coded projects and neither should you. You probably shouldn't use this as I don't know enough Go yet to properly vet the code produced by Claude. Right now the design is mine, the design document was lenghty to ensure I got this doing what I wanted and how I wanted, however, the code is Claude. As I learn Go that will change but I want to be honest about where things stand right now.

This is a summary to what I wanted and wher it ended up: A single web UI for backing up directories, Docker Compose stacks (with database dumps) and libvirt VMs — to an SFTP server (e.g. a Hetzner StorageBox), S3-compatible object storage, or a local directory. One server container plus one lightweight agent per host, talking over mutual TLS. Storage is a [Kopia](https://kopia.io) repository: content-addressed, deduplicated, zstd-compressed, AES-256-GCM encrypted, with optional Reed-Solomon error correction against bitrot. I only included S3 because I thought it might be useful. I am personally using Hetzner because of the cost and decided that before even starting this project. I haven't been using them long so I can't recommend you follow my path there either.

Here's the reality, this is a one person+AI project and I am learning the language used as I work through everything. It does have a real test suite, the backups and restores do work in my environment and have been quite heavily tested. If you want to use this then I advise, very strongly, to read the code before trusting it. I am not asking you to trust me at all as I am using Claude, for the love of God dig into the code yourself and understand before running it in your homelab. For all you know running this could get your fridge pregnant, turn your toaster into a Transformer, cause your signicant other to expect you to actually do the dishes, or summon Abraxas in the middle of your bathroom whilst you are showering and I wouldn't want you blaming me for any of that.

Also I suck at writing README.md stuff...

## Requirements

- Docker and Docker Compose v2 on every host that runs a piece of this (the server, and each agent).
- Somewhere to store backups: an SFTP server, an S3-compatible bucket, or a local/mounted directory.
- No local Go toolchain, whether you pull a published image or build your own (see below) — see [Development](#development) for how the build itself works.

| Component | Runs | Does |
|---|---|---|
| `voidgrid-backup-server` | once | Web UI, JSON API, scheduler, catalog. Never touches backup data. [docs/server.md](docs/server.md) |
| `voidgrid-backup-agent` | on every host with something to back up | Reads data, talks to Docker and libvirt, writes snapshots to the repository. [docs/agent.md](docs/agent.md) |
| `voidgrid-backup-recover` | on demand | Break-glass restore straight from a repository: no server, catalog or agent needed. [docs/recovery.md](docs/recovery.md) |

All three binaries ship in one image, `ghcr.io/voidgrid/voidgrid-backup`; the server is the default entrypoint, run the agent with `--entrypoint /usr/local/bin/voidgrid-backup-agent`. The examples below read the image from `VB_IMAGE` in each `.env.example`. Set it to a published tag, for example `VB_IMAGE=ghcr.io/voidgrid/voidgrid-backup:v0.9.0-beta.1`, or build and load one locally (`docker build -t voidgrid-backup:latest .`, then set `VB_IMAGE=voidgrid-backup:latest`).

## Getting started

Both binaries install the same way — Docker Compose, from the generic examples in [examples/](examples/) (see [examples/README.md](examples/README.md)).

**1. Run the server:**

```sh
cp examples/server/.env.example examples/server/.env   # then edit it
cd examples/server
mkdir -p data && sudo chown 1000:1000 data   # skip if you're already uid 1000; use your own uid/gid otherwise
docker compose up -d
```

Open `http://<host>:<VB_PORT>/`. Full detail: [docs/server.md](docs/server.md).

**2. Run an agent on each host with something to back up:**

```sh
cp examples/agent/.env.example examples/agent/.env   # then edit it
cd examples/agent
docker compose up -d
docker compose logs agent | grep code   # the one-time enrollment code
```

*It runs as root with a minimal set of capabilities and mounts its sources read-only; [docs/agent.md](docs/agent.md) explains why.*

**3.** Enroll the agent on the server's Agents page with that code and the address the server can reach it on (`host:VB_PORT`).

**4.** Add a repository, then path, stack or VM jobs.

Signing in is always required — the first visit is to `/setup` (a token printed in the server's log) to generate 10 one-time recovery codes before anything else works, the same idea as a TOTP app's backup codes. The wizard also lets you configure OIDC right there (or later, from the **Authentication** page), so you can sign in via a provider you already run or trust; without one, a recovery code is the only way in, not a fallback.
Changes to OIDC take effect immediately, no restart. `VB_OIDC_*` (in [examples/server/.env.example](examples/server/.env.example)) also works and always wins over whatever's set through the UI. See [docs/server.md#authentication](docs/server.md#authentication).

The Notifications page (Discord webhook, [Notifarr](https://notifiarr.com/), or email) sends when a **scheduled** job finishes partial or failed. See [docs/server.md#notifications](docs/server.md#notifications).

Both containers have health checks (`voidgrid-backup-server healthcheck`, `voidgrid-backup-agent healthcheck`), and every setting is a flag or an `VB_*` environment variable.

## Development

No local Go toolchain is needed: everything runs in containers.

- `scripts/go.sh test ./...`: build, vet and test (`scripts/go.sh` wraps the Go toolchain).
- `scripts/gen-proto.sh`: regenerate the gRPC code after editing `internal/proto/agent.proto`.
- `scripts/docker-live-test.sh`: live tests against the local Docker daemon and
  real PostgreSQL, MariaDB and Valkey images (throwaway containers).
- `scripts/libvirt-live-test.sh`: live backup of a throwaway transient VM on
  the per-user `qemu:///session` libvirt (never the system daemon).
- `scripts/s3-live-test.sh`: live backup/check/restore against a real
  S3-compatible bucket (e.g. Backblaze B2), using credentials from `.env`.
  Everything it writes goes under a fresh per-run prefix it deletes itself.
- `scripts/sftp-live-test.sh`: the same, against a real SFTP destination
  (e.g. a Hetzner Storage Box), using `HETZNER_*` credentials from `.env`.
- `scripts/docker-deploy-test.sh`: builds the image and boots both
  containers the way `examples/server/` and `examples/agent/` describe —
  the server's nonroot user and writable `./data`, the agent's `cap_drop`/
  `cap_add` set — and confirms both health checks pass. Throwaway copies
  and a locally built image tag; nothing in `examples/` is touched.
- CI runs vet and the tests inside the image build and publishes the image
  to `ghcr.io` — the same `docker build` you'd run locally, just automated.

## License

MIT, see [LICENSE](LICENSE). [Kopia](https://kopia.io/), the storage engine this project embeds, is Apache-2.0; see [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
