# Extending hookinlet

The `hookinlet` program is a thin set of parts. Each part sits behind an
interface in `pkg/inbox`. `server.New` takes the parts as options. An application
can replace any part, or add behaviour around it, without forking. The hosted
product is built this way too. It changes nothing here and supplies its own
implementations. You can do the same.

The packages are pre-1.0. The interfaces can still change, and `CHANGELOG.md`
lists every change. After 1.0.0, semantic versioning applies.

## The parts

| Interface (`pkg/inbox`) | What it does | Default | Conformance suite (`pkg/inboxtest`) |
|---|---|---|---|
| `Store` | Persists inboxes and messages; assigns each message its per-inbox `Seq`. | `pkg/store/sqlite` | `StoreSuite` |
| `Notifier` | Tells readers an inbox may have new messages. Carries no payload. | `pkg/notify/local` (in process) | `NotifierSuite` |
| `Authenticator`, `Principal` | Says who a request is and what it may read or administer. | `pkg/auth/token` | `AuthenticatorSuite` |
| `SecretBox` | Seals and opens verifier secrets, bound to a context. Seal a verifier secret with `inbox.VerifierSecretContext(inbox.UID)`, or the server cannot open it. | `pkg/secretbox` | `SecretBoxSuite` |
| `ContextSecretBox` (optional, of `SecretBox`) | `SealContext` and `OpenContext`: `Seal` and `Open` given the context of the work, so a box that asks a key service can stop when it is cancelled or past its deadline, read what the embedder put on it, and see the inbox's tenant (`inbox.TenantFrom`). The server calls them in place of `Seal` and `Open`. See [Writing a SecretBox](#writing-a-secretbox). | `pkg/secretbox` (which ignores the context) | `SecretBoxSuite`, and `ContextSecretBoxSuite` for a box that does I/O |
| `Hooks` | Runs before and after ingest: reject a delivery, count it. | `inbox.NopHooks` | none: the default does nothing |
| `Admitter` (optional, of `Hooks`) | Asked before every read, stream and MCP wait, with the caller, the inbox and an `AccessKind`; may refuse (`ErrRateLimited` 429, `ErrQuotaExceeded` 402, `ErrForbidden` 403) or return a release function, called when the access ends. | none: without it everything is admitted | `AdmitterSuite` |
| `Backupper` (optional) | A consistent snapshot of the Store, for `GET /v1/admin/backup`; the size may be -1 if the Store streams it. A Store that holds several tenants refuses a tenant-scoped context. | the SQLite store | `BackupperSuite` |
| `TokenUseRecorder` (optional) | `TouchToken(ctx, id, at)` records when a token was last used, shown as `Token.LastUsedAt` and in the admin API's `last_used_at`. The default `Authenticator` calls it at most once per token per interval (five minutes), in the background, with a context of its own, and ignores its failure, so it may be slow or refuse under load. The time only moves forward; an unknown token is `ErrNotFound`. | the SQLite store | `StoreSuite` (the `TokenLastUsed` case, skipped without the interface) |
| `EpochRenewer` (optional) | `RenewEpochs(ctx)` gives every inbox a new epoch and changes nothing else, for a Store restored by means that are not part of the interface (a copied database file, a point-in-time recovery). The server does not call it; a restore procedure does. See [A restore is a new epoch](#writing-a-store). | the SQLite store (`hookinlet renew-epochs` runs it) | `EpochRenewerSuite` |
| `BodyScreener` (optional, of `Hooks`) | Shown the body of every delivery that is about to be stored, after it verified, and may refuse it (the errors `BeforeIngest` may return) or store something else in its place, for redaction or schema checks. A replaced message is marked `modified`. See [Redacting or checking a body](#redacting-or-checking-a-body). | none: bodies are stored as received | `BodyScreenerSuite` (it returns at once, leaves the body it is given alone, and survives concurrent calls; what it refuses or replaces is your own tests, and the server's tests cover what is done with the answer) |
| `UnverifiedLimiter` (optional, of `Hooks`) | Says, per inbox, how many unverified messages it may keep, in place of `Options.MaxUnverified`: a positive number is the cap, 0 the server's limit, a negative number no cap. For caps that depend on something the server does not know, such as a plan. Needs a Store with `UnverifiedCounter`. | none: every inbox gets the server's limit | `UnverifiedLimiterSuite` (it returns at once and leaves the inbox alone; the cap itself is the server's tests) |
| `IngestObserver` (optional, of `Hooks`) | Told how every delivery to a known inbox ended (`IngestEvent`: the inbox, an `IngestOutcome` such as `stored` or `verification_failed`, the status, size, source address, the host it was addressed to, for a stored message its `Seq` and whether it is verified, and for a failed signature a `VerifyFailure` kind), including refused and failed ones that `AfterIngest` never sees. For counts, audit or logs per inbox or tenant. See [Observing how deliveries end](#observing-how-deliveries-end). | none: nothing is observed | `IngestObserverSuite` (it returns at once for every outcome and leaves the event alone; what the server reports is the server's tests) |
| `UnverifiedCounter` (optional) | How many unverified messages an inbox holds (bounded by a limit), so the server can cap them (`Options.MaxUnverified`). Without it the server does not cap, and says so when it starts. | the SQLite store | `UnverifiedCounterSuite` |
| `ReaderTracker` (optional) | Remembers how far each named reader of an inbox has read, so the lag can be shown (`PUT`, `GET` and `DELETE` on `/v1/inboxes/{slug}/readers`, see [api](api.md#named-readers-and-lag)): `AckReader` sets a position for `(inbox, name)` in the inbox's current epoch, within a cap given per call (`Options.MaxReadersPerInbox`), `Readers` lists them, `ForgetReader` deletes one. A position is set only by an explicit acknowledgement, never by a read, and belongs to one epoch: the Store drops every position when an inbox's epoch changes (`RenewEpochs`, and the epochs a `Backupper` snapshot is given) and deletes an inbox's with the inbox. Without it the three operations answer `400 unsupported`. | the SQLite store | `ReaderTrackerSuite` |
| `StatusReporter` (optional) | The schema version, the size of the data and the free space, for `GET /v1/admin/status` and the console. | the SQLite store | none |
| `Challenger` (optional, of the error `Authenticate` returns) | `WWWAuthenticate() string`: the `WWW-Authenticate` value for the `401`, in place of `Bearer realm="hookinlet"`. See [Choosing the 401 challenge](#choosing-the-401-challenge). | none: the server sends its own | `ChallengerSuite` (the value is one the server sends; the rest is the server's tests) |
| `Unrouted` (optional, of the error `Authenticate` returns) | `UnroutedHost() bool`: when true, the server answers `404` in place of `401`, for a request for a host that serves no org. See [Answering 404 for a host that serves no org](#answering-404-for-a-host-that-serves-no-org). | none: the server answers `401` | `AuthenticatorSuite` (`AuthFixture.UnroutedHost`; the rest is the server's tests) |
| `ScopedPrincipal` (optional) | A `Principal` that can read a known list of inboxes, so listing does not have to search. `InboxIDs(ctx)` may look the list up, and may fail. | the agent principal | checked by `AuthenticatorSuite` |
| `TenantPrincipal` (optional) | A `Principal` that belongs to one tenant. After authenticating, the server scopes the request's context to it (`inbox.WithTenant`), so slugs are looked up in that tenant and nothing outside it is reachable. See [Tenants](#tenants). | none: the shipped principals are not in a tenant | `StoreSuite` (the `Tenants` case) for the Store, and `servertest.ServerSuite` for what the server scopes |
| `verify.Registry` | Maps verifier names to signature checks. | `verify.Default()` | the package's own tests |

The server finds the optional parts with a type assertion. It copes when they are
missing:

- With no `Backupper`, `GET /v1/admin/backup` answers `501`.
- `Hooks` without `Admitter` admits every read, stream and wait.
- `Hooks` without `IngestObserver` hears only what `AfterIngest` says.
- `Hooks` without `BodyScreener` stores every body as received.
- A `SecretBox` without `ContextSecretBox` is called with `Seal` and `Open`, with no context.

The server's own limits (`MaxConcurrentReads`, `MaxStreams`, `MaxStreamsPerInbox`)
apply after `Admit`, whatever it says.

`/readyz` opens a sample of the stored verifier secrets. This catches a master key that is not the one the data was sealed under. It answers `503` only if none of the secrets opens.

- The sample holds up to 25 secrets, oldest first. It takes at most 3 per tenant before it takes more from any one tenant.
- It looks at no more than the 5000 oldest inboxes. Once it has 25 secrets, it looks at no more than 500 further inboxes for other tenants.
- The server keeps the answer for 30 seconds, or 5 seconds while it is failing.

Suppose a `SecretBox` keeps a key per tenant, and one tenant's key is gone. The server logs it, with the tenant, how many of its secrets did not open, and a few slugs. The server stays in rotation for the other tenants, as long as another tenant has a secret among the inboxes the check looks at. With many tenants, the oldest inboxes get the places. If every inbox with a secret belongs to one tenant, the server cannot tell that tenant's lost key from a wrong master key. It answers `503` for both.

A `SecretBox` whose opens are expensive, such as one that asks a key service, can turn the check off with `Options.SkipReadinessSecretCheck` and report its own health. If the sample takes more than 20 seconds, `/readyz` answers `503` and says that checking the secrets timed out. It checks again after 5 seconds.

`Server.CheckSecrets` runs in the `hookinlet` program before it listens. It always opens every secret of every tenant, and it fails if any one does not open.

`GET /metrics` is part of the admin API and needs the admin token. To serve the same series elsewhere, for an operator's scraper and without the admin API on that address, mount `Server.MetricsHandler()` on a listener or a route of your own. It does no authentication. You must protect the place you mount it.

## Tenants

A Store may hold the inboxes of several independent parties. Each has its own
tenant key (`Inbox.Tenant`, an opaque string, empty by default). A hosted service
uses this to keep its customers' inboxes apart on one server. If you never use
tenants, you have one tenant, the empty one, and see no difference.

- A **context scoped to a tenant** (`inbox.WithTenant(ctx, "acme")`) limits every
  `Store` call to that tenant. An inbox of another tenant is `ErrNotFound`,
  whether you name it by ID, slug or ingest key. Listings leave it out.
  `CreateInbox` makes the inbox in that tenant. Two tenants can both have an inbox
  called `alerts`.
- A **context that is not scoped** is the operator's view. A delivery, the retention
  sweep and the command line use it. IDs and ingest keys reach every inbox, and a
  slug names an inbox in the default tenant. `CreateInbox` makes the inbox in
  `Inbox.Tenant`, so provisioning code can create one for any tenant.
- **The server scopes the context for you.** Say the `Principal` your
  `Authenticator` returns implements `TenantPrincipal`. Then every Store call made
  for that request (the read API, the admin API, streams, MCP) runs in its tenant.
  Your `Principal` still decides which of those inboxes the caller may read
  (`CanReadInbox`).
- **A tenant's admin manages its tenant's inboxes and nothing else.** A
  `TenantPrincipal` whose `CanAdmin` is true may create, list, show, change and
  delete its tenant's inboxes. It may also set their verifier secrets and rotate
  their ingest keys. Another tenant's inbox is `404`, as if it did not exist. Some
  admin operations act on the whole store. These belong to the operator (an admin
  that is not a `TenantPrincipal`): tokens (a token has no tenant), backups,
  running retention, `/v1/admin/status` and `/metrics`. If a tenant's admin tries
  one, it gets `403` with code `forbidden` and nothing happens. The log gets a
  warning that names the operation and the principal's Go type. Each admin
  operation's `x-admin-scope` in `docs/openapi.json` says which kind it is. A
  hosted service should give its customers principals whose `CanAdmin` is false,
  and keep admin for its own operator principal.
- **A principal that is not a `TenantPrincipal` is not scoped.** A principal that
  belongs to a tenant must implement it. The shipped token authenticator does not.
  It is for one tenant, and its all-inboxes tokens read every inbox in the Store.
- **`Options.RequireTenant` makes a forgotten `Tenant()` fail closed.** Without it,
  a customer's principal that does not implement `TenantPrincipal` is unscoped. It
  names inboxes in the default tenant, and MCP's `list_inboxes` shows it every
  tenant's inboxes that its `CanReadInbox` allows. With it, the server refuses a
  principal that is neither a `TenantPrincipal` nor an admin (`CanAdmin`). This
  covers the read API, streams (also when the server checks an open stream or MCP
  wait again), MCP and the admin API. The caller gets `500` with code `internal`,
  which gives no reason. The log gets an error that names the principal's Go type
  and never the credential. An admin in no tenant is still served, because that is
  how an operator gets its view across tenants. An admin that is a
  `TenantPrincipal` keeps to its tenant as before. A hosted service whose callers
  all belong to tenants should turn it on. It is off by default. If your
  `Authenticator` never returns a `TenantPrincipal`, as with the shipped token
  one, turning it on would refuse every caller but the admin.
- Ingest keys and UIDs are unique across tenants, because an ingest key alone finds
  the inbox a delivery is for. A `Store` you write must keep to all of this. The
  `Tenants` case of `StoreSuite` checks it. It also checks that one tenant cannot
  read, write, list or delete another's inbox by any method.
- **Tokens belong to the operator, not to a tenant.** An `inbox.Token` has no
  tenant. `GetTokenByHash`, `ListTokens` (its `InboxID` filter included) and
  `DeleteToken` act on every token, whether or not the context is scoped.
  `GetTokenByHash` has to, because the server calls it to authenticate a request
  before it knows the tenant. A scope adds one check, in `CreateToken`: every inbox
  in `InboxIDs` must belong to the tenant, or the call returns `ErrNotFound`. An
  `AllInboxes` token is not limited to the tenant and reads every inbox in the
  Store. The `TokensAreNotScopedByTenant` case of `StoreSuite` checks this. So the
  agent tokens of `pkg/auth/token`, and the admin API's `/v1/admin/tokens`, which
  works through these methods, are for the operator. The server refuses a tenant's
  admin (a `TenantPrincipal` whose `CanAdmin` is true) on `/v1/admin/tokens`.
  Through these methods, that admin could list tokens of other tenants, revoke any
  token by its `id`, or issue an `all_inboxes` token that reads every tenant's
  inboxes. An embedder that gives each tenant's callers their own credentials
  keeps them in its own `Authenticator`, whose principals are `TenantPrincipal`s.

## Putting them together

`server.New` takes an `Options`. Only `Store` is required (and `Addr`, if you call
`Run`). The read API, the admin API, SSE and MCP are served only when there is an
`Authenticator`, because there is no safe default for who may read.

```go
store, err := sqlite.Open(ctx, "./data", sqlite.WithExclusiveLock()) // or your own inbox.Store; the lock also lets it migrate
box, err := secretbox.New(masterKey)            // 32 bytes

srv, err := server.New(server.Options{
	Addr:          ":8080",
	Store:         store,
	SecretBox:     box,
	Notifier:      local.New(),
	Authenticator: token.NewAuthenticator(store, adminToken),
	Hooks:         myHooks,                     // optional
})
err = srv.Run(ctx)                              // returns after a clean shutdown
```

`examples/custom-hooks` is a complete program built this way. It has a per-inbox
rate limit as the hook. It has an `IngestObserver` that counts, per inbox, how
deliveries ended. It has a `BodyScreener` that redacts chosen JSON keys. CI
compiles and tests it. Copy it as a starting point. `Options` has the rest
(limits, timeouts, `TrustedProxies`, `PublicBaseURL`, a `Verifiers` registry).
Each field says what it does and what its default is.

`srv.Handler()` returns the whole `http.Handler`. Use it if you would rather mount
it in a server of your own than call `Run` (no `Addr` is needed then). `Run` and
`Serve` do two things the handler alone does not. They start the retention
schedule. On shutdown, they end open streams and MCP waits first. If you serve the
handler yourself, run `srv.Maintain(ctx)` in a goroutine for the schedule. It also
finishes any delete that was cut short. Call `srv.Shutdown(ctx)` to end the
streams.

Every numeric and duration option follows one rule. It is written in `Options`'
comment and in [Stability](stability.md#the-go-packages). Zero is the default. A
negative number is an error, unless it turns a bound off (`RetentionInterval`,
`IngestMemoryBudget`).

## Writing hooks

`Hooks` has two methods. Embed `inbox.NopHooks` to implement only one.

```go
type limiter struct{ inbox.NopHooks }

func (limiter) BeforeIngest(ctx context.Context, req *inbox.IngestRequest) error {
	if tooMany(req.Inbox, req.SourceIP) {
		return fmt.Errorf("%w: slow down", inbox.ErrRateLimited) // 429
	}
	return nil
}
```

`BeforeIngest` gets an `*inbox.IngestRequest`. It holds the inbox, the body size,
the source address (as recorded, honoring trusted proxies), the host the delivery
was addressed to (see below), the method, the path after the inbox key, and the
request headers (read-only). It runs after the server finds the inbox and reads
the body, and before it checks the signature. A refusal is cheap. There is a
cost to weigh. The hook also sees unverified requests. If a limit counts every
request, anyone who knows an inbox's URL can use up its allowance with unsigned
posts. The real sender, which may not retry, is then refused. You have three
choices. Limit by something a stranger cannot spend, such as the source address
of unverified traffic. Or meter what was accepted, in `AfterIngest`. Or accept the
trade-off knowingly, as the example does to keep it short. To refuse, return an
error that wraps `ErrRateLimited` (429), `ErrQuotaExceeded` (402), or
`ErrForbidden` (403). Wrap `ErrNotFound` to answer 404 (see below). Any other
error is a 500. So if a lookup of your own returns an error that wraps
`ErrNotFound`, that is now a 404, not a 500. The server calls the hook
concurrently, from every request.

### Which host a delivery arrived on

`req.Host` is the `Host` header of the request as the server received it. It is in
lower case, with any trailing dot removed from the name. It has a port only if the
header had one: `hooks.example.com`, `hooks.example.com:8443`, `[::1]:8080`. It is
empty if the request sent no `Host`. The server never reads `X-Forwarded-Host`,
`Forwarded` and the like, because anyone can send them. If a proxy in front of the
server rewrites `Host`, `req.Host` is what the proxy sent. The embedder must have
the proxy pass the original on. The sender chooses the value. Compare it with the
hostnames you serve. Do not use it unchecked as a key, a label or a file name.

An embedder that serves several hostnames can use it to answer only for the right
one. If an inbox exists but is not served on the host the delivery arrived on,
return `ErrNotFound`:

```go
func (h hosts) BeforeIngest(ctx context.Context, req *inbox.IngestRequest) error {
	if !h.serves(req.Inbox, req.Host) {
		return inbox.ErrNotFound // 404, the same as an unknown ingest key
	}
	return nil
}
```

The server then answers just as it does for an ingest key that names no inbox. The
status, headers (apart from the request id) and body are the same. There is no
`Retry-After`, and no log line above debug level. The hook runs after the server
reads the body, so the sender sees the same answer but not in the same time. Also,
`IngestObserver` is told about it as `not_found`, which an unknown key is not.
`AfterIngest` has no host, because its arguments are fixed. `IngestEvent.Host`
carries the host to an observer.

`AfterIngest` runs after the message is committed, or recognized as a duplicate
(`created == false`, so a meter does not count it twice). It must not block. Its
context may be cancelled as soon as the response is sent, so work that outlives
the call needs `context.WithoutCancel`. The server logs a panic in it and
swallows it. The delivery is already stored, and the sender must not be told it
failed.


### Admitting reads, streams and waits

`Hooks` covers ingest. To limit what a caller may read, also implement
`inbox.Admitter` on the same value. The server finds it with a type assertion, so
`Hooks` itself does not change.

```go
func (l *limiter) Admit(ctx context.Context, p inbox.Principal, in *inbox.Inbox, kind inbox.AccessKind) (func(), error) {
	if !l.take(p, kind) {
		return nil, fmt.Errorf("%w: too many %ss", inbox.ErrQuotaExceeded, kind) // 402
	}
	return func() { l.give(p, kind) }, nil
}
```

`Admit` runs once per access. It runs after the caller is authenticated and may
read the inbox, and before the access takes a slot of the server's own, so a
refusal costs nothing. `kind` is one of three values. `AccessRead` is a read that
answers at once. This includes the MCP query, get and list tools and a wait with a
timeout of zero. (`in` is nil for the list tool.) `AccessStream` is an SSE stream.
`AccessWait` is an MCP wait. To refuse, return an error that wraps `ErrRateLimited`
(429), `ErrQuotaExceeded` (402) or `ErrForbidden` (403). Over MCP the agent is told
the same in words. Any other error is a 500. The release function may be nil. The
server calls it once, when the access ends. For a read, that is when the answer has
been written, or, for a read that is not held that long, when the handler returns.
For a stream or wait, it is when the stream or wait ends. The server's own limits
apply after `Admit`, whatever it answers. `inboxtest.AdmitterSuite` checks an
implementation.

### Saying when to retry

A refusal from `BeforeIngest` or `Admit` can say when to come back: wrap it with
`inbox.RefuseAfter(err, d)`, or return your own error type with a
`RetryAfter() time.Duration` method (`inbox.RetryAfterer`, found with `errors.As`).

```go
return nil, inbox.RefuseAfter(fmt.Errorf("%w: plan limit", inbox.ErrRateLimited), 90*time.Second)
```

The `429`, `402` or `403` then carries `Retry-After` in whole seconds, rounded up,
at least 1 and at most 86400 (a day). A duration that is not positive sends no
header. Neither does an error that is not one of the three refusals. Over MCP the
tool error says it instead, as `rate_limited: too many requests; retry after 90 s`.
Neither the HTTP answer nor the tool error carries your error's text.

### Which request a hook is called for

`inbox.AccessFrom(ctx)` tells `Admit`, `BeforeIngest`, `AfterIngest`,
`ObserveIngest` and `MaxUnverified` which request they are called for. Use it for
an audit trail. An `Authenticator` reads it from `r.Context()`, because the server
attaches it before it calls `Authenticate`:

- `Operation` is the operation's `operationId` in [the OpenAPI document](openapi.json)
  (`getInbox`, `headGetInbox`, `listMessages`, `headListMessages`, `getMessage`,
  `headGetMessage`, `getMessageRaw`, `headGetMessageRaw`, `streamMessages`, `ingest`,
  `ingestPut`, `ingestWithPath`, `ingestPutWithPath`), or `mcp.` and the tool's name for
  an MCP tool call (`mcp.list_inboxes`, `mcp.query_messages`, `mcp.get_message`,
  `mcp.wait_for_messages`). It is a fixed name, never a path.
- `RequestID` is the request's `X-Request-Id`, the `request_id` of its access log line.
- `ClientIP` is the address the request came from. Ingest stores it as a message's
  `source_ip`. It is the connection's peer. If the peer is in
  `Options.TrustedProxies`, it is the client that the peer's `X-Forwarded-For`
  names. The server ignores that header from any other peer, so a caller cannot
  choose the value. It may be **empty** when the peer address could not be read. An
  allowlist built on it must **fail closed**. An empty `ClientIP` is not on the
  list, and the refusal is the same `inbox.ErrUnauthenticated` as for a bad
  credential.

For `Authenticate`, `Operation` is empty for the `405` the server sends when a path
does not take the method. It is `mcp` for the HTTP request that carries MCP tool
calls, because the server checks the credential before it knows the tool. When the
server rechecks a stream or an MCP wait, it sees that stream's operation, or `mcp.`
and the tool's name.

`AccessFrom` reports false outside a request. `inbox.WithAccess` makes a context
that carries one, for testing a hook.

### Redacting or checking a body

`BeforeIngest` runs before a delivery is verified, so it cannot trust what it sees.
`AfterIngest` runs after the message is committed, so it cannot change it. To
refuse a verified delivery for what is in it, or to store it with something
removed, also implement `inbox.BodyScreener` on the same value:

```go
func (r redactor) ScreenBody(ctx context.Context, req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	if !validAgainstSchema(req.Inbox, req.Body) {
		return inbox.BodyDecision{}, fmt.Errorf("%w: not a valid order", inbox.ErrForbidden) // 403
	}
	if clean, changed := redact(req.Body); changed {
		return inbox.BodyDecision{Replace: true, Body: clean}, nil
	}
	return inbox.BodyDecision{}, nil // keep it as received
}
```

It runs after verification and before the message is stored. It also runs after the
handshake step, so a Slack `url_verification` is answered as before and never shown
to it. The server asks it about a delivery that verified. It also asks about one
that is kept unverified (`on_verify_fail=store`, or an inbox whose verifier is
`none`). `req.Verified` says which. Three kinds of delivery never reach it: one
refused for its signature, a handshake, and one that `BeforeIngest` refused. The
server also asks it about a repeat of a delivery already stored, because only the
Store can tell a repeat. So you pay for its work on redeliveries too. The server
calls it concurrently, from every request.

It answers in one of three ways:

- **Allow:** the zero `inbox.BodyDecision`. The body is stored as received, and nothing about the
  delivery differs from a server without a screener.
- **Refuse:** return an error that wraps `ErrRateLimited` (429), `ErrQuotaExceeded`
  (402), `ErrForbidden` (403), or `ErrNotFound` (404, as for an unknown ingest key).
  This works as for `BeforeIngest`. Use `inbox.RefuseAfter` to say when to retry.
  Nothing is stored, and an `IngestObserver` is told `body_refused` with the
  status. Any other error is a `500`, and the observer is told `error`. The server
  writes the error text to the log, so it must not hold the body.
- **Replace:** set `Replace: true` and the `Body` to store, which may be empty. The
  replacement may be at most the inbox's `MaxBodyBytes`. The server does not
  truncate a longer one. It answers `500` and logs the cause. A replacement equal
  to what was received is not a modification.

**What a replacement changes.** The signature covered the bytes that were received,
so you cannot check it against the stored body. The stored message is marked
`modified` (`Message.Modified`). It shows as `modified: true` in the read API, on
the stream and in the MCP tools, and is absent otherwise. Its `verified` stays
`true`, because the sender did send it and the signature did match. `GET .../raw`
returns the stored, replaced bytes. Code that re-verifies stored messages, or
replays them to a downstream service with the original signature headers, must
skip messages with `modified`. If you use a `BodyScreener`, a `Store` you write
must keep `NewMessage.Modified` and return it as `Message.Modified`. `StoreSuite`
checks this, and skips the case for a `Store` that does not keep the field.

**Which body each part uses.**

| Part | Uses |
|---|---|
| Signature verification | The body received. |
| `MaxBodyBytes`, enforced when the body is read | The body received. The replacement is held to the same limit. |
| The ingest memory budget (`IngestMemoryBudget`) | The bytes received, as they arrive. A replacement is held beside the original until the delivery ends, so its bytes are counted too; if the budget has no room, the sender gets the `503` it would for a body, and should retry. |
| The deduplication key | The body received, so a repeat collapses into the earlier message whatever the screener makes of it, even a screener that gives a different replacement each time. |
| `event_type_pointer` | The body that is stored, so an event type never holds something the screener took out. |
| The stored size (`size`), the stored bytes, `raw` | The body that is stored. |
| `IngestEvent.Size` and the `size` in the delivery's log line | The bytes received. |

**Time and failure.** `ScreenBody` runs on the request's goroutine and must not
block, just as `BeforeIngest` and `AfterIngest` must not. The sender waits for it,
and senders give up on a slow endpoint. Give a call to a service of your own a
short deadline. Decide what happens when it fails: an error is a `500`, and the
sender retries. The server logs a panic in it, naming only the type of the value
because the value may hold the body. It answers `500` and stores nothing. This
differs from a panic in `ObserveIngest`, which the server swallows because the
outcome is already decided. A screener that redacts must not fail open and store
the unredacted body. The server gives the screener the body without a copy. The
screener must not modify it. If it edits the body in place and says to keep it,
the server refuses with a `500`, because what is stored would no longer be what
verified. Return a replacement instead.

**Wrapping hooks hides it.** Suppose a wrapper embeds the `inbox.Hooks` interface.
It hides every optional interface of the value inside, `BodyScreener` included. The
server then stores every body as received, and nothing in the log says a screener
was skipped. Embed the concrete type, or forward `ScreenBody` with a method of your
own. Test through the wrapper that a body is redacted. `examples/custom-hooks` has
a `Redactor` that does this (set `EXAMPLE_REDACT_KEYS=card,email`).

**Refusing has a cost.** Many senders retry a delivery that got a `4xx` or `5xx`.
Some disable an endpoint after repeated failures. If you refuse on content, a
payload that does not fit your schema can cause a retry storm, or a webhook that
stops sending at all. A `5xx` from a failing screener does the same for every
delivery. Where you can, accept the delivery and replace what you cannot keep, or
note it in your own records (from `AfterIngest`). Refuse only what you must never
hold. A refusal with `RefuseAfter` tells a well-behaved sender when to come back.

### Observing how deliveries end

`AfterIngest` sees only deliveries that were stored or recognised as duplicates.
`/metrics` counts outcomes only in total. To see every outcome per inbox (for your
own metrics, an audit trail or logs), also implement `inbox.IngestObserver` on the
same value:

```go
func (c *counter) ObserveIngest(ctx context.Context, ev *inbox.IngestEvent) {
	c.add(ev.Inbox.Tenant, ev.Inbox.Slug, ev.Outcome) // take a lock, add one; nothing slow
}
```

The server calls it once per delivery to a known inbox. The call comes after the
outcome is decided (and after `AfterIngest`, for a stored message or a duplicate)
and before the response is written. It runs on the request's goroutine and must
not block, because the sender waits for it. Its context may already be cancelled,
so work that outlives the call needs `context.WithoutCancel`. The server logs a
panic in it and swallows it. The response does not change.

`ev.Outcome` is one of the `inbox.Ingest*` constants: `stored`, `duplicate`,
`handshake`, `verification_failed`, `rate_limited`, `quota_exceeded` and `forbidden`
(refused by `BeforeIngest`), `body_refused` (refused by a `BodyScreener`, with the status it answered), `unverified_limit`, `storage_full`, `overloaded`,
`too_large`, `bad_request`, `not_found` (the inbox is disabled, was deleted while the
delivery was received, or `BeforeIngest` answered `ErrNotFound`; a `BodyScreener` that does is `body_refused`) and `error`. More may be added, so treat one you do not know
as a refusal. The event carries the `Host` the delivery was addressed to, as in `IngestRequest`. It never carries headers, the body, or the reason text of a
signature failure. The server logs that text. So nothing you do with the event can leak them.

When `Outcome` is `verification_failed`, `ev.VerifyFailure` says which kind of
failure it was, so an observer can tell a sender that sends no signature from one
whose signature is wrong:

| `VerifyFailure` | Meaning |
|---|---|
| `missing` | A header the verifier needs was not sent. |
| `malformed` | A header was sent but could not be read as what the verifier expects: wrong prefix or encoding, a bad timestamp, or the header sent more than once. |
| `mismatch` | The signature or token was well formed and did not match. |
| `stale` | The signature matched, but its timestamp was outside the tolerance window. The signature is checked first, so a forged delivery is `mismatch`, never `stale`. |

The field is empty for every other outcome. It is also empty for a failure from a
custom verifier that does not classify its failures (a `verify.Failure` with an
empty `Kind`). The server does not pass on a kind that a custom verifier makes up.

The observer does not see a delivery whose ingest key names no inbox, or one with
the wrong method. There is no inbox to count it against, and the observer would
otherwise see every guess anyone makes at a key.

### Checking a hook of your own

The server's own tests cover what it does with a hook's answers. Four suites check,
without a server, what holds for every implementation of a hook:

```go
inboxtest.UnverifiedLimiterSuite(t, func(t *testing.T) inbox.UnverifiedLimiter { return newLimiter(t) })
inboxtest.IngestObserverSuite(t, func(t *testing.T) inbox.IngestObserver { return newObserver(t) })
inboxtest.AdmitterSuite(t, func(t *testing.T) inbox.Admitter { return newAdmitter(t) })
inboxtest.BodyScreenerSuite(t, func(t *testing.T) inbox.BodyScreener { return newScreener(t) })
```

`UnverifiedLimiterSuite` and `IngestObserverSuite` call the hook with an ordinary
context, with one that carries `inbox.AccessFrom`, and with one that is already
cancelled. They check three things. The hook returns at once, because the server
calls both on the request's goroutine. It does not change the inbox or the event
it is given. It survives being called from many requests together. The suite calls
the observer with an event of every `inbox.Ingest*` outcome, and with one the
package does not know yet. Your own tests must check which cap a limiter answers
for which inbox, and what an observer counts. `AdmitterSuite` is described above.
`BodyScreenerSuite` calls `ScreenBody` with bodies of every kind (empty, binary,
large, JSON), for an inbox that verified and one that did not. It checks that the
screener returns at once, does not change the body or the inbox it is given, does
not answer with a replacement over the inbox's `MaxBodyBytes`, and survives
concurrent calls. Your own tests must check what it refuses and what it replaces.

No suite can reach some contract points, because they are about what the server
does. Check them in your own tests:

- A limiter's answer is the cap: positive is the cap, `0` the server's limit,
  negative none. The server asks it only for an unverified delivery. It has an
  effect only on a Store with `UnverifiedCounter`.
- An observer hears only about deliveries to a known inbox, after `AfterIngest`.
  A panic in it does not change the response.
- A screener's refusal is answered as `BeforeIngest`'s is. Its replacement is
  capped and counted as above. A panic in it stores nothing. The server's own tests
  check those.
- A `Principal` that belongs to a tenant implements `TenantPrincipal` with the same
  `Tenant` for its life; one that does not is unscoped, and reaches the default tenant.
  `StoreSuite` checks what a Store does with a scoped context, and
  `servertest.ServerSuite` checks which requests the server scopes (see
  [Checking what the server does with tenants](#checking-what-the-server-does-with-tenants)).

### Checking what the server does with tenants

Four behaviours belong to the server, not to one interface. They are part of the
contract:

- which requests a `TenantPrincipal` scopes (the read API, streams, MCP, the admin
  API and the delivery to an ingest key). This includes that a tenant's principal
  cannot name another tenant's slug, and that a principal in no tenant sees only
  the default tenant
- `Options.RequireTenant`
- the readiness check's sample of stored secrets across tenants
- `Options.Fallback`

`servertest.ServerSuite` builds a real server over your `Store` and `Authenticator`
and sends it requests, so you can check these where you embed it. It lives in the
package `github.com/hookinlet/hookinlet/pkg/server/servertest`. That package is
separate from `inboxtest` because the server's own tests import `inboxtest`.

```go
func TestMyServer(t *testing.T) {
	servertest.ServerSuite(t, servertest.Harness{
		NewStore: func(t *testing.T, now func() time.Time) inbox.Store { return openMyStore(t, now) },
		PrepareTenant: func(t *testing.T, s inbox.Store, tenant string) { registerTenant(t, s, tenant) }, // or nil
		NewAuthenticator: func(t *testing.T, s inbox.Store) (inbox.Authenticator, servertest.Callers) {
			return myAuthenticator(s), servertest.Callers{
				ReaderA:  bearer(tokenFor(t, s, "org-a")), // a TenantPrincipal of org-a that reads its inboxes
				ReaderB:  bearer(tokenFor(t, s, "org-b")),
				AdminA:   bearer(adminTokenFor(t, s, "org-a")), // optional
				Operator: bearer(operatorToken(t, s)),          // an admin in no tenant
				NoTenant: bearer(carelessToken(t, s)),          // optional: reads everything, in no tenant, not an admin
			}
		},
	})
}
```

The suite makes inboxes in `org-a`, `org-b` and the default tenant. It calls
`PrepareTenant` for the first two, as `StoreSuite` does. It then calls
`NewAuthenticator` and starts the server. `ReaderA` and `ReaderB` must be
`TenantPrincipal`s of those tenants. They read every inbox in their tenant and
cannot admin. The `Operator` is an admin that reads every inbox and is in no
tenant. `AdminA` and `NoTenant` are optional, and the cases that need them are
skipped without them. The groups are `Tenants`, `RequireTenant`, `Readiness` and
`Fallback`. `Readiness` seals secrets with the `SecretBox` from
`Harness.NewSecretBox`, or the `secretbox` package's, and makes one tenant's fail
to open. The suite is a separate function, so an `Authenticator` or `Store` that
passes its own suites does not start failing on upgrade.

## Writing a Store

The `Store` documentation in `pkg/inbox/interfaces.go` is the contract. The suite
lets you test it. These points are easy to get wrong:

- **`Seq` is a cursor.** It is per inbox. The Store assigns it in commit order, in
  the same step that makes the message visible. A reader that has seen `k` and asks
  for messages after `k` never misses one that commits later. If your database
  hands out ids at insert but commits out of order, serialize appends per inbox.
- **`Query` is exact and a snapshot.** A short page means nothing more matches up
  to the `LatestSeq` it reports. That number comes from the same snapshot as the
  rows. Readers skip ahead on that promise.
- **Read your writes.** Once `Append` returns, every later read, on any node, sees
  the message. A wake-up followed by a read must not find it missing.
- **The Store owns the clock** (UTC, microsecond precision). `ReceivedAt` never
  decreases as `Seq` grows within an inbox, even if the system clock steps back.
  `Append` uses the later of the clock and the previous message's `ReceivedAt`.
  So within one inbox, ordering by (`ReceivedAt`, `Seq`) and ordering by `Seq`
  agree, and the messages in a range of times are a range of `Seq`s. One index on
  (inbox, received_at, seq) can serve a query filtered by time (`Query.Since`,
  `Query.Until`) and paged by `Seq`. Retention by age always deletes a prefix.
- **A restore is a new epoch.** `Inbox.Epoch` must change for every inbox when you
  restore the Store from a backup, so that an old cursor is refused. A
  `Backupper`'s snapshot does this (`BackupperSuite` checks it). Suppose you
  restore a Store by any other means, such as a copied database file or a
  point-in-time recovery. Then you must renew every epoch before it serves again.
  Restoring is not a method of `Store`, so nothing does it for you. Implement
  `inbox.EpochRenewer` (`RenewEpochs(ctx) error`) and call it as the last step of
  the restore. `EpochRenewerSuite` checks three things. After a call, every inbox
  has a different epoch, so a client that holds a cursor from the old one gets
  `409 epoch_changed`. The messages, their sequence numbers, the tokens and the
  inboxes' settings are as they were. A second call is safe and renews again. The
  SQLite store has it, and `hookinlet renew-epochs` runs it. A Store that also
  implements `inbox.ReaderTracker` drops every named reader's position in the
  same step, in `RenewEpochs` and in the snapshot a `Backupper` makes. A position
  is a sequence number of one epoch, and `ReaderTrackerSuite` checks that none
  survives a renewal.
- **Scope by tenant, except tokens.** A context scoped with `inbox.WithTenant` limits
  every method to that tenant's inboxes (see [Tenants](#tenants)). The token methods
  belong to the operator. `GetTokenByHash`, `ListTokens` and `DeleteToken` ignore the
  scope. `CreateToken` only checks that a token's inboxes belong to the tenant.
- **An inbox has an ingest key** (`Inbox.IngestKey`, the `{key}` in `/in/{key}`). The
  caller supplies it when it creates the inbox. A Store does not make one up. It is
  a capability, so it should be random.
- **A modified message says so.** `NewMessage.Modified` is true when the server's
  `BodyScreener` replaced the body. Keep it with the message and return it as
  `Message.Modified`. A `Store` that drops it makes a redacted body look as if the
  sender sent it, and `StoreSuite` fails it.
- **Deduplication** applies only to verified messages, by `DedupeKey` (the
  `DeliveryID` when it is empty). So a forged delivery cannot suppress the real
  one. When a signature does not cover the id, the server builds the key from the
  id and a hash of the body.
- **Secrets stay sealed.** A Store saves `VerifierSecret` as it is given and never
  sees plaintext. It returns inboxes whose `String` and log forms hide secrets and
  token hashes.
- **`Ping` is about the whole Store.** Any error from it makes `/readyz` answer
  `503` and takes the server out of rotation. `ErrStorageFull` says there is no room
  left at all. A quota on one tenant or inbox must never show there, or every
  server sharing the Store fails readiness at once. Refuse the write instead.
  `Append` returns `ErrStorageFull` for it (`503`, and the sender retries). Or a
  hook refuses the delivery first with `ErrQuotaExceeded` (`402`).
- **Large deletes must not block writes.** `DeleteInbox` and `Prune` run in small
  batches. Deleting everything in one transaction stalls every delivery.
- **`Prune` may run at the same time as itself.** Each server in a deployment runs
  the retention sweep. So several `Store` values on one backend call `Prune`
  together, and one server can call it while another changes an inbox's retention.
  It must be safe. Deletes are per inbox and idempotent. A message that another
  call already removed is not an error. Two calls that select the same messages
  may each count them, so the counts do not add up. It leaves no gap, whatever else
  is running. If `RetentionDays` or `RetentionMaxCount` changes while it runs, it
  applies the old limit or the new one, and the next call applies the new one.
  `StoreSuite` checks this from several goroutines. It also checks on a second
  `Store` value when you give `StoreOptions.Peer`.
- **A refused write is `ErrStorageFull` and leaves nothing behind.** When there is no
  room (a full disk, a size limit), `Append` returns an error that wraps
  `inbox.ErrStorageFull`. The server answers it with `503` so the sender retries.
  `Append` stores no part of the message and uses up no sequence number. Once there
  is room again, the next message gets the next number. `StoreSuite` checks this
  when you give `StoreOptions.MakeFull`.

Run the suite from an ordinary test:

```go
func TestMyStore(t *testing.T) {
	inboxtest.StoreSuite(t, func(t *testing.T, now func() time.Time) inbox.Store {
		return openMyStore(t, now) // must use now for every timestamp it assigns
	})
}
```

The suite closes each Store itself. Three things need something only your test can
do. You give them as a `StoreOptions` after `newStore`
(`inboxtest.StoreSuite(t, newStore, inboxtest.StoreOptions{...})`, or the same call as
`inboxtest.StoreSuiteWith`):

- `PrepareTenant` makes a Store ready to hold inboxes for a tenant. Use it for a
  Store that has to be told of its tenants first. The suite calls it once for each
  of `org-a`, `org-b` and `org-c` on each Store it makes, before the first call
  that names the tenant. It does not call it for the default tenant, which every
  Store has, or for a tenant key the Store must refuse. A Store that needs no
  registration leaves it nil.

- `MakeFull` makes the Store unable to take another message (for example, a size
  limit lowered to what it holds). It returns a function that gives the Store room
  again. The suite then expects `ErrStorageFull` from `Append`, no message stored
  and no sequence number used. Left nil, that case is skipped.
- `Peer` returns a second `Store` value on the same backend. The concurrent `Prune`
  check then calls `Prune` on both, as two servers do. Left nil, the check runs on
  one value from several goroutines.

Add the suites that match what your Store can do:

- **Backups.** If your Store can take backups, add
  `inboxtest.BackupperSuite(t, newStore, restore)`. `restore` opens a Store from a
  snapshot's bytes. A snapshot holds every tenant's data. So a Store that keeps
  inboxes under tenants refuses `Backup` for a context scoped with
  `inbox.WithTenant`. It returns an error that wraps `ErrForbidden`, or gives a
  snapshot of that tenant alone, if it can. The suite checks this, and skips the
  check for a Store that is not multi-tenant.
- **Restore without a snapshot.** If your Store restores by other means than a
  snapshot, add `inboxtest.EpochRenewerSuite(t, newStore)` (see **A restore is a new
  epoch** above).
- **Counting unverified messages.** If your Store can count them cheaply (reading
  no more rows than the limit), implement `inbox.UnverifiedCounter` and add
  `inboxtest.UnverifiedCounterSuite(t, newStore)`. The server then caps the
  unverified messages an inbox keeps (see
  [verifiers](verifiers.md#unverified-messages-are-capped)).
- **Named readers.** If your Store can remember how far a named reader has read,
  implement `inbox.ReaderTracker` and add `inboxtest.ReaderTrackerSuite(t, newStore)`.
  It checks the ordering, and the bounds on `seq` and the name. It checks that
  another epoch is refused and that a cap holds under concurrent calls (the count
  and the write are one step). It checks that an unknown inbox is `ErrNotFound`,
  and that renewing the epochs and deleting the inbox drop the positions. For a
  Store that keeps tenants, it checks that one tenant's readers are invisible to
  another. The server then offers the reader operations of the
  [read API](api.md#named-readers-and-lag).

**Token expiry and last use.** An `inbox.Token` may have an `ExpiresAt` and a
`LastUsedAt`. A Store that keeps `ExpiresAt` has three duties:

- At `CreateToken`, it must refuse an expiry that is not after its own clock. Call
  `Token.ValidateAt` with that clock. `Validate` reads the system clock.
- Once the expiry has passed by that clock, `GetTokenByHash` must not return the
  token. It returns `ErrNotFound`, so an Authenticator built on the Store cannot
  serve it or tell it from one that never existed.
- `ListTokens` still returns the token, so an operator can revoke it.

The default `Authenticator` checks the expiry again. So a Store that does not
enforce it is still safe with that Authenticator, but a different one would be
wrong. A Store that does not keep `ExpiresAt` at all returns the token without one,
and the `TokenExpiry` case is skipped. The admin API still accepts an `expires_at`
for it, but the value has no effect. If your Store cannot keep it, tell the people
who run it. `LastUsedAt` is optional. It comes with `inbox.TokenUseRecorder`, whose
case skips without it.

## Wrapping a Store

To add something around a Store (tenancy, a quota, a byte count per tenant), wrap it
in a type of your own and give the server the wrapper. Besides the 19 methods of
`inbox.Store`, the server looks for four optional interfaces on the Store it is
given. It uses a type assertion, and does without each one it does not find. The
default `Authenticator` finds `inbox.TokenUseRecorder` the same way. A sixth,
`inbox.EpochRenewer`, is not looked for by the server. A restore procedure of your
own looks for it:

| Optional interface | Without it |
|---|---|
| `inbox.UnverifiedCounter` | Unverified messages are not capped: `Options.MaxUnverified` and an `UnverifiedLimiter` on your `Hooks` have no effect. Every delivery stored unverified (with `on_verify_fail=store`, or the verifier `none`) is accepted with `200`. The only sign is a warning in the log when the server starts. |
| `inbox.Backupper` | `GET /v1/admin/backup` answers `501 not_implemented`, and `hookinlet backup` says the store does not support backups. |
| `inbox.ReaderTracker` | The named-reader operations (`GET`, `PUT` and `DELETE` on `/v1/inboxes/{slug}/readers`) answer `400 unsupported`, and no consumer lag is shown. Nothing is stored and nothing breaks: a reader still keeps its own cursor. |
| `inbox.StatusReporter` | `GET /v1/admin/status` reports `schema_version` and `database_bytes` as `0` and leaves out `free_bytes`. |
| `inbox.TokenUseRecorder` | No token's last use is recorded: `Token.LastUsedAt` stays unset and the admin API leaves out `last_used_at`. Nothing fails. |
| `inbox.EpochRenewer` | Nothing in the server. Your restore procedure, which asserts the Store it holds to `EpochRenewer`, finds no way to renew the epochs and cannot tell readers holding old cursors to start again. |

The core grows by adding optional interfaces like these
([Stability](stability.md#the-go-packages)), so a later release may add to this
table.

There are two easy ways to get a wrapper wrong:

- **Embedding the interface hides them.** A `struct{ inbox.Store }` has exactly the 19
  methods of `inbox.Store`, whatever the value inside it can do. The server finds
  none of the optional interfaces. Nothing fails. The cap is silently off, backups
  answer `501`, and the status shows zeros.
- **Embedding the concrete type skips the wrapper.** A `struct{ *sqlite.Store }` keeps
  the optional interfaces, because the methods are promoted. But every method that
  the wrapper does not write itself goes straight to the inner store, past
  whatever the wrapper is for. That includes any method the concrete type gains
  later.

Embed the interface for the methods you do not change. Forward each optional
interface explicitly, and apply the wrapper's own logic where it applies:

```go
type metered struct {
	inbox.Store // the methods this wrapper does not change
	meter       *Meter
}

func (s *metered) Append(ctx context.Context, id inbox.ID, m *inbox.NewMessage) (*inbox.Message, bool, error) {
	msg, created, err := s.Store.Append(ctx, id, m)
	if err == nil && created {
		s.meter.Add(ctx, id, msg.Size) // best effort: see "Counting, copying and forwarding messages"
	}
	return msg, created, err
}

// The optional interfaces of the store inside, forwarded one by one.
func (s *metered) CountUnverified(ctx context.Context, id inbox.ID, limit int64) (int64, error) {
	return s.Store.(inbox.UnverifiedCounter).CountUnverified(ctx, id, limit)
}

func (s *metered) Backup(ctx context.Context) (io.ReadCloser, int64, error) {
	return s.Store.(inbox.Backupper).Backup(ctx)
}

func (s *metered) Status(ctx context.Context) (inbox.StoreStatus, error) {
	return s.Store.(inbox.StatusReporter).Status(ctx)
}

func (s *metered) RenewEpochs(ctx context.Context) error {
	return s.Store.(inbox.EpochRenewer).RenewEpochs(ctx)
}

func (s *metered) AckReader(ctx context.Context, id inbox.ID, name, epoch string, seq int64, maxReaders int) (inbox.ReaderPosition, error) {
	return s.Store.(inbox.ReaderTracker).AckReader(ctx, id, name, epoch, seq, maxReaders)
}

func (s *metered) Readers(ctx context.Context, id inbox.ID) ([]inbox.ReaderPosition, error) {
	return s.Store.(inbox.ReaderTracker).Readers(ctx, id)
}

func (s *metered) ForgetReader(ctx context.Context, id inbox.ID, name string) error {
	return s.Store.(inbox.ReaderTracker).ForgetReader(ctx, id, name)
}
```

Forward only the interfaces the inner store has. A method that exists but cannot
work is worse than none, because the server would call it instead of doing without.

`inboxtest.AssertStoreWrapperForwards(t, wrapper, inner)` checks a wrapper. It fails
if the wrapper lacks an optional interface that the inner store implements. The
failure names each missing interface and what the server does without it. Call it in
a test of the wrapper, with the store it wraps in production. It checks that the
methods exist, not what they do. A wrapper that embeds the concrete type passes it.

### Counting, copying and forwarding messages

- **Hooks are best effort for metering.** `AfterIngest` runs after the commit, and
  only if `Append` returned without an error. Sometimes the Store commits and
  `Append` then returns an error anyway. An example is an acknowledgement lost on
  the way back from a database. Or the server may stop between the commit and the
  hook. In both cases the message is stored, and `AfterIngest` is never called for
  it. The sender got no `200` and sends again. If the delivery is verified and has a
  dedupe key, the resend is a duplicate (`created == false`). Anything else is
  stored a second time. Either way, a count kept in `AfterIngest` falls short of
  what is stored. For an exact count, keep it inside the Store's own transaction.
  Or take it from the inbox's `LastSeq` (`Store.LatestSeq`). That number only goes
  up, a duplicate does not consume a sequence number, and it counts every message
  created, verified or not.
- **Decide on `Message.Verified`.** With `on_verify_fail=store`, the Store keeps a
  forged delivery with `Verified == false`. The server never deduplicates
  unverified messages, so each replay of one is a new message (`created == true`).
  A count of what customers are billed for, or of genuine deliveries, should look
  at `Verified`.
- **`Store.Append` does not signal the Notifier.** The server calls
  `Notifier.Publish` after a delivery that created a message. Anything else that
  appends (a Store or a hook that writes to another inbox) must call `Publish` for
  that inbox after the commit. Otherwise a reader waiting on that inbox does not see
  the message until something else wakes it. That could be the next delivery the
  server makes to that inbox. It could be `Options.SSERecheckInterval`, if you set
  it (it is off by default). For an MCP wait, it could be the wait's timeout or the
  next authorization recheck (`Options.StreamRevalidateInterval`). An SSE stream is
  not re-read on that recheck.
- **Copying a message to another inbox from `AfterIngest` is not durable.** The copy
  is a second write after the first has committed. `AfterIngest` must not block. A
  copy made in the background can still be running after the sender has its `200`.
  If the process stops then, the copy is lost and nobody sends the delivery again.
  A copy made before the response is lost too, if the process stops after the
  commit and before the copy. The sender's retry of a verified delivery is then a
  duplicate (`created == false`), which a hook that copies new messages skips. Also,
  you cannot deduplicate a copy like the original. `Message` has the `DeliveryID`
  but not the dedupe key the server stored with it (for an id the signature does
  not cover, the id and a hash of the body). A fan-out or forward that must not
  lose messages belongs inside the Store's `Append`, in the same transaction as the
  original. An embedder that owns the Store can do this. Once that transaction
  commits, the embedder calls `Publish` for the other inbox itself, as above.
- **Forwarding stored messages from a cursor.** Retention (`Store.Prune`) deletes old
  messages whether or not you have forwarded them. Hold retention back for inboxes
  you forward from, or watch for the loss. Messages were deleted before you read
  them if either of these is true. One is a page whose `OldestSeq` is above your
  cursor plus one. The other is an unfiltered page with no messages whose
  `LatestSeq` is above your cursor. Stored headers can include the sender's
  signature header (`X-Hub-Signature-256`, `Stripe-Signature`, `webhook-signature`
  and the like). The server drops only the `token` verifier's header, and
  `Authorization`, `Cookie` and `Proxy-Authorization`, before storing. A forwarder
  must not pass the signature header on as though it signed the request itself.

## Writing a Notifier

A notifier carries no data. It says "look again", and the reader asks the Store.
So replay, reconnects, and slow readers share one code path. A missed signal costs
delay, never data.

```go
func TestMyNotifier(t *testing.T) {
	inboxtest.NotifierSuite(t, func(t *testing.T) inbox.Notifier { return newMyNotifier(t) })
}
```

An in-process notifier must not drop signals. One that crosses the network may
drop them (delivery at most once). If yours does, run the server with a positive
`HOOKINLET_SSE_RECHECK_INTERVAL` (`Options.SSERecheckInterval`). That makes every
stream and MCP wait re-read the Store on that interval regardless. The server calls
`Publish` with a context that may already be cancelled, and `Publish` must still
deliver.

`CancellingReleasesEverythingItHeld` counts goroutines before and after a thousand
subscriptions are made and cancelled. A notifier that opens its backend connection
the first time it is used therefore looks like a leak. Open the connection before
the check (in the function that makes the notifier), or close it when the notifier
is closed.

Readers subscribe first and read the Store second. So a subscription has to be live
when `Subscribe` returns: every `Publish` that begins after that must reach it. A
notifier that crosses the network waits for its backend to confirm the subscription
before returning, but only briefly, because `Subscribe` must not block the caller.
(For example, it waits for the reply to a pub/sub `SUBSCRIBE`.) If it returns
before the confirmation, a signal published in between can be lost. This is the
at-most-once delivery that the recheck interval covers. The reader sees the message
on its next re-read.

Signals coalesce (merge into one) when they arrive before the subscriber reads. A
notifier that crosses the network delivers a burst of publishes spread over time.
A subscriber that reads the first at once sees the later ones as new signals, as it
should. Run such a notifier with
`inboxtest.NotifierSuiteWith(t, newNotifier, inboxtest.NotifierOptions{Settle: d})`.
Here `d` is how long a burst of a thousand publishes takes to arrive. The
coalescing check then waits that long before it reads, and still requires one
signal.

The suite runs one notifier. Take a notifier whose instances reach each other
through a backend. It can lose a wake-up between instances and still pass a suite
that has only one. Give `NotifierOptions.Peer` a function that returns a second
notifier on the same backend as the first. The suite then also checks three things.
A `Publish` on either instance wakes the subscribers of both. Another inbox's
subscribers are not woken. Cancelling a subscription on one instance leaves the
other's alone. Left nil, the suite runs as before. The in-process notifier has one
instance, so its test passes the notifier itself as its own peer.

## Writing a SecretBox

A `SecretBox` seals a verifier secret when an inbox is created or its secret is
set. It opens the secret on every delivery to an inbox that has one, on each
`/readyz` sample and in `Server.CheckSecrets`. The server does not cache what it
opens. If your box asks a key service for a data key, the box should cache the key
itself, for as long as it judges safe.

```go
inboxtest.SecretBoxSuite(t, func(t *testing.T) inbox.SecretBox { return newMyBox(t) })
```

A box that does I/O should also implement `inbox.ContextSecretBox`. Its
`SealContext` and `OpenContext` get the context of the work. The server then calls
them instead of `Seal` and `Open`. They must give the same results. They must give
up with an error that wraps `ctx.Err()` once the context ends.

The server always gives the box a context scoped (`inbox.WithTenant`) to the tenant
of the inbox whose secret it is. So `inbox.TenantFrom` reports that tenant and
`scoped` true, and a box that keeps a key per tenant can pick the right key. An
inbox in the default tenant is scoped to `""`. That is not the same as not scoped:
a box can tell the default tenant from a context that says nothing. The scoping is
for the box only. The Store calls of the same work keep their own context. A
delivery's are not scoped at all, because the server finds the inbox by its ingest
key. The context is:

- **A delivery**: the request's context, cancelled when the sender goes away.
- **The admin API** (creating an inbox with a secret, setting one): the request's
  context. The tenant is the one the inbox is made in or belongs to. For a
  `TenantPrincipal` that is the caller's tenant. For a caller in none, it is the
  default tenant. The box is given the same tenant when a delivery later opens the
  secret.
- **`/readyz`**: the check's own context. It outlives the probe and has a deadline
  of 20 seconds for the whole sample. The server does not treat a check cut short
  by that deadline as secrets that do not open. `/readyz` answers `503` and says
  that checking the secrets timed out (the key service may be slow or down). It
  logs a warning and checks again after 5 seconds.
- **`Server.CheckSecrets`** and `hookinlet rekey`: the caller's context (`hookinlet
  serve` passes `context.Background()`).

Code of your own that seals or opens with a box should call
`inbox.SealWith(ctx, box, …)` and `inbox.OpenWith(ctx, box, …)`. They pass the
context to a box that takes one. To check that a box honours the context:

```go
inboxtest.ContextSecretBoxSuite(t, func(t *testing.T) inbox.SecretBox { return newMyBox(t) })
```

## Writing an Authenticator

`Authenticate` runs on every request, before the server knows whether the caller
may see anything. It must not reveal why it refused. Every failure looks the same
and wraps `inbox.ErrUnauthenticated`. The server reads any other error to mean it
could not check the credential, and answers 500. A `Principal` decides by inbox
**ID**, never by slug or name. A credential for one inbox must not reveal whether
another exists (the server answers 404 for both). One refusal may be a `404`: the
one for a host that serves no org. See
[Answering 404 for a host that serves no org](#answering-404-for-a-host-that-serves-no-org).

```go
inboxtest.AuthenticatorSuite(t, func(t *testing.T) inboxtest.AuthFixture { … })
```

If your credentials can expire, set `AuthFixture.Expire`. It makes one of the
fixture's tokens expire. The suite then checks that the next call refuses it with
the same error text as a token that never existed. A caller must not be able to
tell the two apart. Left nil, that case is skipped.

### Choosing the 401 challenge

A `401` carries `WWW-Authenticate: Bearer realm="hookinlet"`. Suppose the error your
`Authenticate` returns (or any error it wraps, found with `errors.As`) also has a
`WWWAuthenticate() string` method (`inbox.Challenger`). The server then sends that
string instead. This applies to every `401` the server writes: the read API, the
admin API, a stream, the MCP endpoint, and the `405` that checks the credential
first.

A self-hoster who puts an OAuth server in front can use this to point clients to
the discovery document:

```go
type needsLogin struct{}

func (needsLogin) Error() string { return "unauthenticated" }
func (needsLogin) Unwrap() error { return inbox.ErrUnauthenticated }
func (needsLogin) WWWAuthenticate() string {
	return `Bearer resource_metadata="https://hooks.example.com/.well-known/oauth-protected-resource"`
}
```

(Serve that document with [`Options.Fallback`](#serving-other-paths-next-to-the-api).)

The server sends the string only if all of these hold:

- It is one line of printable ASCII (no control characters, tab included).
- It is not empty, and is at most 4096 bytes.
- It starts with an HTTP authentication scheme: a token as RFC 9110 defines it,
  such as `Bearer`. Nothing follows, or a space and the parameters.

The server does not parse the parameters. If the string fails any rule, the server
sends `Bearer realm="hookinlet"` and logs a warning, once. The same string goes to
every caller who is refused, so do not build it from the request's credentials. An
error that does not wrap `ErrUnauthenticated` is still a server error, whatever
else it has.

`inboxtest.ChallengerSuite(t, func(t *testing.T) inbox.Challenger { return yourError })`
checks the value against these rules. A mistake that would send the server's own
challenge then shows up in your tests, not in a warning in the log. The suite also
checks that the value is the same on every call, and that the error wraps
`ErrUnauthenticated`.

### Answering 404 for a host that serves no org

An embedder may serve many parties from one process and tell them apart by the
request's host. Some requests then arrive for hosts that serve nobody. A `401` is
the wrong answer for those. It invites the caller to authenticate to something that
does not exist. Suppose the error your `Authenticate` returns (or any error it
wraps) also has an `UnroutedHost() bool` method (`inbox.Unrouted`) that returns
true. The server then answers `404` instead. The body is the same as its other
`404`s (`{"error":{"code":"not_found","message":"not found"}}`), with no
`WWW-Authenticate` header. This applies wherever the server would write a `401`:
the read API, the admin API, a stream, the MCP endpoint, and the `405` that checks
the credential first.

```go
type noSuchHost struct{}

func (noSuchHost) Error() string      { return "unauthenticated" }
func (noSuchHost) Unwrap() error      { return inbox.ErrUnauthenticated }
func (noSuchHost) UnroutedHost() bool { return true }
```

- **The answer is about the host, not the credential.** Return it for every request
  for such a host, whatever the request carries: a valid token, an invalid one, or
  none. The `404` must be the same for all of them, so that it says nothing about
  any token. On a host that does serve an org, a bad credential is still the same
  `401` for every cause, and must not report true. What the caller presents must
  never let anyone tell a host that serves no org from a missing org.
- **It must wrap `ErrUnauthenticated`.** An error that only has the method is still
  a server error. So is a bare `inbox.ErrNotFound`. Returning `ErrNotFound` from
  `Authenticate` does not produce a `404`. An `Authenticator` that returns it
  today keeps getting a `500`.
- **What it discloses.** The `404` tells anyone who can reach the server that a host
  serves no org. The `401` of a host that does serve one would tell them it does.
  That is fine when host names are not secret. (A wildcard DNS name tried by anyone
  is guessable anyway.) Do not use it where the set of hosts that serve an org is
  itself private. It must reveal nothing more: not whether a token exists, expired,
  or belongs to another org.
- **No challenge.** The `404` has no `WWW-Authenticate`. A client that learns how to
  authenticate from the challenge (an OAuth client reading `resource_metadata`)
  learns nothing on such a host.
- **Rate limiting still applies.** `Authenticate` runs before the server knows
  anything about the caller, and it still runs for these requests. Put the same
  rate limit in front of it. A host that answers `404` is just as cheap for a
  scanner to try as one that answers `401`.
- **Ingest and the health checks do not authenticate**, so this does not change what
  they answer for such a host. Route those yourself if they should not answer there.

`inboxtest.AuthenticatorSuite` checks this when the fixture sets `UnroutedHost` (a
host that serves no org). Every caller of that host, valid token included, gets no
principal, `ErrUnauthenticated`, the same error text, and the same answer to
`Unrouted`. The suite also checks, with or without that field, that a bad
credential on any other host never asks for a `404`. It checks that those refusals
stay identical to each other.

## Serving other paths next to the API

`Options.Fallback` is an `http.Handler` that answers requests for paths the server
has no route for, in place of the fixed `404`. Use it when a self-hosted server
also serves a file under `/.well-known/`, or a small static site, at the same
address:

```go
mux := http.NewServeMux()
mux.Handle("/.well-known/oauth-protected-resource", metadataDocument)
mux.Handle("/", http.FileServer(http.Dir("./public")))

srv, err := server.New(server.Options{ /* … */ Fallback: mux })
```

- **What reaches it.** Any request whose first path segment is not one the server
  uses. Those segments are `in`, `v1`, `mcp`, `healthz`, `readyz`, `metrics` and
  `admin`. A request under one of them gets the server's own answer, whether or not
  the server serves that path in this configuration. With `DisableAdmin` set,
  `/v1/admin/...` and `/admin` still answer the same `404` as before. A server with
  no `Authenticator` still answers `404` for the read API and MCP. The root path `/`
  and paths such as `/robots.txt` or `/.well-known/...` go to the `Fallback`.
- **Without it** (the default), every unrouted path answers
  `{"error":{"code":"not_found","message":"not found"}}` with status `404`, as
  before.
- **Which listener.** With `AdminAddr` set, the `Fallback` serves on the public
  listener only. The admin listener keeps its `404`.
- **No authentication.** The server does not authenticate requests to the
  `Fallback`, so it must serve nothing private. If it needs a credential, check it
  yourself.
- **Middleware and limits.** The `Fallback` runs inside the server's request ID,
  access log and panic recovery (a panic becomes a JSON `500`). The server-wide
  limits apply:
  - Request headers are limited by `MaxHeaderBytes` and must arrive within 10
    seconds.
  - A request with a body must deliver it within `ReadBodyTimeout` (60 seconds).
  - The client must take the response within `ResponseTimeout` (2 minutes from when
    the request arrived). After that, the write fails. A handler that streams for
    longer can move the write deadline with
    `http.NewResponseController(w).SetWriteDeadline`.
  - The body is capped at 1 MiB. `Options.FallbackMaxBodyBytes` changes that (a
    negative number removes the cap). A read past the cap fails with
    `*http.MaxBytesError`.

  Responses carry `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`.
  The handler can set its own `Cache-Control` for files that may be cached.
- **Shutdown.** `Run` and `Serve` wait for a `Fallback` request that is in progress,
  up to `ShutdownTimeout` (10 seconds by default). Then they close its connection.
  If you mount `srv.Handler()` in your own server, `srv.Shutdown(ctx)` waits up to
  the deadline of `ctx`. The server tells its own streams and MCP waits to finish.
  It cannot tell a `Fallback` handler to finish. That handler sees its request's
  context cancelled only when the client goes away or the connection is closed.
  During the drain delay before shutdown, `/readyz` reports not ready, and the
  `Fallback` keeps answering.
- **With `Handler()`.** The `Fallback` is part of the returned handler. It works the
  same when you mount `srv.Handler()` yourself. The deadlines then depend on the
  server you mount it in.
- **Metrics.** The `Fallback`'s responses are counted under the route `/`, which
  is where unrouted requests were already counted.

## Adding a verifier

```go
reg := verify.Default()
err := reg.Register("acme-v2", func(config json.RawMessage) (verify.Verifier, error) { … })
srv, _ := server.New(server.Options{ /* … */ Verifiers: reg })
```

See [verifiers](verifiers.md) for what a `Verifier` must do. It must check the exact
bytes received. It must compare in constant time (the comparison takes the same
time whatever the input). It must say which headers carry its credential, so the
server does not store them.

## Rules the server keeps, and expects of you

- Handlers reach storage, notification, authentication, and secrets only through
  these interfaces. The only exception is the optional capabilities above.
- Never log message bodies, tokens, secrets, or the headers that carried them.
- Compare secrets with `crypto/subtle` or `hmac.Equal`.
- Honor context cancellation everywhere. A cancelled call leaves no partial effect.
