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

When you approve it, check the **address** the server will use to reach the
agent. It is pre-filled from where the registration came from, which for
containers on one Docker network is the agent's container IP, and that changes
when the stack is recreated or rebooted. Replace it with the agent's Docker
service name (for example `agent:9443`, same Docker network, not the default
`bridge`) or with the host's DNS name or fixed IP and the published port (`VB_PORT`,
default 9443). It can be changed later with **Change address** on the server's
Agents page; see [server.md](server.md#network-and-security).

## Configuration

Every setting is a flag and an environment variable; a flag given on the
command line wins over the environment.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-server` | `VB_SERVER` | | `host:port` of the server's agent registration listener. Needed until the agent is approved. |
| `-token` | `VB_TOKEN` | | Registration token from the server. Needed until the agent is approved. |
| `-listen` | `VB_LISTEN` | `:9443` | gRPC address the server connects to. In the example compose `VB_PORT` sets this and the published port together. |
| `-health-listen` | `VB_HEALTH_LISTEN` | `127.0.0.1:9444` | HTTP `/healthz`; keep it on loopback. Empty disables it. |
| `-data` | `VB_DATA` | `/data` | Agent key, certificates and, unless `VB_CACHE_DIR` is set, Kopia's local cache |
| `-docker` | `VB_DOCKER_SOCKET` | `/var/run/docker.sock` | Docker socket; empty disables stack backups |
| `-libvirt-socket` | `VB_LIBVIRT_SOCKET` | `/var/run/libvirt/libvirt-sock` | libvirt socket; empty disables VM backups |
| `-libvirt-uri` | `VB_LIBVIRT_URI` | `qemu:///system` | libvirt connection URI |
| `-cache-dir` | `VB_CACHE_DIR` | | Base directory for Kopia's local cache, one subdirectory per repository. Empty keeps it under `-data`. See [Kopia's local cache](#kopias-local-cache). |
| `-ipv4-only` | `VB_IPV4_ONLY` | `false` | Connect to SFTP and S3 storage over IPv4 only (also notification webhooks and OIDC, which use Go's default HTTP transport; not SMTP). Off by default, so IPv6 works. Turn it on for a host with no IPv6 whose resolver sometimes answers with only an IPv6 address, which fails with `network is unreachable`. |
| `-cache-metadata-mb` | `VB_CACHE_METADATA_MB` | `256` | Size limit, in MiB per repository, of the cache an incremental backup needs |
| `-cache-content-mb` | `VB_CACHE_CONTENT_MB` | `64` | Size limit, in MiB per repository, of the cache of file data read back for restores and checks. `0` turns the whole cache off. |

A socket that doesn't exist at start disables that feature with a warning in
the log; the rest keeps working.

## Kopia's local cache

Kopia keeps a cache for each repository: the repository's index, the previous
snapshot's directories (an incremental backup compares against them) and file
data read back for restores and checks. Without it every run downloads that
again, which over SFTP to a distant storage box is slow, because each request
waits for a round trip. The cache holds the repository's blobs as they are
stored there, so it is encrypted the same way, with integrity checks.

- Each repository gets its own directory, `<cache dir>/<repository>-<connection>`.
- Both limits are hard limits. Per repository the cache stays within about
  the content limit plus the metadata limit, plus the repository's index blobs,
  which are small. With three repositories and `1024` for both, that is a little
  over 6 GiB.
- `VB_CACHE_CONTENT_MB=0` turns the whole cache off (Kopia ignores the metadata
  setting then).
- Put `VB_CACHE_DIR` on a fast disk by mounting one into the container, for
  example `/cache`, and pointing `VB_CACHE_DIR` at it. If the mount is missing
  when the agent starts, the cache is created inside the container and is lost
  when it is recreated; the log line `kopia cache` at startup shows where it is.
- A changed setting takes effect the next time a repository is opened. The
  settings are local to this host: nothing is written to the repository.
- When a repository's connection details change (its path moved with Edit, or
  its credentials), the agent takes the cache of the old connection over to the
  new one, but only if the old connection recorded the repository's Kopia ID
  (written the first time a connection opens) and the new one is to the same
  repository. Otherwise the old cache is left in its directory and the new
  connection starts empty; delete the old one by hand. Wiping a repository
  removes its cache on the agent that ran the wipe.

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
- **Restores**: when the agent cannot restore ownership, the run is still a success. Its summary says "complete, files belong to uid:gid N:N (… does not run as root, so the ownership in the backup was not restored)", and the snapshot page says so before you choose that agent. Only a file that could not be written makes a restore partial.

## Paths and read-only mounts

Docker and libvirt report **host** paths, so every stack directory, bind
mount source and VM disk directory must be mounted into the agent **at the
same path** it has on the host (`/srv/stacks:/srv/stacks`).

The path is the one the tool reports, not where the data really lives. libvirt
gives a VM disk's path exactly as the domain defines it, so if that path goes
through a symlink on the host (a domain pointing at `/home/<user>/dockers/...`
where `dockers` is a link), that path must exist inside the container. Mount the
real directory onto the reported path, for example
`/data/dockers:/home/<user>/dockers:ro`. The VMs page shows the paths the agent
will open; a disk whose path is missing in the container fails the backup.

Mount backup sources **read-only** (`:ro`). They can still be backed up, but
the agent refuses an in-place restore to them up front and the UI marks them
"read-only in the agent". Restore to another directory instead, and give the
agent one writable directory for that (`RESTORE_DIR` in the example). Mount a
path read-write only if you want in-place restores of it.

## When the network fails

A scheduled or manual backup, a maintenance cycle and a repository check open
the repository first. If that fails with a network error that may pass (a DNS
lookup that fails, an unreachable network or host, a refused or reset
connection, a timeout), the agent does not fail the run at once. It waits 15
seconds and tries again, doubling the wait each time up to 5 minutes, until it
has waited 15 minutes in all; only then does the run fail, with the last error.
Each wait is logged as `network error, trying again`. A login that is refused, a
wrong key or a missing path is not retried. The wait comes before anything is
paused or snapshotted, so a stack keeps running and a VM stays off its overlay
while the agent waits. Actions you start by hand to change things, such as
adding a repository or testing access, are not retried, so a typo in a host name
is reported straight away. The server does the same when it runs maintenance
itself.

## SFTP key naming

An SFTP repository stores one key file **path**, and that path is read
inside the agent. For a repository to be usable from more than one agent
(see [Using a repository from another agent](server.md#using-a-repository-from-another-agent)),
give every agent a key at that same in-container path, with a different
key behind it.

- Use one key pair per agent, named after the host, e.g.
  `voidgrid_<hostname>` on that host, mounted read-only at the path the
  repository uses:

  ```yaml
  volumes:
    - /root/.ssh/voidgrid_<hostname>:/keys/sftp_key:ro
  ```

- Authorize each public key on the SFTP server, one line per agent with the
  host name as the comment, so a single host can be revoked by deleting its
  line:

  ```
  ssh-ed25519 AAAA... voidgrid_<hostname>
  ```

  On a Hetzner Storage Box this is the `.ssh/authorized_keys` file in its
  home directory. Add keys with Hetzner's `install-ssh-key` command (for
  example `cat ~/.ssh/id_ed25519.pub | ssh -p23 uXXXXX@uXXXXX.your-storagebox.de install-ssh-key`),
  which appends. Copying the file with `scp` replaces the whole file and removes
  every key already there. Port 23 takes the one-line OpenSSH format shown
  above; port 22 needs the RFC 4716 form, which `install-ssh-key` also writes.
- Don't reuse one key everywhere unless you accept that revoking it cuts off
  every host at once.
- The key file must be readable by the user the agent runs as.

The `known_hosts` entry is part of the repository, so it doesn't need
distributing.

## Restoring on a different host

The server can run a restore on any enrolled agent (see
[Restoring on a different agent](server.md#restoring-on-a-different-agent)).
Prepare the agent that will receive it ahead of time, not during an incident:

- Mount a writable directory for the target (`RESTORE_DIR` in the example).
- For an SFTP repository with a key file, mount a key at the path the
  repository uses ([SFTP key naming](#sftp-key-naming)).
- A restore into a directory needs no Docker socket or libvirt socket. Only
  in-place restores do, and those run on the job's own agent only.
- Snapshots written by another host are readable. The agent opens the
  repository the first time it is asked.

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

The image has no ping, dig or nc. To test whether this container can reach an
address, for example the server's registration port, run the binary's own check
from the host (it resolves the name, then opens a TCP connection to each
address):

```sh
docker exec <agent-container> voidgrid-backup-agent check server:9442
```

It prints the DNS result and each connection attempt, and exits 0 when an
address accepted a connection, 1 when none did.

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

## Progress

While a backup uploads, the agent logs `backup progress` every 30 seconds: the
source path, files seen (hashed and unchanged), bytes hashed, the directory it
is in, and, once Kopia has sized the source, the estimated size and a
percentage. The estimate only exists after the scan, so a first backup of a large
tree shows no percentage for a while. The server polls the same figures (the
`Progress` call, which does not wait for the agent's operation lock) while the job
runs and shows them next to "running" on the Jobs page and the job page; an
agent older than the server just shows "running".

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

The agent talks to libvirt over a Unix socket that is mounted into the
container. Hosts with a single `libvirtd` have `/var/run/libvirt/libvirt-sock`.
Newer distributions split libvirt into per-driver daemons, and the QEMU socket is
then `/var/run/libvirt/virtqemud-sock`. Set `LIBVIRT_SOCKET` in `.env` to
whichever exists on the host; the example compose file mounts it at the path the
agent expects, so `VB_LIBVIRT_SOCKET` stays as it is.

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
| The server shows this agent as "unreachable" after a restart | The server's stored address for it is probably a container IP that changed. Check with `docker exec <server-container> voidgrid-backup-server check <address>`, then set a Docker service name or the host's name or IP on the server's Agents page (**Change address**) |
| Stacks page: "no Docker socket configured" | The socket volume and `VB_DOCKER_SOCKET` |
| VMs page: "no libvirt socket configured" | The socket volume and `VB_LIBVIRT_SOCKET` |
| "is mounted read-only in the agent" on restore | Intended: restore to a directory, or mount that path read-write |
| Restore on another agent: "connect to repository" fails | The key file is not at the repository's path in that agent, its public key is not authorized, or the storage is not reachable from that host |
| Restore on another agent: "a restore on another agent needs a target directory" | Enter a target directory; in-place restores run only on the job's own agent |
| Partial backup: "could not read ..." | Capabilities and SELinux (see Permissions) |
| Snapshots missing after redeploying an agent | It registered as a new agent; approve it as replacing the old one to keep its ID |
