# Changelog

All notable changes to the public `pkg/` API are listed here (guardrail 10).
Until v1.0.0, breaking changes are allowed; after it, semantic versioning
applies.

## Unreleased

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
