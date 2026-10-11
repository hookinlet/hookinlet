# Roadmap

Ideas that are out of scope for the current phase. Each is labelled **OSS**
(belongs in this repository) or **Cloud** (belongs in the hosted product).

## Webhook forwarding (relay)

Receiving a webhook and also passing it on to other destinations, so an inbox
can sit in front of an existing service instead of replacing it.

- **Cloud:** first-class forwarding. Destinations per inbox, durable retries
  with backoff, a job table, per-destination signing secrets, replay of past
  messages, and SSRF protection for user-supplied URLs.
- **OSS:** `examples/relay`, a small program that tails an inbox with the
  query API's `after=` cursor (or SSE) and forwards each message. It shows
  self-hosters how to relay without the core growing a delivery queue. It
  exists: see `examples/relay`.

The core already provides what a relay needs: per-inbox sequence numbers that
are safe cursors, and `Hooks.AfterIngest` for embedders.

## Cap on unverified messages (OSS)

With `on_verify_fail=store`, anyone who knows an ingest key can fill an inbox
with unverified messages, and once count-based retention applies they push
genuine ones out. Options: a separate, smaller retention limit for unverified
messages, or a per-inbox cap enforced at append time. Until then, keep
`on_verify_fail=reject` (the default) unless you are debugging a sender, and
use `Hooks.BeforeIngest` to rate limit.
