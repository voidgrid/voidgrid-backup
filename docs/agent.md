# voidgrid-backup-agent

The agent runs on every host that has something to back up. It reads the
data, talks to Docker and libvirt on that host, and writes snapshots straight
to the repository. The server only tells it what to do.

## Quick start

```sh
cp examples/agent/.env.example examples/agent/.env   # then edit it
cd examples/agent
docker compose up -d
docker compose logs agent | grep code   # the one-time enrollment code
```

Enter the code on the server's Agents page with the address the server can
reach this agent on (`host:VB_PORT`).

## Configuration

Every setting is a flag and an environment variable; a flag given on the
command line wins over the environment.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen` | `VB_LISTEN` | `:9443` | gRPC address the server connects to |
| `-health-listen` | `VB_HEALTH_LISTEN` | `127.0.0.1:9444` | HTTP `/healthz`; keep it on loopback. Empty disables it. |
| `-data` | `VB_DATA` | `/data` | Agent key, certificates and Kopia cache |
| `-hostname` | `VB_HOSTNAME` | the container/host name | Name recorded on snapshots. **Set it and keep it stable**: snapshots are grouped by it and can only be browsed and restored by an agent with the same name. |
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

## Enrollment

On first start the agent creates a key, a self-signed bootstrap certificate
and a one-time secret, and logs an enrollment code:

```
hbe1-<secret>-<certificate pin>
```

The server uses the pin to check it reached this agent and the secret to
prove it was given the code. After enrollment the agent requires the server's
client certificate for everything and the code stops working. The identity
lives in the data directory; delete it to enroll the agent again.

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
- "Pause" is suggested when SQLite files are found.

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
| No enrollment code in the log | The agent is already enrolled; delete its data directory to enroll it again |
| Stacks page: "no Docker socket configured" | The socket volume and `VB_DOCKER_SOCKET` |
| VMs page: "no libvirt socket configured" | The socket volume and `VB_LIBVIRT_SOCKET` |
| "is mounted read-only in the agent" on restore | Intended: restore to a directory, or mount that path read-write |
| Partial backup: "could not read ..." | Capabilities and SELinux (see Permissions) |
| Snapshots from before a rename can't be browsed | `VB_HOSTNAME` changed; set it back |
