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
| `hmac` | anything that signs with an HMAC | configurable | optional | optional header (**not signed** unless you put it in the template) | from the inbox's `event_type_pointer` |
| `token` | anything that can send a header | not a signature: the header must equal the secret | no | none | from `event_type_pointer` |
| `none` | anyone | nothing | no | none | from `event_type_pointer` |

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
> metadata, not proof. Someone holding one captured, validly signed body could
> replay it under a new delivery id, and GitHub provides no timestamp that
> would let a receiver stop that. Treat the delivery id as a retry
> de-duplicator, not as replay protection.

### `slack`

Headers: `X-Slack-Request-Timestamp` and `X-Slack-Signature: v0=<hex>`. The
secret is the app's signing secret.

When Slack sends a `url_verification` request, hookinlet answers it with the
challenge (as plain text) **after** the signature checks out, and does not store
it. A forged handshake gets a 401 and no challenge.

Config: `{"tolerance_seconds": 300}` (optional).

### `standardwebhooks`

Headers: `webhook-id`, `webhook-timestamp`, and `webhook-signature`, which holds
one or more space-separated `v1,<base64>` values. The secret is written
`whsec_<base64>`; hookinlet decodes the base64 part to get the key. Signature
versions other than `v1` are ignored.

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
| `payload_template` | What is signed. `{body}` (exactly once) and `{timestamp}`; any other brace is an error. | `{body}`, or `{timestamp}.{body}` with a timestamp header |
| `tolerance_seconds` | Only with `timestamp_header`. | 300 |
| `delivery_id_header` | Header whose value deduplicates retries. | none |

The template is parsed once when the inbox is loaded, so text in a delivery that
happens to look like `{timestamp}` is never expanded.

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

Accepts every delivery. Use it only for an inbox that deliberately takes
anything, or while you are wiring up a sender and want to see what it sends.

## When verification fails

| `on_verify_fail` | Result |
|---|---|
| `reject` (default) | `401` with `{"error": {"code": "verification_failed", "message": "webhook verification failed: <reason>"}}`. Nothing is stored. |
| `store` | The delivery is stored with `verified: false` and a `verify_error`, and the sender gets `200` (so it stops retrying). |

The reason is a short fixed phrase (`signature mismatch`, `timestamp outside
tolerance`, `missing X-Hub-Signature-256 header`, ...). It never contains the
signature, the secret, or any part of the request.

An unverified delivery is stored as received, but **nothing the sender claimed
is trusted**: its delivery id and event type are left empty. This matters for
two reasons:

- Deduplication only considers *verified* deliveries. Otherwise a forged request
  that reuses a real delivery id could cause the genuine delivery to be dropped
  as a duplicate.
- Agents reading the inbox can tell verified from unverified with the
  `verified` filter and should treat unverified content as untrusted input.

## Trying it out

Create an inbox (the verifier secret is read from the environment, never from a
flag, because flags show up in process listings):

```sh
export HOOKINLET_MASTER_KEY=$(openssl rand -base64 32)   # keep this; it encrypts stored secrets
HOOKINLET_VERIFIER_SECRET='my-shared-secret' \
  hookinlet inbox create --slug demo --verifier hmac \
  --verifier-config '{"header":"X-Signature","prefix":"sha256="}'
```

Send a signed request:

```sh
body='{"hello":"world"}'
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac 'my-shared-secret' -hex | sed 's/^.* //')
curl -i -X POST http://localhost:8080/in/demo \
  -H "X-Signature: sha256=$sig" -d "$body"
# HTTP/1.1 200 OK
# {"seq":1}
```

Change one character of `$body` and send the same signature: you get a `401`.

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
