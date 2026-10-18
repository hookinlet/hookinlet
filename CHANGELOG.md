# Changelog

Notable changes, and every change to the public `pkg/` Go API, are listed here.
Until v1.0.0, breaking changes are allowed; after it, semantic versioning
applies.

## 0.9.0 - 2026-10-18

A lower default ingest memory budget, with the memory it implies written down, and two log lines made quieter.
Nothing to migrate: upgrade by starting the new version on the same data directory. One change can answer
differently: with the default budget halved, a burst of large deliveries is refused with `503` sooner (see
**Changed**). If you are coming from 0.1.x, read the 0.2.0 **Upgrading** notes below first, because 0.2.0 needs
stored verifier secrets set again.

**Changed**

- **The ingest memory budget defaults to 128 MiB, not 256 MiB, and the memory it implies is documented.** A test that
  held as many 9 MiB bodies in flight as the budget allowed (most abandoned part way) took the process to about 750 MB
  resident at the default, and a 35-minute soak with an ingest storm in it peaked near 1 GB. The memory is about three
  times the bytes the budget counts plus 50 MB (Go lets its heap grow to twice what is live, and a body is read into a
  buffer with spare room); measured peaks were 139, 229, 380 and 750 MB for budgets of 32, 64, 128 and 256 MiB. The
  deploying guide's Resources section now says to size memory for three times the budget and has that table.
  `HOOKINLET_INGEST_MEMORY_BUDGET` and `server.Options.IngestMemoryBudget` still set it; one set explicitly is unchanged.
  What changes with the default: a burst of large deliveries is refused with `503 overloaded` (which senders retry) at
  half the bytes, and an inbox's share of the budget, an eighth of it, is 16 MiB, so an inbox that takes 10 MiB bodies
  receives one at a time. Ordinary traffic never comes near it. If you size for the old default, set the variable to
  `268435456` to keep it.
- **An inbox at its unverified-message cap logs its refusal once a minute, not once per delivery.** An inbox at its cap
  refuses every unverified delivery, so a sender holding its ingest key could fill the log with one WARN line per
  request (a 35-minute test wrote 170,901 of them). The line `delivery refused: too many unverified messages` is now
  written for the first refusal and then at most once a minute for each inbox, with `refusals_not_logged` saying how many
  it left out since the last one. `hookinlet_deliveries_total{result="unverified_limit"}` still counts every refusal.
- **A client that hangs up while its answer is written is logged at DEBUG.** `response failed` with a broken pipe or a
  reset connection was an ERROR line for each such client (a read storm wrote eight); any other failure to write an answer is
  still an ERROR.

## 0.8.0 - 2026-10-18

Two additions to the ingest hooks, both optional, and a corrected log line. Nothing to migrate.

**Added**

- **`inbox.IngestRequest.Host`** is the host a delivery was addressed to, for an embedder that serves several
  hostnames from one server. It is the request's `Host` header as the server received it, in lower case, without a
  trailing dot, and with the port only if the header had one. It is never taken from `X-Forwarded-Host` or another
  forwarding header; behind a proxy that rewrites `Host`, it is what the proxy sent. `inbox.IngestEvent.Host` carries
  the same value to an `IngestObserver`; `AfterIngest` has no host, because its arguments are fixed.
- **A `BeforeIngest` error that wraps `inbox.ErrNotFound` is answered `404`**, in the same words and with the same
  headers as an ingest key that names no inbox, and with no `Retry-After`. The server logs nothing above debug level
  for it, as for an unknown key. A hook that checks `req.Host` against the hostnames an inbox is served on uses it to
  answer `404` for an inbox that exists but is not served there. Until now such an error was a `500`. The other
  refusals (`429`, `402`, `403`, and `Retry-After`) and every other error (`500`) are unchanged. See
  [Which host a delivery arrived on](docs/extending.md#which-host-a-delivery-arrived-on).

**Fixed**

- The warning "delivery refused: too many unverified messages" now names the cap that applied, including one from an
  `UnverifiedLimiter`, where it named `Options.MaxUnverified` whichever applied.

## 0.7.0 - 2026-10-18

Two additions to the Go API, both optional: an `Authenticator` can choose the challenge on a `401`, and a server can
answer paths it has no route for. Nothing to migrate.

**Added**

- **`inbox.Challenger`** lets an `Authenticator` choose the `WWW-Authenticate` value of a `401`. An error that wraps
  `inbox.ErrUnauthenticated` may have a `WWWAuthenticate() string` method; the server uses its result on every `401`
  it writes (the read API, the admin API, streams, MCP) in place of `Bearer realm="hookinlet"`. A self-hoster with an
  OAuth server in front can send `Bearer resource_metadata="https://..."` to point clients to its discovery document.
  The value is used only if it is one line of printable ASCII, at most 4096 bytes, and starts with an authentication
  scheme (an RFC 9110 token such as `Bearer`); otherwise the server sends its own challenge and logs a warning once.
  Without the method nothing changes. `docs/openapi.json` now describes the header as a challenge matching that
  pattern instead of the fixed string. See [Choosing the 401 challenge](docs/extending.md#choosing-the-401-challenge).
- **`server.Options.Fallback`** is an `http.Handler` that answers paths the server has no route for, in place of the
  `404`, for serving a `/.well-known/` document or a static file next to the API. It never sees a path under a
  segment the server uses (`in`, `v1`, `mcp`, `healthz`, `readyz`, `metrics`, `admin`), including the admin API when
  `DisableAdmin` is set, which keep their `404`. It runs inside the server's middleware, request deadlines and
  shutdown, and its request body is capped at 1 MiB (`Options.FallbackMaxBodyBytes`; zero is the default, a negative
  number removes the cap). It is not used on the `AdminAddr` listener. With no `Fallback` the `404` is unchanged. See
  [Serving other paths next to the API](docs/extending.md#serving-other-paths-next-to-the-api).

## 0.6.0 - 2026-10-18

Two additions, both optional and found by type assertion: an ingest observer can tell why a signature failed, and a
reader can tell which stored header carries an `hmac` inbox's signature. Nothing to migrate.

**Added**

- **`IngestEvent.VerifyFailure`** says how verification failed when `Outcome` is `verification_failed`: `missing`
  (a header the verifier needs was not sent), `malformed` (a header was sent but unreadable, or sent more than once),
  `mismatch` (a well-formed signature or token that did not match) or `stale` (the signature matched, its timestamp
  was outside the tolerance). The constants are `inbox.VerifyFailureMissing`, `VerifyFailureMalformed`,
  `VerifyFailureMismatch` and `VerifyFailureStale`; the field is empty for other outcomes and for a verifier that does
  not classify its failures. It names no header and carries no request data. To support it, `verify.Failure` has a
  new `Kind` field (`verify.FailureMissing`, `FailureMalformed`, `FailureMismatch`, `FailureStale`) and
  `verify.FailureKindOf` reads it; the built-in verifiers set it, and a custom verifier may leave it empty. A
  repeated header counts as `malformed`. See [Observing how deliveries end](docs/extending.md#observing-how-deliveries-end).
- **`signature_headers` on an inbox's metadata.** `GET /v1/inboxes/{slug}` and MCP `list_inboxes` now name the stored
  request headers that carry the signature, for verifiers whose header is set per inbox (`hmac`), so a client that
  forwards stored headers can leave the signature out. Names only, never values; absent for the presets, whose
  header is fixed by the sender. The new optional interface is `verify.SignatureHeaderer`
  (`SignatureHeaders() []string`), found by type assertion on the built verifier; the response field is optional and
  added to `docs/openapi.json`.

## 0.5.0 - 2026-10-18

A fix for servers where a tenant has its own administrator: such an admin could reach admin operations that act on
the whole store, such as another tenant's tokens and a backup of every tenant's data, and now cannot (see
**Changed**). Also a faster unverified-message cap on servers with many inboxes, and clearer `Store` and `Notifier`
contracts with new conformance cases. There is nothing to migrate: upgrade by starting the new version on the same
data directory. If you are coming from 0.1.x, read the 0.2.0 **Upgrading** notes below first, because 0.2.0 needs
stored verifier secrets set again. If you run a tenant's admin through the admin API, check **Changed** before
upgrading. If you wrote your own `Store` or `Notifier`, run the conformance suites again: the new cases may fail it.

**Changed**

- **A tenant's admin can no longer use the admin operations that act on the whole store.** For an
  `inbox.TenantPrincipal` whose `CanAdmin` is true, `/v1/admin/tokens` (list, issue, revoke), `/v1/admin/backup`,
  `/v1/admin/retention/run`, `/v1/admin/status` and `/metrics` now answer `403` with code `forbidden`, do nothing,
  and log a warning naming the operation and the principal's Go type. Before, such an admin could list other tenants'
  `all_inboxes` tokens, revoke any token by id, issue an `all_inboxes` token that read every tenant's inboxes, and
  download a backup of every tenant's data. Its operations on its own tenant's inboxes are unchanged, and the
  operator (an admin that is not a `TenantPrincipal`, such as the admin token) keeps every operation. Each admin
  operation in `docs/openapi.json` now says which kind it is (`x-admin-scope`: `tenant` or `store`), and the server
  refuses a document whose admin operation does not say. See [Tenants](docs/extending.md#tenants).
- **The unverified-message cap locks each inbox on its own.** The count and the append that keep an inbox within
  `Options.MaxUnverified` (or its `UnverifiedLimiter` cap) were made under one lock for the whole server, so every
  unverified delivery on a node, to any inbox of any tenant, waited for the one before it, including its Store round
  trips. They are now made under a lock for that inbox alone: deliveries to one inbox still cannot overshoot its cap
  on one server, and deliveries to other inboxes no longer wait for them. A delivery whose request ends while it waits
  for the lock gives up. Across several servers on one database the cap is still approximate. Nothing in the Go API
  changes.

**Go API**

- **Contract clarifications and new suite cases for `Store` and `Notifier`.** No method changes and no change in
  behaviour. The `Store` documentation now says that the token methods are not scoped by tenant (`GetTokenByHash`,
  `ListTokens` and `DeleteToken` act on every token whatever the context's tenant, and an `AllInboxes` token reads
  every inbox), that `Ping` reports the Store as a whole and must not fail for one tenant's or inbox's quota, and
  what monotonic `ReceivedAt` lets a Store do; `Notifier.Subscribe` says when a subscription must be live, and what
  a distributed notifier does about it. `StoreSuite` has three new cases, `TokensAreNotScopedByTenant`,
  `DeleteInboxRacingAppendsAndTokens` and `PruneCancelledPartway`, which a Store that passed before may now fail.
  Added `inboxtest.NotifierSuiteWith` and `inboxtest.NotifierOptions` with `Settle`, so that a notifier whose
  signals cross a network can pass the coalescing check; `NotifierSuite` is unchanged. See
  [Writing a Store](docs/extending.md#writing-a-store) and [Writing a Notifier](docs/extending.md#writing-a-notifier).

## 0.4.0 - 2026-10-17

Changes for servers that hold more than one tenant's inboxes, and for anyone writing hooks: `/readyz` no longer lets
one tenant's unreadable secrets fail the whole server, `RequireTenant` makes a principal without a tenant fail
closed, hooks can say when to retry and which request they serve, and a `SecretBox` can use the request's context
and see the inbox's tenant. There is nothing to migrate: upgrade by starting the new version on the same data
directory. If you are coming from 0.1.x, read the 0.2.0 **Upgrading** notes below first, because 0.2.0 needs stored
verifier secrets set again. Visible to MCP clients: `list_inboxes` can page, and a refusal's tool error now starts
with its code. Visible to operators: `/readyz` samples secrets across tenants, and its `503` reasons changed.

**Added**

- **MCP `list_inboxes` pages.** It takes an optional `after` and, when a result is `truncated`, returns `next_after`
  to pass back for the next 100 inboxes, so a token that can read more than 100 can list them all. The cursor is
  opaque: it names the last inbox listed by slug and epoch, not by ID, and every page is checked against the caller.
  A page continues after that inbox, so inboxes created or deleted elsewhere between calls cause no skips or repeats;
  if that inbox was deleted, the call fails and the client lists again without `after`. Nothing reports a total, and
  a token not limited to a list still stops without saying so when a call has looked through 10,000 inboxes. A client
  that never passes `after` gets the same inboxes and `truncated` as before. See [`list_inboxes`](docs/mcp.md#list_inboxes).
- **`server.Options.RequireTenant`** refuses a caller whose `Principal` is neither an `inbox.TenantPrincipal` nor an
  admin (`CanAdmin`), so that a hosted embedder's principal that forgets `Tenant()` fails closed instead of running
  unscoped (naming inboxes in the default tenant, and seeing every tenant's inboxes in MCP `list_inboxes` that its
  `CanReadInbox` allows). The refusal applies to the read API, streams and MCP waits (also when they are checked
  again), every MCP call and the admin API; it is a `500` with code `internal`, and the log line names the
  principal's type. An admin in no tenant is still served, as the operator's view. Off by default: nothing changes
  unless it is set. See [Tenants](docs/extending.md#tenants).

**Changed**

- **`/readyz` samples the stored secrets across tenants.** It takes at most 3 secrets of each tenant, oldest first,
  before more of any one (still 25 in all, from at most the 5000 oldest inboxes, and no more than 500 inboxes past the
  point where it has 25 in hand), and still answers `503` only if none
  of them opens. One tenant whose secrets do not open, such as one whose key a key service has lost, no longer takes
  the server out of rotation for every tenant when it owns the oldest inboxes; it is logged with its tenant key and how
  many of its secrets did not open. The `503` reason and the log no longer assume the master key was changed, since a
  key service can fail too. A server whose secrets all belong to one tenant still answers `503`, as for a wrong key.
  The start-up check's error names each inbox's tenant after its slug.
- **Hook refusals can carry `Retry-After`.** A `429`, `402` or `403` from an embedder's `BeforeIngest` or `Admit`
  (ingest, reads, `HEAD`s and streams) has `Retry-After` when the hook says when to retry, in whole seconds rounded
  up, from 1 to 86400; the document lists it as optional on those responses. Over MCP such a refusal's tool error now
  starts with the condition's code and ends with the wait when there is one, as
  `rate_limited: too many requests; retry after 90 s` (it was the message alone).

**Go API**

- **Added `inbox.ContextSecretBox`**, an optional interface of `SecretBox` (found by type assertion) with
  `SealContext(ctx, plaintext, binding)` and `OpenContext(ctx, ciphertext, binding)`, and the helpers
  `inbox.SealWith` and `inbox.OpenWith`, through which the server now seals and opens every secret. A box that asks a
  key service gets the delivery's request context (cancelled when the sender goes away), the admin request's context,
  the readiness check's context (with its deadline) or the caller's context for `Server.CheckSecrets`, always scoped
  with `inbox.WithTenant` to the tenant of the inbox whose secret it is (`""` for the default tenant, so it is never
  unscoped); the Store calls of the same work keep their own context. A `SecretBox` without it is called as
  before. `pkg/secretbox.Box` implements it and ignores the context. `inboxtest.SecretBoxSuite` checks that the
  context methods agree with `Seal` and `Open`, and the new `inboxtest.ContextSecretBoxSuite` that a box doing I/O
  gives up on a cancelled or expired context. A secret check whose context ends while a secret is being opened now
  fails with that error instead of counting the secret as unreadable; for `/readyz` that is a `503` saying that
  checking the secrets timed out (the key service may be slow or down), with a warning in the log, checked again
  after 5 seconds. See
  [Writing a SecretBox](docs/extending.md#writing-a-secretbox).
- **Added `inbox.RetryAfterer`, `inbox.RefuseAfter(err, d)`, `inbox.AccessInfo`, `inbox.AccessFrom(ctx)` and
  `inbox.WithAccess(ctx, a)`.** A refusal wrapped with `RefuseAfter` (or any error with `RetryAfter() time.Duration`)
  sets the `Retry-After` above. `AccessFrom` tells `Admit`, `BeforeIngest`, `AfterIngest`, `ObserveIngest` and
  `MaxUnverified` which operation they are called for (the OpenAPI `operationId`, or `mcp.<tool>` for an MCP tool
  call) and the request id of the access log line, so an embedder can audit admissions without wrapping
  `Handler()`. No interface changes. See [Saying when to retry](docs/extending.md#saying-when-to-retry).

- **Added `inboxtest.AssertStoreWrapperForwards(t, wrapper, inner)`**, which fails, naming each one and what the
  server does without it, when a Store wrapper lacks an optional Store interface (`inbox.UnverifiedCounter`,
  `inbox.Backupper`, `inbox.StatusReporter`) that the store it wraps implements. A wrapper that embeds `inbox.Store`
  hides all three, and the server then silently stops capping unverified messages, answers backups with `501` and
  reports zeros in the admin status. `docs/extending.md` has a new section, **Wrapping a Store**, on this and on what
  hooks can and cannot be relied on for: metering, copying to another inbox, signalling the Notifier, and forwarding
  stored messages.

## 0.3.0 - 2026-10-17

Two changes for servers that hold more than one tenant's inboxes, and a way to see how every delivery ended.
There is nothing to migrate: upgrade by starting the new version on the same data directory. If you are coming
from 0.1.x, read the 0.2.0 **Upgrading** notes below first, because 0.2.0 needs stored verifier secrets set
again. One change is visible to MCP clients: `list_inboxes` now lists at most 100 inboxes for every token (see
**Changed**). The HTTP API, the command line and the console are otherwise unchanged.

**Changed**

- **A delivery's log lines name the inbox's tenant.** The lines `delivery`, `delivery rejected`, `delivery refused by hook`,
  `delivery refused: too many unverified messages`, `delivery refused: no room to store it` and `panic after commit` carry a
  `tenant` attribute next to `slug` when the inbox belongs to a tenant. A slug is unique only within a tenant, so on a
  server that holds several tenants' inboxes the slug alone did not say which inbox a line was about. An inbox in the
  default (empty) tenant logs exactly what it did before.
- **The MCP `list_inboxes` tool lists at most 100 inboxes for every token.** A token limited to some inboxes (an
  agent token, or any `inbox.ScopedPrincipal`) used to get all of them, however many it named; it now gets the
  first 100 it can read, in the order the principal gives them, with `truncated: true` when it can read more, as a
  token for all inboxes and the admin token already did. This changes what MCP clients see: a client whose token
  reads more than 100 inboxes no longer gets them all from `list_inboxes`, and should treat `truncated` as meaning
  some were left out. The result says nothing about how many more there are. Inboxes past the limit are no longer
  looked up, so the cost of a call no longer grows with the number of inboxes a token names.

**Go API**

- **Added `inbox.IngestObserver`**, an optional interface of `Hooks` (found by type assertion, so `Hooks` is
  unchanged), with `inbox.IngestEvent` and `inbox.IngestOutcome` and its constants. A `Hooks` that implements it is
  told how every delivery to a known inbox ended: stored, duplicate, handshake, or refused or failed and why
  (verification failed, refused by `BeforeIngest`, unverified cap, storage full, overloaded, too large, unreadable,
  inbox disabled or gone, internal error), with the inbox (so its tenant), the status sent, the size, the source
  address and, for a stored message, its `Seq` and whether it is verified. `AfterIngest` sees only stored messages
  and duplicates, and `/metrics` counts outcomes only in total; this is for counting, auditing or logging them per
  inbox or tenant. It runs synchronously after the outcome is decided (after `AfterIngest`), before the response; a
  panic in it is logged. A delivery whose ingest key names no inbox is not observed. The event carries no headers,
  body or verification reason. A `Hooks` without it behaves as before.
- `examples/custom-hooks` counts outcomes per inbox with it, and serves the counts on `EXAMPLE_COUNTS_ADDR`.

## 0.2.0 - 2026-10-16

The first release that changes how data is stored and what the Go interfaces under `pkg/` look like, made now
because nothing depends on either yet. If you have no stored verifier secrets, upgrading is starting the new
version on the same data directory; if you have, read **Upgrading** first. The HTTP API, the command line and the
console are unchanged.

**Breaking**

- **A verifier secret is sealed to the inbox's UID, not its slug.** Every inbox has a new `UID`: 128 random bits,
  made when the inbox is created and never changed. `inbox.VerifierSecretContext` takes it, so a secret sealed for
  one inbox does not open on another, including a later inbox that was given the same slug (which the slug could
  not prevent) and, in a store that holds several organizations' inboxes, one with the same slug in another
  organization. `Inbox.UID` is set by `CreateInbox` when the caller leaves it empty; a caller that must seal
  something to the inbox before the row exists chooses it (`inbox.NewUID`). `UpdateInbox` ignores it, and a
  restore from a backup keeps it, so that what was sealed still opens. The conformance suite (`pkg/inboxtest`)
  checks all of this for any `Store`.
- **Inboxes have a tenant, and slugs are unique within one.** `Inbox.Tenant` is an opaque key, empty by default,
  that keeps the inboxes of independent parties apart in one `Store`: the context passed to a `Store` call can be
  scoped to a tenant (`inbox.WithTenant`), and then every method acts only within it, so another tenant's inbox is
  `ErrNotFound` by ID, slug or ingest key and a slug can be used by every tenant. A context that is not scoped is
  the operator's view, as before. A `Principal` that implements the new `inbox.TenantPrincipal` has the server
  scope each of its requests. Installations that do not use tenants see no difference. The `StoreSuite`
  conformance suite checks it for any `Store`.
- **`ScopedPrincipal.InboxIDs` takes a context and returns an error:** `InboxIDs(ctx) ([]ID, error)`, so a
  principal that has to look its inboxes up can.

**Changed**

- **`/readyz` opens a sample of the stored secrets, not all of them.** It used to list every inbox and open every
  verifier secret each time the answer expired, so its cost grew with the number of inboxes. It now opens at most 25
  (looking at no more than 5000 inboxes); a master key that opens nothing shows in any sample, and one inbox with a
  damaged secret is still found at start-up, which checks them all. `Options.SkipReadinessSecretCheck` turns the check
  off for a `SecretBox` that has its own health check, such as one that calls a key service.

**Added**

- **`Server.MetricsHandler()`** serves the series of `GET /metrics` (GET and HEAD, no authentication) for an embedder
  to mount on a listener of its own, so that metrics can be scraped without the admin API on the same address.
  `GET /metrics` is unchanged.

**Upgrading**

- The migration gives each existing inbox a UID. A verifier secret that is already stored was sealed to the slug,
  and the migration cannot seal it again because it does not have the master key, so such a secret no longer
  opens. The server names those inboxes and refuses to start; set each secret again with
  `hookinlet inbox set-secret SLUG --secret-stdin`. There is no way back to 0.1.x on a migrated data directory
  except the copy the migration keeps.
- Migration 10 adds the tenant column and moves the slug to a column that is unique with it. The old column stays,
  unused, because SQLite cannot drop its constraint without rebuilding the table and every message with it.
- If you implement a `Store`, a `Principal` or a `SecretBox` of your own, run the conformance suites in
  `pkg/inboxtest` again: the `UIDs` and `Tenants` cases of `StoreSuite` are new.

## 0.1.4 - 2026-10-16

A small release: a wrong label in the console fixed, and clearer documentation. The server's behaviour is
unchanged, so there is nothing to migrate; upgrade by starting the new version on the same data directory.

**Console**

- **The console no longer says that keeping a failed delivery refuses it.** The choice "When a signature fails"
  offered "Refuse it, but keep a copy marked as failed", but with `on_verify_fail=store` the sender is not refused:
  it gets a `200`, so it stops retrying, and the message is stored marked not verified. The choice is now
  "When verification fails" with two options that say what each does ("Refuse it: the sender gets a 401 and
  nothing is stored (default)" and "Keep it, marked not verified: the sender gets a 200 and stops retrying"),
  a note that an inbox keeps a limited number of unverified messages, and, for an inbox whose verifier is
  `none`, a line saying that nothing is checked in place of the choice.

**Documentation**

- **The default is stated where it was missing.** `on_verify_fail` defaults to `reject`; the administration
  guide, the verifiers guide, the `hookinlet inbox create` and `inbox update` help, and the OpenAPI document
  (which now describes the field, and marks the default on creating an inbox) say so.
- **The README is shorter and opens with what hookinlet does.** The step-by-step walk-through (run it, send a
  signed event, read it back) moved to [docs/quickstart.md](docs/quickstart.md), which is still run by the docs
  tests; the README keeps the warning about the verifier `none` and the commercial-license contact.
- The MCP guide shows a project's `.mcp.json` for connecting a client, and the examples README says more about
  what `relay` does and does not do with headers and timestamps.

**Release process**

- A release now waits for CI on the tagged commit instead of failing when CI is still running; a run that
  finishes without passing, or none appearing within three minutes, is still an error.

## 0.1.3 - 2026-10-15

A release for the cap on unverified messages. **Upgrading runs one migration** (a partial index; it is quick, and a
copy of the database is kept first like any migration), and **the default behaviour changes**: see the first
bullet below.

**An inbox now keeps at most 1000 unverified messages.** With `on_verify_fail=store`, or an inbox whose
verifier is `none`, anyone who knows an ingest key could fill an inbox with unverified messages, and
under `retention_max_count` they pushed the genuine ones out. Now a delivery that would be stored unverified,
over the cap, is answered `429 rate_limited` and not stored (so not acknowledged). A verified delivery is never refused
for the cap, and nothing stored is evicted for it. `HOOKINLET_MAX_UNVERIFIED` sets it (`0` turns it off). Room comes
back as retention deletes unverified messages, so give such an inbox a `retention_days`. See
[verifiers](docs/verifiers.md#unverified-messages-are-capped).

- **Behaviour change:** an inbox that already holds 1000 or more unverified messages refuses further
  unverified deliveries until retention or you delete some, or you raise the cap.

**Go API**

- **Added `inbox.UnverifiedCounter`**, an optional interface of `Store` (found by type assertion, so `Store`
  is unchanged), and `inboxtest.UnverifiedCounterSuite`. A Store without it is not capped, and the server logs a
  warning. `server.Options.MaxUnverified` and `server.DefaultMaxUnverified` are new.
- **Added `inbox.UnverifiedLimiter`**, an optional interface of `Hooks` (found by type assertion, so `Hooks` is
  unchanged). A `Hooks` that implements it sets the cap on unverified messages per inbox, in place of
  `Options.MaxUnverified` (positive: the cap; 0: the server's limit; negative: no cap for that inbox). The default
  does nothing.

**Other**

- The SQLite store gains a migration (a partial index on unverified messages, so counting them reads at most
  the cap however large the inbox is).
- `docs/openapi.json`: the `429` on the ingest operations now also covers this refusal (no new code or response).
  `hookinlet_deliveries_total{result="unverified_limit"}` counts it.

## 0.1.2 - 2026-10-15

A small release: an optional interface for limiting reads, streams and waits, a warning in the console,
and corrections to the documentation. Nothing changes unless you use the new interface, so there is
nothing to migrate; upgrade by starting the new version on the same data directory.

**Go API**

- **Added `inbox.Admitter`**, an optional interface of `Hooks` (found by type assertion, so `Hooks` is unchanged),
  and `inbox.AccessKind` (`AccessRead`, `AccessStream`, `AccessWait`). A `Hooks` that implements it is asked, per
  caller and inbox, before every read, stream and MCP wait, and can refuse with `ErrRateLimited`, `ErrQuotaExceeded`
  or `ErrForbidden` or hold something until the access ends. The default does nothing. `inboxtest.AdmitterSuite`
  checks an implementation. `docs/openapi.json` now lists `402`, `403` and `429` (never sent by the default
  server) on the read and stream operations. `ScopedPrincipal`'s documentation now says a large finite set is allowed.

**Console**

- **The console warns when an inbox's verifier is `none`.** Such an inbox accepts anything and stores
  it as not verified, and it is what the quickstart starts with, so it is easy to leave on a real inbox.
  The inbox list and the inbox page now show an amber `none (unverified)` marker in place of the
  verifier's name, the inbox page carries a note that links to its settings, and the new-inbox and
  settings forms show a callout while `none` is chosen. Nothing in the API or the server changed.

**Documentation**

- What a long backup of a large database does under heavy load: reads and admin calls are refused as busy
  while it runs (7 minutes for a 2.6 GB database with 250 open streams and about 300 deliveries a second).
- A revoked token's stream can stay open longer than the usual 30 seconds when the server is too busy to
  make the check that ends it.

**Tooling**

- `scripts/soak` is a load driver for the built binary (`go run -tags soak ./scripts/soak`), used to
  soak-test releases: mixed ingest, reads, streams that drop and resume, admin changes, backups, restarts
  and a kill, checking that no acknowledged message is lost, duplicated or reordered. It is behind a build
  tag, so `go build` and `go test` do not see it. It waits for a follower that is still catching up before
  judging it.

## 0.1.1 - 2026-10-13

Upgrade from 0.1.0 by starting the new version on the same data directory; there is no
migration. It closes two ways for anyone who can reach the server, without a credential, to hold
connections open (see Security below), so it is worth taking promptly.

**Security**

- **A request could hold its connection open for good, without a credential.** One that promised
  a body and never sent it waited forever anywhere but at a known ingest key (on `/healthz`, a
  `404`, an unknown ingest key, or `/mcp` without a token), and so did one whose client sent
  requests one after another on a connection and never read the answers. With nothing else
  happening, that exhausts descriptors and memory. Every request's body now has
  `ReadBodyTimeout` to arrive, and every answer `ResponseTimeout` to be taken. An operation that
  waits before it answers (a free read slot, an MCP wait) counts that from when its answer is
  ready, and an ingest's answer counts from when the delivery has been handled.
- **A client with a valid token could do the same with requests that are refused early**, such
  as a stream asked for with a stale `epoch`, which the per-credential limits did not count. Such
  an answer now gets `ResponseTimeout` plus the longest wait the request could have made. The admin
  API, whose caller holds the admin token, is unchanged.

**Fixes**

- **An empty `hookinlet.db` is refused.** A restore or copy that did not finish leaves a
  zero-byte file, which the server (and `hookinlet check`) used to take for a new database and
  start on, with none of the data. `sqlite.Open` now returns an error for it, also when a
  leftover `-wal` is beside it. Do not create the file ahead of time (`touch`, a single-file bind
  mount): let hookinlet make it.
- **A backup that fails no longer leaves its journal in the data directory.** A snapshot that
  ran out of room left `backup-N.tmp-journal` behind for good; it is removed with the snapshot,
  and swept like it at the next start.
- **A delivery that queued for the database writer was stamped with a time that had already
  passed**, and the store then logged "the clock is behind a message already stored" for a clock
  that had not moved. The time is now read once the delivery holds the writer.
- **`GET /v1/admin/tokens` no longer fails with "not found" when an inbox is deleted while it
  runs.** The vanished inbox is left out of the listing.
- **`hookinlet version` after `go install`.** A binary installed with
  `go install github.com/hookinlet/hookinlet/cmd/hookinlet@v0.1.0` printed `dev`; it now prints
  the module version without its `v` (`0.1.0`), as the release builds do. A build from a checkout
  reports the version Go records for it (a pseudo-version such as `0.1.1-0.<time>-<commit>`,
  with `+dirty` for a modified tree) instead of `dev-<commit>`.
- **The image's `HEALTHCHECK` is now checked.** The image has carried it since 0.1.0 (it is in
  the config of both platforms), but some runtimes do not report it, such as Podman's
  Docker-compatible API; [deploying](docs/deploying.md#health-checks) says how to set
  the check there. CI and the release workflow now fail if a platform's image config lacks it.

**Documentation**

- Restore: what a restore must not leave behind (an old `-wal` or `-shm`), what a reader that
  sends no `epoch` sees after a restore, and how to restore into a Docker volume.
- A backup of a large database under heavy load can make the write-ahead log grow well past its
  usual size while it runs.
- The Caddy and nginx examples no longer leave the console sign-in page (`/admin`) public; the
  verifiers walk-through keeps the master key that is already set; a quarantined macOS binary
  and the other small gaps found by installing from the published artifacts.

**Go API**

- `server.Options.ReadBodyTimeout` applies to the body of any request, not only an ingest's,
  and `server.Options.ResponseTimeout` to answers given before a request reaches an operation,
  to ingest answers, and to MCP answers.

## 0.1.0 - 2026-10-13

The first release. hookinlet is a webhook inbox for AI agents: it receives webhooks,
checks their signatures, stores them, and lets an agent read them back reliably over
REST, a live event stream, or MCP.

**What it does**

- **Receives and verifies.** Stripe, GitHub, Slack, Standard Webhooks, generic HMAC and a
  shared token, or none. The body is stored byte for byte and checked against the raw bytes.
  A delivery is acknowledged only once it is committed, and repeated deliveries are recognised.
  Inboxes are reached at a random ingest key, not a guessable name.
- **Lets an agent read it back.** A REST API with cursors that cannot skip or repeat a message,
  a Server-Sent Events stream, and an MCP endpoint (list, query, get, wait). Several named
  tokens per inbox, scoped to some inboxes or all, replaced without a gap. Cursors carry the
  inbox's epoch, so a restored or recreated inbox is noticed, and a reader is told when
  retention removed messages it had not read. In MCP, what the sender wrote is under an
  `untrusted` key and bodies are left out unless asked for.
- **Is run and managed** with an admin API, a CLI that works through it or directly in the data
  directory, `hookinlet apply` for inboxes declared in a file, retention, and consistent
  backups. The whole HTTP API is one OpenAPI 3.1 document that the server is generated from
  and tested against.
- **Is safe to run.** Verifier secrets are sealed with a key-identified format bound to their
  inbox, and a wrong master key stops the server at start-up (`hookinlet rekey` moves them to
  a new one). A copy of the database is kept before each migration. One server per data
  directory, enforced by a lock. A full disk is a retryable 503, the write-ahead log is bounded,
  and unauthenticated ingest has a memory budget. The admin API can be off, or on its own
  address. `/metrics`, `hookinlet healthcheck`, draining on shutdown, and secrets from files.
- **Ships verifiably.** Static binaries for Linux and macOS (amd64, arm64) and a multi-arch image,
  with checksums, SBOMs, signed build provenance for the archives, and the licenses of everything inside.
  Releases are cut only from a commit on main that passed CI, after a vulnerability check.

## Changes during development

<details>
<summary>The detail behind the summary above, by area: what changed in the public Go packages, the API and
the configuration, newest first. Of interest mainly to people embedding the Go packages.</summary>

### Late changes before the first release
- **The console works on a separate admin listener.** With `HOOKINLET_ADMIN_LISTEN_ADDR` the console was
  served on the admin address but could not read messages there (the read API was on the public
  address only). The admin listener now also serves the read API when the console is on.
- **The console marks invisible and direction-changing characters** in the request path, query and
  content type as well, and in more kinds of them (Arabic letter mark, line and paragraph separators,
  tag characters).
- **One credential cannot use up what everyone shares.** A read token may hold only a quarter of the
  read slots and a tenth of the stream slots (the admin token is not limited), and one inbox's
  deliveries may hold only an eighth of the ingest memory budget, so a token that never reads its
  answers, or an ingest key that sends slow bodies, no longer turns every other inbox away. New in
  `pkg/server`: `Options.MaxReadsPerCredential` and `Options.MaxStreamsPerCredential` (settings
  `HOOKINLET_MAX_READS_PER_CREDENTIAL` and `HOOKINLET_MAX_STREAMS_PER_CREDENTIAL`).
- **Admin console: managing, searching and testing.** The console can now manage: a new-inbox form with a section for each
  kind of sender (Stripe, GitHub, Slack, Standard Webhooks, HMAC, shared token, none) that shows the
  agent token and ingest URL once; a Settings tab per inbox (general settings, verification, replacing
  the secret, rotating the ingest URL, deleting with a typed confirmation); a Maintenance page (status,
  run retention, download a backup); and a command palette (`⌘K`). The console has Playwright
  end-to-end tests and an axe accessibility scan of every screen in both themes, run in CI.
- **Admin console: inboxes and messages.** Each inbox has a page: its facts and ingest path, and its messages, newest
  first, with filters (event type, verification, time), older messages on request, and a live mode that
  follows the stream. A message opens at its own address (`/admin/inboxes/SLUG/messages/SEQ`) and shows
  its body, headers, exact raw bytes (as text or hex) and details, with a copy-as-curl for reading it back.
  Everything a sender wrote is drawn as text, with a reminder that it is untrusted.
- **Admin console: shell, themes, overview and tokens.** The console now has an overview (inbox count, messages received,
  the last 24 hours, the last retention run, and an activity line per inbox), an inbox list with a
  filter, and a Tokens page that issues tokens (shown once, with a ready curl line) and revokes them.
  Light and dark themes, a layout that works on a phone, and self-hosted Inter and JetBrains Mono
  (SIL OFL, listed in `THIRD_PARTY_LICENSES`).
- **`hookinlet check`** verifies the database file (SQLite's integrity check, or `--quick`), for use after a
  crash, a disk problem or a restore. The server still does not scan the file at startup. New in `pkg/`:
  `sqlite.Store.Check`.
- **Admin console: serving and status.** A web console is served at `/admin` (switch off with
  `HOOKINLET_ADMIN_UI=false`). This release carries the plumbing and a sign-in page that shows
  the server's status; the screens follow. New admin endpoint `GET /v1/admin/status`. New in
  `pkg/`: `Options.DisableUI`, the optional `inbox.StatusReporter` (implemented by the SQLite store).
  The built app is committed under `internal/ui/dist` and built from `web/`.
- **A clock that jumps forward no longer makes retention delete messages early.** A retention run that
  finds the wall clock more than ten minutes ahead of the monotonic clock since the last run uses the time
  that really passed, and logs it; the next run accepts the clock as it reads.
- **A database restored with a wide file mode is made private when it is opened** (and its `-wal` and
  `-shm` files), with a warning in the log. Before, the server kept whatever mode the copy came with.
- **`/readyz` no longer fails for the whole server because one inbox's secret does not open.**
  It fails when the master key opens none of the stored secrets (a wrong key), and logs a warning
  naming the inboxes when only some do not open. Starting still refuses until they are fixed.
- **An MCP `wait_for_messages` holds no read slot while it waits.** Idle waits could use up every
  slot and make reads answer 503; the slot is taken as the answer is returned.
- **Changes to the public packages:** `sqlite.Open` applies migrations only when given
  `WithExclusiveLock()`; without it, a database that needs migrating fails with the new
  `*sqlite.NeedsMigrationError` (servers built on the package should pass the option, as
  `examples/custom-hooks` now does). `Server.Serve` refuses a nil listener and a server with
  `Options.AdminAddr` set (use `ServeAdmin` or `Run`, or mount `AdminHandler`), and `ServeAdmin`
  refuses nil listeners. `verify.Registry.New` fails when a factory returns no verifier.
  `inboxtest.AuthenticatorSuite` requires `CanReadInbox(nil)` to be false and has a `Revoke`
  fixture field. `hookinlet backup --data-dir` on a database that needs migrating refuses: back
  up with the old binary first, or upgrade (a copy is kept).
- **A command that does not hold the data-directory lock no longer migrates the database.** It
  refuses and says to stop the server and run `hookinlet migrate` (which takes the lock). Before,
  `inbox show` run with the new binary beside the old server upgraded the schema under it.
- **A rolled-back upgrade no longer keeps a stale pre-migration copy.** If the marker says no
  migration was applied, the next upgrade takes a fresh copy. Migrations are logged, with the
  place the copy was kept, and a copy that fails leaves no `-journal` behind.
- **`/readyz` counts pages the database can reuse as room**, so it recovers after retention
  has freed space on a volume that is still nearly full.
- **A client that asks for a big answer and never reads it can no longer pin memory.**
  Responses have a write deadline (`server.Options.ResponseTimeout`, 2 minutes), and a read slot
  is kept until the response has been written, so at most 16 pages are in flight (it was 16
  being built and any number waiting on slow clients).
- **A JSON body with a number or escape that common parsers refuse is delivered as `body_text`**
  (number literals over 100 characters or outside float64, unpaired surrogate escapes), so one
  such body cannot make every page that holds it unreadable.
- The example clients keep the epoch with the cursor (see Cursor integrity).

### The public face

#### Changed
- **The admin API is under `/v1/admin`** (was `/admin`). `/metrics`, ingest (`/in/...`), and
  the health checks stay where they were.
- **MCP messages put what the sender wrote under `untrusted`** (`path`, `query`, `headers`,
  `content_type`, `delivery_id`, `event_type`, and the body); the fields the server set stay at
  the top. `query_messages` and `wait_for_messages` return messages **without bodies** unless
  `bodies: true` (replacing `summary`, which meant the opposite and was off by default).
  REST and SSE messages are unchanged.
- **`server.Options` follows one rule**: zero is the default, a negative number turns a bound
  off where that makes sense (`RetentionInterval`, `IngestMemoryBudget`) and is an error
  everywhere else (it used to be taken as the default). `Addr` is only needed to `Run`.

- The admin lists' `next` links carry the limit the page was cut with (clamped, or the
  default), as the message lists' always did. `body_truncated` is a top-level field of an MCP
  message: it is the server's flag, not something the sender wrote.

#### Added
- `docs/stability.md`: what is stable, what is not, how a change is announced, what an inbox
  identifier is, and where CORS, TLS and the Host check stand.
- `Server.Maintain(ctx)` runs the retention schedule for an embedder that serves `Handler()`
  itself.
- Tests that fail when a core interface (`Store`, `Hooks`, `SecretBox`, `Authenticator`,
  `Principal`, `Notifier`) gains or loses a method, when the REST, wire and MCP message types
  stop carrying the same fields, and when the generator template we changed stops being
  upstream's plus the recorded edits (`scripts/check-template.sh`, in CI).

### Storage and startup (part 2)

#### Added
- **One server per data directory.** `sqlite.WithExclusiveLock` takes an advisory lock
  (`hookinlet.lock`) that `serve`, `rekey` and `renew-epochs` use; a second server stops
  at once with `*sqlite.LockedError`. Other commands do not take it.
- **A full disk is a 503.** A write that fails for lack of room (no space, a quota, a file-size limit) is `inbox.ErrStorageFull` (behaviour a Store may offer; the conformance suites cannot induce it);
  ingest answers `503 storage_full` with `Retry-After` and does not acknowledge; `Store.Ping`
  (so `/readyz`) fails while the volume has under `sqlite.MinFreeBytes` (32 MiB) or for 30
  seconds after a write failed for room. The ingest operations document `503`.
- **An ingest memory budget.** `server.Options.IngestMemoryBudget` (`--ingest-memory-budget`,
  256 MiB by default and at least 10 MiB; 0 in the flag or negative in the option turns it
  off) bounds the body bytes held by deliveries being received, counted as they arrive;
  one whose bytes would go over is answered `503 overloaded` with `Retry-After`.
- **The write-ahead log is cut back** to `sqlite.DefaultWALLimit` (64 MiB; `WithWALLimit`)
  whenever it is reset, instead of staying as large as the biggest burst.
- **`GET /metrics`** (admin token): Prometheus text, with no series labelled by anything a
  client chooses.
- **`hookinlet healthcheck`** and a `HEALTHCHECK` in the image (the image has no `curl`).
- **Draining.** `server.Options.DrainDelay` (`--drain-delay`): after being told to stop,
  `/readyz` answers 503 for that long while the server still serves. A second signal ends
  the process at once.
- **Secrets from files**: `HOOKINLET_ADMIN_TOKEN_FILE`, `..._MASTER_KEY_FILE`,
  `..._OLD_MASTER_KEY_FILE`, `..._VERIFIER_SECRET_FILE`.

#### Changed
- **`inbox.Hooks.BeforeIngest` takes an `*inbox.IngestRequest`** (breaking): the inbox, size,
  source address, method, path suffix, and request headers.
- **`Backupper.Backup` may return size -1** for a snapshot it streams; the admin API then omits
  `Content-Length` (it is optional in the document now) and `hookinlet backup` accepts it;
  a streamed backup that fails part way aborts the connection, so the client sees an error and
  not a clean end.
- `ErrorOverloaded` has a second code, `storage_full`.
- `go.mod` says `go 1.27` with `toolchain go1.27.2`, so embedders are not made to use the
  patch release.

### Storage and startup (part 1)

#### Added
- **A copy of the database before a migration.** With migrations to apply to a database
  that has data, the store writes `hookinlet.db.before-migration-N.bak` (mode 0600, the
  latest only) first, and refuses to migrate if it cannot (`sqlite.MigrationSnapshotPrefix`).
  A new copy replaces the old only once it is complete, and an upgrade that was cut short
  keeps the copy it already has (a marker file, `hookinlet.db.migrating`, says it was).
- **A wrong master key is found at start-up and in `/readyz`.** `serve` opens every stored
  verifier secret before it listens and refuses to start, naming some inboxes and the
  remedy; `/readyz` answers 503 (`not_ready`) if they stop opening. `Server.CheckSecrets`.
- **`hookinlet rekey`** seals every secret again under a new master key
  (`HOOKINLET_OLD_MASTER_KEY`). It opens every secret before it writes any, so a wrong old
  key changes nothing; it leaves alone an inbox changed while it ran; and it can be run
  again if it is interrupted. With no old key it brings secrets from earlier releases up to
  the new format. `serve` warns while any secret is still in the old format.

#### Changed
- **`inbox.SecretBox.Seal` and `Open` take a context** (breaking). The default box now
  writes version 2: a key id (a fingerprint of the key) and the context (the inbox's slug)
  are authenticated with the value, so a wrong key is `secretbox.ErrWrongKey` and not
  just `ErrOpen`, and a secret copied onto another inbox's row does not open
  (`inbox.VerifierSecretContext`). Version 1 values still open, under any context, until
  `hookinlet rekey` writes them again: the binding protects secrets sealed by this release
  and those rekeyed (`secretbox.Legacy` reports the rest). `Box.KeyID`.
- The message `body` column is now the last column of the row (migration 0007), so a read
  that does not want bodies no longer follows the overflow pages of the ones it skips. The
  table is rebuilt: it takes time and disk space in proportion to the database.

### Cursor integrity

#### Added
- **Epochs.** Every inbox has an `epoch`, an opaque string that is fixed for its life,
  differs for an inbox made again under the same slug, and is renewed in every backup
  snapshot (a restored server's sequence numbers are not the original's). Inbox info,
  pages, messages, `next` links, stream events and the MCP tools carry it. `epoch=` on
  `GET .../messages` and `.../stream`, and `epoch` on `query_messages` and
  `wait_for_messages`, name the epoch a cursor came from; another one is `409
  epoch_changed` (an MCP tool error starting `epoch_changed`). `inbox.Inbox.Epoch`,
  `inbox.ErrEpochChanged`.
- **Gaps.** A page read with `after=` has `gap: true` when retention removed messages
  after the cursor before it was read; an MCP result has the same; a stream sends one
  `event: gap` first. `inbox.Page.OldestSeq` (set by every Store) is what it is computed
  from. A cursor of 0 never reports a gap.
- The store logs a clock that stepped back (`sqlite.WithLogger`).
- `epoch` on `GET .../messages/{seq}` and `.../raw` (and `get_message`), so a `seq` from
  an earlier life of the inbox can be refused.
- `hookinlet renew-epochs` (and `sqlite.Store.RenewEpochs`) gives every inbox a new
  epoch, for use after restoring from a copy of the data directory or restoring one
  snapshot twice. Snapshots from `Backup` have new epochs already.

#### Changed
- `message_count` is kept by every append and removal, so it no longer assumes that
  removal takes a prefix of the inbox.
- `DeleteInbox` marks the inbox as being deleted first: from then on no reader, writer,
  token or ingest key can reach it. If the process stops part way, the retention sweep
  finishes the delete (before this it was finished by deleting again).
- `wire.PageRequest.NextLink` takes the epoch. Every `Message` and `MessagePage` has an
  `epoch`, and `InboxInfo` too: clients that reject unknown fields must allow them.
- A Store must now set `Inbox.Epoch` on create (ignoring the caller's value), return
  `Page.OldestSeq`, and `Backupper` snapshots must renew epochs (all three are in the
  conformance suites).

### Tokens

#### Added
- **Several named agent tokens per inbox, each scoped to some inboxes or to all of
  them.** `POST /admin/tokens` (`{"name", "inboxes": [slugs]}` or `{"name",
  "all_inboxes": true}`), `GET /admin/tokens` (`?inbox=`), `DELETE /admin/tokens/{id}`;
  `hookinlet token create|list|revoke`. A token is replaced by issuing the new one and
  revoking the old, with no moment in which an agent has none that works.
- `inbox.Token`, `inbox.TokenID`, and the Store methods `CreateToken`, `GetTokenByHash`,
  `ListTokens`, and `DeleteToken`, with conformance tests in `inboxtest`. Tokens are
  looked up by an indexed hash.
- `inbox.ScopedPrincipal` (a `Principal` that can say which inboxes it reads).
- `--admin-enabled` and `--admin-listen-addr` (and `server.Options.DisableAdmin` /
  `AdminAddr`, `Server.ServeAdmin`, `Server.AdminHandler`): the admin API can be turned
  off, or served on its own address and nowhere else.
- Admin actions are logged with the caller's address (`client_ip`).

#### Changed
- **The admin token must not be mostly one repeated character** (at least 10 different
  characters), as well as being at least 32 long.
- `inbox.Store.GetInboxByTokenHash` is replaced by `GetTokenByHash`, and
  `Inbox.AgentTokenHash` is gone. `inbox.SingleInboxPrincipal` is replaced by
  `ScopedPrincipal`; `token.Agent` now carries a list of inboxes, and `token.Reader`
  (all inboxes, no admin) is new. A Store that was written against the old interface
  needs the four token methods.
- Deleting an inbox revokes the tokens that read only that inbox. A token for several
  inboxes keeps working for the rest.

#### Removed
- `POST /admin/inboxes/{slug}/rotate-token`, `hookinlet inbox rotate-token`, and
  `admin.Service.RotateToken`. Issue a new token and revoke the old one.

### The trust model

#### Changed
- **A verifier must be named** when an inbox is created (admin API, `hookinlet
  inbox create`, `hookinlet apply`). There is no default. `none` is a choice.
- **`verified` means a verifier checked something.** A delivery to an inbox whose
  verifier is `none` is stored with `verified: false`, with no event type and no
  delivery id taken from the sender, and it is not deduplicated.
- **Inboxes are reached at an ingest key, not at the slug.** New inboxes get a
  random key (`ingest_key` in the admin API; `/in/{key}`), because a name anyone
  can guess is not something to protect an inbox with (docs/admin.md explains why).
  An admin may choose one, and `POST /admin/inboxes/{slug}/rotate-ingest-key` /
  `hookinlet inbox rotate-ingest-key` replaces it.
- **Deduplication** trusts only delivery ids that the signature covers (Stripe,
  Slack, Standard Webhooks). For GitHub and HMAC inboxes a duplicate is the same
  id and the same body; the same id with another body is a new message.
- **A verifier secret is checked when it is set** where the verifier can tell
  (Standard Webhooks needs `whsec_` and base64). Changing an inbox's verifier
  removes its secret. Calls that change an inbox answer with `warnings`.

#### Added (`pkg/`)
- `inbox.Inbox.IngestKey`, `inbox.ValidIngestKey`, and `inbox.NewMessage.DedupeKey`.
  `Store.ResolveIngestKey` resolves by ingest key. The key is required: a `Store`
  refuses to create an inbox without one (`ErrInvalid`) and does not make one up,
  and an update has to carry the key it was given.
- `verify.Result.IDSigned` and `verify.Result.Unauthenticated`, and the optional
  `verify.SecretChecker`.
- Migration 0003 (ingest keys, dedupe keys).

### The document drives the server

#### Changed
- Routes, the credential each operation needs, and the checks on path parameters,
  the query string, and the JSON body come from `docs/openapi.json` and the code
  generated from it. The only route written in Go is the catch-all that answers 404.
- A method a path does not take is a JSON 405 with an `Allow` header made from the
  document. `/healthz` and `/readyz` answer it too (they used to answer 404) and now
  take `HEAD`. A path that needs a credential checks it before answering 405.
- Every response carries `Cache-Control: no-store` and
  `X-Content-Type-Options: nosniff`.
- A query string with invalid UTF-8 is refused (400), except on ingest and the health checks, which take any query string as they always did.
- A JSON-RPC body that is not an object (a batch, or `null`) and one whose `id` is not a string or an integer are refused before the MCP SDK sees them.
- MCP tools take their names, descriptions, and argument schemas from the
  document. The arguments are validated by the SDK against those schemas.

#### Added
- `PUT` on `/in/{key}` and `/in/{key}/{rest}` is documented as operations of its
  own (the server always accepted it), and `HEAD` on the health checks.
- The document describes the MCP tools (`x-mcp-tools`) and the frames of the event
  stream (`x-frames`); tests hold the server and `docs/mcp.md` to them.

#### Removed
- `wire.ParseMessages`, `wire.ParseStream`, `wire.QueryValues`, and
  `wire.RejectParams` (internal): requests are built from the generated parameters
  by `wire.NewPageRequest` and `wire.NewStreamRequest`.

### The spec is the contract, enforced in Go

#### Changed
- The server refuses a request that breaks `docs/openapi.json`: a query parameter
  the document does not list, a repeated one, or a value of the wrong type or
  outside its range, on the read API, the stream, and the admin API; and an admin
  request body with an unknown field, a wrong type, or a value outside its schema.
  The answer is 400 in the usual error shape, after authentication.
- `admin.CreateParams` and `admin.Patch` omit empty fields when marshalled, where
  they used to write `null`. The admin API now refuses `null` for these fields,
  as its document says.
- The admin API's `GET`/`DELETE` of one inbox, `rotate-token`, `retention/run`, and
  `backup` refuse a query parameter, where they used to ignore it, and the document
  lists 400 for them. `?limit=` with no value is refused on the list.
- JSON responses are written with a `Content-Length`, with fields in alphabetical
  order, and no longer end in a newline.

#### Added
- `internal/api`, generated from the document, and `make generate`. CI fails when
  it is out of date. Every operation (ingest, the read API, the stream, the admin
  API, the backup download, MCP, and the health checks) is served through it, so a
  status the document does not list for an operation does not compile.

### Durable commits

#### Changed
- `pkg/store/sqlite` opens its connections with `synchronous=FULL` instead of
  `NORMAL`. A delivery acknowledged with a `2xx` now survives power loss, not
  only a crash of the process. A sequential append costs about 36 us more
  (98 us to 134 us for 1 KiB on the development machine). No exported API
  changed.

### Open-source polish

#### Added
- `docs/openapi.json`, an OpenAPI 3.1 description of ingest, the read API, the
  stream, the admin API, and the MCP endpoint. Tests call every operation on a
  real server, force every response the document lists, and require that nothing
  the server says is missing from it; requests generated from its schemas must be
  accepted or refused as it says; and it must list exactly the routes the server
  registers.
- `README.md`, `SECURITY.md`, issue and pull request templates, and an expanded
  `CONTRIBUTING.md`.
- `examples/`: `curl` (polling and streaming), `python` and `go-agent` agents,
  `relay` (forwards each message to another URL, at least once and in order), with
  `examples/README.md`.
- `scripts/test-docs.sh` runs the marked examples in the documentation; CI runs it
  and lints the OpenAPI document.

#### Changed
- Every package under `pkg/` says in its documentation that it is pre-1.0.
- `GET /admin/inboxes` refuses a repeated `after` or `limit` (400), as the read API
  does for its parameters. It used to use one of them silently.

### Packaging and docs

#### Added
- Release automation: GoReleaser configuration for static `linux` and `darwin`
  binaries on `amd64` and `arm64` with checksums and an SBOM per archive, and a
  workflow that publishes a multi-arch container image to GHCR on a version tag.
- `docs/deploying.md`, `docs/extending.md`, `docs/releasing.md`, `README.md`, and
  `examples/custom-hooks`, a program that embeds the server with its own
  `inbox.Hooks` (a per-inbox rate limit).
- A test of start-up time (`TestColdStartWithAnExistingDatabaseIsFast`).

#### Changed
- The Dockerfile cross-compiles for the target platform instead of building under
  emulation. No change to the image's contents or behaviour.

### Admin API, retention, backup, CLI

#### Added
- The admin API (`/admin/inboxes`, `/admin/inboxes/{slug}`, `…/secret`,
  `…/rotate-token`, `/admin/retention/run`, `/admin/backup`), mounted when
  `Options.Authenticator` is set and usable only by a principal with
  `CanAdmin() == true`. See `docs/admin.md`.
- `inbox.Backupper`, an optional `Store` capability that returns a consistent
  snapshot (`Backup(ctx) (io.ReadCloser, int64, error)`); `pkg/inboxtest`:
  `BackupperSuite`. The SQLite store implements it.
- `server.Options`: `PublicBaseURL` (used for the ingest URLs the admin API
  returns) and `RetentionInterval` (default one hour; negative turns the schedule
  off). Wired to `HOOKINLET_PUBLIC_BASE_URL` and `HOOKINLET_RETENTION_INTERVAL`.
- Retention runs on a schedule: once after the server is listening, then every
  `RetentionInterval`. `Server.Serve` does not return until a running sweep has
  stopped.
- Commands: `hookinlet inbox list|show|update|set-secret|rotate-token|delete`,
  `hookinlet retention run`, and `hookinlet backup --out FILE`.

#### Changed
- `inbox.Inbox.Validate` rejects `RetentionDays` above `inbox.MaxRetentionDays`
  (36,500). Larger values made the prune cutoff overflow into the future and
  delete every message.
- `Store.DeleteInbox` may work in steps: the inbox is closed to ingest and
  agents first, and an interrupted delete is finished by deleting again. The
  SQLite store now does this, so deleting a large inbox no longer stalls writes.
- `HOOKINLET_RETENTION_INTERVAL` accepts `off` and must be at least `1s`.
- `hookinlet inbox create` and the other inbox commands call a running server
  when given `--url` (or `HOOKINLET_URL`) and the admin token
  (`HOOKINLET_ADMIN_TOKEN`); without it they work in the data directory as
  before. Creating an inbox now checks the verifier name and configuration, so a
  misspelled `--verifier-config` key is an error at creation.
- The SQLite store prunes 250 rows per transaction (was 5000), so deliveries
  arriving during a large prune wait at most a short batch.

### MCP endpoint

#### Added
- `POST /mcp`: a stateless Streamable HTTP MCP endpoint with the read-only tools
  `list_inboxes`, `query_messages`, `get_message`, and `wait_for_messages`
  (see `docs/mcp.md`). It uses the same bearer tokens as the read API.
- `server.Options`: `DisableMCP`, `MCPInlineBodyBytes` (default 16 KiB), and
  `Version` (reported to MCP clients; default `dev`). `MCPInlineBodyBytes` is
  limited to 64 KiB, and `HOOKINLET_MCP_INLINE_BODY_BYTES` above that is a
  configuration error. Wired to
  `HOOKINLET_MCP_ENABLED`, `HOOKINLET_MCP_INLINE_BODY_BYTES`, and the build
  version.
- `inbox.SingleInboxPrincipal`: an optional capability of a `Principal` limited
  to one inbox, checked by `AuthenticatorSuite`. `token.Agent` implements it.
- Dependency: `github.com/modelcontextprotocol/go-sdk` v1.8.0 (and its
  permissively licensed dependencies).

#### Changed
- The REST message JSON can carry `body_truncated: true`, but only where a size
  cap is requested, which the REST API never does; its responses are unchanged.

### SSE and the notifier

#### Added
- `pkg/notify/local`: `Notifier`, the in-process `inbox.Notifier`.
- `pkg/inboxtest`: `NotifierSuite`, the conformance suite for `inbox.Notifier`.
- `GET /v1/inboxes/{slug}/stream`: Server-Sent Events, resumable with
  `Last-Event-ID`.
- `server.Options`: `MaxStreams`, `MaxStreamsPerInbox`, `StreamRevalidateInterval`,
  `SSEHeartbeat`, and `SSERecheckInterval` (wired to `HOOKINLET_MAX_STREAMS`,
  `HOOKINLET_MAX_STREAMS_PER_INBOX`, `HOOKINLET_SSE_HEARTBEAT`, and
  `HOOKINLET_SSE_RECHECK_INTERVAL`).

#### Changed
- The default `Options.Notifier` is now `local.New()`. It was a notifier that never
  signalled, which was only suitable while nothing waited on it.
- Contract: `Store.Query` must be exact and read a consistent snapshot, and
  `Notifier.Publish` must signal existing subscriptions even with a cancelled
  context. `StoreSuite` and `NotifierSuite` enforce both.
- `Server.Shutdown` ends open streams, in addition to stopping new connections.

### The read API

#### Added
- `pkg/auth/token`: `Authenticator`, an `inbox.Authenticator` that accepts the
  admin token or an inbox's agent token, with the principals `Admin` and
  `Agent`.
- `pkg/server`: the read API (`GET /v1/inboxes/{slug}`, `/messages`,
  `/messages/{seq}`, `/messages/{seq}/raw`), mounted when `Options.Authenticator`
  is set.
- `pkg/inboxtest`: `AuthenticatorSuite`, the conformance suite for
  `inbox.Authenticator` and `inbox.Principal`.
- `server.Options`: `Authenticator`, `MaxHeaderBytes` (default 64 KiB),
  `MaxConcurrentReads` (default 16), and `ReadWaitTimeout` (default 10s).
- Migration 0002: an index on `(inbox_id, delivery_id, seq)`.

#### Changed
- The contract for `inbox.Authenticator` and `inbox.Principal` is spelled out:
  failures must be indistinguishable, an error that does not wrap
  `ErrUnauthenticated` becomes a server error, and `CanReadInbox` must follow the
  inbox's ID, not its slug, and not depend on `Disabled`.
- `HOOKINLET_ADMIN_TOKEN` must be at least 32 characters with no whitespace.
- Requests whose headers exceed 64 KiB (plus a few KiB of slack) get 431.
  Go's default was 1 MiB.
- `Store.Stats` computes the message count by subtraction instead of counting
  rows. The result is the same; it relies on retention removing only a prefix of
  an inbox, which the conformance suite now verifies against a real walk.

### Ingest and verification

#### Added
- `pkg/verify`: `Verifier`, `Result`, `Failure`, `Factory`, `HeaderRedactor`, and
  `Registry` (with `Default`, `Register`, `New`), plus the presets `stripe`,
  `github`, `slack`, `standardwebhooks`, `hmac`, `token`, and `none`.
- `pkg/inbox`: `NopHooks` and `NopNotifier`, the defaults when an embedder
  supplies neither.
- `pkg/server`: the ingest handler (`POST|PUT /in/{key}[/...]`), `GET /readyz`,
  and the `Options` fields `Store`, `SecretBox`, `Notifier`, `Hooks`,
  `Verifiers`, `TrustedProxies`, `Now`, and `ReadBodyTimeout`.
- Commands: `hookinlet migrate` and `hookinlet inbox create` (the latter is
  temporary and is replaced by the admin API).

#### Changed
- `server.New` now requires `Options.Store`. A server with no store could
  only answer `/healthz`.
- A failed signature returns the generic message `webhook verification failed`;
  the specific reason is logged and, for stored deliveries, kept in
  `verify_error`.

### Contracts, store, and suites

#### Added
- `pkg/inbox`: the domain types and the `Store`, `Notifier`, `Authenticator`,
  `Principal`, `Hooks`, and `SecretBox` interfaces, with their behavioral
  contracts in the doc comments. First publication of this API.
- `pkg/inboxtest`: conformance suites `StoreSuite` and `SecretBoxSuite`. An
  implementation of an interface must pass its suite.
- `pkg/store/sqlite`: the default single-node `Store`.
- `pkg/secretbox`: the default `SecretBox` (AES-256-GCM).
- `pkg/auth/token`: agent token generation, hashing, and constant-time
  comparison.
- `pkg/server`: `server.New(Options)` with request ids, panic recovery, an
  access log, `/healthz`, and graceful shutdown.

</details>
