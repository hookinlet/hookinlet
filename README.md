<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/banner-dark.png">
  <img alt="hookinlet: webhooks into named inboxes. Agents read them over REST, SSE or MCP." src=".github/banner-light.png">
</picture>

[![ci](https://img.shields.io/github/actions/workflow/status/hookinlet/hookinlet/ci.yml?branch=main&label=ci&labelColor=3A4245&style=flat-square)](https://github.com/hookinlet/hookinlet/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-AGPL--3.0-08776B?labelColor=3A4245&style=flat-square)](LICENSE)
[![release](https://img.shields.io/github/v/release/hookinlet/hookinlet?include_prereleases&sort=semver&label=release&labelColor=3A4245&color=0E1112&style=flat-square)](https://github.com/hookinlet/hookinlet/releases)
[![docker](https://img.shields.io/badge/docker-ghcr.io%2Fhookinlet%2Fhookinlet-0E1112?labelColor=3A4245&style=flat-square)](https://github.com/hookinlet/hookinlet/pkgs/container/hookinlet)
[![mcp](https://img.shields.io/badge/mcp-%2Fmcp-0E1112?labelColor=3A4245&style=flat-square)](docs/mcp.md)

Your agent needs to know when Stripe, GitHub, or Slack says something, but it cannot hold a public URL and it must
not trust what arrives. hookinlet is that URL: a single Go binary that receives webhooks into named inboxes,
verifies their signatures, and keeps a queryable history. Agents read it three ways, REST, an SSE stream, or an
MCP endpoint, using a per-inbox sequence number as the cursor. SQLite by default; runs anywhere.

[Docs](#documentation) · [Quickstart](#quickstart) · [MCP tools](#read-from-an-agent) · [hookinlet Cloud (waitlist)](https://hookinlet.com/#cloud)

## Quickstart

Run it, send it a signed event, read it back. You need `curl` and `openssl`; the examples further down also use
`jq`, `python3`, and Go. The server needs nothing.

Two secrets configure a server, and you must keep both. The admin token manages it. The master key encrypts the
signing secrets it stores, so a server started later with a different key cannot read them.

```sh
export HOOKINLET_ADMIN_TOKEN=$(openssl rand -base64 32)
export HOOKINLET_MASTER_KEY=$(openssl rand -base64 32)

docker run -d --name hookinlet -p 127.0.0.1:8080:8080 -v hookinlet-data:/data \
  -e HOOKINLET_ADMIN_TOKEN -e HOOKINLET_MASTER_KEY \
  ghcr.io/hookinlet/hookinlet
```

Prefer a binary? Download one from the [releases page](https://github.com/hookinlet/hookinlet/releases) (Linux and
macOS, amd64 and arm64, static), or build it from a clone:

```sh
go build -o hookinlet ./cmd/hookinlet     # then put ./hookinlet on your PATH
```

### Send a signed event, read it back

This walk plays the part of GitHub with `openssl`, so there is nothing to sign up for. Run it from a clone, with
`hookinlet` on your `PATH`. Every command is run by the project's tests, in this order, in one shell.

Start a server. If you started the Docker container above, skip this block and keep the shell where you exported
the two secrets: the walk talks to whichever server answers on port 8080. This one uses a throwaway data directory
and listens on this machine only:

<!-- exercise -->
```sh
export HOOKINLET_ADMIN_TOKEN=$(openssl rand -base64 32)
export HOOKINLET_MASTER_KEY=$(openssl rand -base64 32)
export HOOKINLET_DATA_DIR=$(mktemp -d)
export HOOKINLET_LISTEN_ADDR=127.0.0.1:8080
export HOOKINLET_LOG_LEVEL=warn
hookinlet serve &
for i in $(seq 50); do curl -sf localhost:8080/readyz >/dev/null && break; sleep 0.2; done
curl -sf localhost:8080/readyz      # {"status":"ready"}; if this fails, something else may be using port 8080 (on IPv6 too: try 127.0.0.1:8080)
```

Create an inbox that accepts only GitHub's signature, for a secret you choose. (This one is made up; a real one
belongs in a file or a password manager, never in a command line.) The response carries the **agent token** that
reads this inbox. It is shown once (more tokens, for several inboxes or all, are issued with `hookinlet token create`, and a token is replaced by issuing the new one and revoking the old). The response also carries the **ingest path**, the URL senders post to. Its last part is a
random key, not the slug: whoever has it can post to the inbox, so it is not a name anyone can guess ([why](docs/admin.md#why-the-ingest-url-is-not-the-slug)). (The admin API shows it
again whenever you ask.)

<!-- exercise -->
```sh
curl -s -X POST localhost:8080/v1/admin/inboxes \
  -H "Authorization: Bearer $HOOKINLET_ADMIN_TOKEN" -H 'Content-Type: application/json' -d @- > created.json <<'EOF'
{"slug": "github-demo", "verifier": "github", "verifier_secret": "my-webhook-secret"}
EOF

export HOOKINLET_AGENT_TOKEN=$(sed -n 's/.*"agent_token":"\([^"]*\)".*/\1/p' created.json)
export INGEST=$(sed -n 's/.*"ingest_path":"\([^"]*\)".*/\1/p' created.json)    # /in/<random key>
```

Send an event signed the way GitHub signs them, HMAC-SHA256 of the body in `X-Hub-Signature-256`:

<!-- exercise -->
```sh
body='{"action":"opened","number":42,"pull_request":{"title":"Add retries"}}'
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac 'my-webhook-secret' -hex | sed 's/^.* //')

curl -s -X POST localhost:8080$INGEST \
  -H "X-Hub-Signature-256: sha256=$sig" -H 'X-GitHub-Event: pull_request' \
  -H 'X-GitHub-Delivery: demo-1' -H 'Content-Type: application/json' -d "$body"
```

```json
{"seq":1}
```

Change one character of the body and send the same signature. It is refused, and nothing is stored:

<!-- exercise -->
```sh
curl -s -X POST localhost:8080$INGEST \
  -H "X-Hub-Signature-256: sha256=$sig" -H 'X-GitHub-Event: pull_request' \
  -H 'Content-Type: application/json' -d '{"action":"opened","number":43}'
```

```json
{"error":{"code":"verification_failed","message":"webhook verification failed"}}
```

Read the inbox from the start, with the agent token:

<!-- exercise -->
```sh
curl -s 'localhost:8080/v1/inboxes/github-demo/messages?after=0' -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN"
```

```json
{"messages":[{"seq":1,"inbox":"github-demo","received_at":"2026-01-15T09:30:00.000000Z","method":"POST",
  "headers":{"X-Github-Event":["pull_request"], "…":"…"},"content_type":"application/json","size":70,
  "verified":true,"verifier":"github","delivery_id":"demo-1","event_type":"pull_request","source_ip":"127.0.0.1",
  "body":{"action":"opened","number":42,"pull_request":{"title":"Add retries"}}, "…":"…"}],
 "next":"/v1/inboxes/github-demo/messages?after=1&epoch=…&limit=50","latest_seq":1}
```

Shortened; the [API reference](docs/api.md#messages) lists every field. `body` is your JSON, as JSON. `next` is the
cursor to keep. For the exact bytes that were sent, read `/v1/inboxes/github-demo/messages/1/raw`.

When you are done: `kill %1` stops the server this walk started, and `rm created.json` removes the file holding
the agent token.

### With a real sender

For GitHub, Slack, Stripe, or your own service, give the sender the inbox's `ingest_url` (the admin API shows it;
`HOOKINLET_PUBLIC_BASE_URL` sets the host part) and the same secret. The server must be reachable from the internet
for that: put it behind TLS, or use a tunnel to try it from a laptop. [Verifiers](docs/verifiers.md#trying-it-with-stripe)
shows the loop against Stripe's CLI, and [Deploying](docs/deploying.md) covers what a server needs.

## Read from an agent

An agent needs the server's address, an inbox's agent token, and a cursor. Choose how it reads.

**MCP.** Point any MCP client at `/mcp`. This is Claude Code's `.mcp.json`; other clients use other field names
([MCP](docs/mcp.md) has them):

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

| Tool | Arguments | Returns |
|---|---|---|
| `list_inboxes` | none | slug, name, verifier, `latest_seq`, `epoch` |
| `query_messages` | `inbox`, `epoch`, `after` or `before`, `limit`, `types`, `since`, `until`, `verified`, `bodies` | messages (without bodies unless `bodies`), and `next_after` to keep as the cursor |
| `get_message` | `inbox`, `epoch`, `seq` | one message with its full body |
| `wait_for_messages` | `inbox`, `epoch`, `after`, `types`, `timeout_seconds`, `limit`, `bodies` | messages after `after`; waits up to 60 s for one |

The tools are read-only. A token sees only the inboxes it was issued for. Messages come without bodies unless you ask (`bodies: true`), and a body over 16 KiB is then left out and flagged (`body_truncated`), so neither fills the agent's context; `get_message` returns the whole thing. What the sender wrote is under each message's `untrusted` key. A raw call:

<!-- exercise -->
```sh
curl -s localhost:8080/mcp -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_messages","arguments":{"inbox":"github-demo","after":0}}}'
```

**REST.** Ask for what came after the last seq you handled. It works from anywhere, including a cron job:

<!-- exercise -->
```sh
examples/curl/poll.sh github-demo       # Ctrl-C to stop
```

**SSE.** Hold a connection open and be told as messages arrive. A client that reconnects with the `Last-Event-ID`
of the last event it saw gets every later message exactly once:

<!-- exercise -->
```sh
curl -sN -m 2 -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN" 'localhost:8080/v1/inboxes/github-demo/stream?after=0' || [ $? -eq 28 ]   # 28: curl's timeout, which ends this demo
```

```
retry: 3000

id: 1
event: message
data: {"seq":1,"inbox":"github-demo", …}
```

REST, SSE, and MCP use the same cursor.

> [!WARNING]
> Everything in a message was written by whoever sent the webhook, including `verified` ones. `verified` means
> the message passed its inbox's verifier: for a signing scheme, that the sender held the secret, which does not
> make what it wrote true or safe. (An inbox whose verifier is `none`, which has to be asked for by name, stores
> everything as **not** verified. Use a real verifier for anything that matters.) An agent should treat message
> content as data to analyze and never as instructions to follow.

### Write a client

[`examples/`](examples) has the loop in a few languages, each about a page, standard library only:

| Example | |
|---|---|
| [`examples/curl`](examples/curl) | `poll.sh` and `stream.sh`, with `curl` and `jq` |
| [`examples/python`](examples/python) | `agent.py`, polling or streaming |
| [`examples/go-agent`](examples/go-agent) | the same in Go |
| [`examples/relay`](examples/relay) | forwards every message to another URL, with retries, in order |
| [`examples/custom-hooks`](examples/custom-hooks) | the server embedded in your own program, with a rate limit added |

<!-- exercise -->
```sh
python3 examples/python/agent.py github-demo --count 1
go run ./examples/go-agent -inbox github-demo -n 1
```

`examples/relay` passes webhooks on to an existing service: the original bytes and headers, so the receiver can
still check the sender's signature, at least once and in order. It retries while the destination is down or
misconfigured and skips a message only when the destination refuses that message for good (400, 410, 413, 415,
422). Schemes that sign a timestamp are checked by the receiver against a five minute window, so a message
delivered after a long outage may be refused. Headers that say where a request came from are not passed on; the
rest are the sender's, so a receiver must not take them on trust. hookinlet keeps no delivery queue of its own, and
the relay shows how little one takes.

## The console

Open `/admin` on a running server and sign in with the admin token. It does what the command line does, and
shows what is arriving: the inboxes and their activity, every message (body, headers, exact bytes) with a live
mode, new inboxes with a form for each kind of sender, secrets, retention, tokens, and backups. It is built into
the binary, follows your system's light or dark theme, and works on a phone. `HOOKINLET_ADMIN_UI=false` turns it
off. The token stays in the page's memory, and everything a sender wrote is shown as text, never as HTML.

![The overview: inboxes, their verifiers and a line of recent activity](docs/images/console-overview.png)

![An inbox: its messages, and one open with its verified body](docs/images/console-messages.png)

## Configuration

Every setting is a flag and a `HOOKINLET_*` environment variable. The flag wins.

```sh
HOOKINLET_ADMIN_TOKEN=…             # required, at least 32 characters
HOOKINLET_MASTER_KEY=…              # required, 32 bytes, base64
HOOKINLET_LISTEN_ADDR=:8080
HOOKINLET_DATA_DIR=./data           # one SQLite file; one instance per directory
HOOKINLET_RETENTION_INTERVAL=1h     # how often retention runs; "off" for never
HOOKINLET_TRUSTED_PROXIES=10.0.0.0/8   # whose X-Forwarded-For is believed
HOOKINLET_MASTER_KEY_FILE=/run/secrets/master_key   # a secret may be a file instead (also ADMIN_TOKEN, OLD_MASTER_KEY, VERIFIER_SECRET)
```

Inboxes live in the database. Create them with the command line or the [admin API](docs/admin.md); retention is
per inbox, by age, by count, or both:

```sh
# the verifier secret comes from HOOKINLET_VERIFIER_SECRET, never from a flag
hookinlet inbox create --slug stripe --verifier stripe --retention-days 30
hookinlet inbox create --slug github --verifier github --retention-max-count 50000
```

[Deploying](docs/deploying.md) lists every setting, the health checks, and what a host must provide.

### Signature presets

| `verifier` | Checks |
|---|---|
| `stripe` | `Stripe-Signature` (HMAC-SHA256 over the timestamp and body, five minute tolerance) |
| `github` | `X-Hub-Signature-256` |
| `slack` | `X-Slack-Signature` and `X-Slack-Request-Timestamp`; the `url_verification` handshake is answered for you |
| `standardwebhooks` | `webhook-id`, `webhook-timestamp`, `webhook-signature` (Svix and others) |
| `hmac` | any header you name; sha1, sha256, sha384, or sha512; hex or base64 |
| `token` | a shared token in a header you name |
| `none` | nothing: it accepts everything and stores it as **not** verified |

Checks run on the exact bytes that were sent, with constant-time comparisons, before anything is parsed. Details,
and what each scheme does and does not prove, are in [verifiers](docs/verifiers.md).

## What it does and does not do

- **One node.** SQLite and the in-process notifier assume one process per data directory. On a laptop, with a
  million messages in an inbox, a page read takes under 20 ms at the 95th percentile (worst p95 measured: 11.4 ms),
  and while a million-row retention sweep runs a delivery waits 24 ms or less at the 99th percentile. [Performance](docs/performance.md) says how it was measured.
- **Ready in milliseconds once migrated.** About 10 to 12 MiB of memory idle on Linux. (An upgrade that has migrations to apply to a large database takes longer: [Upgrading](docs/deploying.md#upgrading).) No outbound network calls, no telemetry.
- **Inboxes as a file.** `hookinlet apply -f inboxes.yaml` creates the inboxes a file declares and changes the ones that
  differ. It never deletes anything. See [applying a file](docs/admin.md#applying-a-file).
- **Backups while it runs.** `hookinlet backup --out file.db` takes a consistent snapshot.
- **A console, no users or billing.** The admin API and the command line manage it, and a web console at `/admin` does the same with a browser (below). There is one admin token and no accounts.
- **Not a delivery service for third parties.** It guarantees what it stored and its order. What an agent or relay
  does next is yours.

## Documentation

| | |
|---|---|
| [**Deploying**](docs/deploying.md) | What it needs, every setting, health checks, upgrades, resource use |
| [**Administration**](docs/admin.md) | Inboxes, tokens, secrets, retention, backups, the command line, and [inboxes declared in a file](docs/admin.md#applying-a-file) |
| [**API**](docs/api.md) | Ingest, reading, paging, the live stream |
| [**OpenAPI**](docs/openapi.json) | The same, machine-readable (OpenAPI 3.1), checked against the server by tests |
| [**MCP**](docs/mcp.md) | The tools, their limits, and how to connect clients |
| [**Verifiers**](docs/verifiers.md) | Stripe, GitHub, Slack, Standard Webhooks, generic HMAC, shared token |
| [**Troubleshooting**](docs/troubleshooting.md) | A symptom, its cause, and the fix |
| [**Stability**](docs/stability.md) | What may change, how a change is announced, cursors and ids, CORS and TLS |
| [**Extending**](docs/extending.md) | Embedding the server; replacing the store, notifier, auth, or secrets |
| [**Performance**](docs/performance.md) | What was measured, on what, and how to repeat it |

## hookinlet Cloud

A hosted version is coming. [Join the waitlist](https://hookinlet.com/#cloud): tell us your volume and what you'd
pay. It will be built on this code through the same public interfaces described in
[Extending](docs/extending.md), and this repository stays complete and useful on its own.

## Status

Pre-1.0. The HTTP API and the command line are what the documentation says. The Go packages under `pkg/` can
still change; [CHANGELOG.md](CHANGELOG.md) lists every change to them, and after v1.0.0 semantic versioning applies.

Bug reports, questions, and ideas are welcome as issues, under the [code of conduct](CODE_OF_CONDUCT.md). Code contributions are by invitation for now; see
[CONTRIBUTING.md](CONTRIBUTING.md). To report a vulnerability, see [SECURITY.md](SECURITY.md) and please do not
open a public issue.

## License

[AGPL-3.0](LICENSE). If you run a modified hookinlet as a network service, you share your changes.

**Using it in your own program.** Running hookinlet unmodified, alongside your software, and calling its API from
any language puts no condition on your code. Importing its packages (`pkg/server`, `pkg/inbox`, and the rest) into
a program that you then make available over a network makes that program a work based on hookinlet, and the AGPL
asks you to offer its source to the people who use it. If that does not suit your program, run hookinlet as a
separate service and use its API. The example clients in [`examples/`](examples) (curl, Python, Go, and the relay) are
MIT licensed, so you can copy them into your own code; the one example that embeds the server's packages stays AGPL, because a program built from it is covered by the AGPL. [`LICENSING.md`](LICENSING.md) says in plain words what needs
nothing from you and what does, and how to ask for a commercial license (**licensing@hookinlet.com**) if the AGPL
does not fit. This is a summary, not legal advice. Third-party licenses for what is inside the binary are in
[`THIRD_PARTY_LICENSES`](THIRD_PARTY_LICENSES).
