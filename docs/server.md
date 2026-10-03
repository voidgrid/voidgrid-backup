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

Job schedules are evaluated in the server's `TZ`. The time zone database is
built into the binary, so `TZ` works in the minimal image.

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
| S3 | endpoint, bucket, prefix, region, access key, secret | "Plain HTTP" only for a local test server. |
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

### Jobs

There are three kinds of job, all with a cron schedule (standard 5 fields,
blank for manual only) and retention counts (keep latest/hourly/daily/
weekly/monthly/annual):

- **Path job** (Jobs page): absolute directories on one agent, plus
  gitignore-style excludes. `/`, `/proc`, `/sys`, `/dev` and `/run` are refused.
- **Stack job** (agent → stacks): one compose project. See
  [agent.md](agent.md#compose-stacks).
- **VM job** (agent → VMs): one libvirt domain. See [agent.md](agent.md#virtual-machines).

A job page has "Back up now", "Verify repository", the schedule switch, run
history and the snapshot list. Deleting a job removes it and its run history;
its snapshots stay in the repository.

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
| partial | It finished, but something needs attention: unreadable files (named), a failed database dump, a crash-consistent VM copy, ownership that couldn't be restored, or **ACTION NEEDED** for a VM disk left on an overlay |
| failed | Nothing usable was produced |
| running | In progress. Runs still "running" when the server restarts are marked failed on start. |

### Browsing and restoring

Click a snapshot to browse it. From any directory you can restore that
directory (or the whole snapshot) as files to a directory on the agent.
Without "overwrite" the target must be empty or not exist yet, so a restore
never half-completes over existing data.

Stack and VM snapshots also offer a whole-stack / whole-VM restore and, for
stacks, "import dump". In-place restores ask you to type the stack or VM name.

### Notifications

One server-wide setting, not per-job: a **scheduled** job that finishes
`partial` or `failed` is sent to whichever of these are enabled. A manual
run never notifies — you're already watching it in the UI — and neither
does a success.

| Channel | What it needs |
|---|---|
| Discord | A webhook URL from a channel's Integrations settings. Posts directly, no third party involved. |
| [Notifarr](https://notifiarr.com/) | An API key and the numeric Discord channel ID to deliver to, from Notifarr's Passthrough integration. Goes through Notifarr rather than posting to Discord directly, so it shows up alongside whatever else you already route through it. |
| Email | An SMTP server: host, port, optional username/password, a from address and at least one recipient. Port 465 is implicit TLS; anything else tries STARTTLS if the server offers it. |

All three can be on at once. A secret field (webhook URL, API key, SMTP
password) left blank when saving keeps whatever was already stored for that
channel — the way to turn a channel off is to uncheck "enabled," not to
blank its secret. "Send test notification" fires one test message to every
currently-enabled channel and reports each result, without needing a real
failure to test with.

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
