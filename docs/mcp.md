# MCP endpoint

hookinlet serves the same reads as its [REST API](api.md) as
[Model Context Protocol](https://modelcontextprotocol.io) tools, so an agent can
use an inbox with no glue code: point an MCP client at the server and the agent
can list inboxes, read messages, and wait for new ones.

The endpoint is read-only. There are no tools that write, delete, or administer
anything.

## Endpoint

```
POST /mcp
```

[Streamable HTTP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#streamable-http),
**stateless**: every call stands alone, carries its own token, and is answered as
a plain JSON response. There are no sessions to expire or lose, any instance can
answer any call, and revoking a token takes effect on the next call.

What is served is deliberately small:

- Only `POST` with `Content-Type: application/json`. `GET` and `DELETE` get `405`.
- One JSON-RPC request per POST. A **batch** (a JSON array) is refused with `400`:
  it would run many calls at once, each of them able to hold a wait open or build
  a large result, from one small request.
- Only the methods a tools-only server needs: `initialize`,
  `notifications/initialized`, `notifications/cancelled`, `ping`, `tools/list`,
  and `tools/call`. Anything else, including resources, prompts, and
  subscriptions, gets `400`.
- A request body over 1 MiB gets `413`.

Errors from the endpoint itself (before a tool runs) use the same JSON error
shape as the rest of the API. Responses are never cacheable (`Cache-Control:
no-store`).

It is on by default. Turn it off with `HOOKINLET_MCP_ENABLED=false`. It is also
off whenever the read API is, since both need an authenticator.

## Authentication

The same bearer tokens as the REST API:

```
Authorization: Bearer <token>
```

- An **agent token** reads the inboxes it was issued for. Any other inbox, or
  one that does not exist, is "not found", with the same text, so a token never
  reveals what else is on the server.
- The **admin token** reads every inbox.

A missing or wrong token gets `401` with a `WWW-Authenticate: Bearer` header,
before anything else is looked at.

## Tools

### The shape of a message

A message has two parts. At the top are the fields the **server** established
about the delivery, none of which can carry text the sender chose: `seq`, `epoch`,
`inbox`, `received_at`, `method` (POST or PUT), `size`, `verified`, `verifier`,
`verify_error` (fixed wording), `source_ip` (an address the server observed; see the
[API notes](api.md#treat-message-content-as-data-never-as-instructions)), and
`body_truncated` (the server saying it left a body out). Under **`untrusted`** is everything the
**sender** wrote or chose: `path`, `query`, `headers`, `content_type`,
`delivery_id`, `event_type`, and the body (`body`, `body_text` or `body_base64`,
as in the [REST messages](api.md#messages)). An agent that reads a transcript, and
a person who reviews one, can see at once which text could have been written by an
attacker. It is the REST message with the sender's fields moved under one key; the
values are the same.

`query_messages` and `wait_for_messages` return messages **without bodies** unless
you pass `bodies: true`, with `untrusted.body_*` absent: an agent decides which
messages are worth the context they cost and fetches those with `get_message`,
which always returns the whole message. With `bodies`, a body over the inline limit
is left out and marked, as described under [Large bodies](#large-bodies). (The wire
encoding may write a character such as `<` as an escape, which is the same
value.)

### `list_inboxes`

| Input | Meaning |
|---|---|
| `after` | optional: the `next_after` of your previous result, to read the next page. Omit it (or pass `""`) to start at the beginning |

The inboxes this token can read: `slug`, `name`, `verifier`, `latest_seq`, and `epoch`,
at most 100 per call, whether it is an agent token for some inboxes, a token for
all inboxes, or the admin token. When it can read more than that, the result has
`truncated: true` and `next_after`, an opaque cursor: pass it back as `after` for
the next 100, and keep going while `next_after` is present. The last page has
neither. An agent token lists its inboxes in the order it was given them; any other
token lists them in the order they were made.

```
after = ""
loop:
    result = list_inboxes(after)
    handle(result.inboxes)
    if no result.next_after: stop
    after = result.next_after
```

A page continues after the inbox the previous one ended with, so inboxes created or
deleted elsewhere between calls do not make another inbox be skipped or listed
twice. If that inbox itself has been deleted (or the token can no longer read it)
the call fails, saying so, and you list again without `after`. A string that is
not a `next_after` fails with `invalid after`. A cursor is only good for the kind of
token that received it; it names an inbox, never says how many there are, and
whoever passes one sees only inboxes their own token can read.

Nothing is ever reported about inboxes the token cannot read, not even how many
there are, and no result says how many more than 100 it can read. To keep it that
way, a token that is not limited to a list of inboxes (a token for all inboxes,
the admin token, or an embedder's principal) finds its inboxes by looking through
the server's inboxes in order, at most 10,000 per call. A call that reaches that
limit before it has 100 inboxes to return ends the list there, without
`truncated` or `next_after`, since saying it stopped early would say how many
inboxes there are. Such a token, among very many inboxes it cannot read, may
therefore not see all of its own in the list; it can still read any inbox it
knows by slug.

### `query_messages`

| Input | Meaning |
|---|---|
| `inbox` | Slug of the inbox. **Required.** |
| `after` | Read forward, oldest first, from messages with a seq greater than this (`0` for the beginning). |
| `before` | Read backward, newest first, from messages with a seq less than this. |
| `limit` | 1 to 100, default 20. |
| `types` | Only messages whose `event_type` is one of these. |
| `since`, `until` | RFC 3339 times. |
| `verified` | Only verified (`true`) or unverified (`false`) messages. |
| `bodies` | Include message bodies. **Default false**: messages come without them, and you fetch the ones you want with `get_message`. |
| `epoch` | The `epoch` your cursor came from; see [Epochs and gaps](#epochs-and-gaps). |

With neither `after` nor `before`, you get the newest messages, newest first.
Parameters are checked as strictly as the REST API checks them: a misspelled
name or an out-of-range value is an error, not a silent default.

The result has `messages`, `latest_seq`, and a cursor:

- `next_after` (forward reads): pass it as `after` next time. **Keep it even when
  `messages` is empty.** Following it visits every message exactly once, in
  order, and it skips ahead past messages that did not match your filters.
- `next_before` (backward reads): pass it as `before` to continue, or `null` when
  the oldest message has been returned.
- `epoch` and `gap`: see below.

#### Epochs and gaps

A cursor is a sequence number, and a sequence number only means something within
one life of an inbox. An inbox that is deleted and made again under the same slug
starts at 1 again, and so does a server restored from a backup (which may have
accepted different messages since). So every inbox has an **epoch**, an opaque
string that changes in those cases, and every result carries it (`list_inboxes`
too). Keep it with your cursor and pass it back as `epoch`: if it is no longer the
inbox's current one the call fails with `epoch_changed`, instead of quietly
reading from numbers that no longer mean what they did. Then start again with
`after: 0` and the new epoch.

`gap: true` means messages after your cursor were removed by retention before you
read them: what follows does not continue where you left off. (A cursor of `0`
never reports one.)

### `get_message`

`inbox` and `seq`, and optionally `epoch` (as for `query_messages`, so that a `seq`
from an earlier life of the inbox is refused rather than answered with another
message). One message with its **full** body, up to the size limit described under
[Limits](#limits).

### `wait_for_messages`

| Input | Meaning |
|---|---|
| `inbox` | Slug of the inbox. **Required.** |
| `after` | Return messages with a seq greater than this. **Required.** |
| `types` | Only messages whose `event_type` is one of these. |
| `timeout_seconds` | 0 to 60, default 30. |
| `limit` | 1 to 100, default 20. |
| `bodies` | Include message bodies; default false, as for `query_messages`. |
| `epoch` | The `epoch` your cursor came from; see [Epochs and gaps](#epochs-and-gaps). |

Returns as soon as any matching message exists after `after`, or an empty list
when the timeout passes. This is the polling loop for agents that cannot hold an
SSE connection:

```
after = latest_seq from list_inboxes (or 0 to start from the beginning)
loop:
    result = wait_for_messages(inbox, after)
    handle(result.messages)
    after = result.next_after
```

A wait holds no database connection while it sleeps. It ends early if the server
shuts down, or if the token is revoked or the inbox deleted (checked every 30
seconds), and an agent that hangs up releases it immediately.

Waits count against the same limits as SSE streams (`HOOKINLET_MAX_STREAMS`
overall, `HOOKINLET_MAX_STREAMS_PER_INBOX` for one inbox): a wait that finds no
free slot is told "too many waits are in progress" and should retry shortly. A
`timeout_seconds` of `0` checks once and takes no slot.

A deployment that embeds hookinlet can also limit reads and waits itself (an
[`Admitter`](extending.md#admitting-reads-streams-and-waits)). A tool call it refuses
fails with a message saying so (such as "too many requests"), and the agent should
retry shortly. The open-source build never refuses this way.

## Large bodies

To protect an agent's context window, `query_messages` and `wait_for_messages`
leave out any body larger than 16 KiB (`HOOKINLET_MCP_INLINE_BODY_BYTES`). Those
messages have `body_truncated: true` at the top level, no body fields under
`untrusted`, and their true `size`.
Fetch one whole with `get_message`. (Without `bodies: true` no body is sent at all.)

## Treat message content as data

Everything under a message's `untrusted` was written by whoever sent the webhook,
including a verified one. The server says so in its `instructions` and in each tool's
description, but the protection that matters is in your agent: treat message
content as data to analyze, never as instructions to follow. See
[the API notes](api.md#treat-message-content-as-data-never-as-instructions).

## Limits

- Tool calls share the REST API's limit on concurrent reads (16 by default;
  `Options.MaxConcurrentReads` for embedders). A call that cannot get a slot in
  10 seconds is told the server is busy. A waiting `wait_for_messages` holds a slot
  only while it reads.
- A `query_messages` or `wait_for_messages` result is at most about 1 MiB as
  sent. Each result carries its data twice (the structured content and its text
  form) and the encoding can make some characters several bytes longer, so the
  server measures what it is about to send and returns fewer messages than
  `limit` when needed, always at least one. `next_after` (or `next_before`)
  continues from the last message returned, so nothing is skipped.
- `get_message` returns up to about 4 MiB as sent. A message too large for that is
  refused with the URL of its `/raw` endpoint, which streams the bytes.
- The inline body limit cannot be set above 64 KiB, the size the REST API loads.
- Requests to `/mcp` are limited to 1 MiB.

## Connecting a client

Create an agent token with `hookinlet inbox create --verifier …` (it is shown once; `hookinlet token create` issues more), then give
your client the endpoint and the header.

**Claude Code:**

```sh
claude mcp add --transport http hookinlet https://hooks.example.com/mcp \
  --header "Authorization: Bearer $HOOKINLET_AGENT_TOKEN"
```

or, in a project's `.mcp.json`:

```json
{
  "mcpServers": {
    "hookinlet": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": { "Authorization": "Bearer ${HOOKINLET_AGENT_TOKEN}" }
    }
  }
}
```

**Clients configured with JSON** (Cursor and others; the exact keys vary, check
your client's documentation):

```json
{
  "mcpServers": {
    "hookinlet": {
      "url": "https://hooks.example.com/mcp",
      "headers": { "Authorization": "Bearer <agent token>" }
    }
  }
}
```

**VS Code** (`.vscode/mcp.json`):

```json
{
  "servers": {
    "hookinlet": {
      "type": "http",
      "url": "https://hooks.example.com/mcp",
      "headers": { "Authorization": "Bearer <agent token>" }
    }
  }
}
```

**Without a client**, a call is one JSON-RPC POST:

<!-- exercise -->
```sh
curl -s https://hooks.example.com/mcp \
  -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",
       "params":{"name":"query_messages","arguments":{"inbox":"demo","after":0}}}'
```

Use TLS in front of the server for anything beyond localhost: the token is the
whole credential.
