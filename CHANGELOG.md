# Changelog

All notable changes to the public `pkg/` API are listed here (guardrail 10).
Until v1.0.0, breaking changes are allowed; after it, semantic versioning
applies.

## Unreleased

### Phase 9: the spec is the contract, enforced in Go

#### Changed
- The server refuses a request that breaks `docs/openapi.json`: a query parameter
  the document does not list, a repeated one, or a value of the wrong type or
  outside its range, on the read API, the stream, and the admin API; and an admin
  request body with an unknown field, a wrong type, or a value outside its schema.
  The answer is 400 in the usual error shape, after authentication.
- `admin.CreateParams` and `admin.Patch` omit empty fields when marshalled, where
  they used to write `null`. The admin API now refuses `null` for these fields,
  as its document says.

#### Added
- `internal/api`, generated from the document, and `make generate`. CI fails when
  it is out of date.

### Phase 8: open-source polish

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

### Phase 7: packaging and docs

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

### Phase 6: admin API, retention, backup, CLI

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

### Phase 5: MCP endpoint

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
  permissively licensed dependencies; see `PLAN.md`).

#### Changed
- The REST message JSON can carry `body_truncated: true`, but only where a size
  cap is requested, which the REST API never does; its responses are unchanged.

### Phase 4: SSE and the notifier

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

### Phase 3: the read API

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

### Phase 2: ingest and verification

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

### Phase 1: contracts, store, and suites

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
