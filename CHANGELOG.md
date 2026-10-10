# Changelog

All notable changes to the public `pkg/` API are listed here (guardrail 10).
Until v1.0.0, breaking changes are allowed; after it, semantic versioning
applies.

## Unreleased

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
