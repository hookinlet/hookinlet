# Deploying

hookinlet is one program with one data directory. This page says what it needs
from wherever you run it. It does not give recipes for particular hosts: any
platform that meets these requirements will do.

## Quick start

With Docker, two environment variables and a volume:

```sh
export HOOKINLET_ADMIN_TOKEN=$(openssl rand -base64 32)   # keep these two
export HOOKINLET_MASTER_KEY=$(openssl rand -base64 32)

docker run -d --name hookinlet -p 8080:8080 \
  -v hookinlet-data:/data \
  -e HOOKINLET_ADMIN_TOKEN -e HOOKINLET_MASTER_KEY \
  ghcr.io/hookinlet/hookinlet:latest
```

(`latest` is the newest final release. A pre-release such as `0.1.0-rc.1` has only
its own version tag.)

Create an inbox, send it a webhook, and read it back:

```sh
curl -s -X POST localhost:8080/admin/inboxes \
  -H "Authorization: Bearer $HOOKINLET_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"slug": "demo", "verifier": "none"}'
# → {"inbox": {…, "ingest_path": "/in/q3V9xR1hT0kYz2mP8uWb6A"}, "agent_token": "hki_…"}   (the token is shown once)

curl -s -X POST localhost:8080/in/q3V9xR1hT0kYz2mP8uWb6A -d '{"hello": "world"}'
# → {"seq":1}

curl -s localhost:8080/v1/inboxes/demo/messages -H "Authorization: Bearer hki_…"
```

An inbox whose verifier is `none` (it has to be asked for) accepts anything sent to its URL and stores it as not verified. To accept
only signed webhooks from Stripe, GitHub, and others, see
[verifiers](verifiers.md). All the ways to manage inboxes are in
[administration](admin.md), the agent-facing API in [api](api.md), and the MCP
tools in [mcp](mcp.md).

Without Docker, download the archive for your platform from the release page,
check it, and run it:

```sh
sha256sum -c --ignore-missing checksums.txt
tar -xzf hookinlet_<version>_linux_amd64.tar.gz
HOOKINLET_ADMIN_TOKEN=… HOOKINLET_MASTER_KEY=… ./hookinlet serve
```

Binaries are built for Linux and macOS on amd64 and arm64. They are static
(`CGO_ENABLED=0`) and need nothing installed. The macOS binaries are not
notarized: one downloaded through a browser is quarantined, and
`xattr -d com.apple.quarantine hookinlet` (or fetching it with `curl`) lets it run.

## What it needs

- **A persistent, writable data directory** (`HOOKINLET_DATA_DIR`, `/data` in the
  image). It holds one SQLite database. Losing it loses every message and inbox.
  The image runs as a non-root user (uid 65532) and `/data` is declared a volume.
- **Exactly one instance per data directory.** SQLite and the in-process
  notifier assume a single process. Running two against one directory, or putting
  the directory on a network filesystem that does not honor file locks, corrupts
  data or loses wake-ups. Scaling past one node is outside the open-source core.
- **TLS in front.** hookinlet serves plain HTTP. Terminate TLS in a proxy or
  load balancer, and set `HOOKINLET_TRUSTED_PROXIES` to the proxy's addresses
  (CIDRs) so that `X-Forwarded-For` is believed when recording where a delivery
  came from. Without it the header is ignored and the proxy's address is recorded.
  Set `HOOKINLET_PUBLIC_BASE_URL` to the public address (`https://hooks.example.com`)
  so that the ingest URLs the admin API hands out are right.
- **Proxy timeouts that fit long-lived requests.** An SSE stream is quiet for up
  to 15 seconds between pings (`HOOKINLET_SSE_HEARTBEAT`), and an MCP
  `wait_for_messages` call can wait up to 60 seconds without sending anything. The
  proxy's idle or read timeout should exceed both, or those requests are cut
  off. Streams send `X-Accel-Buffering: no`; a proxy that buffers responses
  anyway will hold events back.
- **The two secrets, kept.** `HOOKINLET_ADMIN_TOKEN` (at least 32 characters, no
  whitespace, not mostly one repeated character) is the credential for the admin
  API and can read every inbox. Keep the admin API off the public address with
  `HOOKINLET_ADMIN_LISTEN_ADDR`, or turn it off with `HOOKINLET_ADMIN_ENABLED=false`
  ([why and how](admin.md#keeping-the-admin-api-off-the-public-address)).
  `HOOKINLET_MASTER_KEY` (32 random bytes, base64) encrypts verifier secrets in
  the database. **Lose the master key and every stored verifier secret is
  unreadable**; keep it with your backups, and never in the data directory.
  Prefer environment variables: flags show up in process listings. Each secret is
  sealed with a short id of the key and with its inbox's slug, so the server can tell a
  wrong key from damage: **it refuses to start** when a stored secret does not open
  with the key it was given (and says which inboxes, and what to do), and `/readyz`
  fails if that becomes true later. To change the key on purpose, stop the server and
  run `hookinlet rekey` with the old key in `HOOKINLET_OLD_MASTER_KEY` and the new one
  in `HOOKINLET_MASTER_KEY`; it seals every secret again under the new key, changes
  nothing if any secret opens with neither, and can be run again if interrupted.
  (The same command with no old key brings secrets written by an older release up to
  the current format; they work meanwhile.) A secret sealed under a key you no longer
  have cannot be recovered: set it again with `hookinlet inbox set-secret`.
- **SIGTERM or SIGINT to stop it.** It stops accepting connections, ends streams
  and waits, and lets deliveries that are being stored finish, for up to
  `HOOKINLET_SHUTDOWN_TIMEOUT` (10 seconds by default); anything still running
  then is cut off. It then stops the retention sweep and checkpoints the database,
  which is quick. A platform that waits less than the timeout before killing the
  process risks cutting off a delivery; killing it at once never damages the data
  (SQLite recovers from the log), but the delivery in flight is lost, and the
  sender, who got no answer, will normally retry.
- **Nothing else.** It makes no network calls of its own, at startup or later.

## Settings

Every setting is a flag and an environment variable (`--listen-addr` is also
`HOOKINLET_LISTEN_ADDR`). A flag wins over the variable. `hookinlet serve -h`
lists them all.

| Variable | Default | Meaning |
|---|---|---|
| `HOOKINLET_ADMIN_TOKEN` | required | Admin credential. |
| `HOOKINLET_MASTER_KEY` | required | Encrypts verifier secrets. |
| `HOOKINLET_LISTEN_ADDR` | `:8080` | Address to listen on. |
| `HOOKINLET_DATA_DIR` | `./data` | Where the database lives. |
| `HOOKINLET_PUBLIC_BASE_URL` | from the request | Public address, for ingest URLs. |
| `HOOKINLET_TRUSTED_PROXIES` | none | CIDRs whose `X-Forwarded-For` is believed. |
| `HOOKINLET_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `HOOKINLET_LOG_FORMAT` | `json` | `json` or `text`. |
| `HOOKINLET_MAX_STREAMS` | `1000` | SSE streams open at once; more get 503. Also bounds MCP waits. |
| `HOOKINLET_MAX_STREAMS_PER_INBOX` | half of the above | Most streams or waits on one inbox. |
| `HOOKINLET_SSE_HEARTBEAT` | `15s` | Ping interval on idle streams. |
| `HOOKINLET_SSE_RECHECK_INTERVAL` | `0` (off) | Re-read the store this often even without a wake-up. Needed only by a notifier that can drop signals; the built-in one does not. |
| `HOOKINLET_ADMIN_ENABLED` | `true` | Serve the admin API (`/admin/...`). `false` leaves inboxes and tokens to the command line. |
| `HOOKINLET_ADMIN_LISTEN_ADDR` | none | Serve the admin API on this address instead of `HOOKINLET_LISTEN_ADDR`, and nothing else there. |
| `HOOKINLET_MCP_ENABLED` | `true` | Serve `/mcp`. |
| `HOOKINLET_MCP_INLINE_BODY_BYTES` | `16384` | Largest body MCP tools return inline (at most 65536). |
| `HOOKINLET_RETENTION_INTERVAL` | `1h` | How often retention runs; `off` for never. |
| `HOOKINLET_SHUTDOWN_TIMEOUT` | `10s` | How long shutdown waits. |

A bad setting is reported, all of them at once, and the server does not start.

## Health checks

- `GET /healthz` answers `200` while the process is up.
- `GET /readyz` answers `200` when the database is reachable and `503` when it
  is not. Use it for readiness, and `/healthz` for liveness.

Neither needs a credential.

## Starting, and how fast

The server is ready as soon as the database is open: there is no warm-up, no
index build, and no network call. Measured from exec to the first successful
`/readyz` on a database of a million messages, restarting with the file already in
the operating system's cache, the median of seven starts was **4 ms on Linux and
11 ms on macOS** (a 2026 laptop); the limit this project holds itself to is 500 ms.
Start-up time does not grow with the size of the database. A start on a host whose
disk cache is empty, or the platform's own time to start a container, comes on top
of that. Retention, which can be slow on a large database, runs after
the server is listening and never delays it. A platform that stops an idle
instance and starts it on the next request therefore does not make that request
wait for hookinlet.

> **Scale to zero.** An open SSE stream or MCP wait is an open connection, which
> some platforms count as activity and so keep the instance awake. An agent that
> wants the instance to sleep when idle should poll `/v1/inboxes/{slug}/messages`
> rather than hold a connection.

## Resources

One instance is small. Idle, the process uses about 10 to 12 MiB of memory on
Linux. Each open SSE stream costs about 50 KiB, so 500 of them come to about
35 MiB. A page of messages is built in memory and bounded (16 MiB at most), and
reads are limited to 16 at a time. Deliveries are not limited in number: each one
is held in memory while it is read and checked, up to the inbox's body limit
(1 MiB by default, at most 10 MiB), so a burst of large deliveries uses that much
each. Put a rate limit in front, or in a hook (see [extending](extending.md)). Disk is
what grows: size the volume for the messages you keep, and set retention on
inboxes that receive a lot. A backup needs free space in the data directory for a
second copy of the database while it runs. Each stream or wait holds a file
descriptor; the default of 1000 needs a limit comfortably above that.

## Upgrading

Start the new version on the same data directory. It applies any database
migrations itself, once, before it listens (`hookinlet migrate` does only that,
and reports the schema version). Migrations only move forward: a database whose schema is newer than
a version knows (one that added a migration) is refused by that older version.

**A copy is kept for you.** When there are migrations to apply to a database that
already holds data, the server first writes a complete copy of it as it was to
`hookinlet.db.before-migration-N.bak` in the data directory (`N` is the schema
version it was at; mode 0600). If the upgrade goes badly, stop the server, put that
file back as `hookinlet.db` (remove any `-wal` and `-shm` beside it), and start the
previous release. If the copy cannot be made (no room for a second copy of the
database), the server refuses to migrate and changes nothing. Only the latest copy
is kept, and nothing deletes it, so remove it when you trust the upgrade. It is as
sensitive as the database. Taking your own [backup](admin.md#backups) first is still
sensible. Changes that affect anyone
embedding the packages are listed in `CHANGELOG.md`; this is a pre-1.0 project
and those may be breaking.

## Backups

`GET /admin/backup`, `hookinlet backup --out FILE`, or copying the data
directory while the process is stopped. See [administration](admin.md#backups),
including how to restore (after restoring from a copy of the data directory, run
`hookinlet renew-epochs`, so that readers' old cursors are not trusted). Keep the master key with the backup, but apart from it.

## Logs

Logs go to standard error, as JSON lines unless `HOOKINLET_LOG_FORMAT=text`. Each
request is logged with its route, status, duration, and an `X-Request-Id` (which
is also sent back to the client). They name inboxes by slug and sequence number
and never contain message bodies, tokens, or secrets.

## Checking a release

Each release has, per archive, a SHA-256 in `checksums.txt` and a software bill of
materials (`*.sbom.json`). The container image is published for `linux/amd64`
and `linux/arm64` with build provenance and an SBOM attached as attestations
(`docker buildx imagetools inspect ghcr.io/hookinlet/hookinlet:<version>`). A tag
with a suffix, such as `v0.1.0-rc.1`, is a pre-release: its image gets no
`latest` tag.
