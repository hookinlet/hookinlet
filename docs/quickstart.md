# Quickstart

Run it, send it a signed event, read it back. This is the [README](../README.md#quickstart)'s quickstart step by
step: without Docker, with a forged event refused, and with a real sender at the end. You need `curl` and
`openssl`; the server needs nothing.

Two secrets configure a server, and you must keep both. The admin token manages it. The master key encrypts the
signing secrets it stores, so a server started later with a different key cannot read them.

## Get the binary

Download one from the [releases page](https://github.com/hookinlet/hookinlet/releases) (Linux and macOS, amd64 and
arm64, static), or build it from a clone:

```sh
go build -o hookinlet ./cmd/hookinlet     # then put ./hookinlet on your PATH
```

## Send a signed event, read it back

This walk plays the part of GitHub with `openssl`, so there is nothing to sign up for. Run it from a clone, with
`hookinlet` on your `PATH`. Every command is run by the project's tests, in this order, in one shell.

Start a server. If you started the Docker container from the README and have not run its other commands yet, skip
this block and keep the shell where you exported the two secrets: the walk talks to whichever server answers on port 8080. This one uses a throwaway data
directory and listens on this machine only:

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
belongs in a file or a password manager, never in a command line.)

The response carries the **agent token** that reads this inbox, shown once ([more tokens](admin.md#tokens)),
and the **ingest path** that senders post to. The path ends in a random key, not the slug: whoever has it can post
to the inbox ([why](admin.md#why-the-ingest-url-is-not-the-slug)). The admin API shows it again whenever you
ask.

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

Shortened; the [API reference](api.md#messages) lists every field. `body` is your JSON, as JSON. `next` is the
cursor to keep. For the exact bytes that were sent, read `/v1/inboxes/github-demo/messages/1/raw`.

The README's [Read from an agent](../README.md#read-from-an-agent) carries on from here, in the same shell, with
the same inbox and agent token.

When you are done: `kill %1` stops the server this walk started, and `rm created.json` removes the file holding
the agent token.

## With a real sender

For GitHub, Slack, Stripe, or your own service, give the sender the inbox's `ingest_url` (the admin API shows it;
`HOOKINLET_PUBLIC_BASE_URL` sets the host part) and the same secret.

The server must be reachable from the internet for that: put it behind TLS, or use a tunnel to try it from a
laptop. [Verifiers](verifiers.md#trying-it-with-stripe) shows the loop against Stripe's CLI, and
[Deploying](deploying.md) covers what a server needs.
