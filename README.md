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

All three binaries ship in one image, `ghcr.io/voidgrid/voidgrid-backup`; the server is the default entrypoint, run the agent with `--entrypoint /usr/local/bin/voidgrid-backup-agent`. The examples below read the image from `VB_IMAGE` in each `.env.example`. Set it to a published tag, or build and load one locally (`docker build -t voidgrid-backup:latest .`, then set `VB_IMAGE=voidgrid-backup:latest`).

```sh
VB_IMAGE=ghcr.io/voidgrid/voidgrid-backup:latest         # newest stable release
VB_IMAGE=ghcr.io/voidgrid/voidgrid-backup:beta           # newest release of any kind, pre-releases included
VB_IMAGE=ghcr.io/voidgrid/voidgrid-backup:v0.9.0-beta.1  # one exact version, never moves
```

Use the same tag for the server and every agent. Beta releases are what's being tested and can change in ways that don't carry over, so give a test install its own data directory and don't point it at backups you care about. To move to a newer image, `docker compose pull && docker compose up -d`.

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
cp examples/agent/.env.example examples/agent/.env   # then edit it: VB_SERVER and VB_TOKEN
cd examples/agent
docker compose up -d
```

*It runs as root with a minimal set of capabilities and mounts its sources read-only; [docs/agent.md](docs/agent.md) explains why.*

**3.** The agent registers itself and shows up on the server's Agents page under "Waiting for approval". Approve it there and pick its name. The token for `VB_TOKEN` is on the same page (or `docker exec voidgrid-backup-server /usr/local/bin/voidgrid-backup-server token`), and `VB_SERVER` is the server's host and agent port (default 9442).

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

### What the live tests need

The live tests are optional and aren't part of `scripts/go.sh test ./...`; each sits behind a build tag. They all run the Go tests inside a `golang` container, so every one needs Docker and network access to pull images.

- `docker-live-test.sh`: access to the local Docker socket, and the ability to pull `alpine:3`, `postgres:16-alpine`, `mariadb:11.4` and `valkey/valkey:9-alpine`.
- `libvirt-live-test.sh`: `virsh` and `qemu-img` on the host, and a per-user libvirt session (`qemu:///session`) with its socket under `/run/user/<uid>/libvirt`.
- `s3-live-test.sh`: a `.env` at the repo root with `B2_ENDPOINT` (host only, no scheme), `B2_BUCKET_NAME`, `B2_KEY_ID` and `B2_KEY_KEY`. The names say B2, but any S3-compatible bucket you can write to works. The region comes from the endpoint's second dot-separated label; set `B2_REGION` to override it.
- `sftp-live-test.sh`: a `.env` at the repo root with `HETZNER_LINK` (the host), `HETZNER_USER`, `HETZNER_SSH_PORT` (defaults to 22; a Storage Box uses 23) and either `HETZNER_KEY` or `HETZNER_PASSWORD`. `HETZNER_KEY` must be an absolute path to a private key already authorized on the server (a leading `~/` is expanded).

[`env.example`](env.example) lists every name with `CHANGEME` placeholders. It has no leading dot so it shows up in a checkout; copy it to `.env` (`cp env.example .env`) and fill in the section for the destination you want to test. Each script reads only its own section, so you can leave the other one alone: a script whose variables are still `CHANGEME` prints a loud SKIPPING banner, tests nothing and exits 0. The scripts source `.env` as shell, so use plain `KEY=value` lines. The S3 and SFTP tests write only under a fresh per-run prefix or directory and remove it when they finish, pass or fail.

## License

MIT, see [LICENSE](LICENSE). [Kopia](https://kopia.io/), the storage engine this project embeds, is Apache-2.0; see [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
