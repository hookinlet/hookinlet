# Administration

Everything that sets hookinlet up or looks after it: creating inboxes, changing
their settings, rotating tokens, retention, and backups. There are two ways in
and they do the same things:

- the **admin API**, on a running server, with the admin token;
- the **command line**, which calls that API when you give it `--url`, and
  otherwise works directly in the data directory with no server running.

## Who may do this

The admin API accepts only the admin token (`HOOKINLET_ADMIN_TOKEN`, set when the
server starts). An agent token gets `403`, a missing or wrong token gets `401`,
and neither can tell what is behind the path. The admin token can read every
inbox and change everything, so treat it like a root password: keep it in the
environment, never in a flag, and use TLS in front of the server.

Responses are JSON, never cached (`Cache-Control: no-store`), and errors use the
same shape as the rest of the API: `{"error": {"code": "…", "message": "…"}}`.

Nothing here ever returns a verifier secret, the sealed form of one, or a token
hash. An inbox shows only `has_verifier_secret: true`. The one thing that is
shown exactly once is a new agent token, in the response that creates it or
rotates it.

## The API

| Method and path | Purpose |
|---|---|
| `POST /admin/inboxes` | Create an inbox. Returns the agent token once. |
| `GET /admin/inboxes` | List inboxes, 100 at a time by default and at most 1000 (`?limit=`; follow `next`). |
| `GET /admin/inboxes/{slug}` | Show one, with its message count. |
| `PATCH /admin/inboxes/{slug}` | Change settings. |
| `PUT /admin/inboxes/{slug}/secret` | Set or replace the verifier secret. |
| `POST /admin/inboxes/{slug}/rotate-token` | New agent token, returned once. The old one stops working at once. |
| `DELETE /admin/inboxes/{slug}` | Delete the inbox and all of its messages (`204`). It is closed to deliveries and agents first, and its messages go in small batches so that other inboxes keep receiving; if it is interrupted, run it again. |
| `POST /admin/retention/run` | Apply retention now. Returns `{"deleted": N}`. |
| `GET /admin/backup` | Download a consistent copy of the database. |

Request bodies are JSON (`Content-Type: application/json`, else `415`), at most
64 KiB (else `413`), and strict: an unknown field, or anything after the JSON
value, is a `400` rather than being ignored.

Other statuses: `404` for an inbox that does not exist, `409` when a slug is taken
or a change could not be settled (or a sweep or backup is already running),
`501` when a verifier secret is given but no master key is configured, or the
Store cannot make backups.

### Create

<!-- exercise -->
```sh
curl -s -X POST "$BASE/admin/inboxes" \
  -H "Authorization: Bearer $HOOKINLET_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"slug": "stripe-api", "verifier": "stripe", "verifier_secret": "whsec_example",
       "retention_days": 30, "event_type_pointer": "/type"}'
```

```json
{
  "inbox": {
    "id": 1, "slug": "stripe-api", "name": "stripe-api", "version": 1,
    "verifier": "stripe", "verifier_config": {}, "has_verifier_secret": true,
    "on_verify_fail": "reject", "max_body_bytes": 1048576,
    "retention_days": 30, "retention_max_count": 0, "event_type_pointer": "/type",
    "disabled": false, "last_seq": 0,
    "ingest_path": "/in/stripe-api",
    "ingest_url": "https://hooks.example.com/in/stripe-api",
    "created_at": "…", "updated_at": "…"
  },
  "agent_token": "hki_…"
}
```

Only `slug` is required (3 to 64 characters from `a-z`, `0-9`, and `-`; it never
changes). The other fields are `name`, `verifier` (default `none`),
`verifier_config`, `verifier_secret`, `on_verify_fail` (`reject` or `store`),
`max_body_bytes` (default 1 MiB, at most 10 MiB), `retention_days`,
`retention_max_count`, and `event_type_pointer`. The verifier and its
configuration are checked when you save them, so a typo is an error now and not
a failed delivery later. See [verifiers](verifiers.md).

A verifier that needs a secret, created without one, comes with a `warnings`
entry: deliveries fail until you set it.

`ingest_url` is built from `HOOKINLET_PUBLIC_BASE_URL` if set, and otherwise from
the request's `Host` (using `X-Forwarded-Proto` only when the request came from
one of `HOOKINLET_TRUSTED_PROXIES`).

### Change settings

`PATCH` takes any of `name`, `verifier`, `verifier_config`, `on_verify_fail`,
`max_body_bytes`, `retention_days`, `retention_max_count`, `event_type_pointer`,
and `disabled`. Omitted fields are left alone. A disabled inbox answers
deliveries with `404` while agents can still read what it holds.

Each change is checked against the inbox's version, so two admins changing
different settings of the same inbox at once both take effect; for the same
setting the last one wins. A conflict that cannot be settled is a `409` to retry.
`retention_days` is at most 36,500 (a hundred years).

### The verifier secret and the token

`PUT …/secret` takes `{"secret": "…"}`. The secret is sealed with the master key
before it is stored (see `HOOKINLET_MASTER_KEY`) and cannot be read back.

`POST …/rotate-token` issues a new agent token and invalidates the old one
immediately. Open SSE streams and MCP waits that used the old token end within
about 30 seconds.

## The command line

<!-- exercise -->
```sh
# against a running server (HOOKINLET_ADMIN_TOKEN holds the admin token)
export HOOKINLET_URL=https://hooks.example.com
hookinlet inbox create --slug stripe-prod --verifier stripe     # secret: HOOKINLET_VERIFIER_SECRET
hookinlet inbox list
hookinlet inbox show stripe-prod
hookinlet inbox update stripe-prod --retention-days 30 --disabled=false
echo whsec_example | hookinlet inbox set-secret stripe-prod --secret-stdin
hookinlet inbox rotate-token stripe-prod
hookinlet inbox delete stripe-prod --yes
hookinlet retention run
hookinlet backup --out hookinlet-backup.db
```

Without a server, straight in the data directory (also for first-time setup). If `HOOKINLET_URL` is set in
your shell, unset it first: giving both a server address and a data directory is an error.

```sh
unset HOOKINLET_URL
hookinlet inbox create --slug stripe-prod --data-dir ./data
```

- **Which mode.** `--url` (or `HOOKINLET_URL`) selects the API and requires the
  admin token in `HOOKINLET_ADMIN_TOKEN`. Without it the commands open
  `--data-dir` (`HOOKINLET_DATA_DIR`, default `./data`). Passing both `--url` and
  `--data-dir` is an error.
- **Secrets are never flags.** A verifier secret comes from
  `HOOKINLET_VERIFIER_SECRET` or, for `set-secret`, `--secret-stdin`, and
  sealing it in direct mode needs `HOOKINLET_MASTER_KEY`. The admin token comes
  from the environment; `--token` exists but shows up in process listings.
- **Direct mode needs a database.** Every command but `inbox create` refuses a
  data directory with no `hookinlet.db`, so a mistyped `--data-dir` is an error and
  not an empty list or a backup of nothing.
- **Plain `http://`** to another machine is refused, because it would send the
  admin token in the clear. `--insecure-http` overrides that on a network you
  trust; `http://localhost` is always fine. Redirects are not followed.
- **`--json`** prints the server's answer as JSON for scripts.
- Direct mode and a running server can be used at the same time: the database
  is safe to share, and the version check covers a change that lands between a
  command's read and its write.

## Retention

Each inbox can limit how much it keeps: `retention_days` deletes messages older
than that many days, `retention_max_count` keeps only the newest N, and with
both, a message goes if either rule says so. `0` means no limit. Sequence
numbers are never reused, so an agent's cursor stays valid after messages are
pruned; it simply finds nothing older.

The server applies every inbox's policy once when it starts (after it is
listening, so startup is not delayed) and then every
`HOOKINLET_RETENTION_INTERVAL` (default `1h`, at least `1s`, or `off` to run
only on request). `POST /admin/retention/run` and
`hookinlet retention run` do the same on demand; only one sweep runs at a time,
and a second request while one is running gets `409`.

A sweep deletes in small batches, bounded by rows and by bytes, so that deliveries
keep flowing. Measured while a million rows were pruned (or an inbox that size
was deleted) and senders appended, the store's append p99 was 16 to 17 ms; see
`docs/performance.md`. A sweep interrupted by shutdown simply resumes at
the next one.

## Backups

`GET /admin/backup` (or `hookinlet backup --out FILE`) returns a complete,
consistent copy of the database as it was at one moment, taken while deliveries
carry on. It contains every inbox, every message, the agent token **hashes**, and
the verifier secrets **still sealed**; it is useless without the master key and
sensitive with it, so protect it like the data directory.

- The server writes the snapshot into the data directory first, so that
  directory needs free space for a second copy of the database. The temporary
  file is private to the owner and is removed when the download ends, however it
  ends. One left behind by a crash is removed at a later start, once it is more
  than an hour old; until then it is a full copy of the database, so keep the
  data directory private.
- Only one backup runs at a time (`409` otherwise).
- A store that cannot produce one (some embedded backends) answers `501`.
- The command writes beside the destination and renames, so an interrupted
  backup never leaves a file that looks complete. It refuses to overwrite
  without `--force`.

**Restoring.** Stop the server, remove any `hookinlet.db-wal` and
`hookinlet.db-shm` beside the database, put the backup at `<data-dir>/hookinlet.db`,
and start the server with the **same master key**. Check the copy first if you
like: `sqlite3 backup.db 'PRAGMA integrity_check'` should print `ok`.
