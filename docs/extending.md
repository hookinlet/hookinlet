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
| `Hooks` | Runs before and after ingest: reject a delivery, count it. | `inbox.NopHooks` | none: the default does nothing |
| `Admitter` (optional, of `Hooks`) | Asked before every read, stream and MCP wait, with the caller, the inbox and an `AccessKind`; may refuse (`ErrRateLimited` 429, `ErrQuotaExceeded` 402, `ErrForbidden` 403) or return a release function, called when the access ends. | none: without it everything is admitted | `AdmitterSuite` |
| `Backupper` (optional) | A consistent snapshot of the Store, for `GET /v1/admin/backup`; the size may be -1 if the Store streams it. | the SQLite store | `BackupperSuite` |
| `UnverifiedLimiter` (optional, of `Hooks`) | Says, per inbox, how many unverified messages it may keep, in place of `Options.MaxUnverified`: a positive number is the cap, 0 the server's limit, a negative number no cap. For caps that depend on something the server does not know, such as a plan. Needs a Store with `UnverifiedCounter`. | none: every inbox gets the server's limit | the server's tests |
| `UnverifiedCounter` (optional) | How many unverified messages an inbox holds (bounded by a limit), so the server can cap them (`Options.MaxUnverified`). Without it the server does not cap, and says so when it starts. | the SQLite store | `UnverifiedCounterSuite` |
| `ScopedPrincipal` (optional) | A `Principal` that can read a known list of inboxes, so listing does not have to search. | the agent principal | checked by `AuthenticatorSuite` |
| `verify.Registry` | Maps verifier names to signature checks. | `verify.Default()` | the package's own tests |

The optional ones are found with a type assertion, and the server copes when they
are absent: no `Backupper` means `GET /v1/admin/backup` answers `501`, and `Hooks` without `Admitter` admits every read, stream and wait. The server's own limits (`MaxConcurrentReads`, `MaxStreams`, `MaxStreamsPerInbox`) apply after `Admit`, whatever it says.

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
per-inbox rate limit as the hook. It is compiled and tested in CI; copy it as a
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
address (as recorded, honoring trusted proxies), the method, the path after the
inbox key, and the request headers (read-only). It runs after the inbox is found
and the body is read, before the
signature is checked, so a refusal is cheap. That has a cost to weigh: it sees
unverified requests too, so a limit that counts every request lets anyone who knows
an inbox's URL use up its allowance with unsigned posts, and the real sender,
which may not retry, is then refused. Limit by something the stranger cannot
spend (the source address of unverified traffic, say), or meter what was accepted
in `AfterIngest`, or accept the trade-off knowingly, as the example does for
brevity. Return an error wrapping
`ErrRateLimited` (429), `ErrQuotaExceeded` (402), or `ErrForbidden` (403); any
other error is a 500. It is called concurrently, from every request.

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
  never decreases with `Seq` within an inbox.
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
