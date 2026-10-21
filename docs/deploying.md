# Deploying

hookinlet is one program with one data directory. This page says what it needs from wherever you run it. It does
not give recipes for particular hosts. Any platform that meets these needs will do.

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

(`latest` is the newest final release. A pre-release such as `0.1.0-rc.1` has only its own version tag.)

Three things people trip on:

- **Keep the two values.** They exist only in your shell. Restart the container with the same master key, or not
  at all. A different key cannot open the secrets already stored. The server says so and refuses to start (see
  [troubleshooting](troubleshooting.md#the-server-refuses-to-start)). Put both values in a password manager or a
  secrets file (`HOOKINLET_MASTER_KEY_FILE`) before you close the shell.
- **Use a named volume, or give a directory to uid 65532.** The image runs as a non-root user.
  `-v hookinlet-data:/data` (a Docker volume) just works. `-v ./data:/data` fails with "permission denied" on
  Linux unless you first run `chown 65532:65532 ./data`.
- **`-p 127.0.0.1:8080:8080`** publishes the port on this machine only. Plain `-p 8080:8080` exposes the admin
  API to your network. Read
  [Keeping the admin API off the public address](admin.md#keeping-the-admin-api-off-the-public-address) before
  you do that.

If the container does not come up, `docker logs hookinlet` says why.

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

An inbox whose verifier is `none` accepts anything sent to its URL and stores it as not verified. You have to ask
for `none` on purpose. To accept only signed webhooks from Stripe, GitHub, and others, see
[verifiers](verifiers.md). [Administration](admin.md) covers all the ways to manage inboxes. The agent-facing
API is in [api](api.md), and the MCP tools are in [mcp](mcp.md).

Without Docker, download the archive for your platform from the release page, check it, and run it:

```sh
sha256sum -c --ignore-missing checksums.txt
tar -xzf hookinlet_<version>_linux_amd64.tar.gz
cd hookinlet_<version>_linux_amd64
HOOKINLET_ADMIN_TOKEN=… HOOKINLET_MASTER_KEY=… ./hookinlet serve
```

Binaries are built for Linux and macOS on amd64 and arm64. They are static (`CGO_ENABLED=0`) and need nothing
installed. The macOS binaries are not notarized, so macOS quarantines one downloaded through a browser. Run
`xattr -d com.apple.quarantine hookinlet` (or fetch the file with `curl`) to let it run.

## What it needs

- **A persistent, writable data directory** (`HOOKINLET_DATA_DIR`, `/data` in the image). It holds one SQLite
  database. Losing it loses every message and inbox. The image runs as a non-root user (uid 65532), and `/data`
  is declared a volume.
- **Exactly one instance per data directory.** SQLite and the in-process notifier assume a single process.
  Running two against one directory corrupts data or loses wake-ups. So does putting the directory on a network
  filesystem that does not honor file locks. Scaling past one node is outside the open-source core.
- **TLS in front.** hookinlet serves plain HTTP. Terminate TLS in a proxy or load balancer. Set
  `HOOKINLET_TRUSTED_PROXIES` to the proxy's addresses (CIDRs). Then hookinlet believes `X-Forwarded-For` when it
  records where a delivery came from. Without it, hookinlet ignores the header and records the proxy's address.
  Set `HOOKINLET_PUBLIC_BASE_URL` to the public address (`https://hooks.example.com`), so the ingest URLs the
  admin API hands out are right.
- **A proxy that passes the right headers.** See [Behind a proxy](#behind-a-proxy) for starting points.
- **Proxy timeouts that fit long-lived requests.** An SSE stream is quiet for up to 15 seconds between pings
  (`HOOKINLET_SSE_HEARTBEAT`). An MCP `wait_for_messages` call can wait up to 60 seconds without sending
  anything. The proxy's idle or read timeout should be longer than both, or the proxy cuts those requests off.
  Streams send `X-Accel-Buffering: no`. A proxy that buffers responses anyway will hold events back.
- **The two secrets, kept.** `HOOKINLET_ADMIN_TOKEN` is the credential for the admin API, and it can read every
  inbox. It must be at least 32 characters, with no whitespace and at least 10 different characters. Keep the
  admin API off the public address with `HOOKINLET_ADMIN_LISTEN_ADDR`, or turn it off with
  `HOOKINLET_ADMIN_ENABLED=false` ([why and how](admin.md#keeping-the-admin-api-off-the-public-address)).
  `HOOKINLET_MASTER_KEY` (32 random bytes, base64) encrypts verifier secrets in the database. **Lose the master
  key and every stored verifier secret is unreadable.** Keep the key with your backups, and never in the data
  directory. Prefer environment variables, because flags show up in process listings.

  hookinlet seals each secret with a short id of the key and binds it to its inbox. So the server can tell a
  wrong key from damage. **It refuses to start** when a stored secret does not open with the key it was given.
  It says which inboxes, and what to do. `/readyz` fails if that becomes true later.

  To change the key on purpose, stop the server and run `hookinlet rekey`. Put the old key in
  `HOOKINLET_OLD_MASTER_KEY` and the new one in `HOOKINLET_MASTER_KEY`. The command opens every secret before it
  writes any, so a wrong old key changes nothing. It seals the secrets again under the new key. It leaves alone
  any inbox that was edited while it ran. You can run it again if it is interrupted. (The same command with no
  old key brings secrets written by an older release up to the current format. They work meanwhile.) A secret
  sealed under a key you no longer have cannot be recovered. Set it again with `hookinlet inbox set-secret`.
- **Secrets from files.** You can give each of `HOOKINLET_ADMIN_TOKEN`, `HOOKINLET_MASTER_KEY`,
  `HOOKINLET_OLD_MASTER_KEY` and `HOOKINLET_VERIFIER_SECRET` as `..._FILE`. That is the path of a file holding
  the value. hookinlet drops one trailing newline. This is how Docker, Kubernetes and systemd hand secrets over.
  Giving both forms is an error.
- **SIGTERM or SIGINT to stop it.** See [Stopping](#stopping).
- **Nothing else.** It makes no network calls of its own, at startup or later.

## Behind a proxy

hookinlet needs the proxy to do four things: terminate TLS, pass `Host` and `X-Forwarded-For` through, not
buffer streams, and wait long enough on quiet connections. Both snippets below assume hookinlet listens on
`127.0.0.1:8080`. So set `HOOKINLET_TRUSTED_PROXIES=127.0.0.1/32` and
`HOOKINLET_PUBLIC_BASE_URL=https://hooks.example.com`.

**With Docker,** hookinlet sees the proxy at the bridge gateway (often `172.17.0.1`), not at `127.0.0.1`. So set
`HOOKINLET_TRUSTED_PROXIES` to that address. `docker network inspect bridge` shows it. Also note that
`HOOKINLET_ADMIN_LISTEN_ADDR=127.0.0.1:...` is the container's own loopback, which the host cannot reach. Bind
it to `0.0.0.0` inside, and publish the port on the host's loopback (`-p 127.0.0.1:9090:9090`).

These snippets are starting points written from each proxy's documentation. The project's tests do not run
them. Try yours with a test delivery and a stream.

**Caddy** (TLS is automatic):

```caddyfile
hooks.example.com {
	# The admin API, its console and metrics are for you, not the internet. Reach them on the
	# admin address instead (HOOKINLET_ADMIN_LISTEN_ADDR=127.0.0.1:9090) and block them here.
	@private path /v1/admin/* /admin /admin/* /metrics
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

    location ~ ^/(v1/admin/|admin(/|$)|metrics) { return 404; }   # API, console, metrics: use the admin address instead

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

With either, ask the server from outside whether it is ready (`curl -s https://hooks.example.com/readyz`), and
send a test delivery.

## Settings

Every setting is both a flag and an environment variable (`--listen-addr` is also `HOOKINLET_LISTEN_ADDR`). A
flag wins over the variable. `hookinlet serve -h` lists them all.

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
| `HOOKINLET_INGEST_MEMORY_BUDGET` | `134217728` | Most bytes of request bodies held in memory at once by deliveries being received; more get 503. The process may use about three times this, plus 50 MB, when it is full (see [Resources](#resources)). `0` turns the bound off. |
| `HOOKINLET_MAX_UNVERIFIED` | `1000` | Most unverified messages one inbox keeps (`on_verify_fail=store`, or the verifier `none`). A delivery over it gets `429` and is not stored; verified deliveries are never refused for it. `0` turns the cap off. See [verifiers](verifiers.md#unverified-messages-are-capped). |
| `HOOKINLET_DRAIN_DELAY` | `0` | After being told to stop, keep serving for this long with `/readyz` at 503, so a load balancer takes the instance out of rotation first. |
| `HOOKINLET_RETENTION_INTERVAL` | `1h` | How often retention runs; `off` for never. |
| `HOOKINLET_SHUTDOWN_TIMEOUT` | `10s` | How long shutdown waits. |

If any setting is bad, the server reports all the bad ones at once and does not start.

## Commands with the image

The image holds one binary, `/hookinlet`, so you run the commands in these docs through Docker. Against a
running container:

```bash
docker exec hookinlet /hookinlet inbox list
docker exec hookinlet /hookinlet backup --out /data/backup.db
docker cp hookinlet:/data/backup.db ./backup.db     # or: curl .../v1/admin/backup -o backup.db
```

The copy stays in the volume. There it is as sensitive as the database, and a second
`backup --out /data/backup.db` is refused without `--force`. The image has no shell, so remove the copy with
another image: `docker run --rm -v hookinlet-data:/data alpine:3 rm /data/backup.db`.

To restore a backup into the volume, stop the container. Replace the database with another image that has a
shell, and remove the old `-wal` and `-shm` files (see [Restoring](admin.md#backups)). Check the result, then
start again with the **same master key**:

```bash
docker stop hookinlet
docker run --rm -v hookinlet-data:/data -v "$PWD":/backup:ro alpine:3 sh -c '
  rm -f /data/hookinlet.db /data/hookinlet.db-wal /data/hookinlet.db-shm /data/backup.db &&
  cp /backup/backup.db /data/hookinlet.db &&
  chown 65532:65532 /data/hookinlet.db && chmod 600 /data/hookinlet.db'
docker run --rm -v hookinlet-data:/data ghcr.io/hookinlet/hookinlet:latest check
docker start hookinlet
```

Some commands need the server stopped: `migrate`, `rekey`, and `renew-epochs`. Run them in a throwaway
container on the same volume, with the secrets they need in its environment:

```bash
docker stop hookinlet
docker run --rm -v hookinlet-data:/data -e HOOKINLET_MASTER_KEY -e HOOKINLET_OLD_MASTER_KEY \
  ghcr.io/hookinlet/hookinlet:latest rekey
```

## Stopping

Send `SIGTERM` or `SIGINT`. Here is what the server then does, in order:

1. If `HOOKINLET_DRAIN_DELAY` is set, it first reports itself not ready for that long while it keeps serving.
2. It stops accepting connections, ends streams and waits, and lets deliveries that are being stored finish. It
   gives this up to `HOOKINLET_SHUTDOWN_TIMEOUT` (10 seconds by default). Anything still running then is cut off.
3. It stops the retention sweep and checkpoints the database, which is quick.

With `HOOKINLET_DRAIN_DELAY` set, the platform must wait for the delay *and* the timeout. Docker's default grace
period of 10 seconds equals the default timeout alone. So give `docker stop --time` or the orchestrator's
termination grace period the sum of the two. A second signal ends the process at once.

A platform that waits less than the timeout before killing the process risks cutting off a delivery. Killing the
process at once never damages the data, because SQLite recovers from the log. But the delivery in flight is
lost. The sender got no answer, so it will normally retry.

## Health checks

- `GET /healthz` answers `200` while the process is up.
- `GET /readyz` answers `200` when the server can do its work. It answers `503` when any of these is true:
  - the database is unreachable;
  - the volume it is on has under 32 MiB free (or the last write failed for lack of room);
  - none of the verifier secrets it tries opens with the master key;
  - the server has been told to stop and is [draining](#stopping).

  Use it for readiness, and `/healthz` for liveness. It tries up to 25 secrets. It looks at no more than the
  5000 oldest inboxes. Once it has 25 in hand, it looks at no more than 500 further ones. So the check costs the
  same however many inboxes there are. It checks again after 30 seconds, or after 5 while it is failing. An inbox
  whose secret does not open is logged and fails its own deliveries, but it does not take the server out of
  rotation. A server may hold several [tenants'](extending.md#tenants) inboxes. Then the sample takes at most 3
  secrets of each tenant before it takes more of any one. So a tenant whose secrets do not open is logged, with
  its tenant key, and does not take the server out of rotation either.

Neither needs a credential. The image has no `curl`, so it carries `hookinlet healthcheck`. That command asks
`/readyz` of this machine, at `HOOKINLET_LISTEN_ADDR`. `--live` asks `/healthz` instead, and `--url` asks another
address. The `Dockerfile` uses it as its `HEALTHCHECK`, with a start period of 60 seconds for a long migration.
It finds the server from the environment. If you change the port, set `HOOKINLET_LISTEN_ADDR` rather than
passing `--listen-addr` in the command.

The healthcheck is in the image's config. A runtime that ignores it will show no health status. For example,
Podman's Docker-compatible API (what the `docker` command talks to on some systems) ignores it, while
`podman run` honours it. There, give the check yourself with `--health-cmd "/hookinlet healthcheck"` (or a
`healthcheck:` entry in Compose).

## Metrics

`GET /metrics` serves counters and gauges in the Prometheus text format. Send the admin token as a bearer token.
Where the admin API is on its own address, use that address. The metrics cover deliveries by how they ended,
requests by route and status, the ingest memory budget in use, open streams, retention sweeps, and a few process
figures. No series is labelled with anything a client chooses (no inbox, path or query), so a scanner cannot add
series. A scrape job:

```yaml
scrape_configs:
  - job_name: hookinlet
    authorization: { credentials_file: /run/secrets/hookinlet_admin_token }
    static_configs: [{ targets: ["hookinlet:8080"] }]
```

## Disk space

If the volume is full, the server cannot store a delivery. It answers `503` (`storage_full`, with `Retry-After`)
and does not acknowledge the delivery. So a sender that retries loses nothing. `/readyz` also fails until there
is room again, so a load balancer stops sending. The server checks free space against a floor of 32 MiB. It cuts
the write-ahead log back to 64 MiB whenever SQLite resets it, so one burst of writes does not leave a large file
behind.

## One server per data directory

A server takes a lock on the data directory (`hookinlet.lock`). It is an advisory lock that the operating system
drops when the process ends, so there is nothing to clean up after a crash. A second server started on the same
directory stops at once and says which process holds the lock. This keeps two writers from corrupting each
other's work. `hookinlet migrate`, `rekey` and `renew-epochs` must not run beside a server, so they take the
same lock. The other commands do not. So `hookinlet inbox ...` still works in the directory of a running
server. Some file systems do not support locks, such as some network and FUSE mounts. On one of those, the
server starts anyway and logs a warning. Nothing then stops a second server.

## Starting, and how fast

The server is ready as soon as the database is open. There is no warm-up, no index build, and no network call.
We measured from exec to the first successful `/readyz` on a database of a million messages, restarting with the
file already in the operating system's cache. The median of seven starts was **4 ms on Linux and 11 ms on
macOS** (a 2026 laptop). The limit this project holds itself to is 500 ms. Start-up time does not grow with the
size of the database. Two things come on top of that: a start on a host whose disk cache is empty, and the
platform's own time to start a container. Retention can be slow on a large database. It runs after the server is
listening and never delays it. So a platform that stops an idle instance and starts it on the next request does
not make that request wait for hookinlet.

> **Scale to zero.** An open SSE stream or MCP wait is an open connection. Some platforms count that as activity
> and so keep the instance awake. An agent that wants the instance to sleep when idle should poll
> `/v1/inboxes/{slug}/messages` rather than hold a connection.

## Resources

One instance is small. Idle, the process uses about 10 to 12 MiB of memory on Linux. Each open SSE stream costs
about 50 KiB, so 500 of them come to about 35 MiB.

**Reads.** The server builds a page of messages in memory and bounds it (16 MiB at most). It allows 16 reads at a
time. A read holds its slot until the response has been written to the client or `ResponseTimeout` (2 minutes)
has passed. So a client that asks and never reads cannot pin more than 16 pages.

**Shares per credential.** One credential may hold only a quarter of the read slots (4 of 16). Otherwise a
client that never reads could hold them all and turn every other inbox's readers away. The admin token is not
limited. In the same way, one credential may hold only a tenth of the stream slots (100 of 1000). One inbox's
deliveries may use only an eighth of the budget below (16 MiB of 128). That share is never less than the inbox's
largest body, so an inbox that takes 10 MiB bodies receives one at a time. These shares keep a token or an
ingest key from taking what everyone shares. The first two are settings (`HOOKINLET_MAX_READS_PER_CREDENTIAL`,
`HOOKINLET_MAX_STREAMS_PER_CREDENTIAL`).

**Deliveries.** The number of deliveries is not limited. The server holds each one in memory while it reads and
checks it, up to the inbox's body limit (1 MiB by default, at most 10 MiB). So a burst of large deliveries uses
that much each. The total is bounded. Deliveries being received hold at most `HOOKINLET_INGEST_MEMORY_BUDGET`
bytes of bodies between them (128 MiB by default; at least 10 MiB, the largest body an inbox may take). The
server counts the bytes as they arrive. So a connection that has promised a large body and sent none costs
nothing. A delivery whose bytes would take the total over gets `503` with `Retry-After` and is not stored.

**Size the memory for three times the budget.** The process uses more than the bytes counted. A Go program lets
its heap grow to twice what is live before it collects. A body is also read into a buffer with some spare room.
We measured with the heaviest load the budget allows (dozens of 9 MiB bodies arriving at once, most abandoned
part way). The resident memory at the busiest was:

| `HOOKINLET_INGEST_MEMORY_BUDGET` | Peak resident memory |
|---|---|
| 32 MiB | 139 MB |
| 64 MiB | 229 MB |
| 128 MiB (the default) | 380 MB |
| 256 MiB | 750 MB |

That is the deliveries alone. So plan for about three times the budget plus 50 MB for them. Then leave room for
the rest of what the server is doing. It builds a page of messages in memory (16 MiB at most, 16 reads at a
time), and each open stream costs a little. We ran a 35-minute test at the default 128 MiB with all of this
together: 250 open streams, about 300 deliveries a second, backups, restarts, a burst of large deliveries that
filled the budget, and a burst of large reads. It peaked at 650 MB. So at the default, give the process about
1 GB if it must never be killed.

Lower the budget on a small host. The table shows what that saves on the deliveries' side (380 MB becomes 229 MB
at 64 MiB). Ordinary traffic is far below this. The same test used about 170 MB most of the time. Only a burst
of large deliveries, or a sender holding many bodies open, reaches the budget. Setting `GOMEMLIMIT` makes the
runtime collect sooner. In the ingest-only test above, it trimmed the peak by only a fifth to a quarter. So the
budget is the setting that matters. A lower budget means more `503`s to a burst of large deliveries, which a
sender retries. Put a rate limit in front, or in a hook (see [extending](extending.md)).

**Disk** is what grows. Size the volume for the messages you keep, and set retention on inboxes that receive a
lot. A backup needs free space in the data directory for a second copy of the database while it runs.

**File descriptors.** Each stream or wait holds one. The default of 1000 streams needs a limit comfortably above
that.

**Slow connections.** A client cannot hold a connection open by not sending or not reading. Whatever a request
is sent to, its headers must arrive within 10 seconds and its body within 60. The server gives up on an answer
the client does not take after `ResponseTimeout`. Some answers have to wait, for a free read slot or in an MCP
wait. The server counts those from when they are ready. A request that is refused or answered early gets
`ResponseTimeout` plus the longest wait it could have made. The admin API has no limit on how long an answer
may take to be ready, because its caller holds the admin token. A stream or a backup bounds every write on its
own.

## Upgrading

Start the new version on the same data directory. It applies any database migrations itself, once, before it
listens, and logs each one. A large database can take minutes to migrate. The move of the message body, 0007,
took about two and a half minutes on a million rows. Nothing listens during that time. So a health check that
restarts the process will restart the migration too. To avoid this, do one of two things:

- Run `hookinlet migrate` as its own step with the old server stopped. It applies the migrations and reports the
  schema version. It refuses to run while another process has the database open.
- Give the health check a generous start period.

Only the process that holds the data directory's lock migrates. A command such as `hookinlet inbox show` run
against an old database refuses and tells you to stop the server and run `hookinlet migrate`. So run those
commands after upgrading, not before. Migrations only move forward. An older version refuses a database whose
schema is newer than it knows (one that added a migration).

**A copy is kept for you.** Say there are migrations to apply to a database that already holds data. The server
first writes a complete copy of it as it was to `hookinlet.db.before-migration-N.bak` in the data directory (`N`
is the schema version it was at; mode 0600). If the upgrade goes badly, stop the server. Put that file back as
`hookinlet.db`, and remove any `-wal` and `-shm` beside it, and `hookinlet.db.migrating`. Then start the
previous release.

If the server cannot make the copy (no room for a second copy of the database), it refuses to migrate and
changes nothing. An upgrade may be cut short (killed, out of disk, a start timeout). Then the next start keeps
the copy that was made before the upgrade began. The database is now partly upgraded, and a copy of that would
be no way back. (A marker file, `hookinlet.db.migrating`, records that. It goes when an upgrade finishes.) The
server keeps only the latest finished upgrade's copy, and nothing deletes it. So remove it when you trust the
upgrade. The copy is as sensitive as the database.

**Room:** the migration that moves the message body (0007) rebuilds the messages table. So allow for about
three times the database's size free in the data directory (the copy, the new table, and the write-ahead log).
The file does not shrink afterwards. Taking your own [backup](admin.md#backups) first is still sensible.
`CHANGELOG.md` lists changes that affect anyone embedding the packages. This is a pre-1.0 project, so those
changes may be breaking.

**From 0.1.x to 0.2.0, your verifier secrets need setting again.** The migration gives every inbox a UID. A
verifier secret is now sealed to that UID instead of the slug. A secret that was already stored was sealed to the
slug. The migration does not have the master key, so it cannot seal the secret again. Those secrets no longer
open. The server names the inboxes and refuses to start. Set each secret again, in the data directory with the
same master key: `hookinlet inbox set-secret SLUG --secret-stdin` (see
[the server refuses to start](troubleshooting.md#the-server-refuses-to-start)). Take your own
[backup](admin.md#backups) before upgrading. After the migration, the copy described above is the only way back
to 0.1.x. If none of your inboxes has a stored verifier secret (the `none` verifier has none), there is nothing
to do.

## Backups

You can back up with `GET /v1/admin/backup`, with `hookinlet backup --out FILE`, or by copying the data
directory while the process is stopped. See [administration](admin.md#backups), including how to restore. After
you restore from a copy of the data directory, run `hookinlet renew-epochs`, so readers' old cursors are not
trusted. Keep the master key with the backup, but apart from it.

## Logs

Logs go to standard error, as JSON lines unless `HOOKINLET_LOG_FORMAT=text`. Each request is logged with its
route, status, duration, and an `X-Request-Id` (which is also sent back to the client). Logs name inboxes by slug
and sequence number. They never contain message bodies, tokens, or secrets.

## Checking a release

Each release has a SHA-256 for each archive in `checksums.txt`, and a software bill of materials
(`*.sbom.json`). The container image is published for `linux/amd64` and `linux/arm64`. It has build provenance
and an SBOM attached as attestations (`docker buildx imagetools inspect ghcr.io/hookinlet/hookinlet:<version>`).
A tag with a suffix, such as `v0.1.0-rc.1`, is a pre-release. Its image gets no `latest` tag.
