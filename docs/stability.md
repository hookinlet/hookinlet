# Stability

What you can build on, what may change, and how a change is announced. hookinlet is
pre-1.0: this page is the promise for `v0.1.0` and after, and `CHANGELOG.md` is where
every change that touches it is written down.

## The HTTP API

The read API, the stream, the admin API and ingest are described by one document,
[`openapi.json`](openapi.json), which the server is tested against: every response it
can give is in the document, and every response in the document is given by a test.
Paths carry the major version (`/v1/...`). Ingest (`/in/...`), the health checks,
`/metrics` and `/mcp` are not versioned in the path: ingest is what senders are
configured with and does not change shape, the health checks and metrics are for
platforms, and MCP is versioned by its own protocol and its tool names. Until 1.0 a
breaking change is made in place, as a minor version with a CHANGELOG entry (this is
how the admin API came to be under `/v1/admin`); from 1.0, a breaking change to `/v1`
would be `/v2`, served beside it.

**Not breaking** (clients must be ready for them):

- **New fields in responses.** Read objects as open: ignore what you do not know.
  (Requests are the opposite: see below.)
- **New error codes**, and new statuses on an operation. Switch on the status, treat a
  code you do not know like the others of its status, and do not parse `message`.
- **New operations, new optional request fields, new optional query parameters,
  new event types on a stream**, a new tool or optional argument on MCP.
- **New values of an enum** in a response (a `verifier` name, a `result` label).
- Limits changing within what the document says (the ranges and defaults in it are
  the promise; a default may change with a CHANGELOG entry).

**Breaking** (a CHANGELOG entry, and while the version is below 1.0, a minor version):
removing or renaming a path, field, parameter, code, tool or argument; changing a
type or what a value means; making an optional request field required; a status that
meant success meaning failure. An operation's `operationId` in `openapi.json`, and the
`mcp.<tool>` name of an MCP tool, are what a hook sees as `inbox.AccessInfo.Operation`, and an
embedder may record them (in an audit log, say): renaming one is a breaking change too.

### What the server is strict about

- **Unknown query parameters are an error** (`400`), as are repeated ones and a
  malformed query string, on every operation but ingest, which stores the query
  string with the message. A typo does not quietly get the default
  page. This will not be relaxed, and it is why a new optional parameter is not a
  breaking change: nobody could have been sending it.
- **Unknown fields in a request body are an error**, and so is anything after the JSON.
- **A limit above the maximum is clamped, not refused**, and the response says what
  was used: the `next` link carries the effective `limit`, and the maximum is in the
  document. A limit below the minimum, or not a number, is a `400`.

### Cursors

A cursor is the `seq` of a message in one [epoch](api.md#epochs-and-gaps) of an inbox.
Following `next` links is always safe. Keep the `epoch` with the cursor if you keep it
past a restore; a `409 epoch_changed` is the server telling you to start again.

### Inbox identifiers

Inboxes are named by their **slug** everywhere a client reads or acts: the read API,
the stream, MCP and the CLI. The admin API also shows a numeric `id`, which is the
order in which inboxes were created, is never reused within a database (a database restored from an older backup hands out
the ids it had then again), and is what the admin list's `after` cursor counts. Do not build on it beyond that: it is an internal key, and an
embedder's Store may hand out another kind. Tokens have a numeric `id` for the same
reason (to revoke one, and to page through them). The Go type `inbox.Inbox` also has a
**UID**, which never changes and is never shared with another inbox: it is what a stored
secret is sealed to, it is not secret, and no API shows it. It has a **tenant** too, an
opaque key that no API shows either, which keeps the inboxes of independent parties
apart in one Store (see [extending](extending.md#tenants)); a slug is unique within a
tenant. An **ingest key** is not an
identifier but a capability: treat it like a password for senders.

## The command line

`hookinlet <command> --json` prints the same shapes as the API and is stable in the
same way. The text output (tables, sentences) is for people and may change. Exit
codes: `0` success, `1` the operation failed, `2` usage or configuration. Flags and
`HOOKINLET_*` variables are added freely; one is removed or changes meaning only with
a CHANGELOG entry.

## The data

The database moves forward only, by migrations that run at start. A copy is kept
before a migration (see [Deploying](deploying.md#upgrading)). A version that finds a
database written by a newer one refuses it. A backup restores into the version that
took it and later ones.

## The Go packages

`pkg/inbox` (the interfaces and types), `pkg/inboxtest` (their conformance suites),
`pkg/auth/token`, `pkg/secretbox`, `pkg/store/sqlite`, `pkg/notify/local`,
`pkg/verify`, `pkg/server`, and `docs` (which embeds `openapi.json` as `docs.OpenAPI`,
for an embedder to serve, validate against, or generate from; the document is the
contract and is stable as described above) are public. Until 1.0 they can change in
breaking ways, every change is in `CHANGELOG.md`, and after 1.0 semantic versioning
applies. Anything under `internal/` is not.

**How the interfaces grow.** `inbox.Store`, `inbox.Hooks`, `inbox.SecretBox`,
`inbox.Authenticator`, `inbox.Principal` and `inbox.Notifier` do not gain methods once
they are 1.0: a new capability is a new, small interface that an implementation may
also satisfy, found by a type assertion (as `Admitter`, `Backupper`, `IngestObserver` and `ScopedPrincipal` are), with
the server falling back to what it did before. A test fails when the methods of one of those
interfaces, their names or their signatures, change, so the change cannot be an accident. A conformance suite ships with a new
interface whose implementations have a behaviour to check (`Admitter`, `Backupper` and
`EpochRenewer` have one). An interface that only the server calls and that has little to
conform to beyond not blocking, such as `IngestObserver`, `UnverifiedLimiter` and
`Challenger`, has a short suite for that (it returns at once, leaves what it is given
alone, is safe to call concurrently, and for a challenge is a value the server sends),
and what the server does with their answers is covered by the server's own tests. A
behaviour that is in neither a suite nor a test is not part of the contract. A suite may
grow cases; the new cases of an existing suite are listed in `CHANGELOG.md`, with whether
an implementation that passed before can fail them.

**Options.** `server.Options` follows one rule: a zero value means the documented
default; a negative number turns off a bound or a schedule where that makes sense (and
is an error where it does not); nothing is required but `Store`, and an address only
to use `Run`. Embedding the handler in your own `http.Server` needs no address.

## What is not promised

Log messages and their fields, the metrics' help texts, the exact wording of error
`message`s, response header order, JSON key order, and timing.

## Cross-origin requests, TLS, and the Host header

- **hookinlet sends no CORS headers.** The read API is for servers and agents, which
  are not subject to CORS, and the bearer token is not something to put in a page. A
  browser on another origin cannot call it, and the raw endpoint says
  `Cross-Origin-Resource-Policy: same-origin`. If you do want a browser client, a
  reverse proxy can add CORS headers; the token then lives in that page.
- **TLS is the proxy's job.** The server speaks plain HTTP. Put it behind a proxy or
  load balancer that terminates TLS, and set `HOOKINLET_PUBLIC_BASE_URL` to the
  public `https://` address so that the ingest URLs it hands out are right. The
  command line refuses to send the admin token over plain `http://` to another
  machine. See [Deploying](deploying.md) for proxy settings.
- **The Host header is not checked.** The MCP SDK would refuse a request that arrives
  on a loopback address with a different `Host` (a guard against DNS rebinding of
  local tools that need no credential). That guard is off here: every call needs a
  bearer token that a browser never attaches by itself, and a proxy on the same host
  legitimately forwards the public `Host` to a loopback listener.
