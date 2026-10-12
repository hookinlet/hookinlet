# Troubleshooting

Find the symptom, read the cause, do the fix. The server logs one line per request
and per event, as JSON (`HOOKINLET_LOG_FORMAT=text` for people), and never logs
bodies, tokens or secrets. `docker logs hookinlet` is the first place to look.

## The server refuses to start

**`invalid configuration`** lists every setting that is wrong, all at once. The
admin token must be at least 32 characters, with no whitespace and not mostly one
repeated character (`openssl rand -base64 32` makes a good one). The master key must be
base64 for exactly 32 bytes (the same command).

**`stored verifier secrets cannot be opened` / `refusing to start`.** The master key is
not the one the secrets were sealed under. Almost always you started with a freshly
generated key instead of the old one. Start again with the right key. If the key was
changed on purpose, move the secrets to the new one with the server stopped:

```sh
HOOKINLET_OLD_MASTER_KEY=<old> HOOKINLET_MASTER_KEY=<new> hookinlet rekey --data-dir /data
```

If the old key is lost, nothing can recover those secrets: set each again with
`hookinlet inbox set-secret SLUG` (it works in the data directory with the new key).

**`another hookinlet process holds the lock on this data directory`.** A second server,
or `rekey` or `renew-epochs`, is running against the same directory. Stop the other
one. Two servers must never share a directory.

**`permission denied` on the data directory** (Docker). The image runs as uid 65532. Use
a named volume (`-v hookinlet-data:/data`), or `chown 65532:65532` the directory you
mount.

**`could not keep a copy of the database before migrating it`.** An upgrade copies the
database first, and there was no room for a second copy. Free space (about three times
the database's size for the upgrade that moves message bodies) and start again. Nothing
was changed.

**`the database is at schema version N and this release needs M`.** A command that does not
hold the data directory's lock will not upgrade a database. Stop the server and run
`hookinlet migrate`.

**`/readyz` says there is no room, though pruning freed a lot.** Pages that pruning
empties stay inside the database file and are reused by new writes, and they count as room.
The file itself does not shrink. Admin calls that write answer `500` while the volume is
completely full (the log says `storage full`); deliveries answer `503`.

**`database schema version N is newer than this build supports`.** The data was
written by a newer hookinlet. Run the newer version, or restore a backup taken by an
older one.

**`listen tcp :8080: bind: address already in use`.** Something else has the port. Set
`HOOKINLET_LISTEN_ADDR`.

## The server is up but not ready

`GET /readyz` answers `503` with a reason: `the store is not reachable`; `there is no
room left to store deliveries` (the volume has under 32 MiB free, or a write just failed
for lack of room); `stored verifier secrets cannot be opened with the master key` (see
above); `the server is shutting down` (it was told to stop and is draining).

## Sending webhooks

| You get | Because | Do |
|---|---|---|
| `404` | The ingest key is wrong, or the inbox is disabled. They look the same on purpose. | Copy the URL from `hookinlet inbox show SLUG`. |
| `401 verification_failed` | The signature did not check. The log says why (`reason`). | Check the secret on the inbox and at the sender; for Stripe and Slack, that the clocks agree within five minutes; that a proxy does not rewrite the body. |
| `413` | The body is over the inbox's `max_body_bytes` (1 MiB default, 10 MiB most). | Raise it with `hookinlet inbox update SLUG --max-body-bytes N`. |
| `503 overloaded` | Too many deliveries are being received at once. | The sender should retry (most do). Raise `HOOKINLET_INGEST_MEMORY_BUDGET` if it is routine. |
| `503 storage_full` | No room to store it; not acknowledged. | Free disk, or set retention. The sender should retry. |
| `429`, `402`, `403` | A hook an embedder installed refused it. | Ask whoever runs the server. |

A delivery to an inbox whose verifier is `none` is stored, but never marked `verified`.

## Reading

| You get | Because | Do |
|---|---|---|
| `401` | Missing, malformed or revoked token. | Send `Authorization: Bearer hki_...`. Tokens in a URL are not accepted. |
| `403` on `/v1/admin/...` | An agent token. | Use the admin token. |
| `404` on an inbox that exists | The token does not cover that inbox. It is indistinguishable from "no such inbox" by design. | `hookinlet token list --inbox SLUG`; issue a token for it. |
| `404` on `/v1/admin/...` | The admin API is off (`HOOKINLET_ADMIN_ENABLED=false`) or on its own address (`HOOKINLET_ADMIN_LISTEN_ADDR`). | Use that address. |
| `409 epoch_changed` | Your cursor is from an earlier life of the inbox (it was recreated, or the server was restored). | Read the inbox's current `epoch` and start again from `after=0`. |
| `gap: true` | Retention removed messages after your cursor before you read them. | Nothing to fix: you missed them. Keep retention longer than your reader can be away. |
| `400 invalid_parameter` | An unknown, repeated or malformed query parameter. | The message names it. Unknown parameters are refused on purpose. |
| A page with fewer messages than `limit` | Pages are also capped by size. | Follow `next` until the page is empty. |

## Streams and waits

**Events arrive in bursts, or only when the connection closes.** A proxy is buffering.
Turn buffering off for the path (`proxy_buffering off;` in nginx, `flush_interval -1` in
Caddy); see [Deploying](deploying.md#behind-a-proxy). **The stream drops every minute or
so:** the proxy's idle timeout is shorter than the heartbeat or an MCP wait. **`503`
on a stream:** `HOOKINLET_MAX_STREAMS` (or the per-inbox cap) is reached.

## MCP

**`unexpected additional properties`.** A tool was called with an argument it does not
have (`summary` was replaced by `bodies`). **Messages have no bodies.** That is the
default; pass `bodies: true`, or fetch one with `get_message`. **`epoch_changed`:** as above.
**`too many waits are in progress`:** `HOOKINLET_MAX_STREAMS` is reached; retry shortly.

## Running the command line

`hookinlet inbox ...` talks to a running server when `HOOKINLET_URL` is set (it needs
`HOOKINLET_ADMIN_TOKEN`), and works in the data directory otherwise. Giving both a
URL and a data directory is an error. The binary on macOS downloaded in a browser is
quarantined: `xattr -d com.apple.quarantine hookinlet`.

## Lost something

- **The admin token** is configuration, not data: set a new `HOOKINLET_ADMIN_TOKEN` and
  restart. Agent tokens are unaffected.
- **An agent token** cannot be recovered (only its hash is stored). Issue another with
  `hookinlet token create` and revoke the lost one.
- **An ingest key** is shown by `hookinlet inbox show SLUG`.
- **The master key** cannot be recovered; see above for what you can do.
