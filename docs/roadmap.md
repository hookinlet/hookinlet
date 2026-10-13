# Roadmap

Ideas that are not built yet. Each is labelled **OSS**
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

## Pruning in `hookinlet apply` (OSS)

`apply -f` creates and updates the inboxes a file declares and never deletes
anything. A `--prune` that also deleted the inboxes the file does not name would
make the file the whole truth, which is what some teams want. It is not there
because deleting an inbox deletes every message in it, for good, and the usual
ways to get a file wrong (a typo in a slug, the wrong file, a file for another
environment, a half-written one) would then destroy data that was never meant to
go. If it is added, it should be opt-in per run, show exactly what it would
delete in `--dry-run` first, require the count and slugs to be confirmed (for
example `--prune --yes` only after a dry run, or a `--prune-limit N` that
refuses to delete more than N), and refuse an empty or unparseable file. Until
then, `inbox delete` is the explicit way, and `apply` lists the inboxes it does
not manage so that they are not forgotten.
