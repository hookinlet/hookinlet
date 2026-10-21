# Verifying deliveries

Anyone who knows an inbox's ingest URL can send it a request. A **verifier**
tells a real delivery from a forged one. It checks a signature (or a shared
token) against the exact bytes that arrived. It uses a secret that only the
sender and the inbox know.

Each inbox has:

| Setting | Meaning |
|---|---|
| `verifier` | Which scheme to use (the table below). |
| `verifier_config` | Settings that are not secret, as a JSON object. An unknown key is an error, so a typo does not slip by. |
| verifier secret | The shared secret. It is stored encrypted with the master key. No API returns it. |
| `on_verify_fail` | What to do with a delivery that fails. `reject` is the default: answer 401 and store nothing. `store` keeps it, marked unverified, and answers 200. See [When verification fails](#when-verification-fails). |

## The presets

| Verifier | Sender | What is signed | Timestamp | Delivery id | Event type |
|---|---|---|---|---|---|
| `stripe` | Stripe | `<t>.<body>` | yes, in the header | `id` in the body | `type` in the body |
| `github` | GitHub | the body | no | `X-GitHub-Delivery` header (**not signed**) | `X-GitHub-Event` header (**not signed**) |
| `slack` | Slack | `v0:<timestamp>:<body>` | yes | `event_id` in the body | `event.type` or `type` in the body |
| `standardwebhooks` | [Standard Webhooks](https://www.standardwebhooks.com/), Svix, and compatible senders | `<id>.<timestamp>.<body>` | yes | `webhook-id` header (signed) | `type` in the body |
| `hmac` | anything that signs with an HMAC | configurable | optional | optional header (**never signed**) | from the inbox's `event_type_pointer`, for verified deliveries only |
| `token` | anything that can send a header | not a signature: the header must equal the secret | no | none | from `event_type_pointer` |
| `none` | anyone | nothing | no | none | none |

Every comparison of a signature, token, or secret takes the same time whatever the input, so timing leaks nothing.

### Common rules

- The signature covers the **raw bytes** of the body. The server does not parse
  or re-encode them first. If a proxy rewrites the body after it was signed, the
  check fails. That is the right result.
- A header that carries a signature, timestamp, or token must appear **exactly
  once**. The server rejects two copies, because a proxy and the origin could
  disagree about which one counts.
- The server checks the signature **before** the timestamp. A forged request
  learns nothing about the tolerance window.
- A timestamp must be within **5 minutes** of the server's clock, in either
  direction. Stripe, Slack, Standard Webhooks, and generic HMAC inboxes with a
  timestamp accept `tolerance_seconds` (1 to 86,400) to change this.
- A timestamp must be a plain decimal number with no leading zeros. A signature
  must use its one standard encoding (base64 with correct padding and zeroed
  trailing bits). If a signature or timestamp could be written several ways, an
  attacker could shift characters between the timestamp and the body without
  changing what was signed.
- If a verifier needs a secret and has none, that is a **server-side
  configuration error**. The sender gets a 500. It does not count as a failed
  signature, so a setup mistake is never mistaken for an attack. `on_verify_fail=store`
  does not hide it either.

### `stripe`

Header: `Stripe-Signature: t=<unix>,v1=<hex>[,v1=<hex>...]`. The secret is the
endpoint's signing secret (`whsec_...`), used exactly as written. One matching
`v1` is enough. This lets Stripe rotate secrets.

Config: `{"tolerance_seconds": 300}` (optional).

### `github`

Header: `X-Hub-Signature-256: sha256=<hex>`. The secret is the webhook secret.
The legacy `X-Hub-Signature` (SHA-1) header is not used.

> **GitHub signs only the body.** `X-GitHub-Delivery` and `X-GitHub-Event` are
> extra data, not proof. Someone who holds one captured, validly signed body can
> replay it at any time. They can use any delivery id and any event type, and
> the replay is stored as **verified**. GitHub sends no timestamp, so a receiver
> cannot stop this. Use the delivery id only to drop retries. It does not
> protect against replays, and hookinlet does not treat it that way: see
> [duplicates](#duplicate-deliveries). Do not let anything with real
> consequences (a deploy, a payment) act on the event type alone.

### `slack`

Headers: `X-Slack-Request-Timestamp` and `X-Slack-Signature: v0=<hex>`. The
secret is the app's signing secret.

When Slack sends a `url_verification` request, hookinlet answers with the
challenge as plain text. It does this **after** the signature checks out, and it
does not store the request. A forged handshake gets a 401 and no challenge. The
challenge goes back as `text/plain` with `X-Content-Type-Options: nosniff`. If
it is longer than 256 characters, hookinlet does not echo it, and stores the
request like any other.

Config: `{"tolerance_seconds": 300}` (optional).

### `standardwebhooks`

Headers: `webhook-id`, `webhook-timestamp`, and `webhook-signature`, which holds
one or more space-separated `v1,<base64>` values. The secret is written
`whsec_<base64>`. hookinlet decodes the base64 part to get the key. It ignores
signature versions other than `v1`. It rejects a `webhook-id` that contains a
`.`. The signed text is `<id>.<timestamp>.<body>`, and a dot in the id would let
the same bytes be read as a different split.

Config: `{"tolerance_seconds": 300}` (optional).

### `hmac` (generic)

For senders that sign with an HMAC but are not a preset.

| Key | Meaning | Default |
|---|---|---|
| `header` | Header carrying the signature. **Required.** | |
| `algorithm` | `sha1`, `sha256`, `sha384`, or `sha512`. | `sha256` |
| `encoding` | `hex`, `base64`, or `base64url` (padding optional). | `hex` |
| `prefix` | Literal text before the signature, such as `sha256=`. It must be present. | none |
| `timestamp_header` | Header with a Unix timestamp in seconds. Enables the tolerance check. | none |
| `payload_template` | What is signed. Use `{body}` (exactly once) and `{timestamp}`. Any other brace is an error. With `timestamp_header` it **must** include `{timestamp}`, and at least one character that is not a digit must separate `{timestamp}` and `{body}`. | `{body}`, or `{timestamp}.{body}` with a timestamp header |
| `tolerance_seconds` | Only with `timestamp_header`. | 300 |
| `delivery_id_header` | Header whose value identifies retries so they can be dropped. The signature never covers it, so use it only for that. | none |

The template is parsed once, when the inbox loads. Text in a delivery that
looks like `{timestamp}` is never expanded. The server rejects a configuration
at load time, with an explanation, if it would make the signed bytes unclear or
would check a timestamp the signature does not cover.

Examples:

```json
{"header": "X-Shopify-Hmac-Sha256", "encoding": "base64"}
```

```json
{"header": "X-Hub-Signature", "algorithm": "sha1", "prefix": "sha1="}
```

```json
{"header": "X-Signature", "timestamp_header": "X-Timestamp",
 "payload_template": "v0:{timestamp}:{body}", "tolerance_seconds": 120}
```

The inbox's `GET /v1/inboxes/{slug}` and MCP `list_inboxes` give the name of the
stored header that carries the signature as `signature_headers`. They give names
only, never values. An example is `["X-Signature"]`. A client that forwards stored
headers can use this to leave that header out. The presets list none, because
the sender fixes their header.

SHA-1 is here only because some senders offer nothing else. HMAC-SHA-1 is
still a sound way to sign a message. Pick SHA-256 when you have a choice.

Not supported: schemes that sign the request URL or sorted form fields (such as
Twilio's), and signatures made with a key pair (such as Ed25519).

### `token`

The sender puts the shared secret in a header. This proves who sent the request.
It does not prove the body is unchanged. Anyone who sees one request can replay
the token. Use it only over TLS, and only for senders that offer nothing better.
hookinlet never stores the header with the delivery.

Config: `{"header": "X-Inbox-Token"}` (optional; this is the default).

### `none`

Accepts every delivery and says so. It stores each one with `verified: false`.
It trusts nothing the sender claims: no delivery id, no event type, no
deduplication. Use it only for an inbox that should take anything, or while you
wire up a sender and want to see what it sends. You must ask for it by name.
Creating an inbox without a verifier is an error.

## When verification fails

| `on_verify_fail` | Result |
|---|---|
| `reject` (default) | `401` with `{"error": {"code": "verification_failed", "message": "webhook verification failed"}}`. Nothing is stored. |
| `store` | The delivery is stored with `verified: false` and a `verify_error`, and the sender gets `200` (so it stops retrying). |

The sender is told only that verification failed. The exact reason is a short
fixed phrase, such as `signature mismatch`, `timestamp outside tolerance`, or
`missing X-Hub-Signature-256 header`. It goes to the server log. In `store`
mode it also goes to the message's `verify_error`. It stays out of the response
because it can name the header and scheme the inbox expects. Whoever holds the
ingest key should not learn that. The phrase never contains the signature, the
secret, or any part of the request.

An unverified delivery is stored as received, but the server **trusts nothing
the sender claimed**. Its delivery id and event type are left empty, and the
inbox's `event_type_pointer` is not applied to it. This matters for two
reasons:

- Deduplication looks only at *verified* deliveries. Otherwise a forged request
  that reuses a real delivery id could make the real delivery get dropped as a
  duplicate.
- Agents that read the inbox can use the `verified` filter to tell verified from
  unverified. They should treat unverified content as untrusted input.

> **`store` mode has a cost.** Anyone who knows the ingest key can store
> unverified messages, so the server caps them (below). Keep the default `reject`
> unless you are debugging a sender. Use a `BeforeIngest` hook to rate limit.

### Unverified messages are capped

An inbox keeps at most `HOOKINLET_MAX_UNVERIFIED` unverified messages. These are
the ones stored by `on_verify_fail=store`, and everything an inbox with the
verifier `none` accepts. The default is 1000. `0` turns the cap off. Every inbox
has the same cap. It protects the disk and the genuine messages. Without it, a
leaked ingest key could fill an inbox and, under `retention_max_count`, push the
verified messages out.

- If a delivery would be stored unverified and would take the inbox over the
  cap, the server answers **`429 rate_limited`**. It does not store the delivery
  or acknowledge it. A sender that retries delivers it once there is room. The
  server never evicts stored messages to make room.
- The cap never refuses a **verified** delivery, however many unverified messages
  the inbox holds. An unverified message never removes a verified one.
- Room comes back when retention deletes unverified messages. Give an inbox that
  stores unverified messages a `retention_days` (or `retention_max_count`).
  Without retention, a full inbox stays full until messages are deleted or you
  raise the cap.
- Under `retention_max_count` N, unverified messages still take slots among the
  newest N, but never more than the cap. When the inbox is full, at least N
  minus the cap of them are verified. Keep N well above the cap.
- The server counts and appends under a lock that covers that inbox alone.
  Concurrent deliveries to the inbox cannot overshoot the cap in one server, and
  deliveries to other inboxes do not wait. With several servers on one shared
  database, the cap is approximate.
- An embedder can set the cap per inbox (for instance by plan). Do this with
  Hooks that implement the optional `inbox.UnverifiedLimiter`
  ([extending](extending.md)).
- The built-in SQLite store supports the cap. A Store from elsewhere has to
  implement the optional `inbox.UnverifiedCounter`. Without it, the server logs a
  warning at start and sets no cap ([extending](extending.md)).

## Trying it out

Create an inbox. The command reads the verifier secret from the environment, not
from a flag, because flags show up in process listings. It writes into the data
directory. Run it in the shell where you started the server in the
[quickstart](quickstart.md#send-a-signed-event-read-it-back), because that shell
has the data directory and master key. The first line of the block keeps the key
that is already set and makes a new one only if none is. A different key would
seal the secret so that the running server cannot open it:

<!-- exercise -->
```sh
export HOOKINLET_MASTER_KEY=${HOOKINLET_MASTER_KEY:-$(openssl rand -base64 32)}   # keep this; it encrypts stored secrets
HOOKINLET_VERIFIER_SECRET='my-shared-secret' \
  hookinlet inbox create --slug demo --ingest-key demo --verifier hmac \
  --verifier-config '{"header":"X-Signature","prefix":"sha256="}'
```

Send a signed request:

<!-- exercise-server -->
```sh
body='{"hello":"world"}'
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac 'my-shared-secret' -hex | sed 's/^.* //')
curl -i -X POST http://localhost:8080/in/demo \
  -H "X-Signature: sha256=$sig" -d "$body"
# HTTP/1.1 200 OK
# {"seq":1}
```

Change one character of `$body` and send the same signature. You get a `401`.

## Trying it with Stripe

Stripe's CLI forwards test events with a real signature. This is the same loop
against Stripe. It needs a Stripe account, so the tests do not run it:

```sh
# the signing secret Stripe will use, whsec_…
secret=$(stripe listen --print-secret)

# create the inbox with that secret
curl -s -X POST localhost:8080/v1/admin/inboxes \
  -H "Authorization: Bearer $HOOKINLET_ADMIN_TOKEN" -H 'Content-Type: application/json' -d @- > created.json <<EOF
{"slug": "stripe", "verifier": "stripe", "verifier_secret": "$secret"}
EOF
# keep the agent token in HOOKINLET_AGENT_TOKEN, as in the README, and forward Stripe's test events to the inbox
INGEST=$(sed -n 's/.*"ingest_path":"\([^"]*\)".*/\1/p' created.json)
stripe listen --forward-to localhost:8080$INGEST &

# send some events, then read the inbox
stripe trigger invoice.paid
curl -s 'localhost:8080/v1/inboxes/stripe/messages?after=0&fields=summary' \
  -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN" | jq -c '.messages[] | {seq, event_type, verified}'
```

```
{"seq":1,"event_type":"customer.created","verified":true}
…
{"seq":5,"event_type":"invoice.paid","verified":true}
```

### Verified is not unmodified

The server checks the signature against the bytes that arrived, before it does
anything else with them. An embedder can give the server a body screener (see
[extending](extending.md#redacting-or-checking-a-body)). The screener may then
store a replacement, such as the body with a card number removed. That message
is stored with `verified: true`, because the delivery did verify. It also gets
`modified: true`, because what is stored is not what was signed. Checking the
signature against the stored body fails. The deduplication below goes by the
bytes that arrived, so a redelivery still collapses. A screener can also refuse
a verified delivery for its content. That has a cost. Many senders retry a
delivery that got a `4xx` or `5xx`, and some disable an endpoint after repeated
failures. A refusal on content can end with the webhook switched off.

## Duplicate deliveries

A verified delivery with a delivery id is a duplicate of an earlier one when the
earlier one has the same id **and** the signature covers that id (`stripe`,
`slack`, `standardwebhooks`). The server answers a duplicate with the earlier
`seq` and does not store it again.

For `github` and `hmac`, the id comes from a header the signature does not cover.
The id alone proves nothing. Whoever captured one signed request could send it
again with an id of their choosing. They could also claim the ids of deliveries
that have not happened yet, and the genuine ones would look like duplicates and
be dropped. So for these, a duplicate is the same id **and** the same body. The
same id with a different body is stored as a new message and keeps its id.

## Secrets are checked when they are set

The server refuses a secret the verifier cannot use when you set it. You do not
find out at the first delivery, which a sender such as GitHub does not retry. For
example, a Standard Webhooks secret has to be `whsec_` followed by base64.
Changing an inbox's verifier removes its secret. A secret set for one scheme
must not become the credential of another. The answer says so, and warns that
the new verifier needs a secret.

## Adding your own verifier

An application that embeds the server can register more schemes:

```go
reg := verify.Default()
err := reg.Register("acme-v2", func(config json.RawMessage) (verify.Verifier, error) {
    // parse config; return an error that wraps verify.ErrConfig if it is invalid
    return acmeVerifier{}, nil
})
srv, _ := server.New(server.Options{ /* ... */ Verifiers: reg })
```

A `Verifier` returns an error that satisfies `verify.IsFailure` when a delivery
does not verify. It returns an error that wraps `verify.ErrConfig` when the
verifier cannot run at all. If its credential travels in a header, it should
also implement `verify.HeaderRedactor`, so the server never stores that header.
Build failures with `verify.Failure{Kind: ...}`. Use `verify.FailureMissing`,
`FailureMalformed`, `FailureMismatch` or `FailureStale`, so an
`inbox.IngestObserver` can tell them apart. An empty `Kind` is allowed. If a
verifier's signature header is set per inbox, it can implement
`verify.SignatureHeaderer` to show the header's name in the inbox's metadata.
Compare secrets and signatures with `hmac.Equal` or `crypto/subtle`. Never use
`==` or `bytes.Equal`.
