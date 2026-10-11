// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"encoding/json"
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
	mux.HandleFunc("/admin/inboxes", s.admin(routes{http.MethodPost: s.adminCreate, http.MethodGet: s.adminList}))
	mux.HandleFunc("/admin/inboxes/{slug}", s.admin(routes{http.MethodGet: s.adminShow, http.MethodPatch: s.adminUpdate, http.MethodDelete: s.adminDelete}))
	mux.HandleFunc("/admin/inboxes/{slug}/secret", s.admin(routes{http.MethodPut: s.adminSetSecret}))
	mux.HandleFunc("/admin/inboxes/{slug}/rotate-token", s.admin(routes{http.MethodPost: s.adminRotate}))
	mux.HandleFunc("/admin/retention/run", s.admin(routes{http.MethodPost: s.adminRetention}))
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

// decodeAdmin reads a JSON request body into v. It is strict: one JSON value,
// no unknown fields, a bounded size. It reports false after answering.
func decodeAdmin(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBody))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return false
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "the request body could not be read")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// The decoder's message names the field and what was wrong with it and
		// repeats no value, so it is safe to show, but not to be trusted for
		// length.
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "the request body is not valid: "+msg)
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "the request body must be a single JSON value")
		return false
	}
	return true
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
	attrs = append([]slog.Attr{slog.String("op", op), slog.String("slug", slug)}, attrs...)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "admin", attrs...)
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

func (s *Server) adminCreate(w http.ResponseWriter, r *http.Request) {
	var p admin.CreateParams
	if !decodeAdmin(w, r, &p) {
		return
	}
	created, err := s.adm.Create(r.Context(), p)
	if err != nil {
		s.adminError(w, r, "create inbox", err)
		return
	}
	created.Inbox = created.Inbox.WithBase(s.baseURL(r))
	s.audit(r, "create inbox", created.Inbox.Slug)
	w.Header().Set("Location", "/admin/inboxes/"+created.Inbox.Slug)
	writeWire(w, r, http.StatusCreated, created)
}

func (s *Server) adminList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var after inbox.ID
	limit := 0
	for k, vs := range q {
		switch {
		case k != "after" && k != "limit":
			writeError(w, http.StatusBadRequest, "invalid_parameter", "unknown parameter")
			return
		case len(vs) > 1:
			writeError(w, http.StatusBadRequest, "invalid_parameter", `parameter "`+k+`": may be given only once`)
			return
		}
	}
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_parameter", `parameter "after": must be a whole number`)
			return
		}
		after = inbox.ID(n)
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_parameter", `parameter "limit": must be a positive whole number`)
			return
		}
		limit = n
	}
	views, next, err := s.adm.List(r.Context(), after, limit)
	if err != nil {
		s.adminError(w, r, "list inboxes", err)
		return
	}
	base := s.baseURL(r)
	for i := range views {
		views[i] = views[i].WithBase(base)
	}
	out := struct {
		Inboxes []admin.View `json:"inboxes"`
		Next    *string      `json:"next"`
	}{Inboxes: views}
	if next != 0 {
		n := "/admin/inboxes?after=" + strconv.FormatInt(int64(next), 10)
		if limit > 0 {
			n += "&limit=" + strconv.Itoa(limit)
		}
		out.Next = &n
	}
	writeWire(w, r, http.StatusOK, out)
}

func (s *Server) adminShow(w http.ResponseWriter, r *http.Request) {
	v, err := s.adm.Get(r.Context(), r.PathValue("slug"))
	if err != nil {
		s.adminError(w, r, "show inbox", err)
		return
	}
	writeWire(w, r, http.StatusOK, v.WithBase(s.baseURL(r)))
}

func (s *Server) adminUpdate(w http.ResponseWriter, r *http.Request) {
	var p admin.Patch
	if !decodeAdmin(w, r, &p) {
		return
	}
	v, err := s.adm.Update(r.Context(), r.PathValue("slug"), p)
	if err != nil {
		s.adminError(w, r, "update inbox", err)
		return
	}
	s.audit(r, "update inbox", v.Slug)
	writeWire(w, r, http.StatusOK, v.WithBase(s.baseURL(r)))
}

func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := s.adm.Delete(r.Context(), slug); err != nil {
		s.adminError(w, r, "delete inbox", err)
		return
	}
	s.audit(r, "delete inbox", slug)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminSetSecret(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Secret string `json:"secret"`
	}
	if !decodeAdmin(w, r, &body) {
		return
	}
	v, err := s.adm.SetSecret(r.Context(), r.PathValue("slug"), body.Secret)
	if err != nil {
		s.adminError(w, r, "set secret", err)
		return
	}
	s.audit(r, "set verifier secret", v.Slug)
	writeWire(w, r, http.StatusOK, v.WithBase(s.baseURL(r)))
}

func (s *Server) adminRotate(w http.ResponseWriter, r *http.Request) {
	c, err := s.adm.RotateToken(r.Context(), r.PathValue("slug"))
	if err != nil {
		s.adminError(w, r, "rotate token", err)
		return
	}
	c.Inbox = c.Inbox.WithBase(s.baseURL(r))
	s.audit(r, "rotate agent token", c.Inbox.Slug)
	writeWire(w, r, http.StatusOK, c)
}

func (s *Server) adminRetention(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx, cancel := s.closingContext(r.Context())
	defer cancel()
	n, err := s.adm.RunRetention(ctx)
	if err != nil {
		s.adminError(w, r, "retention", err)
		return
	}
	s.audit(r, "run retention", "", slog.Int64("deleted", n), slog.Duration("took", time.Since(start).Round(time.Millisecond)))
	writeWire(w, r, http.StatusOK, map[string]int64{"deleted": n})
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
