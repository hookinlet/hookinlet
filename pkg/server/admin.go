// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// closingContext is ctx that also ends when the server starts shutting down.
// http.Server.Shutdown waits for requests but never cancels them, so a prune or
// a download started from the admin API would otherwise hold shutdown until the
// timeout and then be cut off.
func (s *Server) closingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.closeCtx, cancel)
	return ctx, func() { stop(); cancel() }
}

// audit records an admin action. It names the inbox, never anything secret.
func (s *Server) auditCtx(ctx context.Context, op, slug string, attrs ...slog.Attr) {
	attrs = append([]slog.Attr{slog.String("op", op)}, attrs...)
	if slug != "" {
		attrs = append(attrs, slog.String("slug", slug))
	}
	if r, ok := ctx.Value(requestKey).(*http.Request); ok {
		attrs = append(attrs, slog.String("client_ip", clientIP(r, s.opts.TrustedProxies)))
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "admin", attrs...)
}

// baseURLFor is baseURL for code that has only the context of the request.
func (s *Server) baseURLFor(ctx context.Context) string {
	if r, ok := ctx.Value(requestKey).(*http.Request); ok {
		return s.baseURL(r)
	}
	return strings.TrimRight(s.opts.PublicBaseURL, "/")
}

// baseURL is the externally visible address of this server, for the ingest URLs
// the admin API hands out. The configured one wins. Otherwise it is built from
// the request, taking the scheme from X-Forwarded-Proto only when the request
// came from a trusted proxy.
func (s *Server) baseURL(r *http.Request) string {
	if s.opts.PublicBaseURL != "" {
		return strings.TrimRight(s.opts.PublicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p, ok := peerAddr(r.RemoteAddr); ok && containsAddr(s.opts.TrustedProxies, p) {
		if f := r.Header.Get("X-Forwarded-Proto"); f == "https" || f == "http" {
			scheme = f
		}
	}
	if r.Host == "" {
		return ""
	}
	return scheme + "://" + r.Host
}
