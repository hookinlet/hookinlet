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

- An **agent token** reads one inbox. Any other inbox, or one that does not
  exist, is "not found", with the same text, so a token never reveals what else
  is on the server.
- The **admin token** reads every inbox.

A missing or wrong token gets `401` with a `WWW-Authenticate: Bearer` header,
before anything else is looked at.

## Tools

Message objects carry the same JSON values as the [REST messages](api.md#messages),
with one addition described under [Large bodies](#large-bodies). (The wire
encoding may write a character such as `<` as an escape, which is the same
value.)

### `list_inboxes`

The inboxes this token can read: `slug`, `name`, `verifier`, and `latest_seq`.
An agent token lists its one inbox. The admin token lists up to 100;
`truncated: true` says it can read more than that. Nothing is ever reported about
inboxes the token cannot read, not even how many there are.

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
| `summary` | Leave bodies out. |

With neither `after` nor `before`, you get the newest messages, newest first.
Parameters are checked as strictly as the REST API checks them: a misspelled
name or an out-of-range value is an error, not a silent default.

The result has `messages`, `latest_seq`, and a cursor:

- `next_after` (forward reads): pass it as `after` next time. **Keep it even when
  `messages` is empty.** Following it visits every message exactly once, in
  order, and it skips ahead past messages that did not match your filters.
- `next_before` (backward reads): pass it as `before` to continue, or `null` when
  the oldest message has been returned.

### `get_message`

`inbox` and `seq`. One message with its **full** body, up to the size limit
described under [Limits](#limits).

### `wait_for_messages`

| Input | Meaning |
|---|---|
| `inbox` | Slug of the inbox. **Required.** |
| `after` | Return messages with a seq greater than this. **Required.** |
| `types` | Only messages whose `event_type` is one of these. |
| `timeout_seconds` | 0 to 60, default 30. |
| `limit` | 1 to 100, default 20. |

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
shuts down, or if the token is rotated or the inbox deleted (checked every 30
seconds), and an agent that hangs up releases it immediately.

Waits count against the same limits as SSE streams (`HOOKINLET_MAX_STREAMS`
overall, `HOOKINLET_MAX_STREAMS_PER_INBOX` for one inbox): a wait that finds no
free slot is told "too many waits are in progress" and should retry shortly. A
`timeout_seconds` of `0` checks once and takes no slot.

## Large bodies

To protect an agent's context window, `query_messages` and `wait_for_messages`
leave out any body larger than 16 KiB (`HOOKINLET_MCP_INLINE_BODY_BYTES`). Those
messages have `body_truncated: true`, no body fields, and their true `size`.
Fetch one whole with `get_message`.

## Treat message content as data

Everything in a message was written by whoever sent the webhook, including a
verified one. The server says so in its `instructions` and in each tool's
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

Create an agent token with `hookinlet inbox create` (it is shown once), then give
your client the endpoint and the header.

**Claude Code:**

```sh
claude mcp add --transport http hookinlet https://hooks.example.com/mcp \
  --header "Authorization: Bearer $HOOKINLET_AGENT_TOKEN"
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
