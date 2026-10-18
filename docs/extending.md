# Extending hookinlet

The `hookinlet` program is a thin assembly of parts. Each part sits behind an
interface in `pkg/inbox`, and `server.New` takes them as options, so an
application can replace any of them, or add behaviour around them, without
forking. This is also how the hosted product is built on this code: it changes
nothing here, it supplies its own implementations. The same door is open to you.

The packages are pre-1.0: the interfaces can still change, and every change to
them is listed in `CHANGELOG.md`. After 1.0.0 semantic versioning applies.

## The parts

| Interface (`pkg/inbox`) | What it does | Default | Conformance suite (`pkg/inboxtest`) |
|---|---|---|---|
| `Store` | Persists inboxes and messages; assigns each message its per-inbox `Seq`. | `pkg/store/sqlite` | `StoreSuite` |
| `Notifier` | Tells readers an inbox may have new messages. Carries no payload. | `pkg/notify/local` (in process) | `NotifierSuite` |
| `Authenticator`, `Principal` | Says who a request is and what it may read or administer. | `pkg/auth/token` | `AuthenticatorSuite` |
| `SecretBox` | Seals and opens verifier secrets, bound to a context: seal a verifier secret with `inbox.VerifierSecretContext(inbox.UID)`, or the server cannot open it. | `pkg/secretbox` | `SecretBoxSuite` |
| `ContextSecretBox` (optional, of `SecretBox`) | `SealContext` and `OpenContext`: `Seal` and `Open` given the context of the work, so a box that asks a key service can stop when it is cancelled or past its deadline, read what the embedder put on it, and see the inbox's tenant (`inbox.TenantFrom`). The server calls them in place of `Seal` and `Open`. See [Writing a SecretBox](#writing-a-secretbox). | `pkg/secretbox` (which ignores the context) | `SecretBoxSuite`, and `ContextSecretBoxSuite` for a box that does I/O |
| `Hooks` | Runs before and after ingest: reject a delivery, count it. | `inbox.NopHooks` | none: the default does nothing |
| `Admitter` (optional, of `Hooks`) | Asked before every read, stream and MCP wait, with the caller, the inbox and an `AccessKind`; may refuse (`ErrRateLimited` 429, `ErrQuotaExceeded` 402, `ErrForbidden` 403) or return a release function, called when the access ends. | none: without it everything is admitted | `AdmitterSuite` |
| `Backupper` (optional) | A consistent snapshot of the Store, for `GET /v1/admin/backup`; the size may be -1 if the Store streams it. | the SQLite store | `BackupperSuite` |
| `UnverifiedLimiter` (optional, of `Hooks`) | Says, per inbox, how many unverified messages it may keep, in place of `Options.MaxUnverified`: a positive number is the cap, 0 the server's limit, a negative number no cap. For caps that depend on something the server does not know, such as a plan. Needs a Store with `UnverifiedCounter`. | none: every inbox gets the server's limit | the server's tests |
| `IngestObserver` (optional, of `Hooks`) | Told how every delivery to a known inbox ended (`IngestEvent`: the inbox, an `IngestOutcome` such as `stored` or `verification_failed`, the status, size, source address, the host it was addressed to, for a stored message its `Seq` and whether it is verified, and for a failed signature a `VerifyFailure` kind), including refused and failed ones that `AfterIngest` never sees. For counts, audit or logs per inbox or tenant. See [Observing how deliveries end](#observing-how-deliveries-end). | none: nothing is observed | the server's tests |
| `UnverifiedCounter` (optional) | How many unverified messages an inbox holds (bounded by a limit), so the server can cap them (`Options.MaxUnverified`). Without it the server does not cap, and says so when it starts. | the SQLite store | `UnverifiedCounterSuite` |
| `StatusReporter` (optional) | The schema version, the size of the data and the free space, for `GET /v1/admin/status` and the console. | the SQLite store | none |
| `Challenger` (optional, of the error `Authenticate` returns) | `WWWAuthenticate() string`: the `WWW-Authenticate` value for the `401`, in place of `Bearer realm="hookinlet"`. See [Choosing the 401 challenge](#choosing-the-401-challenge). | none: the server sends its own | the server's tests |
| `ScopedPrincipal` (optional) | A `Principal` that can read a known list of inboxes, so listing does not have to search. `InboxIDs(ctx)` may look the list up, and may fail. | the agent principal | checked by `AuthenticatorSuite` |
| `TenantPrincipal` (optional) | A `Principal` that belongs to one tenant. After authenticating, the server scopes the request's context to it (`inbox.WithTenant`), so slugs are looked up in that tenant and nothing outside it is reachable. See [Tenants](#tenants). | none: the shipped principals are not in a tenant | `StoreSuite` (the `Tenants` case) and the server's tests |
| `verify.Registry` | Maps verifier names to signature checks. | `verify.Default()` | the package's own tests |

The optional ones are found with a type assertion, and the server copes when they
are absent: no `Backupper` means `GET /v1/admin/backup` answers `501`, `Hooks` without `Admitter` admits every read, stream and wait, `Hooks` without `IngestObserver` is told nothing more than `AfterIngest` says, and a `SecretBox` without `ContextSecretBox` is called with `Seal` and `Open`, without a context. The server's own limits (`MaxConcurrentReads`, `MaxStreams`, `MaxStreamsPerInbox`) apply after `Admit`, whatever it says.

`/readyz` opens a sample of the stored verifier secrets to catch a master key that is not the one the data was sealed under, and answers `503` only if none of them opens. The sample is up to 25 secrets, at most 3 of each tenant before more of any one, oldest first; it looks at no more than the 5000 oldest inboxes, and, once it has 25 secrets in hand, at no more than 500 further inboxes for other tenants; the answer is kept for 30 seconds (5 while it is failing). So with a `SecretBox` that keeps a key per tenant, one tenant whose key is gone is logged (with the tenant, how many of its secrets did not open, and a few slugs) but does not take the server out of rotation for the others, as long as another tenant has a secret among the inboxes it looks at (with many tenants, those of the oldest inboxes get the places). A server whose inboxes with a secret all belong to one tenant cannot tell that tenant's key being gone from a wrong master key, and answers `503` for both. A `SecretBox` whose opens are expensive, such as one that asks a key service, can turn the check off with `Options.SkipReadinessSecretCheck` and report its own health; if the sample takes more than 20 seconds, `/readyz` answers `503` saying that checking the secrets timed out, and checks again after 5. `Server.CheckSecrets`, which the `hookinlet` program runs before it listens, always opens every secret of every tenant, and fails for any one that does not open.

`GET /metrics` is part of the admin API and needs the admin token. To serve the same series somewhere else, for an operator's scraper and without the admin API on that address, mount `Server.MetricsHandler()` on a listener or a route of your own. It does no authentication; protecting where it is mounted is up to you.

## Tenants

A Store may hold the inboxes of several independent parties, each under its own
tenant key (`Inbox.Tenant`, an opaque string, empty by default). This is how a hosted
service keeps its customers' inboxes apart while running one server. An installation
that never uses it has one tenant, the empty one, and sees no difference.

- A **context scoped to a tenant** (`inbox.WithTenant(ctx, "acme")`) limits every
  `Store` call to it: an inbox of another tenant is `ErrNotFound` whether it is
  named by ID, slug or ingest key, listings leave it out, and `CreateInbox` makes the
  inbox in that tenant. Two tenants can both have an inbox called `alerts`.
- A **context that is not scoped** is the operator's view. A delivery, the retention
  sweep and the command line use it: IDs and ingest keys reach every inbox, and a slug
  names an inbox in the default tenant. `CreateInbox` makes the inbox in
  `Inbox.Tenant`, so provisioning code can create one for any tenant.
- **The server scopes the context for you.** If the `Principal` your `Authenticator`
  returns implements `TenantPrincipal`, every Store call made for that request (the
  read API, the admin API, streams, MCP) runs in its tenant. Your `Principal` is still
  what decides which of those inboxes the caller may read (`CanReadInbox`).
- **A tenant's admin manages its tenant's inboxes and nothing else.** A
  `TenantPrincipal` whose `CanAdmin` is true may create, list, show, change and
  delete its tenant's inboxes, set their verifier secrets and rotate their ingest
  keys; another tenant's inbox is `404`, as if it did not exist. The admin operations
  that act on the whole store are the operator's (an admin that is not a
  `TenantPrincipal`): tokens (a token has no tenant), backups, running retention,
  `/v1/admin/status` and `/metrics`. A tenant's admin gets `403` with code
  `forbidden` and nothing is done; the log gets a warning naming the operation and
  the principal's Go type. Each admin operation's `x-admin-scope` in
  `docs/openapi.json` says which kind it is. A hosted service should give its
  customers principals whose `CanAdmin` is false, and keep admin to its own
  operator principal.
- **A principal that is not a `TenantPrincipal` is not scoped**, so one belonging to a
  tenant must implement it. The shipped token authenticator does not: it is for one
  tenant, and its all-inboxes tokens read every inbox in the Store.
- **`Options.RequireTenant` makes a forgotten `Tenant()` fail closed.** Without it, a
  customer's principal that does not implement `TenantPrincipal` is unscoped: it names
  inboxes in the default tenant, and MCP's `list_inboxes` shows it every tenant's
  inboxes that its `CanReadInbox` allows. With it, the server refuses a principal that
  is neither a `TenantPrincipal` nor an admin (`CanAdmin`), on the read API, streams
  (also when an open stream or MCP wait is checked again), MCP and the admin API. The
  caller gets `500` with code `internal`, which says nothing of why; the log gets an
  error that names the principal's Go type, and never the credential. An admin in no
  tenant is still served, because that is how an operator gets its view across tenants;
  an admin that is a `TenantPrincipal` keeps to its tenant as before. A hosted service
  whose callers all belong to tenants should turn it on. It is off by default, and with
  an `Authenticator` whose principals are never `TenantPrincipal`s, such as the shipped
  token one, it would refuse every caller but the admin.
- Ingest keys and UIDs are unique across tenants, since an ingest key alone finds the
  inbox a delivery is for. A `Store` you write must keep to all of this; the `Tenants`
  case of `StoreSuite` checks it, including that one tenant cannot read, write, list or
  delete another's inbox by any method.
- **Tokens are the operator's, not a tenant's.** An `inbox.Token` has no tenant.
  `GetTokenByHash`, `ListTokens` (its `InboxID` filter included) and `DeleteToken`
  act on every token whether or not the context is scoped, exactly as they do
  unscoped; `GetTokenByHash` has to, because the server calls it to authenticate a
  request before it knows the tenant. The one check a scope adds is `CreateToken`'s:
  every inbox in `InboxIDs` must be one of the tenant's (`ErrNotFound` otherwise). An
  `AllInboxes` token is not limited to the tenant and reads every inbox in the Store.
  The `TokensAreNotScopedByTenant` case of `StoreSuite` checks this. So the agent
  tokens of `pkg/auth/token`, and the admin API's `/v1/admin/tokens`, which works
  through these methods, are for the operator: the server refuses a tenant's admin (a `TenantPrincipal`
  whose `CanAdmin` is true) on `/v1/admin/tokens`, because through these methods it
  could list tokens of other tenants, revoke any token by its `id`, or issue an
  `all_inboxes` token that reads every tenant's inboxes. An embedder that gives each
  tenant's callers credentials of their own keeps them in its own `Authenticator`,
  whose principals are `TenantPrincipal`s.

## Putting them together

`server.New` takes an `Options`. Only `Store` is required (and `Addr`, if you call `Run`), but the
read API, the admin API, SSE and MCP are served only when there is an
`Authenticator`, since there is no safe default for who may read.

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

`examples/custom-hooks` is a complete program built exactly this way, with a
per-inbox rate limit as the hook and an `IngestObserver` that counts how deliveries
ended, per inbox. It is compiled and tested in CI; copy it as a
starting point. `Options` has the rest (limits, timeouts, `TrustedProxies`,
`PublicBaseURL`, a `Verifiers` registry); each field says what it does and what
its default is.

`srv.Handler()` returns the whole `http.Handler`, if you would rather mount it in
a server of your own than call `Run` (no `Addr` is needed then). `Run` and `Serve` do
two things the handler alone does not: they start the retention schedule, and on
shutdown they end open streams and MCP waits first. If you serve the handler yourself,
run `srv.Maintain(ctx)` in a goroutine for the schedule (it also finishes any delete
that was cut short), and call `srv.Shutdown(ctx)` to end the streams.

Every numeric and duration option follows one rule, written in `Options`' comment and
in [Stability](stability.md#the-go-packages): zero is the default, and a negative
number is an error unless it turns a bound off (`RetentionInterval`,
`IngestMemoryBudget`).

## Writing hooks

`Hooks` has two methods; embed `inbox.NopHooks` to implement only one.

```go
type limiter struct{ inbox.NopHooks }

func (limiter) BeforeIngest(ctx context.Context, req *inbox.IngestRequest) error {
	if tooMany(req.Inbox, req.SourceIP) {
		return fmt.Errorf("%w: slow down", inbox.ErrRateLimited) // 429
	}
	return nil
}
```

`BeforeIngest` gets an `*inbox.IngestRequest`: the inbox, the body size, the source
address (as recorded, honoring trusted proxies), the host the delivery was addressed to
(see below), the method, the path after the inbox key, and the request headers
(read-only). It runs after the inbox is found
and the body is read, before the
signature is checked, so a refusal is cheap. That has a cost to weigh: it sees
unverified requests too, so a limit that counts every request lets anyone who knows
an inbox's URL use up its allowance with unsigned posts, and the real sender,
which may not retry, is then refused. Limit by something the stranger cannot
spend (the source address of unverified traffic, say), or meter what was accepted
in `AfterIngest`, or accept the trade-off knowingly, as the example does for
brevity. Return an error wrapping
`ErrRateLimited` (429), `ErrQuotaExceeded` (402), or `ErrForbidden` (403), or
`ErrNotFound` to answer 404 (see below); any other error is a 500, so an error from
a lookup of your own that happens to wrap `ErrNotFound` is a 404 and no longer a 500.
It is called concurrently, from every request.

### Which host a delivery arrived on

`req.Host` is the `Host` header of the request as the server received it, in lower
case, with a trailing dot taken off the name, and with the port only if the header
had one: `hooks.example.com`, `hooks.example.com:8443`, `[::1]:8080`. It is empty if
the request sent no `Host`. `X-Forwarded-Host`, `Forwarded` and the like are never
read, because anyone can send them. If a proxy in front of the server rewrites `Host`,
`req.Host` is what the proxy sent; having the proxy pass the original on is up to
the embedder. The sender chooses the value, so compare it with the hostnames you
serve and do not use it unchecked as a key, a label or a file name.

An embedder that serves several hostnames can use it to answer only for the right one.
An inbox that exists but is not served on the host the delivery arrived on gets
`ErrNotFound`:

```go
func (h hosts) BeforeIngest(ctx context.Context, req *inbox.IngestRequest) error {
	if !h.serves(req.Inbox, req.Host) {
		return inbox.ErrNotFound // 404, the same as an unknown ingest key
	}
	return nil
}
```

The server then answers exactly as it does for an ingest key that names no inbox: the
same status, headers (apart from the request id) and body, with no `Retry-After`, and
no log line above debug level. The hook runs after the body is read, so the sender
sees the same answer but not in the same time; and `IngestObserver` is told of it as
`not_found`, which an unknown key is not. `AfterIngest` has no host, because its
arguments are fixed; `IngestEvent.Host` carries it to an observer.

`AfterIngest` runs after the message is committed (or recognized as a duplicate,
with `created == false`, so a meter does not count it twice). It must not block,
and its context may be cancelled as soon as the response is sent, so work that
outlives the call needs `context.WithoutCancel`. A panic in it is logged and
swallowed: the delivery is already stored, and the sender must not be told it
failed.


### Admitting reads, streams and waits

`Hooks` covers ingest. To limit what a caller may read, also implement `inbox.Admitter`
on the same value; the server finds it with a type assertion, so `Hooks` itself does
not change.

```go
func (l *limiter) Admit(ctx context.Context, p inbox.Principal, in *inbox.Inbox, kind inbox.AccessKind) (func(), error) {
	if !l.take(p, kind) {
		return nil, fmt.Errorf("%w: too many %ss", inbox.ErrQuotaExceeded, kind) // 402
	}
	return func() { l.give(p, kind) }, nil
}
```

`Admit` runs once per access, after the caller is authenticated and may read the inbox,
and before the access takes a slot of the server's own, so a refusal costs nothing.
`kind` is `AccessRead` (a read that answers at once, including the MCP query, get and
list tools and a wait with a timeout of zero; `in` is nil for the list tool),
`AccessStream` (an SSE stream) or `AccessWait` (an MCP wait). Refuse with an error
wrapping `ErrRateLimited` (429), `ErrQuotaExceeded` (402) or `ErrForbidden` (403); over
MCP the agent is told the same in words. Any other error is a 500. The release
function, which may be nil, is called once when the access ends: for a read, when the
answer has been written (or, for a read that is not held that long, when the handler
returns); for a stream or wait, when it ends. The server's own limits apply after
`Admit` whatever it answers. `inboxtest.AdmitterSuite` checks an implementation.

### Saying when to retry

A refusal from `BeforeIngest` or `Admit` can say when to come back: wrap it with
`inbox.RefuseAfter(err, d)`, or return your own error type with a
`RetryAfter() time.Duration` method (`inbox.RetryAfterer`, found with `errors.As`).

```go
return nil, inbox.RefuseAfter(fmt.Errorf("%w: plan limit", inbox.ErrRateLimited), 90*time.Second)
```

The `429`, `402` or `403` then carries `Retry-After` in whole seconds, rounded up, at
least 1 and at most 86400 (a day); a duration that is not positive sends none, and so
does an error that is not one of the three refusals. Over MCP the tool error says it
instead, as `rate_limited: too many requests; retry after 90 s`. Neither the HTTP
answer nor the tool error carries your error's text.

### Which request a hook is called for

`inbox.AccessFrom(ctx)` tells `Admit`, `BeforeIngest`, `AfterIngest`, `ObserveIngest`
and `MaxUnverified` which request they are called for, for an audit trail:

- `Operation` is the operation's `operationId` in [the OpenAPI document](openapi.json)
  (`getInbox`, `headGetInbox`, `listMessages`, `headListMessages`, `getMessage`,
  `headGetMessage`, `getMessageRaw`, `headGetMessageRaw`, `streamMessages`, `ingest`,
  `ingestPut`, `ingestWithPath`, `ingestPutWithPath`), or `mcp.` and the tool's name for
  an MCP tool call (`mcp.list_inboxes`, `mcp.query_messages`, `mcp.get_message`,
  `mcp.wait_for_messages`). It is a fixed name, never a path.
- `RequestID` is the request's `X-Request-Id`, the `request_id` of its access log line.

It is false outside a request. `inbox.WithAccess` makes a context that carries one, for
testing a hook.

### Observing how deliveries end

`AfterIngest` sees only deliveries that were stored or recognised as duplicates, and
`/metrics` counts outcomes only in total. To see every outcome per inbox (for your own
metrics, an audit trail or logs), also implement `inbox.IngestObserver` on the same
value:

```go
func (c *counter) ObserveIngest(ctx context.Context, ev *inbox.IngestEvent) {
	c.add(ev.Inbox.Tenant, ev.Inbox.Slug, ev.Outcome) // take a lock, add one; nothing slow
}
```

It is called once per delivery to a known inbox, after the outcome is decided (and
after `AfterIngest`, for a message that was stored or a duplicate), before the
response is written, on the request's goroutine: it must not block, since the sender
waits for it. Its context may be cancelled already, so work that outlives the call
needs `context.WithoutCancel`. A panic in it is logged and swallowed, and the response
does not change.

`ev.Outcome` is one of the `inbox.Ingest*` constants: `stored`, `duplicate`,
`handshake`, `verification_failed`, `rate_limited`, `quota_exceeded` and `forbidden`
(refused by `BeforeIngest`), `unverified_limit`, `storage_full`, `overloaded`,
`too_large`, `bad_request`, `not_found` (the inbox is disabled, was deleted while the
delivery was received, or `BeforeIngest` answered `ErrNotFound`) and `error`. More may be added, so treat one you do not know
as a refusal. The event carries the `Host` the delivery was addressed to, as in `IngestRequest`. It never carries headers, the body, or the reason text of a
signature failure (the server logs that), so whatever you do with it cannot leak them.

When `Outcome` is `verification_failed`, `ev.VerifyFailure` says which kind of
failure it was, so an observer can tell a sender that sends no signature from one
whose signature is wrong:

| `VerifyFailure` | Meaning |
|---|---|
| `missing` | A header the verifier needs was not sent. |
| `malformed` | A header was sent but could not be read as what the verifier expects: wrong prefix or encoding, a bad timestamp, or the header sent more than once. |
| `mismatch` | The signature or token was well formed and did not match. |
| `stale` | The signature matched, but its timestamp was outside the tolerance window. The signature is checked first, so a forged delivery is `mismatch`, never `stale`. |

The field is empty for every other outcome, and for a failure from a custom
verifier that does not classify its failures (a `verify.Failure` with an empty
`Kind`). A kind a custom verifier makes up is not passed on.

A delivery whose ingest key names no inbox is not observed, nor is one with the wrong
method: there is no inbox to count it against, and an observer would otherwise see
every guess anyone makes at a key.

## Writing a Store

The `Store` documentation in `pkg/inbox/interfaces.go` is the contract; the
suite is what makes it testable. The points that are easy to get wrong:

- **`Seq` is a cursor.** It is per inbox and assigned in commit order, in the
  same step that makes the message visible, so a reader that has seen `k` and asks
  for messages after `k` never misses one that commits later. On a database that
  hands out ids at insert but commits out of order, serialize appends per inbox.
- **`Query` is exact and a snapshot.** A short page means nothing more matches up
  to the `LatestSeq` it reports, and that number comes from the same snapshot as
  the rows. Readers skip ahead on that promise.
- **Read your writes.** Once `Append` returns, every later read, on any node, sees
  the message: a wake-up followed by a read must not find it missing.
- **The Store owns the clock** (UTC, microsecond precision), and `ReceivedAt`
  never decreases with `Seq` within an inbox, even if the system clock steps back:
  `Append` uses the later of the clock and the previous message's `ReceivedAt`. So
  within one inbox, ordering by (`ReceivedAt`, `Seq`) and ordering by `Seq` agree, and
  the messages in a range of times are a range of `Seq`s. A query filtered by time
  (`Query.Since`, `Query.Until`) and paged by `Seq` can be served from one index on
  (inbox, received_at, seq), and retention by age always deletes a prefix.
- **A restore is a new epoch.** `Inbox.Epoch` must change for every inbox when the
  Store is restored from a backup, so that an old cursor is refused. A `Backupper`'s
  snapshot does it (`BackupperSuite` checks it); a Store restored by other means, such
  as a database's own backups, must renew every epoch as part of the restore. The
  suite cannot check that, since restoring is not a method of `Store`.
- **Scope by tenant, except tokens.** A context scoped with `inbox.WithTenant` limits
  every method to that tenant's inboxes (see [Tenants](#tenants)). The token methods
  are the operator's: `GetTokenByHash`, `ListTokens` and `DeleteToken` ignore the
  scope, and `CreateToken` only checks that a token's inboxes are the tenant's.
- **An inbox has an ingest key** (`Inbox.IngestKey`, the `{key}` in `/in/{key}`), which
  the caller supplies when it creates the inbox; a Store does not make one up. It is
  a capability, so it should be random.
- **Deduplication** applies only to verified messages, by `DedupeKey` (the
  `DeliveryID` when it is empty), so a forged delivery cannot suppress the real
  one. The server makes the key the id and a hash of the body when the id is not
  covered by a signature.
- **Secrets stay sealed.** A Store persists `VerifierSecret` verbatim and never
  sees plaintext, and returns inboxes whose `String` and log forms redact
  secrets and token hashes.
- **`Ping` is about the whole Store.** Any error from it makes `/readyz` answer `503`
  and takes the server out of rotation; `ErrStorageFull` says there is no room left at
  all. A quota on one tenant or inbox must never show there, or every server sharing
  the Store fails readiness at once. Refuse the write instead: `Append` returns
  `ErrStorageFull` for it (`503`, and the sender retries), or a hook refuses the
  delivery first with `ErrQuotaExceeded` (`402`).
- **Large deletes must not block writes.** `DeleteInbox` and `Prune` run in small
  batches; deleting everything in one transaction stalls every delivery.

Run the suite from an ordinary test:

```go
func TestMyStore(t *testing.T) {
	inboxtest.StoreSuite(t, func(t *testing.T, now func() time.Time) inbox.Store {
		return openMyStore(t, now) // must use now for every timestamp it assigns
	})
}
```

The suite closes each Store itself. If your Store can take backups, add
`inboxtest.BackupperSuite(t, newStore, restore)`, where `restore` opens a Store
from a snapshot's bytes. If it can count unverified messages cheaply (reading no
more rows than the limit), implement `inbox.UnverifiedCounter` and add
`inboxtest.UnverifiedCounterSuite(t, newStore)`; the server then caps the unverified
messages an inbox keeps (see [verifiers](verifiers.md#unverified-messages-are-capped)).

## Wrapping a Store

To add something around a Store (tenancy, a quota, a byte count per tenant), wrap it
in a type of your own and give the server the wrapper. Besides the 19 methods of
`inbox.Store`, the server looks on the Store it is given for three optional
interfaces, by type assertion, and does without each one it does not find:

| Optional interface | Without it |
|---|---|
| `inbox.UnverifiedCounter` | Unverified messages are not capped: `Options.MaxUnverified` and an `UnverifiedLimiter` on your `Hooks` have no effect. Every delivery stored unverified (with `on_verify_fail=store`, or the verifier `none`) is accepted with `200`. The only sign is a warning in the log when the server starts. |
| `inbox.Backupper` | `GET /v1/admin/backup` answers `501 not_implemented`, and `hookinlet backup` says the store does not support backups. |
| `inbox.StatusReporter` | `GET /v1/admin/status` reports `schema_version` and `database_bytes` as `0` and leaves out `free_bytes`. |

The core grows by adding optional interfaces like these ([Stability](stability.md#the-go-packages)), so
a later release may add to this table.

There are two easy ways to get a wrapper wrong:

- **Embedding the interface hides them.** A `struct{ inbox.Store }` has exactly the 19
  methods of `inbox.Store`, whatever the value inside it can do, so the server finds
  none of the optional interfaces. Nothing fails: the cap is silently off, backups
  answer `501`, and the status shows zeros.
- **Embedding the concrete type skips the wrapper.** A `struct{ *sqlite.Store }` keeps
  them, because the methods are promoted, but every method the wrapper does not write
  itself, including any the concrete type gains later, goes straight to the inner
  store, past whatever the wrapper is for.

Embed the interface for the methods you do not change, and forward each optional
interface explicitly, applying the wrapper's own logic where it applies:

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
```

Forward only the interfaces the inner store has. A method that exists but cannot work
is worse than none: the server would call it instead of doing without.

`inboxtest.AssertStoreWrapperForwards(t, wrapper, inner)` checks a wrapper: it fails,
naming each one and what the server does without it, if the wrapper lacks an optional
interface the inner store implements. Call it in a test of the wrapper with the store
it wraps in production. It checks that the methods exist, not what they do, so a
wrapper that embeds the concrete type passes it.

### Counting, copying and forwarding messages

- **Hooks are best effort for metering.** `AfterIngest` runs after the commit, and
  only if `Append` returned without an error. If the Store commits and `Append` then
  returns an error anyway (an acknowledgement lost on the way back from a database, for
  instance), or the server stops between the commit and the hook, the message is stored
  and `AfterIngest` is never called for it. The sender got no `200` and sends again:
  a verified delivery with a dedupe key is then a duplicate (`created == false`), and
  anything else is stored a second time. Either way a count kept in `AfterIngest`
  falls short of what is stored. An exact count is kept inside the Store's own
  transaction, or taken from the inbox's `LastSeq` (`Store.LatestSeq`): it only goes
  up, a duplicate does not consume a sequence number, and it counts every message
  created, verified or not.
- **Decide on `Message.Verified`.** With `on_verify_fail=store` a forged delivery is
  stored with `Verified == false`, and unverified messages are never deduplicated, so
  each replay of one is a new message (`created == true`). A count of what customers
  are billed for, or of genuine deliveries, should look at `Verified`.
- **`Store.Append` does not signal the Notifier.** The server calls
  `Notifier.Publish` after a delivery that created a message. Anything else that
  appends (a Store or a hook that writes to another inbox) must call `Publish` for
  that inbox after the commit. Otherwise a reader waiting on that inbox does not see
  the message until something else wakes it: the next delivery the server makes to
  that inbox, `Options.SSERecheckInterval` if it is set (it is off by default), or, for
  an MCP wait, its timeout or the next authorization recheck
  (`Options.StreamRevalidateInterval`). An SSE stream is not re-read on that recheck.
- **Copying a message to another inbox from `AfterIngest` is not durable.** The copy
  is a second write after the first has committed. `AfterIngest` must not block, so a
  copy made in the background can still be running after the sender has its `200`; if
  the process stops then, the copy is lost and nobody sends the delivery again. A copy
  made before the response is lost too if the process stops after the commit and
  before the copy: the sender's retry of a verified delivery is then a duplicate
  (`created == false`), which a hook that copies new messages skips. And a copy cannot
  be deduplicated like the original: `Message` has the `DeliveryID` but not the dedupe
  key the server stored with it (for an id the signature does not cover, the id and a
  hash of the body). A fan-out or forward that must not lose messages belongs inside
  the Store's `Append`, in the same transaction as the original, which an embedder that
  owns the Store can do; once that transaction commits, it calls `Publish` for the other
  inbox itself, as above.
- **Forwarding stored messages from a cursor.** Retention (`Store.Prune`) deletes old
  messages whether or not they have been forwarded. Hold retention back for inboxes
  you forward from, or notice the loss: a page whose `OldestSeq` is above your cursor
  plus one, or an unfiltered page with no messages whose `LatestSeq` is above your cursor, means
  messages were deleted before you read them. Stored headers can include
  the sender's signature header (`X-Hub-Signature-256`, `Stripe-Signature`,
  `webhook-signature` and the like; only the `token` verifier's header, and
  `Authorization`, `Cookie` and `Proxy-Authorization`, are dropped before storing). A
  forwarder must not pass it on as though it signed the request itself.

## Writing a Notifier

A notifier carries no data: it says "look again", and the reader asks the Store.
Replay, reconnects, and slow readers therefore share one code path, and a missed
signal costs latency and never data.

```go
func TestMyNotifier(t *testing.T) {
	inboxtest.NotifierSuite(t, func(t *testing.T) inbox.Notifier { return newMyNotifier(t) })
}
```

An in-process notifier must not drop signals. One that crosses the network may
(delivery at most once), and must then be run with a positive
`HOOKINLET_SSE_RECHECK_INTERVAL` (`Options.SSERecheckInterval`), which makes every
stream and MCP wait re-read the Store on that interval regardless. `Publish` is
called with a context that may already be cancelled and must still deliver.

Readers subscribe first and read the Store second, so a subscription has to be live
when `Subscribe` returns: every `Publish` that begins after that must reach it. A
notifier that crosses the network waits for its backend to confirm the subscription
(the reply to a pub/sub `SUBSCRIBE`, say) before returning, but only briefly, since
`Subscribe` must not block the caller. If it returns before the confirmation, a signal
published in between can be lost, which is the at-most-once delivery the recheck
interval covers: the reader sees the message on its next re-read.

Signals coalesce when they arrive before the subscriber reads. A notifier that crosses
the network delivers a burst of publishes spread over time, so a subscriber that reads
the first at once sees the later ones as new signals, as it should. Run such a
notifier with `inboxtest.NotifierSuiteWith(t, newNotifier, inboxtest.NotifierOptions{Settle: d})`,
where `d` is how long a burst of a thousand publishes takes to arrive: the coalescing
check then waits that long before it reads, and still requires one signal.

## Writing a SecretBox

A `SecretBox` seals a verifier secret when an inbox is created or its secret is set,
and opens it on every delivery to an inbox that has one, on each `/readyz` sample
and in `Server.CheckSecrets`. The server does not cache what it opens: a box that
asks a key service for a data key should cache the key itself, for as long as it
judges safe.

```go
inboxtest.SecretBoxSuite(t, func(t *testing.T) inbox.SecretBox { return newMyBox(t) })
```

A box that does I/O should also implement `inbox.ContextSecretBox`, whose
`SealContext` and `OpenContext` are given the context of the work; the server then
calls them and not `Seal` and `Open`. They must give the same results, and give up
with an error that wraps `ctx.Err()` once the context ends.

The box is always given the context scoped (`inbox.WithTenant`) to the tenant of the
inbox whose secret it is, so `inbox.TenantFrom` reports that tenant and `scoped`
true, and a box that keeps a key per tenant can pick it. An inbox in the default
tenant is scoped to `""`, which is not the same as not scoped: a box can tell the
default tenant from a context that says nothing. The scoping is for the box only;
the Store calls of the same work keep their own context (a delivery's are not
scoped at all, since the inbox is found by its ingest key). What the context is:

- **A delivery**: the request's context, cancelled when the sender goes away.
- **The admin API** (creating an inbox with a secret, setting one): the request's
  context. The tenant is the one the inbox is made in or belongs to: the caller's
  for a `TenantPrincipal`, the default tenant for a caller in none. It is the tenant
  the box is given when a delivery later opens the secret.
- **`/readyz`**: the check's own context, which outlives the probe and has a
  deadline of 20 seconds for the whole sample. A check cut short by it is not taken
  for secrets that do not open: `/readyz` answers `503` saying that checking the
  secrets timed out (the key service may be slow or down), logs a warning, and
  checks again after 5 seconds.
- **`Server.CheckSecrets`** and `hookinlet rekey`: the caller's context (`hookinlet
  serve` passes `context.Background()`).

Code of your own that seals or opens with a box calls `inbox.SealWith(ctx, box, …)`
and `inbox.OpenWith(ctx, box, …)`, which pass the context to a box that takes one.
To check that a box honours the context:

```go
inboxtest.ContextSecretBoxSuite(t, func(t *testing.T) inbox.SecretBox { return newMyBox(t) })
```

## Writing an Authenticator

`Authenticate` runs on every request, before the server knows whether the caller
may see anything, so it must not reveal why it refused: every failure looks the
same and wraps `inbox.ErrUnauthenticated`. A different error is taken to mean the
credential could not be checked and becomes a 500. A `Principal` decides by inbox
**ID**, never by slug or name, and a credential for one inbox must not reveal
whether another exists (the server answers 404 for both).

```go
inboxtest.AuthenticatorSuite(t, func(t *testing.T) inboxtest.AuthFixture { … })
```

### Choosing the 401 challenge

A `401` carries `WWW-Authenticate: Bearer realm="hookinlet"`. If the error your
`Authenticate` returns (or any error it wraps, found with `errors.As`) also has a
`WWWAuthenticate() string` method (`inbox.Challenger`), the server sends that string
instead. It applies to every `401` the server writes: the read API, the admin API,
a stream, the MCP endpoint, and the `405` that checks the credential first.

A self-hoster who puts an OAuth server in front can use it to point clients to the
discovery document:

```go
type needsLogin struct{}

func (needsLogin) Error() string { return "unauthenticated" }
func (needsLogin) Unwrap() error { return inbox.ErrUnauthenticated }
func (needsLogin) WWWAuthenticate() string {
	return `Bearer resource_metadata="https://hooks.example.com/.well-known/oauth-protected-resource"`
}
```

(Serve that document with [`Options.Fallback`](#serving-other-paths-next-to-the-api).)

The server sends the string only if it is one line of printable ASCII (no control
characters, tab included), not empty, at most 4096 bytes, and starts with an HTTP
authentication scheme: a token as RFC 9110 defines it, such as `Bearer`, followed by
nothing or by a space and the parameters. It does not parse the parameters. If the
string fails any of that, the server sends `Bearer realm="hookinlet"` and logs a
warning, once. The same string goes to every caller who is refused, so do not build
it from the request's credentials. An error that does not wrap `ErrUnauthenticated`
is still a server error, whatever else it has.

## Serving other paths next to the API

`Options.Fallback` is an `http.Handler` that answers requests for paths the server
has no route for, in place of the fixed `404`. It is for a self-hosted server that
also serves a file under `/.well-known/` or a small static site at the same
address:

```go
mux := http.NewServeMux()
mux.Handle("/.well-known/oauth-protected-resource", metadataDocument)
mux.Handle("/", http.FileServer(http.Dir("./public")))

srv, err := server.New(server.Options{ /* … */ Fallback: mux })
```

- **What reaches it.** Any request whose first path segment is not one the server
  uses. Those segments are `in`, `v1`, `mcp`, `healthz`, `readyz`, `metrics` and
  `admin`, and a request under one of them gets the server's own answer whether or
  not the server serves that path in this configuration. With `DisableAdmin` set,
  `/v1/admin/...` and `/admin` still answer the same `404` as before, and a server
  with no `Authenticator` still answers `404` for the read API and MCP. The root
  path `/` and paths such as `/robots.txt` or `/.well-known/...` go to the
  `Fallback`.
- **Without it** (the default) every unrouted path answers
  `{"error":{"code":"not_found","message":"not found"}}` with status `404`, as
  before.
- **Which listener.** With `AdminAddr` set, the `Fallback` serves on the public
  listener only; the admin listener keeps its `404`.
- **No authentication.** The server does not authenticate requests to the
  `Fallback`, so it must serve nothing private. If it needs a credential, check it
  yourself.
- **Middleware and limits.** The `Fallback` runs inside the server's request ID,
  access log and panic recovery (a panic becomes a JSON `500`). The server-wide
  limits apply: request headers are limited by `MaxHeaderBytes` and must arrive
  within 10 seconds; a request with a body must deliver it within `ReadBodyTimeout`
  (60 seconds); and the response must be taken within `ResponseTimeout` (2 minutes
  from when the request arrived), after which the write fails. A handler that
  streams for longer can move the write deadline with
  `http.NewResponseController(w).SetWriteDeadline`. The body is also capped at 1 MiB;
  `Options.FallbackMaxBodyBytes` changes that (a negative number removes the cap),
  and a read past the cap fails with `*http.MaxBytesError`. Responses carry
  `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`; the handler can
  set its own `Cache-Control` for files that may be cached.
- **Shutdown.** `Run` and `Serve` wait for a `Fallback` request that is in progress,
  up to `ShutdownTimeout` (10 seconds by default), then close its connection; with
  `srv.Handler()` mounted in your own server, `srv.Shutdown(ctx)` waits up to the
  deadline of `ctx`. The server tells its own streams and MCP waits to finish; it
  cannot tell a `Fallback` handler to, which sees its request's context cancelled
  only when the client goes away or the connection is closed. During the drain
  delay before shutdown, `/readyz` reports not ready; the `Fallback` keeps
  answering.
- **With `Handler()`.** The `Fallback` is part of the returned handler, so it works
  the same when you mount `srv.Handler()` yourself; the deadlines then depend on the
  server you mount it in.
- **Metrics.** The `Fallback`'s responses are counted under the route `/`, which
  is where unrouted requests were already counted.

## Adding a verifier

```go
reg := verify.Default()
err := reg.Register("acme-v2", func(config json.RawMessage) (verify.Verifier, error) { … })
srv, _ := server.New(server.Options{ /* … */ Verifiers: reg })
```

See [verifiers](verifiers.md) for what a `Verifier` must do: check the exact bytes
received, compare in constant time, and say which headers carry its credential so
they are not stored.

## Rules the server keeps, and expects of you

- Handlers reach storage, notification, authentication, and secrets only through
  these interfaces. The one exception is the optional capabilities above.
- Never log message bodies, tokens, secrets, or the headers that carried them.
- Compare secrets with `crypto/subtle` or `hmac.Equal`.
- Honor context cancellation everywhere; a cancelled call leaves no partial effect.
