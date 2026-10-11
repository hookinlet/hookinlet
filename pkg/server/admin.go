// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// maxAdminBody bounds an admin request body. Settings are small; the largest
// field is a verifier secret of at most admin.MaxSecretBytes.
const maxAdminBody = 64 << 10

// registerAdmin mounts the admin API, which only the admin principal may use.
// Like the read API it exists only when there is an Authenticator to say who
// the admin is.
func (s *Server) registerAdmin(mux *http.ServeMux) {
	type routes = map[string]http.HandlerFunc
	api := s.adminHandlers()
	mux.HandleFunc("/admin/inboxes", s.admin(routes{http.MethodPost: s.adminBody(api.CreateInbox), http.MethodGet: s.adminBody(api.ListInboxes)}))
	mux.HandleFunc("/admin/inboxes/{slug}", s.admin(routes{http.MethodGet: s.adminBody(api.ShowInbox), http.MethodPatch: s.adminBody(api.UpdateInbox), http.MethodDelete: s.adminBody(api.DeleteInbox)}))
	mux.HandleFunc("/admin/inboxes/{slug}/secret", s.admin(routes{http.MethodPut: s.adminBody(api.SetVerifierSecret)}))
	mux.HandleFunc("/admin/inboxes/{slug}/rotate-token", s.admin(routes{http.MethodPost: s.adminBody(api.RotateAgentToken)}))
	mux.HandleFunc("/admin/retention/run", s.admin(routes{http.MethodPost: s.adminBody(api.RunRetention)}))
	mux.HandleFunc("/admin/backup", s.admin(routes{http.MethodGet: s.adminBody(api.Backup)}))
}

// admin wraps handlers with the checks every admin call shares, in this order:
// the caller must authenticate (401, whatever the path), must be the admin
// (403), and must use a method the path accepts (405). Nothing else is looked
// at before that, so an anonymous caller learns nothing about the API.
func (s *Server) admin(methods map[string]http.HandlerFunc) http.HandlerFunc {
	allow := make([]string, 0, len(methods))
	for m := range methods {
		allow = append(allow, m)
	}
	// Map order is random; the header should not be.
	sortStrings(allow)
	allowHeader := strings.Join(allow, ", ")

	return func(w http.ResponseWriter, r *http.Request) {
		// Responses carry settings, and some a one-time token.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		principal, err := s.opts.Authenticator.Authenticate(r)
		switch {
		case errors.Is(err, inbox.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="hookinlet"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
			return
		case err != nil:
			s.internalError(r.Context(), w, "authenticate", err)
			return
		case !principal.CanAdmin():
			writeError(w, http.StatusForbidden, "forbidden", "this credential cannot use the admin API")
			return
		}
		h, ok := methods[r.Method]
		if !ok {
			w.Header().Set("Allow", allowHeader)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint accepts "+allowHeader)
			return
		}
		if !s.checkQuery(w, r) {
			return
		}
		h(w, r)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// adminBody reads and checks the JSON body of an admin request before the
// generated handler decodes it: the type, the size, and then the shape the
// document gives it. A body that is not what the document says never reaches the
// handler. Requests without a body pass through.
func (s *Server) adminBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		op := s.operation(r)
		if op == nil {
			// A route the document does not describe would be served with none of the
			// checks below. That is a mistake in the code, so it is refused loudly.
			s.log.LogAttrs(r.Context(), slog.LevelError, "admin route is not in the document", slog.String("route", r.Pattern), slog.String("method", r.Method))
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if op.TakesBody() {
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send application/json")
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBody))
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
				return
			case err != nil:
				writeError(w, http.StatusBadRequest, "bad_request", "the request body could not be read")
				return
			}
			if vs := op.CheckBody(body); len(vs) > 0 {
				writeError(w, http.StatusBadRequest, "invalid_request", "the request body is not valid: "+clip(vs[0].String(), 200))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		withRequest(next)(w, r)
	}
}

// withRequest gives a generated handler what it is not passed: the request (some
// answers depend on the address the caller used, or on its headers) and the writer
// (a download clears its write deadline, an upload sets a read deadline).
func withRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { next(w, withRequestValues(r, w)) }
}

func withRequestValues(r *http.Request, w http.ResponseWriter) *http.Request {
	ctx := context.WithValue(r.Context(), requestKey, r)
	return r.WithContext(context.WithValue(ctx, writerKey, w))
}

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
	attrs = append([]slog.Attr{slog.String("op", op), slog.String("slug", slug)}, attrs...)
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
