# Troubleshooting

Find the symptom, read the cause, do the fix. The server logs one line per request and one per event. The lines
are JSON. Set `HOOKINLET_LOG_FORMAT=text` for a format people read more easily. The server never logs bodies,
tokens or secrets. Look first at `docker logs hookinlet`.

## The server refuses to start

**`invalid configuration`** lists every wrong setting at once. The admin token must be at least 32 characters,
with no whitespace and at least 10 different characters. `openssl rand -base64 32` makes a good one. The master key
must be base64 for exactly 32 bytes. The same command makes one.

**`stored verifier secrets cannot be opened` / `refusing to start`.** The master key is not the one that locked the
secrets. Almost always, you started with a freshly generated key instead of the old one. Start again with the right
key. If you changed the key on purpose, stop the server and move the secrets to the new key:

```sh
HOOKINLET_OLD_MASTER_KEY=<old> HOOKINLET_MASTER_KEY=<new> hookinlet rekey --data-dir /data
```

If the old key is lost, nothing can recover those secrets. Set each one again with
`hookinlet inbox set-secret SLUG --secret-stdin`. It works in the data directory with the new key.

**`another hookinlet process (process N) holds the lock on this data directory`.** A second server,
or `migrate`, `rekey` or `renew-epochs`, is running against the same directory. Stop the other
one. Never let two servers share a directory.

**`permission denied` on the data directory** (Docker). The image runs as uid 65532. Use a named
volume (`-v hookinlet-data:/data`), or run `chown 65532:65532` on the directory you mount.

**`could not keep a copy of the database before migrating it`.** An upgrade copies the database
first, and there was no room for a second copy. Free some space and start again. For the upgrade that moves message
bodies, free about three times the database's size. Nothing was changed.

**`the database is at schema version N and this release needs M`.** A command that does not hold
the data directory's lock will not upgrade a database. Stop the server and run `hookinlet migrate`.

**`/readyz` says there is no room, though pruning freed a lot.** Pages that pruning
empties stay inside the database file. New writes reuse them, and they count as room. The file itself does not
shrink. While the volume is completely full, admin calls that write answer `500` (the log says `storage full`), and
deliveries answer `503`.

**`database disk image is malformed`, or answers that are wrong or missing.** The file is damaged. Run
`hookinlet check`. It can run beside the server. If it reports problems, stop the server, copy the data directory
aside, and restore the latest [backup](admin.md#backups). If the error appears right after a restore, the likely
cause is an old `hookinlet.db-wal` or `hookinlet.db-shm` left beside the restored file. Stop the server, remove
both, and start again. Do this only after a restore. After a crash, the log holds deliveries that were
acknowledged, and it must stay where it is.

**`hookinlet.db exists but is empty`.** The file has no content. A copy or restore that did not finish leaves a file like this. The server refuses it and
does not start it as a new database. Copy the backup again, and remove any `hookinlet.db-wal` and
`hookinlet.db-shm` beside it. On a new installation with nothing to lose, delete the file. Do not create the file
ahead of time (`touch`, a single-file bind mount, a `tmpfiles.d` entry). Mount or create the directory instead, and
let hookinlet make the database.

**`database schema version N is newer than this build supports`.** A newer hookinlet wrote the
data. Run the newer version, or restore a backup taken by an older one.

**`listen tcp :8080: bind: address already in use`.** Something else has the port. Set
`HOOKINLET_LISTEN_ADDR`.

## The server is up but not ready

`GET /readyz` answers `503` with a reason. These are the reasons:

- `the store is not reachable`.
- `there is no room left to store deliveries`. The volume has under 32 MiB free, or a write just failed for lack
  of room.
- `none of the stored verifier secrets tried can be opened`. See above. Usually the master key is wrong. If you
  use a `SecretBox` that asks a key service, the service may be failing. Or, when every inbox with a secret is one
  tenant's, that tenant's key may be gone. The log names the tenant.
- `checking the stored secrets timed out`. A `SecretBox` that asks a key service did not answer within 20 seconds.
  The server checks again after 5.
- `the server is shutting down`. The server was told to stop and is draining.

## Sending webhooks

| You get | Because | Do |
|---|---|---|
| `404` | The ingest key is wrong, or the inbox is disabled. They look the same on purpose. | Copy the URL from `hookinlet inbox show SLUG`. |
| `401 verification_failed` | The signature did not check. The log says why (`reason`). | Check the secret on the inbox and at the sender. For Stripe and Slack, check that the clocks agree within five minutes. Check that no proxy rewrites the body. |
| `413` | The body is over the inbox's `max_body_bytes` (1 MiB default, 10 MiB most). | Raise it with `hookinlet inbox update SLUG --max-body-bytes N`. |
| `503 overloaded` | Too many deliveries are being received at once. | The sender should retry (most do). If it happens often, raise `HOOKINLET_INGEST_MEMORY_BUDGET` (128 MiB by default). At the busiest, memory use is about three times the budget plus 50 MB ([Resources](deploying.md#resources)). |
| `503 storage_full` | No room to store it; not acknowledged. | Free disk, or set retention and run it with `hookinlet retention run`. If the volume has 0 bytes free, even that fails, so free some space outside hookinlet first. The sender should retry. |
| `429 rate_limited` on an inbox that stores unverified messages | The inbox holds as many unverified messages as `HOOKINLET_MAX_UNVERIFIED` allows (1000 by default), and this delivery did not verify. Verified deliveries still get in. | Fix the signature or secret so deliveries verify. Or set `retention_days` on the inbox so old unverified messages age out. Or raise the cap. |
| `429`, `402`, `403` | Otherwise, a hook an embedder installed refused it. | Ask whoever runs the server. |

A delivery to an inbox whose verifier is `none` is stored, but never marked `verified`.

## Reading

| You get | Because | Do |
|---|---|---|
| `401` | Missing, malformed, revoked or expired token. | Send `Authorization: Bearer hki_...`. Tokens in a URL are not accepted. `hookinlet token list` shows an expired token (the server does not say why it refused one). |
| `403` on `/v1/admin/...` | An agent token. | Use the admin token. |
| `404` on an inbox that exists | The token does not cover that inbox. It is indistinguishable from "no such inbox" by design. | `hookinlet token list --inbox SLUG`; issue a token for it. |
| `404` on `/v1/admin/...` | The admin API is off (`HOOKINLET_ADMIN_ENABLED=false`) or on its own address (`HOOKINLET_ADMIN_LISTEN_ADDR`). | Use that address. |
| `409 epoch_changed` | Your cursor is from an earlier life of the inbox (it was recreated, or the server was restored). | Read the inbox's current `epoch` and start again from `after=0`. |
| `gap: true` | Retention removed messages after your cursor before you read them. | Nothing to fix: you missed them. Keep retention longer than your reader can be away. |
| `400 invalid_parameter` | An unknown, repeated or malformed query parameter. | The message names it. Unknown parameters are refused on purpose. |
| A page with fewer messages than `limit` | Pages are also capped by size. | Follow `next` until the page is empty. |

## Streams and waits

**Events arrive in bursts, or only when the connection closes.** A proxy is buffering.
Turn buffering off for the path (`proxy_buffering off;` in nginx, `flush_interval -1` in Caddy). See
[Deploying](deploying.md#behind-a-proxy).

**The stream drops every minute or so.** The proxy's idle timeout is shorter than the heartbeat or an MCP wait.

**`503` on a stream.** The server reached `HOOKINLET_MAX_STREAMS` (or the per-inbox cap).

## MCP

**`unexpected additional properties`.** You called a tool with an argument it does not
have (`summary` was replaced by `bodies`).

**Messages have no bodies.** That is the default. Pass `bodies: true`, or fetch one message with `get_message`.

**`epoch_changed`.** See above.

**`too many waits are in progress`.** The server reached `HOOKINLET_MAX_STREAMS`. Retry shortly.

## Running the command line

`hookinlet inbox ...` talks to a running server when `HOOKINLET_URL` is set. It needs `HOOKINLET_ADMIN_TOKEN`.
Otherwise it works in the data directory. Giving both a URL and a data directory is an error.

A macOS binary downloaded in a browser is quarantined, because it is not notarized. It then dies at once with
`Killed: 9`, or macOS says it cannot check it for malware. Run `xattr -d com.apple.quarantine hookinlet` to let it
run. A binary fetched with `curl` or `gh release download` is not quarantined.

## Lost something

- **The admin token** is configuration, not data: set a new `HOOKINLET_ADMIN_TOKEN` and
  restart. Agent tokens are unaffected.
- **An agent token** cannot be recovered (only its hash is stored). Issue another with
  `hookinlet token create` and revoke the lost one.
- **An ingest key** is shown by `hookinlet inbox show SLUG`.
- **The master key** cannot be recovered. See above for what you can do.
