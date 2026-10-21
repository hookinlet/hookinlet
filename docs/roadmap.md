# Roadmap

Ideas that are not built yet. Each has a label. **OSS** means it belongs in this repository. **Cloud** means it
belongs in the hosted product.

## Webhook forwarding (relay)

This means receiving a webhook and also passing it on to other destinations. An inbox can then sit in front of an
existing service instead of replacing it.

- **Cloud:** built-in forwarding. It would have destinations per inbox, retries that survive restarts and wait
  longer each time, a job table, a signing secret for each destination, and replay of past messages. It would also
  block user-supplied URLs that point at internal networks (SSRF protection).
- **OSS:** `examples/relay`, a small program that follows an inbox with the query API's `after=` cursor (or SSE)
  and forwards each message. It shows self-hosters how to relay without the core growing a delivery queue. It
  exists: see `examples/relay`.

The core already provides what a relay needs. Each inbox has sequence numbers, which work as safe cursors.
Embedders get `Hooks.AfterIngest`.

## Pruning in `hookinlet apply` (OSS)

`apply -f` creates and updates the inboxes a file declares. It never deletes anything. A `--prune` option could
also delete the inboxes the file does not name. That would make the file the whole truth, which some teams want.

It is not there because deleting an inbox deletes every message in it, for good. Files go wrong in common ways: a
typo in a slug, the wrong file, a file for another environment, a half-written file. Each of these would destroy
data nobody meant to remove.

If `--prune` is added, it should follow these rules:

- It is opt-in for each run.
- `--dry-run` first shows exactly what it would delete.
- You must confirm the count and the slugs. For example, `--prune --yes` works only after a dry run, or a
  `--prune-limit N` refuses to delete more than N.
- It refuses an empty file or a file it cannot parse.

Until then, `inbox delete` is the explicit way to remove an inbox. `apply` also lists the inboxes it does not
manage, so you do not forget them.
