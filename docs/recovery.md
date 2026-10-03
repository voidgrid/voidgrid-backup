# voidgrid-backup-recover

`voidgrid-backup-recover` is the break-glass restore tool. It reaches a
backup repository directly — no server, no catalog, no agent, no mTLS, and
**no file has to already exist anywhere** except whatever the storage
backend itself inherently requires (an SSH private key for SFTP). Reach for
it when the server host, its `catalog.db`, or anything derived from either
is gone or untrusted, and the normal web UI restore flow isn't an option:
everything that flow depends on (the catalog, an agent's certificate) is
exactly what a disaster like that takes out.

For day-to-day restores, use the job page in the web UI instead
([server.md](server.md#jobs)); it goes through the catalog and an agent, and
is what you want when the server is healthy.

## Keep this safe, separately from the server, before you need it

A backup you can only restore with information that lives on the thing
being backed up is not a backup. For **every** repository, write down
somewhere that does not depend on the server or any of its hosts (a
password manager entry, printed and locked away — your call, but off the
server):

- The repository **password**. The UI already shows this once, at
  creation, for exactly this reason.
- The repository **kind** (`sftp`, `s3` or `filesystem`) and its connection
  details — the same fields you typed into the "Add a repository" form:
  - **SFTP**: host, port, user, path, and *either* the private key file's
    contents *or* the password. (`known_hosts` is optional — see below.)
  - **S3**: endpoint, bucket, prefix, region, access key ID, secret access
    key.
  - **Filesystem**: the directory. (This kind has no independent storage of
    its own — if the disk holding it and the disk holding the server are
    the same failure domain, filesystem repositories don't survive most
    disasters anyway.)

None of this can be derived from the repository's own storage: it's
encrypted, so even holding the Storage Box or S3 bucket in your hands tells
you nothing without the password, and you can't discover the storage
address/credentials from inside the storage they point at. This is true of
any encrypted backup system, not a gap specific to this tool — the fix is
recording these fields once, when the repository is created, not trying to
avoid needing them later.

## Usage

The connection is given as flags, not a file, so nothing has to survive on
the recovering machine beyond what you just wrote down:

```sh
# List every snapshot in the repository, from every host it was ever run on.
voidgrid-backup-recover -password-file pw.txt \
    -kind sftp -sftp-host u123456.your-storagebox.de -sftp-port 23 \
    -sftp-user u123456 -sftp-path backups/homelab -sftp-key-file ./key \
    list

# Restore one, from any host, into an empty directory.
voidgrid-backup-recover -password-file pw.txt \
    -kind sftp -sftp-host ... -sftp-user ... -sftp-path ... -sftp-key-file ./key \
    restore -snapshot <id> -target /restore/here

# S3 instead of SFTP.
voidgrid-backup-recover -password-file pw.txt \
    -kind s3 -s3-endpoint ... -s3-bucket ... -s3-access-key ... -s3-secret-key ... \
    list
```

Run `voidgrid-backup-recover -h`, or `... list -h` / `... restore -h`, for
every flag. Two secrets accept an environment variable instead of a flag, so
they don't have to be typed where a shell might log them:
`$VB_RECOVER_PASSWORD` (repository password) and `$VB_RECOVER_SFTP_PASSWORD`
/ `$VB_RECOVER_S3_SECRET_KEY`.

`-config <file>` is also accepted, as a shortcut *when a repository config
JSON happens to be available* (the same shape the server sends an agent) —
never as a requirement. Don't rely on that file existing when you need this
tool; rely on the checklist above instead.

`-data <dir>` points the Kopia connection cache somewhere persistent across
runs (default: a temp directory, removed when the command exits).

## What it does and doesn't touch

The tool opens the repository **read-only** and can see every host's
snapshots, not just one — that's the point of it. It never writes a
snapshot itself. Restore targets go through the same checks as everywhere
else in this project (no `/`, `/proc`, `/sys`, `/dev`, `/run`, and no
relative paths).

`known_hosts` (SFTP only) pins the server's host key against a
machine-in-the-middle attack. If you didn't save it, leaving it unset trusts
whatever key the server presents on connect — the same trust-on-first-use
behavior a repository has at creation. That's a real, if small, risk during
a recovery you're already doing under pressure; save `known_hosts` (shown
once, when the repository is created) alongside the password if you want to
avoid it.

## Where to run it

It's a plain binary with no server dependency, so run it wherever you can
reach the repository's storage (SFTP host, S3 endpoint, or the filesystem
path) and have the credentials for it — that might be a laptop, not
necessarily any of your homelab hosts. It's built alongside the other two
binaries but isn't part of either container image's normal use: run it with
`docker run --rm --entrypoint /usr/local/bin/voidgrid-backup-recover <image> ...`
if you'd rather not install Go, or build it locally with `scripts/go.sh
build ./cmd/voidgrid-backup-recover`.
