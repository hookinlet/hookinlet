# MCP endpoint

hookinlet offers the same reads as its [REST API](api.md) as [Model Context Protocol](https://modelcontextprotocol.io)
(MCP) tools. MCP is a standard way for an agent to call tools on a server. With it, an agent can use an inbox
with no glue code. Point an MCP client at the server, and the agent can list inboxes, read messages, and wait
for new ones.

The endpoint is read-only. It has no tools that write, delete, or administer anything.

## Endpoint

```
POST /mcp
```

The endpoint uses [Streamable HTTP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#streamable-http)
and is **stateless**. Every call stands alone, carries its own token, and is answered as a plain JSON response.
There are no sessions to expire or lose. Any instance can answer any call. Revoking a token takes effect on the
next call.

The endpoint is deliberately small:

- It takes only `POST` with `Content-Type: application/json`. `GET` and `DELETE` get `405`.
- It takes one JSON-RPC request per POST. A **batch** (a JSON array) gets `400`. A batch would let one small
  request run many calls at once, and each call could hold a wait open or build a large result.
- It supports only the methods a tools-only server needs: `initialize`, `notifications/initialized`,
  `notifications/cancelled`, `ping`, `tools/list`, and `tools/call`. Anything else gets `400`. That includes
  resources, prompts, and subscriptions.
- A request body over 1 MiB gets `413`.

Errors from the endpoint itself (before a tool runs) use the same JSON error shape as the rest of the API.
Responses are never cacheable (`Cache-Control: no-store`).

The endpoint is on by default. Turn it off with `HOOKINLET_MCP_ENABLED=false`. It is also off whenever the read
API is off, because both need an authenticator.

## Authentication

MCP uses the same bearer tokens as the REST API:

```
Authorization: Bearer <token>
```

- An **agent token** reads the inboxes it was issued for. Any other inbox, or one that does not exist, gets
  "not found" with the same text. So a token never reveals what else is on the server.
- The **admin token** reads every inbox.

A missing or wrong token gets `401` with a `WWW-Authenticate` header (`Bearer realm="hookinlet"`, unless the
server's `Authenticator` chooses another challenge). The server checks this before anything else.

## Tools

### The shape of a message

A message has two parts. At the top are the fields the **server** established about the delivery. None of them
can carry text the sender chose:

- `seq`, `epoch`, `inbox`, `received_at`, `method` (POST or PUT), `size`, `verified`, `verifier`, and
  `verify_error` (fixed wording);
- `modified`. It is present, and `true`, only when the body below is not what the sender sent, because the
  server's embedder replaced it before storing. You cannot check its signature against the stored body.
- `source_ip`, an address the server observed (see the
  [API notes](api.md#treat-message-content-as-data-never-as-instructions));
- `body_truncated`, which means the server left a body out.

Under **`untrusted`** is everything the **sender** wrote or chose: `path`, `query`, `headers`, `content_type`,
`delivery_id`, `event_type`, and the body (`body`, `body_text` or `body_base64`, as in the
[REST messages](api.md#messages)). An agent that reads a transcript, and a person who reviews one, can see at
once which text could have been written by an attacker. It is the REST message with the sender's fields moved
under one key. The values are the same.

`query_messages` and `wait_for_messages` return messages **without bodies** unless you pass `bodies: true`. The
`untrusted.body_*` fields are then absent. The agent decides which messages are worth the context they cost, and
fetches those with `get_message`, which always returns the whole message. With `bodies`, the server leaves out
and marks a body over the inline limit, as described under [Large bodies](#large-bodies). (The wire encoding
may write a character such as `<` as an escape. That is the same value.)

### `list_inboxes`

| Input | Meaning |
|---|---|
| `after` | optional: the `next_after` of your previous result, to read the next page. Omit it (or pass `""`) to start at the beginning |

The result lists the inboxes this token can read: `slug`, `name`, `verifier`, `latest_seq`, and `epoch`. For an
`hmac` inbox it also has `signature_headers`, the names (never the values) of the stored headers that carry the
signature. It returns at most 100 per call. This holds for an agent token for some inboxes, a token for all
inboxes, and the admin token. When the token can read more than 100, the result has `truncated: true` and
`next_after`, an opaque cursor. Pass it back as `after` for the next 100, and keep going while `next_after` is
present. The last page has neither. An agent token lists its inboxes in the order it was given them. Any other
token lists them in the order they were made.

```
after = ""
loop:
    result = list_inboxes(after)
    handle(result.inboxes)
    if no result.next_after: stop
    after = result.next_after
```

A page continues after the inbox the previous one ended with. So inboxes created or deleted elsewhere between
calls do not cause another inbox to be skipped or listed twice. If that inbox itself has been deleted (or the
token can no longer read it), the call fails and says so. Then list again without `after`. A string that is not
a `next_after` fails with `invalid after`. A cursor is only good for the kind of token that received it. It
names an inbox and never says how many there are. Whoever passes one sees only the inboxes their own token can
read.

The server never reports anything about inboxes the token cannot read, not even how many there are. No result
says how many more than 100 the token can read. To keep it that way, a token that is not limited to a list of
inboxes looks through the server's inboxes in order, at most 10,000 per call. (This covers a token for all
inboxes, the admin token, and an embedder's principal.) Say a call reaches that limit before it has 100 inboxes
to return. The list ends there, without `truncated` or `next_after`, because saying it stopped early would say
how many inboxes there are. So a token among very many inboxes it cannot read may not see all of its own in the
list. It can still read any inbox it knows by slug.

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

With neither `after` nor `before`, you get the newest messages, newest first. The server checks parameters as
strictly as the REST API does. A misspelled name or an out-of-range value is an error, not a silent default.

The result has `messages`, `latest_seq`, and a cursor:

- `next_after` (forward reads): pass it as `after` next time. **Keep it even when `messages` is empty.**
  Following it visits every message exactly once, in order. It skips ahead past messages that did not match your
  filters.
- `next_before` (backward reads): pass it as `before` to continue. It is `null` when the oldest message has been
  returned.
- `epoch` and `gap`: see below.

#### Epochs and gaps

A cursor is a sequence number, and a sequence number only means something within one life of an inbox. If you
delete an inbox and make it again under the same slug, it starts at 1 again. So does a server restored from a
backup, which may have accepted different messages since. So every inbox has an **epoch**, an opaque string that
changes in those cases. Every result carries it, including `list_inboxes`. Keep it with your cursor and pass it
back as `epoch`. If it is no longer the inbox's current epoch, the call fails with `epoch_changed`. Without
this, the call would quietly read from numbers that no longer mean what they did. Then start again with
`after: 0` and the new epoch.

`gap: true` means retention removed messages after your cursor before you read them. What follows does not
continue where you left off. (A cursor of `0` never reports a gap.)

### `get_message`

Takes `inbox` and `seq`, and optionally `epoch`. `epoch` works as for `query_messages`: the server refuses a
`seq` from an earlier life of the inbox, and does not answer with another message. It returns one message with
its **full** body, up to the size limit described under [Limits](#limits).

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

This tool returns as soon as any matching message exists after `after`. If the timeout passes first, it returns
an empty list. Use it as the polling loop for agents that cannot hold an SSE connection (a stream of events the
server pushes over one open connection):

```
after = latest_seq from list_inboxes (or 0 to start from the beginning)
loop:
    result = wait_for_messages(inbox, after)
    handle(result.messages)
    after = result.next_after
```

A wait holds no database connection while it sleeps. It ends early if the server shuts down, or if the token is
revoked or the inbox is deleted (the server checks every 30 seconds). An agent that hangs up releases the wait
immediately.

Waits count against the same limits as SSE streams (`HOOKINLET_MAX_STREAMS` overall,
`HOOKINLET_MAX_STREAMS_PER_INBOX` for one inbox). A wait that finds no free slot is told "too many waits are in
progress" and should retry shortly. A `timeout_seconds` of `0` checks once and takes no slot.

A deployment that embeds hookinlet can also limit reads and waits itself (an
[`Admitter`](extending.md#admitting-reads-streams-and-waits)). If it refuses a tool call, the call fails with a
message. The message starts with the condition, named as the HTTP API's error code names it (`rate_limited`,
`quota_exceeded` or `forbidden`). It ends with the time to wait, when the deployment gives one (such as
`rate_limited: too many requests; retry after 90 s`). The open-source build never refuses this way.

## Large bodies

To protect an agent's context window, `query_messages` and `wait_for_messages` leave out any body larger than
16 KiB (`HOOKINLET_MCP_INLINE_BODY_BYTES`). Those messages have `body_truncated: true` at the top level, no body
fields under `untrusted`, and their true `size`. Fetch one whole with `get_message`. (Without `bodies: true`, no
body is sent at all.)

## Treat message content as data

Whoever sent the webhook wrote everything under a message's `untrusted`, even for a verified delivery. The
server says so in its `instructions` and in each tool's description. But the protection that matters is in your
agent. Treat message content as data to analyze, never as instructions to follow. See
[the API notes](api.md#treat-message-content-as-data-never-as-instructions).

## Limits

- Tool calls share the REST API's limit on concurrent reads (16 by default; `Options.MaxConcurrentReads` for
  embedders). A call that cannot get a slot in 10 seconds is told the server is busy. A waiting
  `wait_for_messages` holds a slot only while it reads.
- A `query_messages` or `wait_for_messages` result is at most about 1 MiB as sent. Each result carries its data
  twice (the structured content and its text form). The encoding can also make some characters several bytes
  longer. So the server measures what it is about to send, and returns fewer messages than `limit` when needed,
  always at least one. `next_after` (or `next_before`) continues from the last message returned, so nothing is
  skipped.
- `get_message` returns up to about 4 MiB as sent. If a message is too large for that, the call is refused with
  the URL of its `/raw` endpoint, which streams the bytes.
- You cannot set the inline body limit above 64 KiB, the size the REST API loads.
- Requests to `/mcp` are limited to 1 MiB.

## Connecting a client

Create an agent token with `hookinlet inbox create --verifier …`. It is shown once, and `hookinlet token create`
issues more. Then give your client the endpoint and the header.

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

**Clients configured with JSON** (Cursor and others). The exact keys vary, so check your client's
documentation:

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

Put TLS in front of the server for anything beyond localhost. The token is the whole credential.
