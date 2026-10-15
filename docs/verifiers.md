# Verifying deliveries

Anyone who knows an inbox's ingest URL can send it a request. A **verifier**
is what separates a genuine delivery from a forged one: it checks a signature
(or a shared token) against the exact bytes that arrived, using a secret only
the sender and the inbox know.

Each inbox has:

| Setting | Meaning |
|---|---|
| `verifier` | Which scheme to use (the table below). |
| `verifier_config` | Non-secret parameters, as a JSON object. Unknown keys are an error, so a typo is caught instead of silently ignored. |
| verifier secret | The shared secret. Stored encrypted with the master key; never returned by any API. |
| `on_verify_fail` | What to do with a delivery that fails: `reject` (answer 401, store nothing) or `store` (keep it, marked unverified). |

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

Every comparison of a signature, token, or secret is constant-time.

### Common rules

- The signature covers the **raw bytes** of the body. Nothing is parsed or
  re-encoded first, so a signature computed over a body that a proxy later
  rewrote will fail, correctly.
- A header that carries a signature, timestamp, or token must appear **exactly
  once**. Two copies are rejected, because a proxy and the origin could disagree
  about which one counts.
- The signature is checked **before** the timestamp, so a forged request learns
  nothing about the tolerance window.
- Timestamps must be within **5 minutes** of the server's clock, in either
  direction. Stripe, Slack, Standard Webhooks, and generic HMAC inboxes with a
  timestamp accept `tolerance_seconds` (1 to 86,400) to change this.
- Timestamps must be written as plain decimal numbers with no leading zeros,
  and signatures in their one canonical encoding (base64 with correct padding
  and zeroed trailing bits). If a signature or timestamp could be written
  several ways, an attacker could shift characters between a timestamp and the
  body without changing what was signed.
- A verifier that needs a secret and finds none is a **server-side
  configuration error** (the sender gets a 500), not a failed signature, so a
  mistake in setup is never mistaken for an attack, or hidden by
  `on_verify_fail=store`.

### `stripe`

Header: `Stripe-Signature: t=<unix>,v1=<hex>[,v1=<hex>...]`. The secret is the
endpoint's signing secret (`whsec_...`), used exactly as written. Any one
matching `v1` is enough, which lets Stripe rotate secrets.

Config: `{"tolerance_seconds": 300}` (optional).

### `github`

Header: `X-Hub-Signature-256: sha256=<hex>`. The secret is the webhook secret.
The legacy `X-Hub-Signature` (SHA-1) header is not used.

> **GitHub signs only the body.** `X-GitHub-Delivery` and `X-GitHub-Event` are
> metadata, not proof. Someone holding one captured, validly signed body can
> replay it at any time, under any delivery id and with any event type, and the
> replay is stored as **verified**. GitHub provides no timestamp that would let
> a receiver stop that. Treat the delivery id as a retry de-duplicator, not as
> replay protection (and hookinlet does not trust it to be one: see
> [duplicates](#duplicate-deliveries)), and do not let anything with consequences (a deploy, a
> payment) act on the event type alone.

### `slack`

Headers: `X-Slack-Request-Timestamp` and `X-Slack-Signature: v0=<hex>`. The
secret is the app's signing secret.

When Slack sends a `url_verification` request, hookinlet answers it with the
challenge (as plain text) **after** the signature checks out, and does not store
it. A forged handshake gets a 401 and no challenge. The challenge is returned
as `text/plain` with `X-Content-Type-Options: nosniff`, and one longer than 256
characters is not echoed at all (the request is stored like any other).

Config: `{"tolerance_seconds": 300}` (optional).

### `standardwebhooks`

Headers: `webhook-id`, `webhook-timestamp`, and `webhook-signature`, which holds
one or more space-separated `v1,<base64>` values. The secret is written
`whsec_<base64>`; hookinlet decodes the base64 part to get the key. Signature
versions other than `v1` are ignored. A `webhook-id` that contains a `.` is
rejected, because the signed text is `<id>.<timestamp>.<body>` and a dot in
the id would let the same bytes be read as a different split.

Config: `{"tolerance_seconds": 300}` (optional).

### `hmac` (generic)

For senders that sign with an HMAC but are not one of the presets.

| Key | Meaning | Default |
|---|---|---|
| `header` | Header carrying the signature. **Required.** | |
| `algorithm` | `sha1`, `sha256`, `sha384`, or `sha512`. | `sha256` |
| `encoding` | `hex`, `base64`, or `base64url` (padding optional). | `hex` |
| `prefix` | Literal text before the signature, such as `sha256=`. It must be present. | none |
| `timestamp_header` | Header with a Unix timestamp in seconds. Enables the tolerance check. | none |
| `payload_template` | What is signed. `{body}` (exactly once) and `{timestamp}`; any other brace is an error. With `timestamp_header` it **must** include `{timestamp}`, and `{timestamp}` and `{body}` must be separated by at least one character that is not a digit. | `{body}`, or `{timestamp}.{body}` with a timestamp header |
| `tolerance_seconds` | Only with `timestamp_header`. | 300 |
| `delivery_id_header` | Header whose value deduplicates retries. It is never authenticated, so it is only a retry de-duplicator. | none |

The template is parsed once when the inbox is loaded, so text in a delivery that
happens to look like `{timestamp}` is never expanded. A configuration that would
make the signed bytes ambiguous, or that would check a timestamp the signature
does not cover, is rejected when the inbox is loaded, with an explanation.

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

SHA-1 is offered only because some senders offer nothing else. HMAC-SHA-1 is
still a sound message authentication code, but prefer SHA-256 when you can
choose.

Not supported: schemes that sign the request URL or sorted form fields (such as
Twilio's) and asymmetric signatures (such as Ed25519).

### `token`

The sender puts the shared secret in a header. This is **authentication, not
integrity**: anyone who observes one request can replay the token. Use it only
over TLS and only for senders that offer nothing better. The header is never
stored with the delivery.

Config: `{"header": "X-Inbox-Token"}` (optional; that is the default).

### `none`

Accepts every delivery, and says so: what it accepts is stored with
`verified: false`, and nothing the sender claimed is trusted (no delivery id, no
event type, no deduplication). Use it only for an inbox that deliberately takes
anything, or while you are wiring up a sender and want to see what it sends. It
has to be asked for by name: creating an inbox without a verifier is an error.

## When verification fails

| `on_verify_fail` | Result |
|---|---|
| `reject` (default) | `401` with `{"error": {"code": "verification_failed", "message": "webhook verification failed"}}`. Nothing is stored. |
| `store` | The delivery is stored with `verified: false` and a `verify_error`, and the sender gets `200` (so it stops retrying). |

The sender is told only that verification failed. The specific reason is a short
fixed phrase (`signature mismatch`, `timestamp outside tolerance`, `missing
X-Hub-Signature-256 header`, ...) that goes to the server log and, in `store`
mode, to the message's `verify_error`. It is kept out of the response because it
can name the header and scheme an inbox expects, which is not for whoever
happens to hold the ingest key. It never contains the signature, the secret, or
any part of the request.

An unverified delivery is stored as received, but **nothing the sender claimed
is trusted**: its delivery id and event type are left empty (the inbox's
`event_type_pointer` is not applied to it either). This matters for two
reasons:

- Deduplication only considers *verified* deliveries. Otherwise a forged request
  that reuses a real delivery id could cause the genuine delivery to be dropped
  as a duplicate.
- Agents reading the inbox can tell verified from unverified with the
  `verified` filter and should treat unverified content as untrusted input.

> **`store` mode has a cost.** Anyone who knows the ingest key can store
> unverified messages, so they are capped (below). Keep the default `reject`
> unless you are debugging a sender, and use a `BeforeIngest` hook to rate limit.

### Unverified messages are capped

An inbox keeps at most `HOOKINLET_MAX_UNVERIFIED` unverified messages (those
stored by `on_verify_fail=store`, and everything an inbox with the verifier
`none` accepts). The default is 1000, `0` turns the cap off, and it is the same
for every inbox. It protects the disk, and the genuine messages: without it, a
leaked ingest key could fill an inbox and, under `retention_max_count`, push the
verified messages out.

- A delivery that would be stored unverified, and would take the inbox over the
  cap, is answered **`429 rate_limited`**. It is not stored and not acknowledged,
  so a sender that retries delivers it once there is room. Nothing already stored
  is evicted to make room for it.
- A **verified** delivery is never refused for the cap, however many unverified
  messages the inbox holds, and an unverified message never removes a verified one.
- Room comes back when retention deletes unverified messages, so give an inbox
  that stores unverified messages a `retention_days` (or `retention_max_count`);
  without retention a full inbox stays full until messages are deleted or the cap
  is raised.
- Under `retention_max_count` N, unverified messages still take slots among the
  newest N, but never more than the cap, so at least N minus the cap of them are
  verified (when the inbox is full). Keep N well above the cap.
- The count and the append are made under one lock, so concurrent deliveries
  cannot overshoot the cap in one server. With several servers on one shared
  database it is approximate.
- An embedder can set the cap per inbox (for instance by plan) with Hooks that
  implement the optional `inbox.UnverifiedLimiter` ([extending](extending.md)).
- The built-in SQLite store supports the cap. A Store from elsewhere has to
  implement the optional `inbox.UnverifiedCounter`; without it the server logs a
  warning at start and does not cap ([extending](extending.md)).

## Trying it out

Create an inbox (the verifier secret is read from the environment, never from a
flag, because flags show up in process listings). This writes into the data directory,
so run it in the shell where you started the server in the [README](../README.md#send-a-signed-event-read-it-back),
which has its data directory and master key. The first line of the block keeps the key that is set
and makes one only when none is (a different key would seal the secret so that the
running server cannot open it):

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

Change one character of `$body` and send the same signature: you get a `401`.

## Trying it with Stripe

Stripe's CLI forwards test events with a real signature. This is the same loop against Stripe (it needs a Stripe
account, so it is shown, not run by the tests):

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

## Duplicate deliveries

A verified delivery with a delivery id is a duplicate of an earlier one, and is
answered with the earlier `seq` and not stored again, when the earlier one has the
same id **and** the id is one the signature covers (`stripe`, `slack`,
`standardwebhooks`).

For `github` and `hmac` the id comes from a header the signature does not cover, so
the id alone proves nothing: whoever captured one signed request could send it
again with an id of their choosing, or claim the ids of deliveries that have not
happened yet, and the genuine ones would be taken for duplicates and dropped. For
those, a duplicate is the same id **and** the same body. The same id with another
body is stored as a new message, and keeps its id.

## Secrets are checked when they are set

A secret the verifier cannot use is refused when you set it, not discovered at the
first delivery (which a sender such as GitHub does not retry): a Standard Webhooks
secret has to be `whsec_` followed by base64. Changing an inbox's verifier removes
its secret, because a secret set for one scheme must not become a credential of
another; the answer says so, and warns that the new verifier needs one.

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

A `Verifier` returns an error satisfying `verify.IsFailure` when a delivery does
not verify, and an error wrapping `verify.ErrConfig` when the verifier cannot
run at all. If its credential travels in a header, it should also implement
`verify.HeaderRedactor` so that header is never stored. Compare secrets and
signatures with `hmac.Equal` or `crypto/subtle`, never with `==` or
`bytes.Equal`.
