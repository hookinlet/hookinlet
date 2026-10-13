// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/hookinlet/hookinlet/internal/api"
)

// mcpAPI serves POST /mcp through the generated interface.
//
// The JSON-RPC itself is the MCP SDK's: it parses the call, runs the tool, and
// writes the answer. What this does is take the SDK's answer and send it as one of
// the responses the document lists for the operation. A status the SDK produces
// that the document does not list is not passed on; it is logged and answered 500.
//
// Who may call, the method, the content type, the size, and the JSON-RPC methods
// allowed are checked before this, from the document (see routes.go).
type mcpAPI struct {
	s *Server
}

func (a *mcpAPI) Mcp(ctx context.Context, req api.McpRequestObject) (api.McpResponseObject, error) {
	rr := readFrom(ctx)
	id := requestIDOf(ctx)

	// The generated code has read the body as raw JSON; the SDK reads it again.
	var raw []byte
	if req.Body != nil {
		raw = *req.Body
	}
	// The tools need who is calling, and the request, to check that credential again
	// during a long wait.
	caller := &mcpCaller{principal: rr.principal}
	r := rr.r.WithContext(context.WithValue(ctx, mcpCtxKey{}, caller))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	caller.req = r

	rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
	a.s.mcpSDK.ServeHTTP(rec, r)
	// The answer is in memory now, however long the tool took (a wait runs up to a
	// minute): the client gets ResponseTimeout from here to take it.
	if rr.deadline != nil {
		rr.deadline()
	}

	switch rec.status {
	case http.StatusOK:
		return api.Mcp200JSONResponse{Body: rec.body.Bytes(), Headers: api.Mcp200ResponseHeaders{XRequestId: id}}, nil
	case http.StatusAccepted:
		return api.Mcp202Response{Headers: api.Mcp202ResponseHeaders{XRequestId: id}}, nil
	case http.StatusBadRequest:
		return api.Mcp400TextResponse{Body: rec.body.String(), Headers: api.Mcp400ResponseHeaders{XRequestId: id}}, nil
	}
	a.s.log.LogAttrs(ctx, slog.LevelError, "the MCP handler answered a status the document does not list",
		slog.Int("status", rec.status))
	return api.Mcp500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
}

// bufferedResponse holds an answer until the operation has decided how to send it.
// A tool call is answered in one piece (the endpoint replies in JSON, not as a
// stream), so nothing waits on the body.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) Flush()                      {}
