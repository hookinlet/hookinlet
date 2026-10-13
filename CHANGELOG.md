# Changelog

Notable changes, and every change to the public `pkg/` Go API, are listed here.
Until v1.0.0, breaking changes are allowed; after it, semantic versioning
applies.

## Unreleased

Nothing yet.

## 0.1.0

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
