// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// agentHandler serves one authenticated, authorized read of one inbox.
type agentHandler func(w http.ResponseWriter, r *http.Request, in *inbox.Inbox)

// registerAgentAPI mounts the read API. It is only mounted when an Authenticator
// is configured, since there is no safe default for who may read.
func (s *Server) registerAgentAPI(mux *http.ServeMux) {
	mux.HandleFunc("/v1/inboxes/{slug}", s.agent(s.handleInboxInfo))
	mux.HandleFunc("/v1/inboxes/{slug}/messages", s.agent(s.handleMessages))
	mux.HandleFunc("/v1/inboxes/{slug}/messages/{seq}", s.agent(s.handleMessage))
	mux.HandleFunc("/v1/inboxes/{slug}/messages/{seq}/raw", s.agent(s.handleRaw))
}

// agent wraps a handler with the checks every read shares, in this order:
//
//  1. Only GET (and HEAD) are allowed.
//  2. The caller must authenticate. A bad credential is 401 whatever inbox the
//     URL names, so it reveals nothing about which inboxes exist.
//  3. The inbox must exist and the caller must be allowed to read it. Both
//     failures are the same 404, so one inbox's credential cannot be used to
//     learn whether another inbox exists (guardrail 25).
func (s *Server) agent(h agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint accepts GET and HEAD")
			return
		}
		// Responses carry message content and depend on who is asking. Some of
		// that content is chosen by whoever sent the webhook, so no client may
		// guess a type for it.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		ctx := r.Context()
		principal, err := s.opts.Authenticator.Authenticate(r)
		switch {
		case errors.Is(err, inbox.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="hookinlet"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
			return
		case err != nil:
			s.internalError(ctx, w, "authenticate", err)
			return
		}

		in, err := s.opts.Store.GetInboxBySlug(ctx, r.PathValue("slug"))
		switch {
		case errors.Is(err, inbox.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		case err != nil:
			s.internalError(ctx, w, "load inbox", err)
			return
		case !principal.CanReadInbox(in):
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		h(w, r, in)
	}
}

func (s *Server) handleInboxInfo(w http.ResponseWriter, r *http.Request, in *inbox.Inbox) {
	if len(r.URL.Query()) > 0 {
		writeParamError(w, &wire.ParamError{Param: firstKey(r), Reason: "unknown parameter"})
		return
	}
	info, err := wire.FetchInfo(r.Context(), s.opts.Store, in)
	if err != nil {
		s.readError(w, r, "inbox info", err)
		return
	}
	writeWire(w, r, http.StatusOK, info)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request, in *inbox.Inbox) {
	req, err := wire.ParseMessages(r.URL.Query())
	if err != nil {
		writeParamError(w, err)
		return
	}
	page, err := wire.FetchPage(r.Context(), s.opts.Store, in, req)
	if err != nil {
		s.readError(w, r, "messages", err)
		return
	}
	writeWire(w, r, http.StatusOK, page)
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request, in *inbox.Inbox) {
	if len(r.URL.Query()) > 0 {
		writeParamError(w, &wire.ParamError{Param: firstKey(r), Reason: "unknown parameter"})
		return
	}
	m, ok := s.loadMessage(w, r, in)
	if !ok {
		return
	}
	writeWire(w, r, http.StatusOK, wire.FromStored(m, in.Slug, in.Verifier, true))
}

// handleRaw returns the exact bytes that were received, with the Content-Type
// the sender used.
//
// Those bytes and that type are chosen by whoever sent the webhook, so they are
// served as something to download and never as a page: nosniff stops a browser
// from guessing a more dangerous type, the sandbox policy neutralizes any
// script in HTML or SVG that does get rendered, and attachment tells it to save
// the file instead. None of this changes the body or the Content-Type.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request, in *inbox.Inbox) {
	if len(r.URL.Query()) > 0 {
		writeParamError(w, &wire.ParamError{Param: firstKey(r), Reason: "unknown parameter"})
		return
	}
	m, ok := s.loadMessage(w, r, in)
	if !ok {
		return
	}
	h := w.Header()
	if m.ContentType != "" {
		h.Set("Content-Type", m.ContentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Disposition", "attachment")
	h.Set("Content-Length", strconv.Itoa(len(m.Body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(m.Body)
	}
}

// loadMessage reads the seq from the path and loads that message, writing the
// response itself when it cannot. A seq that cannot be a sequence number is
// simply a message that does not exist.
func (s *Server) loadMessage(w http.ResponseWriter, r *http.Request, in *inbox.Inbox) (*inbox.Message, bool) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq < 1 || strconv.FormatInt(seq, 10) != r.PathValue("seq") {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return nil, false
	}
	m, err := s.opts.Store.Get(r.Context(), in.ID, seq)
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return nil, false
	case err != nil:
		s.readError(w, r, "message", err)
		return nil, false
	}
	return m, true
}

// readError answers a failed read: a bad query is the caller's fault, an
// inbox deleted mid-request is a 404, and anything else is ours.
func (s *Server) readError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, inbox.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
	case errors.Is(err, inbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	default:
		s.internalError(r.Context(), w, what, err, slog.String("path", r.Pattern))
	}
}

func writeParamError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
}

func firstKey(r *http.Request) string {
	for k := range r.URL.Query() {
		return k
	}
	return ""
}

// writeWire writes v as JSON using the shared encoder. It builds the whole body
// first so that an encoding failure can still become a 500 instead of a
// truncated 200.
func writeWire(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := wire.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		// This is JSON produced by the encoder, sent as application/json with
		// nosniff. Sender-controlled strings inside it are escaped as JSON
		// strings, so it cannot be read as markup.
		_, _ = w.Write(body) //nolint:gosec // JSON from the encoder, served as application/json with nosniff
	}
}
