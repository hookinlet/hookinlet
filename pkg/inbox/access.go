// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import "context"

// AccessInfo says which request of the server's a hook is called for, so that an
// embedder can audit what it admits and refuses and match it to the server's access
// log. It grows by adding fields, which is why it is a struct.
//
// The server attaches it to the context of the hooks it calls while serving a
// request: Authenticator.Authenticate (through the context of the *http.Request it
// is given, so an Authenticator can decide by where a request came from),
// Admitter.Admit, Hooks.BeforeIngest, Hooks.AfterIngest, IngestObserver.ObserveIngest
// and UnverifiedLimiter.MaxUnverified. Other calls made for the request (to the Store,
// for instance) may see it as well, but that is not promised. Read it with AccessFrom.
//
// Authenticate sees the same Operation as the hooks, with two exceptions: it is empty
// for the 405 the server sends for a method a path does not take (the credential is
// checked first, and no operation is asked for), and it is "mcp" for the HTTP request
// that carries MCP tool calls, since the credential is checked before the tool is known.
// A check the server repeats while a stream or an MCP wait is open sees the stream's
// operation or the tool's name, as the hooks do.
type AccessInfo struct {
	// Operation names what was asked for. For the HTTP API it is the operationId the
	// operation has in docs/openapi.json, such as "listMessages", "headGetMessage",
	// "streamMessages" or "ingest"; for an MCP tool call it is "mcp." and the tool's
	// name, such as "mcp.list_inboxes" or "mcp.wait_for_messages". It is a fixed
	// identifier, never a path, so there are few of them and it carries nothing the
	// caller chose.
	Operation string
	// RequestID is the request's id: the X-Request-Id the response carries and the
	// request_id of the server's access log line for it. It is taken from the
	// request's own X-Request-Id when that is well-formed, so it is not secret and not
	// necessarily unique.
	RequestID string
	// ClientIP is the address the request came from, as text: the same value the
	// server stores as a delivery's SourceIP. It is the address of the connection's
	// peer unless that peer is within Options.TrustedProxies, in which case it is the
	// client address the trusted proxies' X-Forwarded-For names (a header from any
	// other peer is ignored, so a caller cannot choose it). It is empty when the
	// server cannot tell, because the peer address cannot be read, and it may be
	// empty for a request that was not made over the network, such as one made
	// with a test recorder. An embedder that admits or refuses by address, with
	// an allowlist for instance, must treat an empty ClientIP as not allowed
	// (fail closed), and must refuse with the same ErrUnauthenticated as for a bad
	// credential.
	ClientIP string
}

type accessKey struct{}

// WithAccess returns a context that carries a. The server calls it for every request;
// an embedder may use it to test a hook that reads AccessFrom.
func WithAccess(ctx context.Context, a AccessInfo) context.Context {
	return context.WithValue(ctx, accessKey{}, a)
}

// AccessFrom returns the AccessInfo ctx carries. ok is false when it carries none, as
// for a call that is not made for a request (a background job, the command line).
func AccessFrom(ctx context.Context) (a AccessInfo, ok bool) {
	a, ok = ctx.Value(accessKey{}).(AccessInfo)
	return a, ok
}
