# Deploying

hookinlet is one program with one data directory. This page says what it needs
from wherever you run it. It does not give recipes for particular hosts: any
platform that meets these requirements will do.

## Quick start

With Docker, two environment variables and a volume:

```sh
export HOOKINLET_ADMIN_TOKEN=$(openssl rand -base64 32)   # keep these two
export HOOKINLET_MASTER_KEY=$(openssl rand -base64 32)

docker run -d --name hookinlet -p 127.0.0.1:8080:8080 \
  -v hookinlet-data:/data \
  -e HOOKINLET_ADMIN_TOKEN -e HOOKINLET_MASTER_KEY \
  ghcr.io/hookinlet/hookinlet:latest
```

(`latest` is the newest final release. A pre-release such as `0.1.0-rc.1` has only
its own version tag.)

Three things people trip on:

- **Keep the two values.** They exist only in your shell. The container is
  restarted with the same master key or not at all: a different one cannot open the
  secrets already stored, and the server says so and refuses to start (see
  [troubleshooting](troubleshooting.md#the-server-refuses-to-start)). Put them in a
  password manager or a secrets file (`HOOKINLET_MASTER_KEY_FILE`) before you close
  the shell.
- **Use a named volume, or give a directory to uid 65532.** The image runs as a
  non-root user. `-v hookinlet-data:/data` (a Docker volume) just works;
  `-v ./data:/data` fails with "permission denied" on Linux unless you first
  `chown 65532:65532 ./data`.
- **`-p 127.0.0.1:8080:8080`** publishes the port on this machine only. Plain
  `-p 8080:8080` exposes the admin API to your network; read
  [Keeping the admin API off the public address](admin.md#keeping-the-admin-api-off-the-public-address)
  before you do that.

If it does not come up, `docker logs hookinlet` says why.

Create an inbox, send it a webhook, and read it back:

```sh
curl -s -X POST localhost:8080/v1/admin/inboxes \
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
cd hookinlet_<version>_linux_amd64
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
- **A proxy that passes the right headers.** See [Behind a proxy](#behind-a-proxy)
  for starting points.
- **Proxy timeouts that fit long-lived requests.** An SSE stream is quiet for up
  to 15 seconds between pings (`HOOKINLET_SSE_HEARTBEAT`), and an MCP
  `wait_for_messages` call can wait up to 60 seconds without sending anything. The
  proxy's idle or read timeout should exceed both, or those requests are cut
  off. Streams send `X-Accel-Buffering: no`; a proxy that buffers responses
  anyway will hold events back.
- **The two secrets, kept.** `HOOKINLET_ADMIN_TOKEN` (at least 32 characters, no
  whitespace, at least 10 different characters) is the credential for the admin
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
  in `HOOKINLET_MASTER_KEY`; it opens every secret before it writes any (so a wrong old key
  changes nothing), seals them again under the new key, leaves alone any inbox that was
  edited while it ran, and can be run again if interrupted.
  (The same command with no old key brings secrets written by an older release up to
  the current format; they work meanwhile.) A secret sealed under a key you no longer
  have cannot be recovered: set it again with `hookinlet inbox set-secret`.
- **Secrets from files.** `HOOKINLET_ADMIN_TOKEN`, `HOOKINLET_MASTER_KEY`, `HOOKINLET_OLD_MASTER_KEY` and `HOOKINLET_VERIFIER_SECRET` may each be given as `..._FILE`, the path of a file holding the value (one trailing newline is dropped), the way Docker, Kubernetes and systemd hand secrets over. Giving both forms is an error.
- **SIGTERM or SIGINT to stop it.** See [Stopping](#stopping).
- **Nothing else.** It makes no network calls of its own, at startup or later.

## Behind a proxy

hookinlet needs the proxy to terminate TLS, to pass `Host` and `X-Forwarded-For`
through, not to buffer streams, and to wait long enough on quiet connections. Both
snippets below assume hookinlet listens on `127.0.0.1:8080`, so set
`HOOKINLET_TRUSTED_PROXIES=127.0.0.1/32` and
`HOOKINLET_PUBLIC_BASE_URL=https://hooks.example.com`. **With Docker,** hookinlet sees the proxy at the bridge gateway (often `172.17.0.1`), not at
`127.0.0.1`, so set `HOOKINLET_TRUSTED_PROXIES` to that address (`docker network inspect bridge`
shows it), and note that `HOOKINLET_ADMIN_LISTEN_ADDR=127.0.0.1:...` is the container's own
loopback, which the host cannot reach: bind it to `0.0.0.0` inside and publish the port on the host's
loopback (`-p 127.0.0.1:9090:9090`). These are starting points written
from each proxy's documentation: the project's tests do not run them, so try yours with
a test delivery and a stream.

**Caddy** (TLS is automatic):

```caddyfile
hooks.example.com {
	# The admin API and metrics are for you, not the internet. Reach them on the
	# admin address instead (HOOKINLET_ADMIN_LISTEN_ADDR=127.0.0.1:9090) and block them here.
	@private path /v1/admin/* /metrics
	respond @private 404

	request_body {
		max_size 11MB     # the largest body an inbox may take is 10 MiB
	}
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1          # do not hold stream events back
	}
}
```

**nginx** (with certificates from your usual source):

```nginx
server {
    listen 443 ssl;
    server_name hooks.example.com;
    # ssl_certificate / ssl_certificate_key ...

    client_max_body_size 11m;

    location ~ ^/(v1/admin/|metrics) { return 404; }   # use the admin address instead

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Connection "";
        proxy_buffering off;            # streams and waits
        proxy_read_timeout 90s;         # longer than a 60 s wait and a 15 s heartbeat
    }
}
```

With either, ask the server whether it is ready from outside
(`curl -s https://hooks.example.com/readyz`), and send a test delivery.

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
| `HOOKINLET_MAX_STREAMS_PER_CREDENTIAL` | a tenth of `HOOKINLET_MAX_STREAMS`, at least 20 | Most streams or waits one token may hold, so that one token cannot take every slot. The admin token is not limited. Raise it for an agent platform that follows many inboxes with one token. |
| `HOOKINLET_MAX_READS_PER_CREDENTIAL` | a quarter of the 16 read slots | Most read slots one token may hold at once. The admin token is not limited. |
| `HOOKINLET_SSE_HEARTBEAT` | `15s` | Ping interval on idle streams. |
| `HOOKINLET_SSE_RECHECK_INTERVAL` | `0` (off) | Re-read the store this often even without a wake-up. Needed only by a notifier that can drop signals; the built-in one does not. |
| `HOOKINLET_ADMIN_ENABLED` | `true` | Serve the admin API (`/v1/admin/...`). `false` leaves inboxes and tokens to the command line. |
| `HOOKINLET_ADMIN_LISTEN_ADDR` | none | Serve the admin API on this address instead of `HOOKINLET_LISTEN_ADDR`. Only the admin API and the console (`/admin`) are served there, with the read API the console shows messages with (`/v1/inboxes/...`; the same tokens work). With the console off (`HOOKINLET_ADMIN_UI=false`) only the admin API is. |
| `HOOKINLET_MCP_ENABLED` | `true` | Serve `/mcp`. |
| `HOOKINLET_ADMIN_UI` | `true` | Serve the admin console at `/admin`, on the same listener as the admin API. It needs the admin API on (`HOOKINLET_ADMIN_ENABLED`). |
| `HOOKINLET_MCP_INLINE_BODY_BYTES` | `16384` | Largest body MCP tools return inline (at most 65536). |
| `HOOKINLET_INGEST_MEMORY_BUDGET` | `268435456` | Most bytes of request bodies held in memory at once by deliveries being received; more get 503. `0` turns the bound off. |
| `HOOKINLET_DRAIN_DELAY` | `0` | After being told to stop, keep serving for this long with `/readyz` at 503, so a load balancer takes the instance out of rotation first. |
| `HOOKINLET_RETENTION_INTERVAL` | `1h` | How often retention runs; `off` for never. |
| `HOOKINLET_SHUTDOWN_TIMEOUT` | `10s` | How long shutdown waits. |

A bad setting is reported, all of them at once, and the server does not start.

## Commands with the image

The image holds one binary, `/hookinlet`, so the commands in these documents run through
Docker. Against a running container:

```bash
docker exec hookinlet /hookinlet inbox list
docker exec hookinlet /hookinlet backup --out /data/backup.db
docker cp hookinlet:/data/backup.db ./backup.db     # or: curl .../v1/admin/backup -o backup.db
```

The copy stays in the volume, where it is as sensitive as the database and where a second
`backup --out /data/backup.db` is refused without `--force`. The image has no shell, so remove
it with another image: `docker run --rm -v hookinlet-data:/data alpine:3 rm /data/backup.db`.

To restore a backup into the volume, stop the container, replace the database (and remove the
old `-wal` and `-shm` files, see [Restoring](admin.md#backups)) with another image that has a
shell, check it, and start again with the **same master key**:

```bash
docker stop hookinlet
docker run --rm -v hookinlet-data:/data -v "$PWD":/backup:ro alpine:3 sh -c '
  rm -f /data/hookinlet.db /data/hookinlet.db-wal /data/hookinlet.db-shm /data/backup.db &&
  cp /backup/backup.db /data/hookinlet.db &&
  chown 65532:65532 /data/hookinlet.db && chmod 600 /data/hookinlet.db'
docker run --rm -v hookinlet-data:/data ghcr.io/hookinlet/hookinlet:latest check
docker start hookinlet
```

Commands that need the server stopped (`migrate`, `rekey`, `renew-epochs`) run in a
throwaway container on the same volume, with the secrets they need in its environment:

```bash
docker stop hookinlet
docker run --rm -v hookinlet-data:/data -e HOOKINLET_MASTER_KEY -e HOOKINLET_OLD_MASTER_KEY \
  ghcr.io/hookinlet/hookinlet:latest rekey
```

## Stopping

Send `SIGTERM` or `SIGINT`. With `HOOKINLET_DRAIN_DELAY` set, it first reports itself not ready for that long while still serving. It stops accepting connections, ends streams and waits, and lets deliveries that are being stored finish, for up to `HOOKINLET_SHUTDOWN_TIMEOUT` (10 seconds by default); anything still running then is cut off. It then stops the retention sweep and checkpoints the database, which is quick. With `HOOKINLET_DRAIN_DELAY` set, the platform must wait for the delay *and* the timeout (Docker's default grace period of 10 seconds equals the default timeout alone, so give `docker stop --time` or the orchestrator's termination grace period their sum). A second signal ends the process at once. A platform that waits less than the timeout before killing the process risks cutting off a delivery; killing it at once never damages the data (SQLite recovers from the log), but the delivery in flight is lost, and the sender, who got no answer, will normally retry.

## Health checks

- `GET /healthz` answers `200` while the process is up.
- `GET /readyz` answers `200` when the server can do its work, and `503` when it
  cannot: the database is unreachable, the volume it is on has under 32 MiB free
  (or the last write failed for lack of room), none of the stored verifier secrets opens
  with the master key (one inbox whose secret does not open is logged and fails
  its own deliveries, but does not take the server out of rotation), or the server has been told to stop and is
  [draining](#stopping). Use it for readiness, and `/healthz` for liveness.

Neither needs a credential. The image has no `curl`, so it carries
`hookinlet healthcheck` (it asks `/readyz` of this machine, at `HOOKINLET_LISTEN_ADDR`;
`--live` asks `/healthz`; `--url` asks another address), and the `Dockerfile` uses it
as its `HEALTHCHECK` with a start period of 60 seconds for a long migration. It finds
the server from the environment, so set `HOOKINLET_LISTEN_ADDR` rather than passing
`--listen-addr` in the command if you change the port.

## Metrics

`GET /metrics` serves counters and gauges in the Prometheus text format, with the
admin token as a bearer token (so, where the admin API is on its own address, there):
deliveries by how they ended, requests by route and status, the ingest memory budget
in use, open streams, retention sweeps, and a few process figures. No series is
labelled with anything a client chooses (no inbox, path or query), so a scanner cannot
add series. A scrape job:

```yaml
scrape_configs:
  - job_name: hookinlet
    authorization: { credentials_file: /run/secrets/hookinlet_admin_token }
    static_configs: [{ targets: ["hookinlet:8080"] }]
```

## Disk space

A delivery that cannot be stored because the volume is full is answered `503`
(`storage_full`, with `Retry-After`) and is not acknowledged, so a sender that retries
loses nothing, and `/readyz` fails until there is room again, so a load balancer
stops sending. Free space is checked against a floor of 32 MiB. The write-ahead log is
cut back to 64 MiB whenever SQLite resets it, so one burst of writes does not leave a
large file behind.

## One server per data directory

A server takes a lock on the data directory (`hookinlet.lock`, an advisory lock the
operating system drops when the process ends, so there is nothing to clean up after a
crash). A second server started on the same directory stops at once and says which
process holds it, instead of two writers corrupting each other's work. `hookinlet
migrate`, `rekey` and `renew-epochs`, which must not run beside a server, take the same lock.
The other commands do not, so `hookinlet inbox ...` still works in the directory of a
running server. On a file system that does not support locks (some network and FUSE
mounts) the server starts anyway and logs a warning: nothing then stops a second one.

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
reads are limited to 16 at a time, a slot being held until the response has been written
to the client or `ResponseTimeout` (2 minutes) has passed, so a client that asks and never
reads cannot pin more than 16 pages. One credential may hold only a quarter of those slots
(4 of 16), so that a client that never reads cannot hold them all and turn every other inbox's
readers away; the admin token is not limited. In the same way one credential may hold only a
tenth of the stream slots (100 of 1000) and one inbox's deliveries only an eighth of the
budget below (32 MiB of 256; never less than its largest body), so that a token or an ingest key cannot take what everyone
shares. The first two are settings (`HOOKINLET_MAX_READS_PER_CREDENTIAL`,
`HOOKINLET_MAX_STREAMS_PER_CREDENTIAL`). Deliveries are not limited in number: each one
is held in memory while it is read and checked, up to the inbox's body limit
(1 MiB by default, at most 10 MiB), so a burst of large deliveries uses that much
each. The total is bounded: deliveries being received hold at most
`HOOKINLET_INGEST_MEMORY_BUDGET` bytes of bodies between them (256 MiB by default; at
least 10 MiB, the largest body an inbox may take). The bytes are counted as they
arrive, so a connection that has promised a large body and sent none costs nothing,
and a delivery whose bytes would take the total over is answered `503` with
`Retry-After` and not stored. The memory the process uses is a small multiple of the
bytes counted (buffers grow, and the body is copied into the database). Put a
rate limit in front, or in a hook (see [extending](extending.md)). Disk is
what grows: size the volume for the messages you keep, and set retention on
inboxes that receive a lot. A backup needs free space in the data directory for a
second copy of the database while it runs. Each stream or wait holds a file
descriptor; the default of 1000 needs a limit comfortably above that. A connection
cannot be held open by not sending or not reading: whatever a request is sent to, its
headers must arrive within 10 seconds and its body within 60, and an answer the client does
not take is given up after `ResponseTimeout` (an operation that needs a credential counts
that from when it starts building its answer).

## Upgrading

Start the new version on the same data directory. It applies any database
migrations itself, once, before it listens, and logs each one. A large database can take
minutes to migrate (the move of the message body, 0007, took about two and a half on
a million rows), during which nothing listens, so a health check that restarts the process
will restart the migration too: run `hookinlet migrate` as its own step with the old server stopped (the migration refuses while another process has the database open) (it applies
the migrations and reports the schema version) or give the health check a generous
start period. Only the process that holds the data directory's lock migrates: a command
such as `hookinlet inbox show` run against an old database refuses and tells you to stop
the server and run `hookinlet migrate`, so run those after upgrading, not before. Migrations only move forward: a database whose schema is newer than
a version knows (one that added a migration) is refused by that older version.

**A copy is kept for you.** When there are migrations to apply to a database that
already holds data, the server first writes a complete copy of it as it was to
`hookinlet.db.before-migration-N.bak` in the data directory (`N` is the schema
version it was at; mode 0600). If the upgrade goes badly, stop the server, put that
file back as `hookinlet.db` (remove any `-wal` and `-shm` beside it, and
`hookinlet.db.migrating`), and start the previous release. If the copy cannot be made (no room for a second copy of the
database), the server refuses to migrate and changes nothing. If an upgrade is cut
short (killed, out of disk, a start timeout), the next start keeps the copy that was
made before it began: the database is then partly upgraded, and the copy of that would
be no way back. (A marker file, `hookinlet.db.migrating`, records that; it goes when
an upgrade finishes.) Only the latest finished upgrade's copy is kept, and nothing
deletes it, so remove it when you trust the upgrade. **Room:** the migration that
moves the message body (0007) rebuilds the messages table, so allow for about three
times the database's size free in the data directory (the copy, the new table, and the
write-ahead log), and note that the file does not shrink afterwards. It is as
sensitive as the database. Taking your own [backup](admin.md#backups) first is still
sensible. Changes that affect anyone
embedding the packages are listed in `CHANGELOG.md`; this is a pre-1.0 project
and those may be breaking.

## Backups

`GET /v1/admin/backup`, `hookinlet backup --out FILE`, or copying the data
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
