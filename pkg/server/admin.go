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
	"strconv"
	"strings"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
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
	mux.HandleFunc("/admin/backup", s.admin(routes{http.MethodGet: s.adminBackup}))
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
		if op := s.operation(r); op != nil && op.TakesBody() {
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
				writeError(w, http.StatusBadRequest, "invalid_request", "the request body is not valid: "+vs[0].String())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		// The generated handlers get only a context, and some answers depend on the
		// request (the address the caller used).
		next(w, r.WithContext(context.WithValue(r.Context(), requestKey, r)))
	}
}

// adminError answers a failed admin operation.
func (s *Server) adminError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, inbox.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_parameter", strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
	case errors.Is(err, inbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, inbox.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "an inbox with that slug already exists, or it was changed at the same time; check and retry")
	case errors.Is(err, admin.ErrBusy):
		writeError(w, http.StatusConflict, "busy", "that is already running")
	case errors.Is(err, admin.ErrNoSecretBox):
		writeError(w, http.StatusNotImplemented, "not_implemented", err.Error())
	case errors.Is(err, admin.ErrNoBackup):
		writeError(w, http.StatusNotImplemented, "not_implemented", err.Error())
	default:
		s.internalError(r.Context(), w, op, err)
	}
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

// ctxReader fails reads once its context has ended.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// audit records an admin action. It names the inbox, never anything secret.
func (s *Server) audit(r *http.Request, op, slug string, attrs ...slog.Attr) {
	s.auditCtx(r.Context(), op, slug, attrs...)
}

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

// adminBackup streams a snapshot of the whole Store: every inbox, message, and
// sealed secret. Only one runs at a time, since each holds a second copy of the
// database on disk until it has been sent.
func (s *Server) adminBackup(w http.ResponseWriter, r *http.Request) {
	if !s.backupMu.TryLock() {
		writeError(w, http.StatusConflict, "busy", "a backup is already running")
		return
	}
	defer s.backupMu.Unlock()

	start := time.Now()
	ctx, cancel := s.closingContext(r.Context())
	defer cancel()
	snap, size, err := s.adm.Backup(ctx)
	if err != nil {
		s.adminError(w, r, "backup", err)
		return
	}
	defer func() { _ = snap.Close() }()

	// A big database can take longer to send than any write deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "application/vnd.sqlite3")
	h.Set("Content-Disposition", `attachment; filename="hookinlet-`+s.opts.Now().UTC().Format("20060102-150405")+`.db"`)
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(w, ctxReader{ctx, snap})
	if err != nil {
		// The status is sent, so all that can be done is to say so and let the
		// connection break, which tells the client the file is not whole.
		s.log.LogAttrs(r.Context(), slog.LevelWarn, "backup interrupted",
			slog.Int64("sent", n), slog.Int64("size", size), slog.String("err", err.Error()))
		return
	}
	s.audit(r, "backup", "", slog.Int64("size", size), slog.Duration("took", time.Since(start).Round(time.Millisecond)))
}
