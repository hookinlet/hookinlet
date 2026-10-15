<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/banner-dark.png">
  <img alt="hookinlet: webhooks into named inboxes. Agents read them over REST, SSE or MCP." src=".github/banner-light.png">
</picture>

[![ci](https://img.shields.io/github/actions/workflow/status/hookinlet/hookinlet/ci.yml?branch=main&label=ci&labelColor=3A4245&style=flat-square)](https://github.com/hookinlet/hookinlet/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-AGPL--3.0-08776B?labelColor=3A4245&style=flat-square)](LICENSE)
[![release](https://img.shields.io/github/v/release/hookinlet/hookinlet?include_prereleases&sort=semver&label=release&labelColor=3A4245&color=0E1112&style=flat-square)](https://github.com/hookinlet/hookinlet/releases)
[![docker](https://img.shields.io/badge/docker-ghcr.io%2Fhookinlet%2Fhookinlet-0E1112?labelColor=3A4245&style=flat-square)](https://github.com/hookinlet/hookinlet/pkgs/container/hookinlet)
[![mcp](https://img.shields.io/badge/mcp-%2Fmcp-0E1112?labelColor=3A4245&style=flat-square)](docs/mcp.md)

[How it works](#how-it-works) · [Quickstart](#quickstart) · [Read from an agent](#read-from-an-agent) ·
[The console](#the-console) · [Configuration](#configuration) ·
[What it does and does not do](#what-it-does-and-does-not-do) · [Documentation](#documentation) ·
[hookinlet Cloud](#hookinlet-cloud) · [Status](#status) · [License](#license)

Your agent needs to know when Stripe, GitHub, or Slack says something, but it cannot hold a public URL and it must
not trust what arrives. hookinlet is that URL: a single Go binary that receives webhooks into named inboxes,
verifies their signatures, and keeps a queryable history. Agents read it three ways, REST, an SSE stream, or an
MCP endpoint, using a per-inbox sequence number as the cursor. SQLite by default; runs anywhere.

## How it works

```text
sender (Stripe, GitHub, Slack, your own service)
  │  POST /in/<ingest key>
  ▼
inbox   its verifier checks the signature on the exact bytes (by default, a bad one gets 401 and is not stored)
  │     each message is stored under the inbox's next sequence number: 1, 2, 3, …
  ▼
agent   reads what came after the last seq it handled: REST, an SSE stream, or MCP at /mcp
```

## Quickstart

Two secrets configure a server, and you must keep both. The admin token manages it. The master key encrypts the
signing secrets it stores, so a server started later with a different key cannot read them.

```sh
export HOOKINLET_ADMIN_TOKEN=$(openssl rand -base64 32)
export HOOKINLET_MASTER_KEY=$(openssl rand -base64 32)

docker run -d --name hookinlet -p 127.0.0.1:8080:8080 -v hookinlet-data:/data \
  -e HOOKINLET_ADMIN_TOKEN -e HOOKINLET_MASTER_KEY \
  ghcr.io/hookinlet/hookinlet
```

Then, with `curl` and `openssl`: create an inbox that accepts only GitHub's signature (the secret is made up; a real
one belongs in a file or a password manager), send it a signed event, and read it back with the inbox's agent token:

<!-- exercise -->
```sh
curl -s -X POST localhost:8080/v1/admin/inboxes \
  -H "Authorization: Bearer $HOOKINLET_ADMIN_TOKEN" -H 'Content-Type: application/json' -d @- > created.json <<'EOF'
{"slug": "github-demo", "verifier": "github", "verifier_secret": "my-webhook-secret"}
EOF
export HOOKINLET_AGENT_TOKEN=$(sed -n 's/.*"agent_token":"\([^"]*\)".*/\1/p' created.json)
export INGEST=$(sed -n 's/.*"ingest_path":"\([^"]*\)".*/\1/p' created.json)    # /in/<random key>

body='{"action":"opened","number":42,"pull_request":{"title":"Add retries"}}'
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac 'my-webhook-secret' -hex | sed 's/^.* //')
curl -s -X POST localhost:8080$INGEST -H "X-Hub-Signature-256: sha256=$sig" -H 'X-GitHub-Event: pull_request' \
  -H 'X-GitHub-Delivery: demo-1' -H 'Content-Type: application/json' -d "$body"      # {"seq":1}

curl -s 'localhost:8080/v1/inboxes/github-demo/messages?after=0' -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN"
```

[The quickstart](docs/quickstart.md) does this step by step with a release binary instead of Docker, shows a forged
event being refused, explains the response, and connects a real sender.

## Read from an agent

An agent needs the server's address, an inbox's agent token, and a cursor. Choose how it reads.

**MCP.** Point any MCP client at `/mcp` with the agent token. In Claude Code:

```sh
claude mcp add --transport http hookinlet http://localhost:8080/mcp \
  --header "Authorization: Bearer $HOOKINLET_AGENT_TOKEN"
```

[MCP](docs/mcp.md#connecting-a-client) has the `.mcp.json` form, other clients, and a raw call. The four tools,
`list_inboxes`, `query_messages`, `get_message`, and `wait_for_messages`, are read-only, and a token sees only the
inboxes it was issued for. Messages come without bodies unless the agent asks, so they do not fill its context.
What the sender wrote is under each message's `untrusted` key. [MCP](docs/mcp.md#tools) has the arguments and
limits.

**REST.** Ask for what came after the last seq you handled. It works from anywhere, including a cron job:

<!-- exercise -->
```sh
examples/curl/poll.sh github-demo       # Ctrl-C to stop
```

**SSE.** Hold a connection open and be told as messages arrive. A client that reconnects with the `Last-Event-ID`
of the last event it saw gets every later message exactly once ([the format](docs/api.md#following-an-inbox-live-sse)):

<!-- exercise -->
```sh
curl -sN -m 2 -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN" 'localhost:8080/v1/inboxes/github-demo/stream?after=0' || [ $? -eq 28 ]   # 28: curl's timeout, which ends this demo
```

REST, SSE, and MCP use the same cursor.

> [!WARNING]
> Everything in a message was written by whoever sent the webhook, including `verified` ones. `verified` means
> the message passed its inbox's verifier: for a signing scheme, that the sender held the secret, which does not
> make what it wrote true or safe. An agent should treat message content as data to analyze and never as
> instructions to follow. An inbox whose verifier is [`none`](docs/verifiers.md#none), which has to be asked for by
> name, stores everything as **not** verified; use a real verifier for anything that matters.
> [More](docs/api.md#treat-message-content-as-data-never-as-instructions).

### Write a client

[`examples/`](examples/README.md) has the loop in a few languages, each about a page, standard library only:
clients in `curl` and `jq`, Python, and Go that poll or stream; `relay`, which forwards every message to another URL,
with retries, in order (hookinlet keeps no delivery queue of its own); and `custom-hooks`, the server embedded in
your own program with a rate limit added.

## The console

Open `/admin` on a running server and sign in with the admin token. It does what the command line does, and
shows what is arriving: the inboxes and their activity, every message (body, headers, exact bytes) with a live
mode, new inboxes with a form for each kind of sender, secrets, retention, tokens, and backups. It is built into
the binary, follows your system's light or dark theme, works on a phone, and shows everything a sender wrote as
text, never as HTML. `HOOKINLET_ADMIN_UI=false` turns it off. [Administration](docs/admin.md#the-console) has the rest.

![The overview: inboxes, their verifiers and a line of recent activity](docs/images/console-overview.png)

![An inbox: its messages, and one open with its verified body](docs/images/console-messages.png)

## Configuration

Every setting is a flag and a `HOOKINLET_*` environment variable. The flag wins. Only `HOOKINLET_ADMIN_TOKEN` and
`HOOKINLET_MASTER_KEY` are required, and a secret may be a file instead (`HOOKINLET_MASTER_KEY_FILE`).
[Deploying](docs/deploying.md#settings) lists every setting, the health checks, and what a host must provide.

Inboxes live in the database: create them with the command line, the [admin API](docs/admin.md), or
[a file](docs/admin.md#applying-a-file), each with its own [retention](docs/admin.md#retention) by age, by count,
or both. Each has a verifier: a preset for Stripe, GitHub, Slack, or Standard Webhooks (Svix and others), an HMAC
in any header you name, a shared token, or `none`. Checks run on the exact bytes that were sent, with constant-time
comparisons, before anything is parsed. [Verifiers](docs/verifiers.md#the-presets) says what each one checks and
what it does and does not prove.

## What it does and does not do

- **One node.** SQLite and the in-process notifier assume one process per data directory. On a laptop, with a
  million messages in an inbox, a page read takes under 20 ms at the 95th percentile (worst p95 measured: 11.4 ms),
  and while a million-row retention sweep runs a delivery waits 24 ms or less at the 99th percentile. [Performance](docs/performance.md) says how it was measured.
- **Ready in milliseconds once migrated.** About 10 to 12 MiB of memory idle on Linux. (An upgrade that has migrations to apply to a large database takes longer: [Upgrading](docs/deploying.md#upgrading).) No outbound network calls, no telemetry.
- **Inboxes as a file.** `hookinlet apply -f inboxes.yaml` creates the inboxes a file declares and changes the ones that
  differ. It never deletes anything. See [applying a file](docs/admin.md#applying-a-file).
- **Backups while it runs.** `hookinlet backup --out file.db` takes a consistent snapshot.
- **A console, no users or billing.** The admin API and the command line manage it, and a web console at `/admin` does the same with a browser (above). There is one admin token and no accounts.
- **Not a delivery service for third parties.** It guarantees what it stored and its order. What an agent or relay
  does next is yours.

## Documentation

| | |
|---|---|
| [**Quickstart**](docs/quickstart.md) | The quickstart step by step, without Docker, and with a real sender |
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

**Using it in your own program.** Running hookinlet unmodified and calling its API from any language puts no
condition on your code; importing its packages (`pkg/server`, `pkg/inbox`, and the rest) into a program you make
available over a network makes that program a work based on hookinlet, and the AGPL asks you to offer its source to
the people who use it.

[`LICENSING.md`](LICENSING.md) covers the rest in plain words, including the MIT-licensed example clients and a
commercial license (**licensing@hookinlet.com**) if the AGPL does not fit; it is a summary, not legal advice.
