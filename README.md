# voidgrid-backup

A single web UI for backing up directories, Docker Compose stacks (with
database dumps) and libvirt VMs — to an SFTP server (e.g. a Hetzner Storage
Box), S3-compatible object storage, or a local directory. One server
container plus one lightweight agent per host, talking over mutual TLS.
Storage is a [Kopia](https://kopia.io) repository: content-addressed,
deduplicated, zstd-compressed, AES-256-GCM encrypted, with optional
Reed-Solomon error correction against bitrot.

It exists because most self-hosted backup tools cover *one* of "arbitrary
directories," "Docker volumes and databases," or "VM disks," and stitching
three separate tools together is its own maintenance burden. This is one
tool, one catalog, one place to see whether last night's backups actually
ran.

## Status

Built and run by one person for their own homelab, not (yet) independently
audited — read the code before trusting it with anything that matters, the
same advice as for any project this size. That said, it isn't a weekend
sketch either: a real test suite, and both the SFTP and S3 backends are
exercised against real destinations (a Hetzner Storage Box and a Backblaze
B2 bucket), not just mocks — see [Development](#development) for the live
tests. Two things are deliberately not built yet: automatic scheduled
integrity checks (verification is on-demand for now) and per-job tuning of
how thorough a check is — neither is a correctness gap, just not
configurable yet.

## Requirements

- Docker and Docker Compose v2 on every host that runs a piece of this
  (the server, and each agent).
- Somewhere to store backups: an SFTP server, an S3-compatible bucket, or
  a local/mounted directory.
- No local Go toolchain, whether you pull a published image or build your
  own (see below) — see [Development](#development) for how the build
  itself works.

| Component | Runs | Does |
|---|---|---|
| `voidgrid-backup-server` | once | Web UI, JSON API, scheduler, catalog. Never touches backup data. [docs/server.md](docs/server.md) |
| `voidgrid-backup-agent` | on every host with something to back up | Reads data, talks to Docker and libvirt, writes snapshots to the repository. [docs/agent.md](docs/agent.md) |
| `voidgrid-backup-recover` | on demand | Break-glass restore straight from a repository: no server, catalog or agent needed. [docs/recovery.md](docs/recovery.md) |

All three binaries ship in one image; the server is the default entrypoint,
run the agent with `--entrypoint /usr/local/bin/voidgrid-backup-agent`. The
examples below reference an image tag (`VB_IMAGE` in each `.env.example`) —
point it at wherever you publish your own build, or build and load one
locally (`docker build -t voidgrid-backup:latest .`, then set
`VB_IMAGE=voidgrid-backup:latest`).

## Getting started

Both binaries install the same way — Docker Compose, from the generic
examples in [examples/](examples/) (see [examples/README.md](examples/README.md)).

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

It runs as root with a minimal set of capabilities and mounts its sources
read-only; [docs/agent.md](docs/agent.md) explains why.

**3.** Enroll the agent on the server's Agents page with that code and the
address the server can reach it on (`host:VB_PORT`).

**4.** Add a repository, then path, stack or VM jobs.

Signing in is always required — the first visit is to `/setup` (a token
printed in the server's log) to generate 10 one-time recovery codes before
anything else works, the same idea as a TOTP app's backup codes. The wizard
also lets you configure OIDC right there (or later, from the
**Authentication** page), so you can sign in via a provider you already run
or trust; without one, a recovery code is the only way in, not a fallback.
Changes to OIDC take effect immediately, no restart. `VB_OIDC_*` (in
[examples/server/.env.example](examples/server/.env.example)) also works
and always wins over whatever's set through the UI. See
[docs/server.md#authentication](docs/server.md#authentication).

The Notifications page (Discord webhook, [Notifarr](https://notifiarr.com/),
or email) sends when a **scheduled** job finishes partial or failed. See
[docs/server.md#notifications](docs/server.md#notifications).

Both containers have health checks (`voidgrid-backup-server healthcheck`,
`voidgrid-backup-agent healthcheck`), and every setting is a flag or an
`VB_*` environment variable.

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
  on every push to the default branch — the same `docker build` you'd run
  locally, just automated.

## License

MIT, see [LICENSE](LICENSE). Kopia, the storage engine this project embeds,
is Apache-2.0; see [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
