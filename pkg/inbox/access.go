// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import "context"

// AccessInfo says which request of the server's a hook is called for, so that an
// embedder can audit what it admits and refuses and match it to the server's access
// log. It grows by adding fields, which is why it is a struct.
//
// The server attaches it to the context of the hooks it calls while serving a
// request, after the caller is authenticated: Admitter.Admit, Hooks.BeforeIngest,
// Hooks.AfterIngest, IngestObserver.ObserveIngest and UnverifiedLimiter.MaxUnverified.
// Other calls made for the request (to the Store, for instance) may see it as well,
// but that is not promised. Read it with AccessFrom.
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
