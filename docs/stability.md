# Stability

This page says what you can build on, what may change, and how we announce a
change. hookinlet is pre-1.0. This page is the promise for `v0.1.0` and after.
`CHANGELOG.md` records every change that touches it.

## The HTTP API

One document, [`openapi.json`](openapi.json), describes the read API, the stream,
the admin API and ingest. The server is tested against it. Every response the
server can give is in the document, and a test produces every response in the
document. Paths carry the major version (`/v1/...`). Some paths have no version:
ingest (`/in/...`), the health checks, `/metrics` and `/mcp`. Ingest is what
senders are configured with, and its shape does not change. The health checks and
metrics are for hosting platforms. MCP has its own protocol version and tool
names. Until 1.0, a breaking change is made in place, as a minor version with a
CHANGELOG entry. (This is how the admin API came to be under `/v1/admin`.) From
1.0, a breaking change to `/v1` would become `/v2`, served beside it.

**Not breaking** (clients must be ready for these):

- **New fields in responses.** Read objects as open: ignore what you do not know.
  (Requests are the opposite: see below.)
- **New error codes**, and new statuses on an operation. Switch on the status.
  Treat a code you do not know like the other codes of its status. Do not parse
  `message`.
- **New operations, new optional request fields, new optional query parameters,
  new event types on a stream**, a new tool or optional argument on MCP.
- **New values of an enum** in a response (a `verifier` name, a `result` label).
- Limits that change within what the document says. The ranges and defaults in
  it are the promise. A default may change, with a CHANGELOG entry.

**Breaking** (a CHANGELOG entry, and while the version is below 1.0, a minor version):
removing or renaming a path, field, parameter, code, tool or argument; changing a
type or what a value means; making an optional request field required; a status that
meant success meaning failure. A hook sees an operation's `operationId` in
`openapi.json`, and the `mcp.<tool>` name of an MCP tool, as
`inbox.AccessInfo.Operation`. An embedder may record them, for example in an
audit log. Renaming one is a breaking change too.

### What the server is strict about

- **Unknown query parameters are an error** (`400`). So are repeated ones and a
  malformed query string. This holds on every operation except ingest, which
  stores the query string with the message. A typo does not quietly get the
  default page. We will not relax this. It is also why a new optional parameter
  is not a breaking change: nobody could have been sending it.
- **Unknown fields in a request body are an error.** So is anything after the JSON.
- **A limit above the maximum is lowered to the maximum, not refused.** The
  response says what was used: the `next` link carries the `limit` in effect, and
  the maximum is in the document. A limit below the minimum, or not a number, is
  a `400`.

### Cursors

A cursor is the `seq` of a message in one [epoch](api.md#epochs-and-gaps) of an
inbox. Following `next` links is always safe. If you keep a cursor past a restore,
keep the `epoch` with it. A `409 epoch_changed` means the server wants you to
start again.

### Inbox identifiers

Clients name inboxes by their **slug** everywhere they read or act: the read API,
the stream, MCP and the CLI. The admin API also shows a numeric `id`. It follows
the order in which inboxes were created. Within a database it is never reused,
except that a database restored from an older backup hands out the ids it had
then. The admin list's `after` cursor counts these ids. Do not build on the `id`
beyond that. It is an internal key, and an embedder's Store may hand out another
kind. Tokens have a numeric `id` for the same reason: to revoke one, and to page
through them. The Go type `inbox.Inbox` also has a **UID**. It never changes and
no other inbox shares it. A stored secret is sealed to it. It is not secret, and
no API shows it. The type also has a **tenant**. This is an opaque key that no API
shows. It keeps the inboxes of independent parties apart in one Store (see
[extending](extending.md#tenants)). A slug is unique within a tenant. An
**ingest key** is not an identifier but a capability. Treat it like a password
for senders.

## The command line

`hookinlet <command> --json` prints the same shapes as the API and is as stable.
The text output (tables, sentences) is for people and may change. Exit codes: `0`
success, `1` the operation failed, `2` usage or configuration. We add flags and
`HOOKINLET_*` variables freely. We remove one, or change what it means, only with
a CHANGELOG entry.

## The data

The database only moves forward, through migrations that run at start. The server
keeps a copy before a migration (see [Deploying](deploying.md#upgrading)). A
version that finds a database written by a newer version refuses it. You can
restore a backup into the version that made it, or a later one.

## The Go packages

`pkg/inbox` (the interfaces and types), `pkg/inboxtest` (their conformance suites),
`pkg/auth/token`, `pkg/secretbox`, `pkg/store/sqlite`, `pkg/notify/local`,
`pkg/verify`, `pkg/server`, `pkg/server/servertest`, and `docs` are public. The
`docs` package embeds `openapi.json` as `docs.OpenAPI`, for an embedder to serve,
validate against, or generate from. The document is the contract and is stable as
described above. Until 1.0, these packages can change in breaking ways, and
`CHANGELOG.md` lists every change. After 1.0, semantic versioning applies.
Anything under `internal/` is not public.

**How the interfaces grow.** `inbox.Store`, `inbox.Hooks`, `inbox.SecretBox`,
`inbox.Authenticator`, `inbox.Principal` and `inbox.Notifier` will not gain methods
once they reach 1.0. A new capability will be a new, small interface that an
implementation may also satisfy. The server finds it with a type assertion, as it
does for `Admitter`, `Backupper`, `IngestObserver` and `ScopedPrincipal`. If an
implementation lacks it, the server does what it did before. A test fails when
the methods of one of those interfaces change, whether their names or their
signatures, so a change cannot happen by accident. A new interface ships with a
conformance suite when its implementations have a behaviour to check
(`Admitter`, `Backupper` and `EpochRenewer` have one). Some interfaces are only
called by the server and have little to conform to beyond not blocking. Examples
are `IngestObserver`, `UnverifiedLimiter` and `Challenger`. These have a short
suite. It checks that the call returns at once, leaves what it is given alone,
and is safe to call concurrently. For a challenge, it checks that the result is a
value the server sends. The server's own tests cover what the server does with
their answers. A behaviour that is in neither a suite nor a test is not part of
the contract.

Four behaviours belong to the server, not to one interface. They are part of the
contract: which requests a `TenantPrincipal` scopes (the read API, streams, MCP, the
admin API and ingest), `Options.RequireTenant`, how the readiness check samples
stored secrets across tenants, and `Options.Fallback`. An embedder checks them with
`servertest.ServerSuite`. It builds a server over the embedder's `Store` and
`Authenticator` and sends it requests (see
[extending](extending.md#checking-what-the-server-does-with-tenants)). It is a
separate function, so an implementation that passes the suites of its interfaces
does not start failing on upgrade. Suites may grow new cases. `CHANGELOG.md` lists
the new cases of an existing suite, and says whether an implementation that passed
before can fail them.

**Options.** `server.Options` follows one rule. A zero value means the documented
default. A negative number turns off a bound or a schedule where that makes sense,
and is an error where it does not. Only `Store` is required. You need an address
only to use `Run`. Embedding the handler in your own `http.Server` needs no
address.

## What is not promised

Log messages and their fields, the metrics' help texts, the exact wording of error
`message`s, response header order, JSON key order, and timing.

## Cross-origin requests, TLS, and the Host header

- **hookinlet sends no CORS headers.** CORS is the browser rule for calls between
  sites. The read API is for servers and agents, which CORS does not apply to.
  Do not put the bearer token in a web page. A browser on another origin cannot
  call the API, and the raw endpoint sends `Cross-Origin-Resource-Policy: same-origin`.
  If you want a browser client, a reverse proxy can add CORS headers. The token
  then lives in that page.
- **TLS is the proxy's job.** The server speaks plain HTTP. Put it behind a proxy
  or load balancer that ends TLS. Set `HOOKINLET_PUBLIC_BASE_URL` to the public
  `https://` address so the ingest URLs it hands out are right. The command line
  refuses to send the admin token over plain `http://` to another machine. See
  [Deploying](deploying.md) for proxy settings.
- **The Host header is not checked.** The MCP SDK would refuse a request that
  arrives on a loopback address with a different `Host`. That guard defends local
  tools that need no credential against DNS rebinding. The guard is off here.
  Every call needs a bearer token, and a browser never attaches one by itself. A
  proxy on the same host also legitimately forwards the public `Host` to a
  loopback listener.
