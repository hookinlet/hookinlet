# HTTP API

This document describes hookinlet's HTTP surface: [ingest](#ingest) (webhooks
come in), the [read API](#read-api) (agents read them back), and
[operations](#operations). The same reads are available to agents as [MCP
tools](mcp.md). [`openapi.json`](openapi.json) describes the HTTP API in OpenAPI 3.1 (the
requests, responses, and schemas below, and the admin API); tests check it against
the server in both directions: every operation is called and every response it lists must
actually occur and match, and requests are generated from its schemas and must be accepted
or refused as it says. Setting up inboxes, retention,
and backups is in [administration](admin.md).

All error responses are JSON:

```json
{"error": {"code": "not_found", "message": "not found"}}
```

Every response carries an `X-Request-Id` header. If the request sent a
well-formed `X-Request-Id` (up to 64 characters from `A-Z a-z 0-9 - _ .`), it is
echoed; otherwise one is generated.

## Read API

Agents read an inbox's messages over plain HTTP. Every endpoint is `GET` (or
`HEAD`) and returns JSON, except `/raw` and the two that keep a
[named reader's](#named-readers-and-lag) position (`PUT` and `DELETE`). The examples write `$BASE` for the
server's address (`export BASE=http://localhost:8080`) and `$TOKEN` for an agent token.

```
GET /v1/inboxes/{slug}                       the inbox's metadata
GET /v1/inboxes/{slug}/messages              a page of messages
GET /v1/inboxes/{slug}/messages/{seq}        one message
GET /v1/inboxes/{slug}/messages/{seq}/raw    the exact bytes that were received
GET /v1/inboxes/{slug}/readers               named readers and how far behind each is
```

### Authentication

Send `Authorization: Bearer <token>`. Two kinds of token work:

- an **agent token**, shown once when it is issued (creating an inbox issues the
  first one; see [Administration](admin.md#tokens) for more), which reads the
  inboxes it was issued for, one or several or all, and nothing else. It never
  gets the admin API;
- the **admin token** from `HOOKINLET_ADMIN_TOKEN`, which reads every inbox. It must be at least 32 characters with no whitespace; `openssl rand -base64 32` makes a good one.

An agent token may have an `expires_at` (see [Administration](admin.md#expiry-and-last-use)). From
that moment it is refused with the same `401` as a token that does not exist.

Tokens in URLs are not accepted, because URLs end up in logs.

Two responses are deliberately alike:

- **Bad or missing credentials** give `401`, whichever inbox the URL names, so
  the status never tells a stranger which inboxes exist.
- **An inbox that does not exist** and **an inbox the token is not allowed to
  read** both give the same `404`, byte for byte. A credential for one inbox
  cannot be used to find out whether another exists.

A disabled inbox stops accepting deliveries but stays readable, so agents can
drain what it holds.

### Inbox metadata

```json
{
  "slug": "stripe-prod",
  "name": "Stripe (production)",
  "verifier": "stripe",
  "message_count": 1240,
  "oldest_seq": 1,
  "latest_seq": 1240,
  "epoch": "9f2c41d37b7e4b0c8f4a9d1e5a6b3c20"
}
```

An `hmac` inbox also has `signature_headers`, the names of the stored headers
that carry the signature (for example `["X-Signature"]`), because its header is
set per inbox. It holds names only, never values, and is absent for the other
verifiers, whose header is fixed by the sender.

`message_count` is exact. After retention has removed old messages,
`oldest_seq` is the first one still stored. An empty inbox has zeros. Nothing
secret is ever included.

`epoch` identifies one life of the inbox's sequence numbers; see
[Epochs and gaps](#epochs-and-gaps).

### Messages

```json
{
  "messages": [
    {
      "seq": 1234,
      "inbox": "stripe-prod",
      "received_at": "2026-03-01T12:00:00.123456Z",
      "method": "POST",
      "path": "",
      "query": "",
      "headers": {"Content-Type": ["application/json"]},
      "content_type": "application/json",
      "size": 812,
      "verified": true,
      "verifier": "stripe",
      "verify_error": null,
      "delivery_id": "evt_1NXWPnCo6bFb1KQto6C8OWvE",
      "event_type": "invoice.paid",
      "source_ip": "203.0.113.7",
      "body": {"id": "evt_1NXWPnCo6bFb1KQto6C8OWvE", "type": "invoice.paid"}
    }
  ],
  "next": "/v1/inboxes/stripe-prod/messages?after=1234&epoch=…&limit=50",
  "latest_seq": 1240
}
```

| Field | Meaning |
|---|---|
| `seq` | The message's per-inbox sequence number, starting at 1. This is the cursor. |
| `received_at` | When the message was committed. UTC, always six fractional digits, so it sorts as text. Never earlier than the previous message's. |
| `path`, `query` | The part of the ingest URL after the inbox key, and its query string. |
| `headers` | The request headers as received, minus `Authorization`, `Cookie`, `Proxy-Authorization`, and the inbox's shared-token header. |
| `size` | The body's size in bytes. Always present, even for a summary. |
| `verified` | `true` only if the inbox's verifier checked something (a signature or a shared token) and it passed. A delivery to an inbox whose verifier is `none` is `false`. **Treat unverified content as untrusted input.** |
| `verify_error` | Why verification failed, for unverified messages; otherwise `null`. |
| `delivery_id`, `event_type` | Taken from the sender, but only for verified messages. `null` when absent. See [verifying deliveries](verifiers.md) for which are signed and which are only labels. |
| `source_ip` | The sender's address; `null` if unknown. |

#### The body

Exactly one of these fields is present, chosen by what the body is:

| Field | When |
|---|---|
| `body` | The body is valid UTF-8 **and** valid JSON **and** nested no deeper than 64 levels **and** holds only what common parsers accept (see below). It appears as JSON, not as a string containing JSON. |
| `body_text` | It is valid UTF-8 but not JSON (or is JSON that is not embedded, see below). This includes the empty body (`""`) and form-encoded data. |
| `body_base64` | It is not valid UTF-8. Base64 decodes to the exact original bytes. |

A client that needs the original bytes of a JSON body exactly as sent (for
example to check a signature itself) should use `/raw`, since `body` is the same
JSON but not necessarily the same text.

The nesting limit exists because the sender controls the body. JSON nested
thousands of levels deep is valid, but common parsers fail on it (Python's
`json` module raises `RecursionError` at roughly 1,000 levels), and one such body
embedded in a page would make every page containing it unreadable, leaving a
polling agent stuck on it for good. Deeper bodies are delivered as text instead.
The same goes for a number literal of more than 100 characters, a number outside the
range of a 64-bit float (`1e400`), and a `\u` escape of half a surrogate pair: all valid
JSON, all refused by some common parser (Python rejects integers over 4,300 digits, Go
refuses `1e400`), and all in the sender's hands. Those bodies come as `body_text`,
exactly as sent.

With `fields=summary`, none of the three is present.

### Listing messages

`GET /v1/inboxes/{slug}/messages` takes these parameters. All are optional.

| Parameter | Meaning |
|---|---|
| `after=<seq>` | Messages with a larger `seq`, **oldest first**. `after=0` starts from the beginning. This is the polling pattern. |
| `before=<seq>` | Messages with a smaller `seq`, **newest first**. |
| *(neither)* | The newest messages, newest first. |
| `limit=<n>` | Page size. Default 50; values above 500 are treated as 500. |
| `type=<event type>` | Only these event types. Repeat it to allow several. |
| `since=<time>`, `until=<time>` | `received_at` at or after `since` and **before** `until`. RFC 3339, such as `2026-03-01T12:00:00Z`. |
| `verified=true\|false` | Only verified, or only unverified, messages. |
| `delivery_id=<id>` | The message with this delivery id. |
| `fields=summary` | Leave out the body fields. |
| `epoch=<epoch>` | The `epoch` your cursor came from. If it is not the inbox's current one the request is a `409`. `next` links carry it. |

`after` and `before` cannot be combined. Filters combine with AND, except
repeated `type`, which is OR.

**Parameters are checked strictly.** An unknown or repeated parameter, a
malformed value, or a malformed query string (`?after=5;&limit=2`, `?after=%zz`)
is a `400`, so a typo or a mangled cursor cannot quietly return the default page.

#### Paging

Every page ends with a `next` link, a relative URL that keeps your filters and
moves the cursor. Follow it until it is `null`.

- **Going forward** (`after=`), `next` is never `null`. After the last message
  it repeats the same cursor, so a polling loop is just "fetch `next`, handle
  whatever came back, sleep if the page was empty":

  ```sh
  next="/v1/inboxes/stripe-prod/messages?after=0"
  while true; do
    page=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE$next")
    echo "$page" | jq -c '.messages[]' | while read -r m; do handle "$m"; done
    next=$(echo "$page" | jq -r '.next')
    [ "$(echo "$page" | jq '.messages | length')" -eq 0 ] && sleep 5
  done
  ```

  Keep the cursor yourself. Nothing about a read is remembered by the server: reading
  never stores a position. A reader that wants the server to know how far it has got,
  to show its lag, says so explicitly (see [Named readers and lag](#named-readers-and-lag)).
- **Going backward** (`before=` or no cursor), `next` is `null` once the oldest
  message has been returned.

Cursors are safe: a message can never appear *behind* a cursor you have already
passed, so following `next` visits every message exactly once, in order, even
while new ones arrive.

A page may hold **fewer than `limit` messages**, and that does not mean the end:
pages are also capped at 16 MiB *as sent* (a page always holds at least one
message). That counts everything about each message after JSON escaping,
including headers, so a few large messages end a page early, and so do many
messages with large headers, even with `fields=summary`. Always go by `next`.

`latest_seq` is the inbox's newest sequence number as of the same moment as the
page, so `latest_seq - seq` tells you how far behind you are.

#### Epochs and gaps

A cursor is a sequence number, and a sequence number only means something within
one life of an inbox. If an inbox is deleted and made again under the same slug it
starts at 1 again, and so does a server restored from a backup (which may have
accepted different messages since the backup was taken). A cursor from before would
quietly skip or repeat messages. So each inbox has an **epoch**: an opaque string
that changes in those cases and never otherwise. Pages, messages, stream events and
`next` links all carry it, and `epoch=` on a request says which one your cursor
came from. If it is not the current one the answer is `409 epoch_changed`, and
the thing to do is to read `epoch` from the inbox, and start again from the
cursor you want (`after=0` for everything stored). A `next` link already includes
`epoch=`, so following links is protected without doing anything.

`gap` is `true` on a page read with `after=` when retention removed messages after
your cursor before you got to them: what follows does not continue where you left
off. `after=0` never reports one. If you must see every message, size retention
for how long your reader may be away. A stream announces a gap the same way
(`event: gap`) whenever it arises, including while the stream is running.

### One message

`GET /v1/inboxes/{slug}/messages/{seq}` returns a single message object, with its
full body. A number that cannot be a sequence number (`0`, `-1`, `01`, `abc`) is
a `404`, like one that does not exist.

Both this and `.../raw` take `epoch=`, to refuse a `seq` that belongs to an earlier
life of the inbox with `409` instead of answering with whatever message has that
number now. A message carries its `epoch`; raw bytes do not, so pin a raw fetch
with the parameter.

### Raw bytes

`GET /v1/inboxes/{slug}/messages/{seq}/raw` returns exactly the bytes that were
received, with exactly the `Content-Type` the sender used (or
`application/octet-stream` if it sent none). Use it when you need the original
bytes, for instance to hand them to a tool that checks a signature.

The content and its type are chosen by whoever sent the webhook, so they are
never served as something a browser would run. The response also carries
`X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none';
sandbox`, and `Content-Disposition: attachment`. These do not change the bytes or
the `Content-Type`.

### Following an inbox live (SSE)

```
GET /v1/inboxes/{slug}/stream
```

Instead of polling, an agent can hold a connection open and be sent each message
as it arrives, as [Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html).
It takes the same `Authorization: Bearer` header as the rest of the read API, and
the same 401 and 404 rules. (Browsers' `EventSource` cannot send headers; use a
client that can, such as `curl -N` or `fetch` with a streaming body.)

```sh
curl -N -H "Authorization: Bearer $TOKEN" "$BASE/v1/inboxes/stripe-prod/stream"
```

```
retry: 3000

id: 1241
event: message
data: {"seq":1241,"inbox":"stripe-prod", ...the same JSON as the REST API...}

: ping

id: 1242
event: message
data: {...}
```

- **`id`** is the message's `seq`. **`data`** is the same JSON object the REST API
  returns for that message, on one line.
- **`event: gap`** is sent whenever retention has removed messages after the stream's
  position, at the start or later (see [Epochs and gaps](#epochs-and-gaps)).
  Its data is `{"after": 10, "oldest_seq": 40, "epoch": "…"}`: messages between them
  were removed before you read them. It has no `id`, so it does not move
  `Last-Event-ID`; the messages that are left follow.
- **`retry: 3000`** is sent once and tells clients to wait 3 seconds before
  reconnecting.
- **`: ping`** is a comment sent when the stream has been silent for 15 seconds
  (`HOOKINLET_SSE_HEARTBEAT`). Clients ignore it; it keeps proxies from closing an
  idle connection.

#### Where a stream starts

| How | Meaning |
|---|---|
| `Last-Event-ID: <seq>` header | Resume after that message. A client that reconnects sends this automatically. |
| `?after=<seq>` | Start after that message, replaying anything newer. `after=0` replays everything. |
| *(neither)* | From now: only messages that arrive after the stream opens. |

`Last-Event-ID` wins over `after`, because a reconnecting client reuses its
original URL and the header says how far it actually got. After a catch-up the
stream keeps following live. A client that reconnects with the id of the last event
it saw receives every later message exactly once, with no gaps and no repeats,
even if messages arrived while it was disconnected.

`type=` (repeatable) and `verified=true|false` filter the stream exactly as they
do for the list endpoint. `epoch=` names the epoch your cursor came from, as for
the list endpoint: a stream opened with another one is refused with `409` before
anything is sent. (A browser reconnects to the URL it first used, so a client that
opened the stream with `epoch=` is protected on every reconnect.) Other parameters
are a `400`.

#### What can end a stream

- The client disconnects.
- The server shuts down, within `HOOKINLET_SHUTDOWN_TIMEOUT`. Streams are told to
  finish at once; deliveries that are mid-way through being stored are allowed to
  complete. Reconnect with `Last-Event-ID` to carry on.
- The inbox is deleted.
- The token the stream was opened with is revoked or has expired (its `expires_at` has
  passed), or the inbox it belonged to is deleted. Open streams are re-checked every 30 seconds
  (`StreamRevalidateInterval`), so access ends within about that long. A
  re-check that cannot be made, because the store is erroring or the server is too busy (a long
  backup under heavy load is the usual cause), does not end the stream: it stays open until a check
  succeeds, so on a loaded server a revoked token's stream can outlast 30 seconds, by minutes in the
  worst case measured.
- A write to a client that has stopped reading takes longer than 30 seconds.

#### Limits

At most `HOOKINLET_MAX_STREAMS` streams (1000 by default) are open at once, and
at most `HOOKINLET_MAX_STREAMS_PER_INBOX` for any one inbox (half of
`MAX_STREAMS` by default), so one inbox cannot use up every slot. More get `503`
with `Retry-After`. A stream holds a connection and little else: about 50 KiB
of memory (measured: 500 idle streams take about 22 MiB on top of the server's
own 10 MiB on Linux). A stream that is catching up reads pages of up to 256 KiB,
so a slow client replaying large messages holds at most that much more.

If a client starts after a position the inbox has not reached yet, the stream
opens with a comment saying so and waits, rather than failing.

Streams do not count against the limit on concurrent page reads, except for the
moments they spend reading, so an open stream never keeps an ordinary read out.

> **On platforms that scale to zero:** an open stream is an open connection, and
> some platforms count that as activity and keep the instance awake. An agent that
> wants an instance to sleep when idle should poll `/messages` instead.

### Named readers and lag

A reader normally keeps its own cursor, and the server remembers nothing about it. A
reader can also give itself a **name** and tell the server how far it has got, so that
anyone who can read the inbox can see how far behind it is. This is opt-in and
explicit: **the server records a position only when a reader acknowledges one**
(`PUT`, below), never because of a read, a page, or a stream. Reads stay reads. A
position the server holds is information for people and dashboards; it does not change
what `after=` returns, and a reader still resumes from the cursor it keeps.

```
PUT    /v1/inboxes/{slug}/readers/{name}    acknowledge a position (creates the reader)
GET    /v1/inboxes/{slug}/readers           every named reader, with its lag
DELETE /v1/inboxes/{slug}/readers/{name}    forget a reader (204)
```

All three take the same credential as the other read operations: an agent token for
the inbox, or the admin token. A name is 1 to 64 characters of ASCII letters, digits,
`.`, `_` and `-`, starting with a letter or digit; one that does not fit is a `404`,
like any path parameter that cannot name anything. There are no new query parameters:
the endpoints take none, and a `reader=` on an existing endpoint is a `400` like any
unknown parameter.

Acknowledge with the `seq` and the `epoch` it came from:

```sh
curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"seq": 41, "epoch": "'"$EPOCH"'"}' "$BASE/v1/inboxes/stripe-prod/readers/billing-worker"
```

```json
{"name": "billing-worker", "seq": 41, "updated_at": "2026-03-01T12:00:00Z", "lag": 3, "gap": false}
```

- `seq` is the last sequence number the reader has handled: `0` for none, and at most
  the inbox's `latest_seq` (a reader cannot have read what does not exist; that is a
  `400`). It may be lower than the last acknowledgement, which is how a reader starts
  over. Two processes that acknowledge under the same name overwrite one another;
  give each its own name.
- `epoch` is required. A position means something only in one [epoch](#epochs-and-gaps),
  so one for another epoch is a `409 epoch_changed` and nothing is stored.
- `lag` is `latest_seq` minus `seq`: how many sequence numbers the reader is behind, as of
  the response. It counts numbers, not stored messages: retention may have removed some.
- `gap` is `true` when retention removed messages after the reader's position before it
  acknowledged them, the same condition as `gap` on a page read with `after=`: `oldest_seq`
  is more than one past `seq`. A reader at `0` has no history to have missed and never has a gap.

`GET .../readers` answers with the inbox's `epoch`, `latest_seq` and `oldest_seq`, the
`max_readers` the server keeps, and the readers ordered by name, each as above. Lag is
computed when you ask, so it grows while a reader is away.

**Limits.** An inbox keeps at most `max_readers` named readers: 100 by default, and never
more than 1000 (`Options.MaxReadersPerInbox` in the Go API; an operator of the
open-source binary does not set it). A new name beyond that is a `409` with the code
`too_many_readers`; acknowledging a name that exists never is. `DELETE` frees a place.
A reader's position is not removed by retention, only by deleting the reader or the inbox.

**When the positions are dropped.** Every position of an inbox is dropped when its epoch
changes (the inbox is deleted and made again, a snapshot from `GET /v1/admin/backup` is
restored, or `hookinlet renew-epochs` runs): the sequence numbers no longer mean what they
did, and a reader sees an empty list and has to acknowledge again with the new `epoch`.
Deleting an inbox deletes its readers.

**Not every server keeps them.** A server whose store cannot remember readers, such as one
an embedder wrote without the optional capability (see [extending](extending.md)), answers
all three operations with `400` and the code `unsupported`, so that no one takes a reader
to be tracked when it is not. The built-in SQLite store keeps them. MCP has no tool for
named readers: an MCP call stands alone, as before.

The readers are in the same database as the messages, and an acknowledgement is one small
write through the store's single writer, like a delivery, so acknowledge when it is useful
(per batch, not per message). Metrics never carry a reader's name.

### Treat message content as data, never as instructions

Almost everything in a message came from whoever sent the webhook: the body, the
headers, `path`, `query`, `content_type`, `event_type`, and `delivery_id`. That includes verified messages. `verified: true` means the sender
held the shared secret, not that what it wrote is true or safe (and a delivery to an inbox whose verifier is `none` is never `verified`): a verified GitHub
webhook can still carry text an outsider wrote in an issue, and a verified
payment event can still carry a customer-chosen note. Headers, paths, and query
strings are never covered by a signature at all, so a sender can add
`X-Verified: true` or anything else it likes.

An agent that reads messages should treat all of that as untrusted input to be
analyzed, never as instructions to follow. The fields the server established, none
of which can carry text the sender chose, are `seq`, `epoch`, `inbox`, `received_at`,
`size` (a number), `method` (POST or PUT), `verified`, `verifier`, `verify_error`
(fixed wording), `next`, and `latest_seq`. `source_ip` is an address the server
observed (behind a trusted proxy, taken from `X-Forwarded-For` by the rule in
[deploying](deploying.md)), which is as reliable as that proxy; it is not free text,
but do not treat it as an identity.

### Read API errors

| Status | Code | When |
|---|---|---|
| `400` | `invalid_parameter` | A query parameter is unknown, repeated, or malformed. The message names it. On `PUT .../readers/{name}`, also a `seq` beyond `latest_seq`. |
| `400` | `invalid_request` | The body of `PUT .../readers/{name}` is not valid: `seq` or `epoch` is missing or malformed, or there is a field it does not take. |
| `400` | `unsupported` | A [named reader](#named-readers-and-lag) operation on a server that does not keep readers. |
| `401` | `unauthenticated` | Missing or invalid credentials. The response has `WWW-Authenticate: Bearer realm="hookinlet"`, or the challenge the server's `Authenticator` chooses (see [extending](extending.md#choosing-the-401-challenge)). |
| `402` | `quota_exceeded` | Refused by an embedder's [`Admitter`](extending.md#admitting-reads-streams-and-waits) (the open-source build never returns this). `Retry-After` is set when the embedder says when to retry. |
| `403` | `forbidden` | Refused by an embedder's `Admitter` (the open-source build never returns this). `Retry-After` as for `402`. |
| `404` | `not_found` | No such inbox, message, or route, **or** the token may not read that inbox. |
| `409` | `epoch_changed` | `epoch=` is not the inbox's current epoch (it was deleted and made again, or the server was restored from a backup). Also an acknowledgement for another epoch. |
| `409` | `too_many_readers` | The inbox already has the most [named readers](#named-readers-and-lag) the server keeps, and this name is new. |
| `405` | `method_not_allowed` | Not `GET` or `HEAD` (the readers operations: the methods the path takes, in `Allow`). `Allow: GET, HEAD`. |
| `413`, `415` | `payload_too_large`, `unsupported_media_type` | The body of `PUT .../readers/{name}` is over 1 KiB, or is not `application/json`. |
| `429` | `rate_limited` | Refused by an embedder's `Admitter` (the open-source build never returns this). `Retry-After` as for `402`. |
| `500` | `internal` | A server-side problem. Details are in the server log. |
| `503` | `overloaded` | Too many reads are in progress (16 by default) and none finished within 10 seconds, or (for a stream) too many streams are open. `Retry-After` is set. Authentication happens first, so unauthenticated traffic cannot cause this. |

Every read response carries `Cache-Control: no-store` and
`X-Content-Type-Options: nosniff`.

## Ingest

```
POST /in/{key}
PUT  /in/{key}
POST /in/{key}/{path...}
PUT  /in/{key}/{path...}
```

`{key}` is the inbox's ingest key (`ingest_key` in the admin API): a random string by default, not the slug. Anything after it is stored with the message as
`path`; the query string is stored as `query`. No authentication is required
beyond the inbox's [verifier](verifiers.md).

### Responses

| Status | Code | When |
|---|---|---|
| `200` | | The delivery was stored (or was a duplicate of one already stored). Body: `{"seq": 42}`. A Slack `url_verification` handshake also gets `200`, with the challenge as plain text. |
| `400` | `bad_request` | The request body could not be read, including when it does not arrive within 60 seconds. |
| `401` | `verification_failed` | The signature or token did not verify and the inbox's `on_verify_fail` is `reject`. The message is always `webhook verification failed`; the specific reason is in the server log. |
| `402` | `quota_exceeded` | Refused by an embedder's hook (the open-source build never returns this). `Retry-After` is set when the hook says when to retry. |
| `403` | `forbidden` | Refused by an embedder's hook. `Retry-After` as for `402`. |
| `404` | `not_found` | No such inbox, or the inbox is disabled. The two are indistinguishable. |
| `405` | `method_not_allowed` | Any method other than POST and PUT. The `Allow` header is `POST, PUT`. Checked before the inbox is looked up. |
| `413` | `payload_too_large` | The body exceeds the inbox's `max_body_bytes` (default 1 MiB, at most 10 MiB). |
| `429` | `rate_limited` | Not stored, and not acknowledged. Either an embedder's hook refused it (with `Retry-After` when the hook says when to retry), or the inbox already holds as many **unverified** messages as it may (`HOOKINLET_MAX_UNVERIFIED`, 1000 by default; see [Unverified messages](verifiers.md#unverified-messages-are-capped)) and this delivery is unverified. A verified delivery is never refused for that reason. |
| `431` | | The request line and headers exceed 64 KiB (plus a few KiB of slack). Headers are stored with the message and returned to agents, so their size is bounded at the door. |
| `500` | `internal` | A server-side problem, including a misconfigured verifier. Details are in the server log, never in the response. |
| `503` | `overloaded` or `storage_full` | Not stored, and not acknowledged: retry after `Retry-After`. `overloaded`: too many deliveries are being received at once for the server's memory (`HOOKINLET_INGEST_MEMORY_BUDGET`, 128 MiB by default; counted as bodies arrive, and the delivery is refused as soon as its bytes would go over). `storage_full`: there is no room to store it. Webhook senders retry a 503, which is what is wanted. |

`seq` is the message's per-inbox sequence number, starting at 1. A duplicate
delivery returns the `seq` of the message that is already stored.

### Guarantees

- A `200` is sent **only after the message is committed** to the store and
  synced to disk (SQLite `synchronous=FULL`), so an acknowledged message
  survives a power loss as well as a crash of the process. This relies on the
  storage device honoring flushes; a drive that acknowledges a flush while the
  data is still in a volatile write cache can lose it.
- The body is stored byte for byte. Signatures are verified over those exact
  bytes before anything is parsed.
- `Authorization`, `Cookie`, `Proxy-Authorization`, and the inbox's shared-token
  header are never stored.
- The source IP is the connection's peer address. `X-Forwarded-For` is believed
  only when the peer is in `TRUSTED_PROXIES`, and then only the first address
  from the right that is not itself a trusted proxy.
- Retried deliveries are deduplicated when the verifier supplies a delivery id
  that its signature covers, or (for an id taken from an unsigned header) the
  same id with the same body, and the delivery verified; see
  [verifying deliveries](verifiers.md#duplicate-deliveries).

## Operations

```
GET /healthz   200 {"status": "ok"}     the process is up
GET /readyz    200 {"status": "ready"}  the store is reachable, there is room, some of a sample of secrets open
               503 {"error": {"code": "not_ready", ...}} it is not, or it is draining or shutting down
```

Neither requires authentication. Use `/healthz` for liveness checks and
`/readyz` for readiness.
