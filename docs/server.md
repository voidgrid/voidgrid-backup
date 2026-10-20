# voidgrid-backup-server

The server is the single pane of glass: web UI, JSON API, scheduler and the
catalog of agents, repositories, jobs and run history. It never reads or
writes backup data itself: it tells agents what to do over mutual TLS, and
agents talk to the repository directly.

## Quick start

```sh
cp examples/server/.env.example examples/server/.env   # then edit it
cd examples/server
mkdir -p data && sudo chown 1000:1000 data
docker compose up -d
```

`VB_UID`/`VB_GID` default to `1000:1000`, the usual first non-root user on
a Linux host — if that's already you, `./data` often needs no `chown` at
all. Set them to your own user (`id -u`/`id -g`) instead if it isn't: the
image doesn't care which uid it runs as, only that it can write `./data`.

Open `http://<host>:<VB_PORT>/`. The first visit is always to `/setup` —
see [Authentication](#authentication) below before you get there, it needs
a token from the log. Then register an agent (see [agent.md](agent.md)) and
approve it on the Agents page, add a repository, and add jobs.

## Configuration

Every setting is a flag and an environment variable; a flag given on the
command line wins over the environment.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen` | `VB_LISTEN` | `:8080` | HTTP address for the UI, API and `/healthz` |
| `-agent-listen` | `VB_AGENT_LISTEN` | `:9442` | TLS address agents register on. In the example compose `VB_AGENT_PORT` sets this and the published port together. |
| `-data` | `VB_DATA` | `/data` | Directory for `catalog.db`, `pki/` and `session.key` |
| `-poll` | `VB_POLL` | `30s` | How often every agent is checked |
| `-oidc-issuer` | `VB_OIDC_ISSUER` | (empty) | OIDC provider issuer URL. Empty means sign-in is by recovery code only — see [Authentication](#authentication) |
| `-oidc-client-id` | `VB_OIDC_CLIENT_ID` | | OIDC client ID |
| `-oidc-client-secret` | `VB_OIDC_CLIENT_SECRET` | | OIDC client secret |
| `-oidc-redirect-url` | `VB_OIDC_REDIRECT_URL` | | Callback URL exactly as registered with the provider, e.g. `https://backup.example.com/auth/callback` |
| `-oidc-allowed-emails` | `VB_OIDC_ALLOWED_EMAILS` | (empty) | Comma-separated allowlist. Empty allows any identity the provider authenticates |
| | `TZ` | `UTC` | Time zone for schedules and displayed times |
| `-cache-dir` | `VB_CACHE_DIR` | | Base directory for Kopia's local cache of the repositories the server maintains itself, one subdirectory per repository. Empty keeps it under `-data`. |
| `-ipv4-only` | `VB_IPV4_ONLY` | `false` | Connect to SFTP and S3 storage over IPv4 only (also notification webhooks and OIDC discovery; not SMTP). Off by default. See the agent page for when to use it. |
| `-cache-metadata-mb` | `VB_CACHE_METADATA_MB` | `256` | Size limit in MiB, per repository, of the metadata cache |
| `-cache-content-mb` | `VB_CACHE_CONTENT_MB` | `64` | Size limit in MiB, per repository, of the file-data cache. `0` turns the whole cache off. |

Job schedules are evaluated in the server's `TZ`, and so is every time the
interface shows (browsers' own zones are not used, so a cron expression and the
times around it always agree). The header of every page names the zone, for
example `America/Chicago · CDT (UTC-05:00)`, and the schedule fields say which
zone they are in. The time zone database is built into the binary, so `TZ`
works in the minimal image. Set `TZ` to your own zone, as `UTC` is only the
default.

> **Upgrading from v0.9.0-beta.13 or earlier:** those builds read every cron
> expression in UTC, whatever `TZ` was. From v0.9.0-beta.14 they are read in
> `TZ`, as described above. A job set to `0 1 * * *` to run at 20:00 Central
> now runs at 01:00 Central. Change each job's schedule to its local hour
> (`0 20 * * *`) when you upgrade, and until you have, expect one run at the
> old expression's local hour: the next run is counted from the last one.
>
> From v0.9.0-beta.15 the Jobs page offers a one-time **conversion**: a preview
> lists every scheduled job with its schedule now and after converting (the
> same clock time as before, at today's UTC offset, moving the weekdays too
> when an hour crosses midnight). Untick a job you already set in local time,
> then convert. Schedules that are not a single daily time (`*/15`, several
> hours, ranges, a day of the month) are listed but must be edited by hand. The
> conversion can be done once, and it is recorded so it can't shift schedules
> twice; there is also a button to hide the offer without converting. A server
> whose zone is UTC is never offered it.

## The data directory

| Path | What it is | If you lose it |
|---|---|---|
| `catalog.db` | SQLite: agents, repositories (including their passwords), jobs, runs | You have to add repositories and jobs again. The backups themselves are safe in the repositories, if you still have their passwords. |
| `pki/ca.crt`, `pki/ca.key` | The CA every agent trusts | Every agent has to register again |
| `pki/registry.crt`, `pki/registry.key` | Certificate of the agent registration listener; the registration token pins it | Recreated, which changes the token: update `VB_TOKEN` on agents that are not approved yet |
| `pki/server.crt`, `pki/server.key` | The server's client certificate for talking to agents | Recreated from the CA |

`catalog.db` holds repository passwords in plain text, so the directory is
created `0700` and the database `0600`. Back this directory up somewhere that
does not depend on the server.

## Health check

`GET /healthz` returns `200 ok` when the server is up and its catalog answers
a query, `503` otherwise. The image has no curl, so the compose health check
runs the binary's own probe:

```sh
voidgrid-backup-server healthcheck   # exit 0 healthy, 1 unhealthy
```

It reads the same `VB_LISTEN` and probes `/healthz` on loopback.

## Network and security

- Agents register with the server on its own TLS listener (`VB_AGENT_LISTEN`,
  default `:9442`), separate from the web UI port, so the UI can stay behind a
  reverse proxy. Its certificate is self-signed; the registration token pins it.
  A valid token only creates a pending entry that you approve or reject.
- After approval the server connects **to** the agent (default port 9443), so
  the server must be able to reach each agent's address. It accepts only the
  exact certificate issued at approval, so after an agent is replaced the old
  host's certificate stops working.
- Wrong-token attempts on the registration port are logged once per source
  address per minute, followed by one line with the count, so they can't
  flood the log.
- Agent traffic is mutual TLS with the server's own CA. An approved agent
  accepts only the server's client certificate.
- Cookies (the session and the sign-in state) are always `HttpOnly` and
  `SameSite=Lax`, and `Secure` whenever the request arrived over HTTPS, either
  directly or through your reverse proxy. Behind a proxy that ends TLS, the
  server only knows the request was HTTPS if the proxy sends
  `X-Forwarded-Proto: https`; without it the cookies are not marked `Secure`,
  and a browser would send them over plain HTTP if you ever visit the UI that
  way. Traefik sends the header by default; check your own proxy's
  documentation, and in nginx add `proxy_set_header X-Forwarded-Proto $scheme;`. The server trusts the header
  from whoever sends it, which is harmless: a client faking it over plain HTTP
  only makes its own cookie `Secure`, and the browser then discards it. On plain
  HTTP (a LAN with nothing in front) the cookies are not `Secure`, because a
  browser would drop them.
- Cross-site form posts are refused (Go's `net/http.CrossOriginProtection`),
  so another site in your browser can't trigger actions here even while
  you're signed in.

## Authentication

**Signing in is always required — there is no "every route is open" mode.**
OIDC (Authelia, Authentik, Keycloak, Google, etc.) is one way to sign in,
configured through the setup wizard or the **Authentication** page — not
only `-oidc-issuer` and friends (see the table above), though those work
too, and always win over whatever's configured through the UI (see
[Configuring OIDC](#configuring-oidc)). Not configuring it doesn't disable
sign-in, it just means the only way is a recovery code (below). This
doesn't depend on or interact with any reverse proxy's own auth; it's the
server's own, so publishing it directly still requires signing in either
way.

When OIDC is configured:
- Anyone who completes the provider's login is let in, unless an allowed-
  identities list names specific ones — worth setting once more than one
  person could reach the provider.
- The sign-in chooser (`/auth/login`) offers **Sign in with SSO** alongside
  **Use a recovery code**, so a broken provider is never something you have
  to actually reach to see the alternative.

Without it, the chooser only offers the recovery code — still a real,
supported way to run this on a single-host homelab where standing up an
OIDC provider is more infrastructure than the backup tool itself warrants.

Either way:
- The session is a signed cookie, valid 30 days, backed by a key generated
  on first use and stored at `<data>/session.key`. Losing it (or restoring
  a backup from before it existed) just signs everyone out; it holds no
  backup data or secrets from your OIDC provider.
- `/healthz` stays open, for the container health check.
- The JSON API returns `401`, not a redirect, when not signed in.
- Signing out clears the local session only; it doesn't call back to an
  OIDC provider to end that session there too.

### Setup wizard and recovery codes

Every install needs at least one way to sign in that doesn't depend on
external infrastructure — otherwise a wrong OIDC client secret, an
unreachable issuer, or a `redirect_url` mismatch would lock out every route
with no recourse short of editing `.env` and restarting the container. So
the very first thing any install does, OIDC or not, is generate 10
one-time recovery codes, the same idea as a TOTP app's backup codes.

**The first time the server starts:**

1. It logs a one-time **setup token** (look for `setup_token` in the
   container's logs) and refuses to let *anything* else through — the UI,
   the API, the OIDC redirect if one is configured — until setup finishes.
   `/healthz` is the one exception, so the container's own health check
   doesn't fail during this window.
2. Visit `/setup`, enter that token, optionally fill in an OIDC provider
   right there (or leave it for later — see [Configuring
   OIDC](#configuring-oidc)), and submit. The server generates 10 recovery
   codes, shown **once** — record them somewhere that doesn't depend on
   this server (a password manager, or paper), the same as a repository
   password.
3. From then on, `/` redirects to the sign-in chooser (`/auth/login`).

**A recovery code**, entered at `/auth/recovery`, signs you in exactly like
a successful OIDC login would. If no OIDC provider is configured, this is
the *only* way to sign in — not just a fallback. Each code works once;
reusing one is rejected. Once signed in (by either path), visiting `/setup`
again lets you generate a fresh batch of 10, which immediately invalidates
every code in the old one, used or not — do this after using a few, or if
you suspect they've leaked.

`/setup` is unreachable to an anonymous visitor once codes already exist —
that's the "unavailable otherwise" half of it. Nobody who reaches the
server after you've completed it can mint themselves a fresh batch without
already being signed in.

### Configuring OIDC

Set it up during the wizard, or later from the **Authentication** page
(signed in, since it's a normal settings page at that point) — same
fields either way: issuer URL, client ID, client secret, redirect URL
(exactly as registered with the provider), and an optional comma-separated
allowed-identities list. Turning OIDC on, off, or pointing it at a
different provider **takes effect immediately, no restart** — the change
is validated (the server actually performs OIDC discovery against the
issuer you gave it) before it's saved, so a typo is caught right there on
the form instead of surfacing later as a failed restart.

`-oidc-issuer` and friends always win when given, exactly as before: a
server started with those flags/env vars shows its OIDC settings as
"set by environment variables, not editable here" on both pages, and
managing OIDC through the UI only takes effect once they're removed.

If OIDC was working when last configured but its issuer is unreachable at
a later **startup** (network down, the provider moved), the server logs
the failure and falls back to recovery-code-only sign-in rather than
refusing to start — a process that won't boot at all is a worse lockout
than OIDC being temporarily unavailable.

## Using the UI

### Status

The home page (`/`) is the one place to see whether everything is fine, in four
short lines, each a link to the page behind it: **Agents** (how many are
reachable, which are not, how many are waiting for approval), **Jobs** (the
result of each job's last backup, with the names of failed and partial ones and
how many are running now. A restore, check or dump import that failed or was
partial in the last 7 days counts too, even when the job's last backup was
fine, and stays until a later run of that kind succeeds), **Repositories** (whether maintenance last
succeeded, and any that are not set up) and **Next backup** (the soonest
scheduled start). A line turns amber or red when something needs attention, and
the heading says how many do. The page only reads the server's own records, so
it never waits on an agent or a repository, and it refreshes itself every 20
seconds. The agent list is at `/agents`.

### Agents

Agents register themselves (see [agent.md](agent.md#registration)) and wait
under **Waiting for approval**. Each entry shows the agent's key fingerprint;
approve only if it matches the one in that agent's log. Approve one with a name and the address the
server can reach it on (`host:port`, pre-filled from where it connected and
the port it reported); the address is editable. Choosing an existing agent
under **Replaces** makes the new registration take over that agent's ID, jobs
and snapshots, which is how a wiped or redeployed host comes back. Reject
refuses an agent; Forget removes a rejected entry so it can register again.
Agents can be renamed at any time: the name is only a label.

**Delete** an agent from the same page (type its name to confirm). It is refused
while the agent still has jobs, and while it is the Kopia maintenance owner of a
repository: the page then says which repositories, with a form to hand each
one's maintenance to the server or another assigned agent, after which the
delete can be repeated. The owner is read through another assigned agent, or
through the server if it can open the repository; if neither can, the delete is
allowed with a warning, since nothing could maintain that repository anyway.
Deleting removes the agent, its repository assignments (and so its default
repository) and its registrations. Its snapshots stay in their repositories and
can still be browsed and restored through another agent.

#### What Replaces does

Replacing signs the new host's key with the **old agent's ID** and points that
agent at the new address, hostname and certificate. The name, jobs, schedules,
repository assignments, the per-agent SFTP key paths, the default repository
and maintenance ownership all stay, because they hang off the agent ID.

- **Why it matters.** Kopia identifies a source by host (the agent ID), user
  and path. A rebuilt host that replaces its agent keeps that ID, so its next
  backup finds the previous snapshot and is incremental, retention keeps
  pruning the same source, and the repository's maintenance owner still
  matches. Approved as a new agent instead, the host gets a new ID: its first
  backup scans everything again (the data still dedupes), the old snapshots
  belong to another host and are never pruned, and maintenance owned by the old
  agent stalls until you hand it over under **Maintainer...**.
- **What it does not carry.** The Kopia cache (it starts cold on the new host),
  mounts, and anything the jobs name: the key file path in the new container,
  the Docker projects and the libvirt domains must exist there.
- **Do not replace the wrong agent.** The old host loses trust at once, and
  its jobs now run on the new machine: if that is a different machine they
  fail, or back up other content under the old source identity.
- **The preselect.** When a waiting registration reports the same hostname
  (ignoring case) as exactly one approved agent, the form preselects that agent
  and says so, and warns if that agent was reached in the last few minutes. It
  is only a suggestion and nothing is approved for you: choose **(new agent)**
  if it is a different machine.

The **Registration token** is shown on the same page. It is one reusable
secret for all new agents, rotatable with a button; rotating makes the old
token stop working and drops agents that are still waiting, but agents that
are already approved are unaffected. `voidgrid-backup-server token` prints it
on the command line, for deploy scripts.

The table shows reachability, version and last contact, plus any warnings the
agent reports about itself (for example running without the permissions it
needs).

Each agent has two discovery pages:

- **stacks**: the Docker compose projects on that host, for stack jobs.
- **VMs**: the libvirt domains on that host, for VM jobs.

### Repositories

A repository is where snapshots are stored. The chosen agent creates it (or
connects to one already at that location) and becomes its maintenance owner:
the agent that runs Kopia's compaction and garbage collection.

| Kind | Fields | Notes |
|---|---|---|
| SFTP | host, port, user, path, key file or password, known_hosts | Hetzner Storage Box: port `23`, user `uXXXXXX`, a path relative to its home such as `backups/homelab`. The key file is a path **inside the agent** (e.g. `/keys/sftp_key`). Leave known_hosts empty to trust the key seen on first connect; it is pinned from then on. |
| S3 | endpoint, bucket, prefix, region, access key, secret | "Plain HTTP" only for a local test server. Only tested against Backblaze B2's S3 API, on a free account: see [testing.md](testing.md). |
| Filesystem | directory on the agent host | A local disk or a mount on that agent. |

- **Password**: leave it blank to have one generated. It is shown **once**.
  It is the only key to the data: record it somewhere that doesn't depend on
  your hosts (password manager, paper) — **and record the connection
  details you enter here too** (host/bucket, path, user, key or
  credentials). The password alone isn't enough to recover with the server
  gone; see [recovery.md](recovery.md) for what `voidgrid-backup-recover`
  needs and why.
- **Reed-Solomon error correction** (bitrot protection) costs about 2% space
  and can only be chosen when the repository is created.

#### Using a repository from another agent

Every request the server sends to an agent carries the repository's
connection details and its password, so an agent that has never seen the
repository can still open it. The password is stored once, on the server;
you don't copy it to agents. What an agent needs on its own depends on the
kind:

| Kind | Needed on the other agent |
|---|---|
| S3 | Nothing. The access key and secret are part of the repository. |
| SFTP with a password | Nothing. |
| SFTP with a key file | The key file at the **same path** as in the repository (e.g. `/keys/sftp_key`), and its public key authorized on the SFTP server. See [SFTP key naming](agent.md#sftp-key-naming). |
| Filesystem | The same directory reachable from that agent (a shared mount). |

This is what makes a restore on a different agent work (below). To back up
from several agents **into** one repository, see
[Sharing a repository between agents](#sharing-a-repository-between-agents).

#### Sharing a repository between agents

Kopia is built for several clients writing to one repository, and the identity
of each snapshot stays per agent (`voidgrid-backup@<agent ID>`), so agents'
histories and retention never touch each other.

- **Assign** an agent on the Repositories page (**Assign an agent...** on the
  row). The agent first opens the repository, never creating anything, and is
  assigned only if that works; the same form re-tests an assigned agent. A job
  can only be created for an agent assigned to its repository. Existing
  installs were migrated so every agent that already had a job is assigned.
- **Default repository.** Each agent has one default, which the add-job forms
  preselect (agent → stacks or VMs, and Jobs → path job, which follows the
  agent you pick). An agent's first repository becomes its default; change it
  with **Make default** beside the agent. When an agent can use several
  repositories and none is its default, the form asks you to choose.
- **Maintenance when adding a job.** The add-job forms have "Maintenance of this
  repository": leave it, or make the server or the job's agent the maintainer
  (the same change as **Maintainer...** on the Repositories page). It is set
  per repository, so it applies to every job on it, and it is refused while a
  job on the repository is running; the job is created either way and the
  notice says what happened.
- **Keys.** An SFTP repository stores a default key file path; give an agent
  its own with **Key file on that agent**, and authorize each agent's public
  key for the same SFTP user (one `authorized_keys` line per agent, see
  [SFTP key naming](agent.md#sftp-key-naming)). The key is tested when you
  assign. Agents only ever receive their own path.
- **Unassign** is refused while the agent has jobs on the repository.
- **Maintenance**: let the server maintain a repository that several agents
  share ([above](#letting-the-server-maintain-a-repository)); an agent owner
  that is replaced would otherwise stall it. The Repositories page says so for
  a shared repository that an agent still maintains.
- **Default location.** A new repository's SFTP path and S3 prefix start as
  `<hostname>-<first 6 characters of the agent ID>-backup` (for example
  `harbinger-92af5b-backup`; the server uses `voidgrid-server-backup`), so
  unrelated data never shares a directory and a path is readable. The ID part
  is there because adding a repository at a path that already holds one
  connects to it, so two hosts with the same hostname must not share a default.
  It is only a default, but a location is enforced: adding or editing a
  repository is refused when another repository already uses the same place
  (the same SFTP host, port, user and path, the same S3 endpoint, bucket and
  prefix, or the same directory). Two repositories in one place would write over
  each other's blobs. Reconnecting to existing data after a catalog loss still
  works, since the catalog no longer lists the old repository.
- **SFTP connection limit.** A Hetzner Storage Box allows up to 10 simultaneous
  connections per account
  ([Hetzner's Storage Box documentation](https://docs.hetzner.com/robot/storage-box/general)),
  and other SFTP providers have limits of their own. Every backup, check,
  restore, maintenance run, snapshot refresh and repository measuring holds one
  connection to its SFTP host while it works; when the host is at its limit the
  next one waits its turn instead of failing. Set the limit on the Repositories
  page, **SFTP connection limit**: *Automatic* (the default: 10 on a
  `*.your-storagebox.de` host, no limit elsewhere), *at most N* connections to
  every SFTP host, or *Off*. Connections are counted per host and port, not per
  user, because Hetzner counts per box. The limit covers what the server starts;
  an agent's own scheduled or manual command line use is not counted. Jobs
  still overlap in time on a shared box, so keep staggering their schedules
  (the Jobs page warns about overlaps).
- Several agents writing to one repository has only been tested between
  in-process agents on a local filesystem repository, not against a real
  Storage Box or S3.

#### Editing a repository

**Edit...** on a repository's card changes its location or credentials: the
SFTP path, key file, password and known_hosts; the S3 access key and secret;
or the filesystem directory. The kind, host, port, user, bucket, endpoint,
prefix and the Kopia password cannot change, because that would be a different
repository. A blank field keeps what is stored, and secrets are never shown.

- **A new path moves the data.** Kopia's blobs are relative to the storage
  path, so the repository is moved with one directory rename on the storage
  (SFTP, or a filesystem rename on the same filesystem; never S3, see below). The server
  asks an agent that uses the repository to open it first and remember its
  identity, move it, open it at the new path and confirm it is the same
  repository. Only then is the new path saved. If any step after the move
  fails the data is moved back; if even that fails the error says where the
  data is. A destination that already exists is refused.
- **Nothing may be running** on the repository: a backup, maintenance,
  measuring or a wipe refuses the edit, and the edit holds those off until it
  ends. The move waits for the chosen agent's current operation.
- **The cache follows.** An agent that finds an earlier connection to the same
  repository under other settings takes over its Kopia cache (keyed by blob
  ID, so still valid) and drops the old connection, so the first backup after
  an edit is as fast as the one before it. The old settings, including any
  secrets, are not kept.
- Another agent that uses the repository needs nothing: it connects with the
  new settings on its next run. Update the connection details you recorded for
  recovery.

##### Why an S3 repository cannot be moved

The S3 bucket, endpoint and prefix are fixed once a repository is created; only
the access key and secret can be edited. This is deliberate, not an omission.

- **S3 has no rename.** Moving a prefix means copying every object under it to
  the new prefix and then deleting the old ones. A repository is thousands of
  objects (Kopia packs are about 20 MiB each, so 150 GB is on the order of
  7,500 packs plus index blobs). The SFTP and filesystem moves are one
  directory rename, a single metadata operation, which is why they are safe to
  offer in a form.
- **The copy is not atomic.** An interruption leaves two partial copies, and
  nothing in the repository says which is complete. A safe move would have to
  copy, verify every object, switch the settings and only then delete, and be
  able to resume after a failure.
- **Deleting does not always free the space.** On a bucket set to keep old
  versions, deleting the originals only hides them until a lifecycle rule
  removes them, so a move can double what you pay for until then. Copy and request costs depend on the provider.
- **A different bucket or endpoint** is a copy to different storage, not a
  rename.

The prefix is only a name, and the default never has to
change. If you do need a repository at another prefix or bucket:

1. Make sure nothing is running on it.
2. Copy the objects with the provider's tool or with rclone, or use Kopia's own
   `kopia repository sync-to`, which copies a repository to another location.
   Check that every object arrived.
3. Delete the repository's jobs (their snapshots stay in the repository),
   **Remove** the repository (its data is not touched), and add it again at the
   new location with the **same password**: adding connects to a repository
   that already exists there. Create the jobs again. Run history is lost; the
   snapshots are not.
4. Run a backup and a restore from the new location, then delete the old
   objects.

An in-place S3 move (copy, verify, switch, delete, resumable) could be built
the same way as the SFTP one. It has not been, and it should be tested against
a real bucket first, since S3 itself has not yet been exercised against real
storage here.

#### When the maintenance owner is gone

If the host that owns a repository's Kopia maintenance is replaced or removed
without handing it over, a maintenance cycle fails with "the maintenance owner
is ... not a registered agent". The repository's card then shows **Take over
maintenance** with the server (when it can open the repository) and the agents
assigned to it. Choosing one makes it the owner, needs nothing to be running on
the repository, and makes the next cycle due at once. Approving a rebuilt host
under **Replaces**, and the hand-over that **Delete agent** asks for, avoid
getting here.

#### When only the server can reach a repository

A repository is normally opened by an agent. If the server can open it itself
(S3, SFTP with the key at the same path inside the **server** container, or a
filesystem directory given as **Path on the server**), it can stand in when no
agent can reach it. **Manage, Check access** on a repository asks every agent
assigned to it, and the server, to open it and says which can.

- **The card says whether the server can.** Under the location, a repository
  the server cannot open (a filesystem directory with no path on the server, or
  a last check that failed) shows a warning with the reason; one it can open,
  and one never checked, show a plain line. The check is made in the background
  when an agent adds a repository, and again by **Manage, Check access**. The
  page itself never opens the repository.
- **Reads fall back by themselves.** Measuring a repository and refreshing a
  job's snapshot list try the agent first and, if it fails and the server can
  open the repository, do it through the server's own engine. The card says
  "measured by the server" and the job page says the list was made through the
  server. Browsing and restoring through the server already had a picker.
- **Deletes are explicit.** Deleting a snapshot and wiping a repository never
  fall back. A snapshot's delete form offers **through the server** only when
  the job's agent is unreachable and the server can open the repository. The
  wipe form always lists **the server** next to the agents when it can. A job's
  Delete form, when **Also delete its snapshots** is ticked and the job's agent
  is unreachable, offers **Delete the snapshots through: the server**; deleting
  the job alone needs no agent at all. The server only deletes snapshots that
  belong to the job's agent.
- The server then needs the storage credentials and the repository password
  (it already holds the passwords). A compromised server exposes that storage,
  as it already does through the passwords and configs.
- **Creating a repository from the server.** The add-repository form lists
  **the server** next to the agents. The server then creates (or connects to)
  the repository with its own engine, and it maintains it, since creating it
  makes the server the Kopia maintenance owner. An SFTP key path is a path
  inside the server container, and a filesystem repository needs its **Path on
  the server**. No agent is assigned, and none counts as assigned until one is
  tested under **Manage, Assign an agent** (which makes that agent open the
  repository first): the usual shortcut that lets the first agent onto a
  repository nobody was assigned to does not apply to these.
- **Editing through the server.** The edit page offers **the server** when it
  can open the repository, and preselects it when no agent is assigned. It
  moves an SFTP repository (one rename on the storage server) and tests any
  kind, with the same checks as through an agent: the same repository must open
  at the new place, or the data is moved back. It is never chosen for you. A
  filesystem repository is moved through an agent, because the server only sees
  it through a mapped path that the edit does not change.

#### Maintenance

Maintenance (compaction and garbage collection) is run by the repository's
maintenance owner, and only by it. It runs at the end of each backup when
Kopia's own schedule says it is due, and in addition the server runs a **full**
cycle for each repository that has at least one job. A repository with a running
job waits. When the cycle runs is set per repository (Repositories, Manage,
**When maintenance runs**):

- **After the repository's jobs have finished** (the default). The cycle starts
  once every enabled, scheduled job on the repository has run since the last
  cycle began, at least 20 hours after that cycle began. A job that failed has
  still run, so a failing job does not stop maintenance. A scheduled job counts
  only if it is due within the next 12 hours, so a weekly job is not waited for
  by a daily cycle. A repository whose jobs are all manual runs after any one of
  them has run. If the cycle has waited 48 hours past its earliest time it runs
  regardless, so a job that never runs cannot starve it.
- **At a fixed time.** A cron expression in the server's time zone. The cycle
  runs at those times whatever the jobs do, so a time ahead of the first job
  gives "before the jobs". A cycle that would start while a job is running waits
  for it to finish.

A failed cycle is tried again after about an hour in either mode. Switching mode
re-plans the next cycle at once. The first runs after a server start are spread
over 2 hours in "after" mode.
A repository without jobs is never scheduled. The **Run now** button on the
Repositories page runs a full cycle on demand and works for every repository.
On a job's page, tick **then run maintenance** next to **Back up now** to back
the job up and then run a full cycle on its repository, due or not; it is
skipped if the backup fails. Every cycle is recorded with how long it took and
how long it waited for its agent: the Repositories page lists the last 10, and
the Schedule page draws the next one as a striped bar on the agent's lane. A
cycle holds its agent like a backup, so jobs on that agent queue behind it.

The server asks an agent with a job on the repository; if that agent is not the
owner it names the owner (the owner's host name is its agent ID) and the server
asks that agent. If the owner is no longer a registered agent, for example a
host that registered again without "replaces", the row shows that and no agent
can run maintenance.

Running maintenance does **not** free space at once. Kopia keeps deleted data
for at least 24 hours and needs two garbage-collection cycles, so space from
deleted snapshots comes back after about two days of daily runs.

#### Letting the server maintain a repository

By default the agent that owns a repository runs its maintenance. Because the
owner is an agent, replacing or re-registering that host leaves the repository
without a maintainer. A repository can instead be maintained by the server,
whose identity (`voidgrid-backup@voidgrid-server`) does not change:

- Repositories page, **Maintainer...** on the row: pick "server", type the
  repository name, press Change. The server first opens the repository itself
  (it never creates anything), then makes itself the Kopia maintenance owner.
  Nothing else changes; backups still run on the agents.
- It is refused while a job on the repository, a maintenance run or a wipe is
  running. Kopia has no repository-wide maintenance lock, so the owner is only
  changed while nothing is running.
- The server needs to reach the storage itself. S3 and SFTP with a password
  work as they are. SFTP with a key file needs that key at the same path inside
  the **server** container; the test in the previous step tells you whether
  it works.
- A **filesystem** repository is a directory on an agent host. Give its path
  as the server sees it (**Path on the server**, optional) if the same
  directory is mounted in the server container; leave it blank and the
  repository stays maintained by its agent. Use the same share, mounted the
  same way, on both: NFS and SMB caching can show one side stale data for a
  moment. The agents never receive this path. Before switching, an agent that
  uses the repository opens it at its own path and must report the same
  repository ID as the server read through the mapped path; if the IDs differ,
  or no agent can answer (offline, or older than the server), nothing changes.
- The server's clock must be right: Kopia refuses to run maintenance when the
  server's time and the repository's differ too much.
- Choosing "an agent" has that agent open the repository and make itself the
  owner. The server does not need to reach the repository for this, so it works
  even when the server has lost access. An agent older than the server answers
  that it needs updating.

#### Removing backups

- **Delete a snapshot**: on the job page, type its short ID in the box on its
  row and press Delete. Only snapshots of that job's agent can be deleted. The
  data is released later by maintenance (see above).
- **Remove a repository**: Repositories page, Remove... section. Takes the
  repository off the server and leaves its data in storage, so it can be added
  again with its password. Refused while any job uses it.
- **Wipe a repository**: same section. Deletes every blob of the repository from
  storage through the agent you pick, then removes it from the server. Needs no
  repository password, cannot be undone, and frees the space at once. It
  deletes 8 blobs at a time, the repository's format blob last, so a wipe that
  stops halfway can be run again. It refuses
  when no repository is found at the location, and while any job uses it or the
  repository is busy. Empty directories remain on SFTP and filesystem storage.
  If it fails, the error stays on the row and it can be run again.

All of these ask you to type the name (or snapshot ID) and are logged at
`WARN` on the server and the agent.

### Jobs

There are three kinds of job, all with a cron schedule (standard 5 fields,
blank for manual only) and retention counts (keep latest/hourly/daily/
weekly/monthly/annual):

- **Path job** (Jobs page): absolute directories on one agent, plus
  gitignore-style excludes. `/`, `/proc`, `/sys`, `/dev` and `/run` are refused.
- **Stack job** (agent → stacks): one compose project. See
  [agent.md](agent.md#compose-stacks).
- **VM job** (agent → VMs): one libvirt domain. See [agent.md](agent.md#virtual-machines).

The result column of the Jobs list is the job's last backup. Under it, in grey,
are the latest restore, check and dump import of that job with their result and
age, so a failed restore is visible without opening the job.

While a job runs, its row and its page show where the upload is, for example
`running 34% · 23 GiB of 68 GiB · …/library/2024`; before Kopia has sized the
source it shows `scanning, N files`. The figures come from the agent every few
seconds and the pages refresh every three while anything runs. See
[agent.md](agent.md#progress).

A job page has "Back up now", "Verify repository", the schedule switch, a
collapsed **Delete...** form, run history and the snapshot list (the Jobs list
has no delete control: open the job). Deleting a job removes it and its run history;
its snapshots stay in the repository unless you tick **Also delete its
snapshots**, which forgets every snapshot in the job's list first (and keeps the
job if that fails). That cannot be undone, and the space is released by the next
full maintenance cycle.

**Verify repository** runs a check on demand: it confirms every snapshot's
metadata and directory listings, that every content blob still exists, spot
checks (~10%) of file contents by downloading, decrypting and hashing them,
and test restores the newest snapshot of the job's smallest source (up to
500 MiB) to a scratch directory to confirm the restored file count and size
match what was recorded. With Reed-Solomon error correction on, bitrot found
along the way is repaired automatically. It shares the job's run lock, so it
can't overlap a backup or restore of the same job, and shows up in run
history as kind `check`.

### Runs

| Status | Meaning |
|---|---|
| success | Everything was backed up / restored |
| partial | It finished, but something needs attention: unreadable files (named), a failed database dump, a crash-consistent VM copy, or **ACTION NEEDED** for a VM disk left on an overlay |
| failed | Nothing usable was produced |
| running | In progress. Runs still "running" when the server restarts are marked failed on start. |

A finished backup's summary gives the paths done, the changed files, the size of
the source, how much of it was **added to the repository** (new data after
deduplication and compression, from Kopia's upload counter; left out for an
agent that predates the figure) and the old snapshots pruned. A run that
failed with "the agent stopped or lost its connection during the run" means the
agent's connection dropped while it worked: the agent restarted, was stopped or
lost the network. A network failure while opening a repository is retried for
up to 15 minutes first (see [agent.md](agent.md)).

### Browsing and restoring

Click a snapshot to browse it. From any directory you can restore that
directory (or the whole snapshot) as files to a directory on the agent.
Without "overwrite" the target must be empty or not exist yet, so a restore
never half-completes over existing data.

Stack and VM snapshots also offer a whole-stack / whole-VM restore and, for
stacks, "import dump". In-place restores ask you to type the stack or VM name.

#### Restoring on a different agent

With more than one agent enrolled, the snapshot page has a **Browse and
restore on** picker. It defaults to the job's own agent. Pick another one to
browse the snapshot and restore it there, for example when the job's host is
gone or you want the data on a new machine. The snapshot list comes from the
server, so the page opens even when the job's agent is down.

- A restore on another agent always goes into a **target directory** (empty or
  not yet existing, as usual). "Original locations" and in-place restores of
  stacks and VMs run only on the job's own agent.
- Stack and VM restores into a directory write the snapshot's files there
  (`stack/`, `mounts/`, `dumps/`, or the disks and `domain.xml`) and touch no
  containers or VMs, so that agent needs neither Docker nor libvirt. Putting
  the stack or VM back into service afterwards is manual.
- The other agent must be able to open the repository (see
  [above](#using-a-repository-from-another-agent)) and have a writable
  directory for the target ([Paths and read-only mounts](agent.md#paths-and-read-only-mounts)).
- The run is recorded under the original job, and its summary names the agent
  it ran on.

#### Restoring on the server

When the job's agent is broken or gone and no other agent suits, the same
picker offers **The server**. The server then reads the repository itself and
writes the restored files into a directory **inside its own container**, so
mount a volume at the path you restore to, or the files vanish with the
container. It needs no agent, Docker or libvirt.

- It is offered only when the server can open the repository: S3, SFTP with
  the key mounted in the server container (see
  [Letting the server maintain a repository](#letting-the-server-maintain-a-repository)),
  or a filesystem repository with a **Path on the server**. Otherwise the
  option is greyed out and its tooltip says why.
- Like any other restore on a different host it always goes into a target
  directory; in-place restores stay on the job's own agent.
- Files are owned by the user the server runs as, and ownership and special
  permissions that need root are not restored unless it runs as root. That is
  not a failure: choosing **The server** (or an agent that is not root) on the
  snapshot page shows a note saying so, with the uid:gid the files will get, and
  the run finishes as a success whose summary reads "complete, files belong to
  uid:gid N:N (the server does not run as root, so the ownership in the backup
  was not restored)". A restore is only partial when a file could not be written.
- To restore only part of a stack or VM snapshot (say one data folder), browse
  into it and use **Restore as files** on that folder; the "Restore stack"
  form restores the whole snapshot.
- If the server itself is gone too, use
  [voidgrid-backup-recover](recovery.md).

### Schedule

The **Schedule** page draws a window of the day as a timeline, one lane per
agent (or per repository with the link above it). Each scheduled job is a bar
from its start time for as long as its backups take; click a bar to open the
job. The line marks now, and bars that have already started are dimmed.

- **Zoom** the window to 5 or 15 minutes, or 1, 3, 6, 12 or 24 hours, with the
  buttons above the timeline (24 hours is the default). The 5 and 15 minute
  views are for jobs that last seconds. The window starts at the beginning of
  the current hour, or of the current 5 or 15 minutes in those two views.
  **Earlier** and **later** move it by its own length, **now** goes back to the
  current hour, and the **Start at** field sets an exact time. The zoom and
  start are in the page's address (`span` is the length in minutes), so a link
  or a refresh keeps the view. A bar is never drawn narrower than a few pixels,
  and a name that does not fit in its bar is written beside it when there is
  room (otherwise it is in the hover text). A
  run that began before the window and is still going at its left edge is
  drawn from the edge.
- A job's length is the median of its last 20 finished backups (the shorter
  of the middle two for an even count), not counting the time a run waited for
  its agent (runs from before that was recorded still include it). The first
  backup uploads everything and is far longer than the rest, so a median lets
  it stop mattering after a second or third run; hover a bar for the longest
  run. A job with one finished backup uses it. A dashed bar has no finished
  backup yet and assumes 15 minutes. Maintenance bars use the median of the
  last 10 cycles the same way.
- Jobs on one agent run one after another, so overlapping bars in an agent
  lane wait for each other. Jobs on one repository share its connections and
  bandwidth. Each job has its own row in its lane (jobs in name order, then
  maintenance), with every run of it on that row, so a job keeps its row when
  you drag it. Bars that overlap in time are drawn in the warning colour.
- **Drag a bar sideways** to change when its job starts, in 5 minute steps (1
  minute in views of 3 hours or less).
  The bar's job gets a new minute and hour in its cron schedule, and it
  applies from the next scheduled start. This works for a schedule with one
  start time a day (`0 20 * * *`, or limited to weekdays such as
  `0 20 * * 1,3`); dragging past midnight moves the weekdays with it. Other
  schedules (`*/15`, several hours, ranges, a day of the month, `@daily`) are
  not draggable and are edited on the Jobs page. A drag made from a page that
  is out of date is refused, so it can't overwrite a newer edit.
- Manual and switched-off jobs are listed under the timeline.

#### Suggesting start times

Both of these only suggest; nothing changes until you save or press Apply.

- **New jobs.** The add-job forms (Jobs, and each agent's stacks and VMs pages)
  prefill the Schedule field with the earliest start that overlaps nothing on
  the chosen agent and repository, and say so under the field. It follows the
  agent and repository you pick, and it only replaces the field while you have
  not typed in it. A job that has never run is assumed to take 15 minutes, so
  this is a guess: **a backup job needs to run at least twice before its time
  can be placed accurately**, because the first run uploads everything and is by
  far the longest. Once it has, use the plan below.
- **Suggest a plan** (a button under the timeline) proposes start times for the
  jobs on a schedule of one start a day, every day (`30 3 * * *`). The rules:
  start between 00:00 and 07:00 in the server's time zone, one job at a time on
  an agent (they queue behind each other anyway), at most two at once on a
  repository (a Storage Box allows about ten connections in all), a 10 minute
  gap after each job, longest first. Maintenance cycles and jobs on any other
  schedule (weekdays only, several a day, ranges) stay where they are and are
  worked around. A job that already fits stays put, so the plan is stable:
  asking again changes nothing until a length or a schedule changes. The plan
  lists only the jobs that would move, with each one's length (flagged as a
  guess while it has fewer than two finished backups), **Apply** per job or
  **Apply all**. A job whose schedule changed since the plan was drawn is
  skipped, not overwritten. When the window has no free gap for a job it is
  placed where it clashes least and marked.

### Notifications

One server-wide setting, not per-job, sent to whichever of these are enabled:

- A **scheduled** backup sends only when it finishes `partial` or `failed`. A
  success is silent.
- A backup, check or restore you start **by hand** sends whatever its outcome,
  success included. A restore can run for hours, and nobody is expected to
  watch the page that long.
- Maintenance failures are not sent yet; they show on the Repositories page.

A run in progress has a **Cancel** button, on the Jobs list and the job page. It
stops the work (an agent resumes the containers it paused and merges a VM's
disk overlay back), and the run ends as `failed` with the reason "cancelled by
<user>", which sends an alert like any other failure.

| Channel | What it needs |
|---|---|
| Discord | A webhook URL from a channel's Integrations settings. Posts directly, no third party involved. |
| [Notifarr](https://notifiarr.com/) | An API key and the numeric Discord channel ID to deliver to, from Notifarr's Passthrough integration. Goes through Notifarr rather than posting to Discord directly, so it shows up alongside whatever else you already route through it. |
| Email | An SMTP server: host, port, optional username/password, a from address and at least one recipient. Port 465 is implicit TLS; anything else tries STARTTLS if the server offers it. |

#### Setting up Discord

1. In Discord, open the channel's settings (the gear next to its name), then
   **Integrations**, **Webhooks**, **New Webhook**.
2. Pick the channel, name the webhook, and choose **Copy Webhook URL**.
3. In the server's **Notifications** page, tick **Enabled** under Discord, paste
   the URL into **Webhook URL**, and save.
4. Press **Send test notification**. The message should appear in the channel.

The URL contains a secret token: anyone with it can post to the channel. The
page never shows it again; leave the field blank when saving to keep it.

#### Setting up Notifarr

Notifarr posts to Discord through its own bot, so two things are needed on
Discord's side as well as in Notifarr.

1. In Notifarr, open the **Passthrough** integration and copy its API key.
2. In Discord, turn on Developer Mode (User Settings, Advanced), right-click the
   channel the alerts should go to, and choose **Copy Channel ID**. It is a
   number of about 18 digits. The `1234567890` in Notifarr's own examples is a
   placeholder: using it makes Notifarr accept the message and then fail to
   deliver it (Notifarr lists the service with a last status of `404` and a send
   count of 0).
3. Make sure Notifarr's bot is in that Discord server and can view the channel,
   send messages and embed links there (a private channel needs the bot added).
4. In the server's **Notifications** page, tick **Enabled** under Notifarr, paste
   the API key and the channel ID, optionally set the embed colour as six hex
   digits, and save.
5. Press **Send test notification**. The result says what Notifarr answered. The
   first message creates a service named `voidgrid-backup` in Notifarr's
   Passthrough page; its send count goes above 0 once a message is delivered.

#### Setting up email

Give the server an SMTP host and port, a from address and at least one
recipient. Use port 465 with **Implicit TLS** ticked, or 587 (or 25) with it
unticked, in which case the connection starts in plain text and upgrades with
STARTTLS if the server offers it. Add a username and password if the server
needs them. Press **Send test notification** to check it.

All three can be on at once. A secret field (webhook URL, API key, SMTP
password) left blank when saving keeps whatever was already stored for that
channel. To turn a channel off, uncheck "enabled" and save: its settings and
secret are kept, so it can be switched back on later without retyping them, and
it is not sent to or tested while off (handy to test one channel at a time). "Send test notification" fires one test message to every
currently-enabled channel and reports each result, without needing a real
failure to test with.

### Help

The **Help** link in the header opens this documentation (server, agent,
recovery and testing) inside the UI. The pages are built into the server, so
they always match the version that is running and need no internet access. The
newest docs are in the repository on GitHub.

## JSON API

Read-only, and behind the same login as the UI when one is configured (see
[Authentication](#authentication)) — a request with no valid session gets
`401`, not a redirect:

| Endpoint | Returns |
|---|---|
| `GET /api/agents` | Agents with status and warnings |
| `GET /api/repositories` | Repositories, secrets redacted |
| `GET /api/jobs` | Jobs and their configuration |
| `GET /api/jobs/{id}/runs` | The last 100 runs of a job |

## Upgrading

Pull the new image and recreate the container. The catalog schema migrates
itself on start. Upgrade agents too: an agent older than the server may not
know newer RPCs, which shows up as an error on the run.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Agent "unreachable" | The error under it on the Agents page; can the server reach `host:port`? Was the agent's data directory lost (then register it again, approving it as replacing this one)? |
| An agent never appears under "Waiting for approval" | Its `VB_SERVER` reaches the registration port, and its `VB_TOKEN` is the current one (rotating changes it) |
| Schedules run at the wrong hour | `TZ` in the server's `.env` |
| A run is "partial" | The run's detail lines say exactly what and where |
| "the agent stopped or lost its connection during the run" | The agent restarted or lost the network while it worked: its log around that time, and whether its container was recreated |
| Adding or editing a repository says it "already uses" a place | Another repository has the same SFTP host, port, user and path, S3 endpoint, bucket and prefix, or directory. Every repository needs its own |
| Backups sit as "running" but nothing moves | They may be waiting for a free connection to the storage host: the server log says `waited for a free storage connection`; see **SFTP connection limit** under Sharing a repository between agents |
