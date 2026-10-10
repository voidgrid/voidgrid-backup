# voidgrid-backup-agent

The agent runs on every host that has something to back up. It reads the
data, talks to Docker and libvirt on that host, and writes snapshots straight
to the repository. The server only tells it what to do.

## Quick start

```sh
cp examples/agent/.env.example examples/agent/.env   # then edit it
cd examples/agent
docker compose up -d
```

Set `VB_SERVER` (the server's host and agent registration port) and
`VB_TOKEN` (from the server's Agents page, or
`docker exec voidgrid-backup-server /usr/local/bin/voidgrid-backup-server token`)
in `.env` first. The agent then appears on the server's **Agents** page under
"Waiting for approval"; approve it there and pick its name.

## Configuration

Every setting is a flag and an environment variable; a flag given on the
command line wins over the environment.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-server` | `VB_SERVER` | | `host:port` of the server's agent registration listener. Needed until the agent is approved. |
| `-token` | `VB_TOKEN` | | Registration token from the server. Needed until the agent is approved. |
| `-listen` | `VB_LISTEN` | `:9443` | gRPC address the server connects to. In the example compose `VB_PORT` sets this and the published port together. |
| `-health-listen` | `VB_HEALTH_LISTEN` | `127.0.0.1:9444` | HTTP `/healthz`; keep it on loopback. Empty disables it. |
| `-data` | `VB_DATA` | `/data` | Agent key, certificates and Kopia cache |
| `-docker` | `VB_DOCKER_SOCKET` | `/var/run/docker.sock` | Docker socket; empty disables stack backups |
| `-libvirt-socket` | `VB_LIBVIRT_SOCKET` | `/var/run/libvirt/libvirt-sock` | libvirt socket; empty disables VM backups |
| `-libvirt-uri` | `VB_LIBVIRT_URI` | `qemu:///system` | libvirt connection URI |

A socket that doesn't exist at start disables that feature with a warning in
the log; the rest keeps working.

## Permissions

The agent reads other containers' private data (often root-owned `0600`
files and `0700` database directories) and restores it with the original
ownership. A non-root process can do neither, so the agent runs as **root
inside its container with only these capabilities**:

| Capability | Needed for |
|---|---|
| `DAC_READ_SEARCH` | Reading files and directories it doesn't own |
| `DAC_OVERRIDE` | Writing restores into directories it doesn't own |
| `CHOWN` | Restoring ownership |
| `FOWNER` | Restoring modes and times on files it doesn't own |
| `FSETID` | Keeping setuid/setgid bits |

Everything else is dropped (`cap_drop: [ALL]`) and `no-new-privileges` is set.
Access to the Docker socket already makes the agent root-equivalent, so a
non-root agent would lose the ability to back things up without gaining real
isolation.

On SELinux hosts (Fedora, RHEL) add `label=disable` to the agent's
`security_opt`. Do **not** relabel your data with `:z`/`:Z` to make it
readable: that changes the labels other containers rely on.

The agent checks itself and says what's missing instead of failing quietly:

- **Agents page**: a warning when it lacks any of the capabilities above.
- **Backups**: a partial run names the first unreadable files.
- **Restores**: a partial run says when ownership could not be restored.

## Paths and read-only mounts

Docker and libvirt report **host** paths, so every stack directory, bind
mount source and VM disk directory must be mounted into the agent **at the
same path** it has on the host (`/srv/stacks:/srv/stacks`).

Mount backup sources **read-only** (`:ro`). They can still be backed up, but
the agent refuses an in-place restore to them up front and the UI marks them
"read-only in the agent". Restore to another directory instead, and give the
agent one writable directory for that (`RESTORE_DIR` in the example). Mount a
path read-write only if you want in-place restores of it.

## Health check

`/healthz` on `VB_HEALTH_LISTEN` returns JSON:

```json
{"healthy":true,"grpc":"ok","enrolled":true,"docker":"ok","libvirt":"disabled","data_dir":"ok"}
```

It is unhealthy (HTTP 503) only when the gRPC listener doesn't accept
connections or the data directory isn't writable. Docker and libvirt are
optional, so their state is reported but doesn't make the agent unhealthy.
The compose health check runs the binary's own probe (the image has no curl):

```sh
voidgrid-backup-agent healthcheck   # exit 0 healthy, 1 unhealthy
```

## Registration

On first start the agent creates a key and a self-signed certificate. It then
connects to the server's registration listener (`VB_SERVER`), checks the
server's certificate against the pin inside `VB_TOKEN` (no CA and no domain
name are involved), and registers. A valid token only makes the agent appear
under "Waiting for approval" on the Agents page; nothing is granted until you
approve it. The agent logs its key fingerprint
(`docker compose logs agent | grep fingerprint`); the pending entry shows the
same value. Compare them before approving: the name shown there is whatever
the agent says, so anyone else holding the token could pick a familiar one. On approval the server issues a certificate for the agent's own
key, which the agent installs without a restart. From then on the agent
requires the server's client certificate for everything, and `VB_SERVER` and
`VB_TOKEN` are no longer used.

The agent's identity is the ID the server assigns. It is the snapshot host name
too, so renaming the agent in the UI is free: only the label changes. To bring
a wiped or redeployed agent back as an existing one, choose it under
"Replaces" when approving: it keeps that agent's ID, jobs and snapshots. The
agent's key and certificate live in the data directory; deleting it makes the
agent register as a new one.

If the server rejects the agent, the agent logs it and keeps checking; use
"Forget" on the server to let it register again. A wrong or rotated token
is logged on every retry.

## Path backups

Absolute directories, with gitignore-style excludes (`*.tmp`, `/cache/`). The
agent's own data directory is always excluded. One snapshot per path.

## Compose stacks

The agent's **stacks** page lists every compose project on the host (found by
Docker Compose's container labels), its services, mounts, databases and
SQLite files. A stack job takes one snapshot per project containing:

| In the snapshot | What |
|---|---|
| `stack/` | The compose directory: compose file, `.env`, anything inside it |
| `mounts/` | Each ticked mount that lives outside the compose directory |
| `dumps/<service>.sql` / `.rdb` | A dump of each database you picked |
| `voidgrid-backup.json` | Where everything came from, for restores |

**Defaults on the stacks page**:

- A database you dump has its raw data mount unticked: the dump is the
  consistent copy.
- Host files like `/etc/localtime` and sockets are unticked, so an in-place
  restore can't overwrite the host's own files.
- Redis/Valkey defaults to no dump (usually a cache).
- "Pause" is suggested when SQLite or DuckDB files are found. Both are recognised by their file header (any of `.db`, `.sqlite`, `.sqlite3`, `.duckdb`, `.ddb`, a few levels deep in a mount) and listed on the stacks page. They are copied as plain files: pausing makes the copy crash-consistent, not a dump, so test a restore.

**Database dumps** run inside the database container with `docker exec` and
stream straight into the snapshot (nothing is staged on disk). Credentials
come from the container's own environment:

| Kind | Command | Credentials |
|---|---|---|
| postgres | `pg_dumpall` | `POSTGRES_USER` (default `postgres`), local socket trust |
| mariadb | `mariadb-dump` / `mysqldump --all-databases --single-transaction` | `MARIADB_ROOT_PASSWORD` or `MYSQL_ROOT_PASSWORD` |
| redis | `valkey-cli` / `redis-cli --rdb -` | `REDIS_PASSWORD` or `VALKEY_PASSWORD`, if set |

A failing dump (non-zero exit) is reported on the run and not stored
half-written; the file backup still completes.

**While reading files** the job can leave containers running, pause them
(recommended with SQLite) or stop them. Only the application containers are
affected: the databases being dumped and the agent itself are never paused or
stopped, and everything is always resumed.

**Restoring a stack**:

- *To a directory*: the whole snapshot tree is written there; no container
  is touched.
- *In place*: you type the project name to confirm. The stack is stopped
  (optional), files go back to the compose directory and each mount's
  original path, dumps are put in `.voidgrid-backup-dumps/` in the compose
  directory, and the stack is started again. Files added since the snapshot
  are not deleted.
- *Import dump*: loads a PostgreSQL or MariaDB dump into the running
  container. A Redis/Valkey dump has to replace the data file with the server
  stopped; the UI explains how.

If the stack's containers don't exist any more (a rebuilt host): restore in
place, run `docker compose up -d` in the compose directory, then import the
dumps.

## Virtual machines

The agent's **VMs** page lists the libvirt domains and their disks. Only
writable file-backed disks can be backed up; CD-ROMs and block devices are
shown but skipped, and a disk on a backing image is flagged because only its
top layer is copied.

A **running** VM is backed up like this:

1. An external disk-only snapshot moves each chosen disk onto a temporary
   overlay file next to it. With "freeze" ticked and the QEMU guest agent
   running in the VM, guest filesystems are flushed and frozen for that
   instant. Without a guest agent the copy is crash-consistent (like pulling
   the power) and the run says so.
2. The base images, which no longer change, are read into the snapshot with
   the domain XML.
3. Each overlay is merged back (`blockcommit --active --pivot`) and deleted.
   This always runs, even if step 2 failed.

If a merge fails the snapshot is still kept and the run shows **ACTION
NEEDED** with the exact command, for example:

```sh
virsh blockcommit myvm vda --active --pivot
```

A **shut-off** VM is copied directly.

**Restoring a VM**:

- *To a directory*: the disks and `domain.xml` are written there.
- *In place*: the VM must be shut off. You type its name to confirm, and the
  disks are written back over their image files. If the VM no longer exists
  it is defined again from the saved XML.

## Troubleshooting

| Symptom | Look at |
|---|---|
| The agent exits at start about `VB_SERVER`/`VB_TOKEN` | It is not approved yet and one of them is missing or malformed |
| "does not match the token" in the log | `VB_SERVER` points at a different server than the token came from |
| The agent never shows up under "Waiting for approval" | `VB_SERVER` and the registration port are reachable from this host, and the token is current (it changes when rotated) |
| Stacks page: "no Docker socket configured" | The socket volume and `VB_DOCKER_SOCKET` |
| VMs page: "no libvirt socket configured" | The socket volume and `VB_LIBVIRT_SOCKET` |
| "is mounted read-only in the agent" on restore | Intended: restore to a directory, or mount that path read-write |
| Partial backup: "could not read ..." | Capabilities and SELinux (see Permissions) |
| Snapshots missing after redeploying an agent | It registered as a new agent; approve it as replacing the old one to keep its ID |
