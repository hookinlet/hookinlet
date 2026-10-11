// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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
		in, ok := s.authorize(w, r)
		if !ok || !s.checkQuery(w, r) {
			return
		}
		// Only authenticated, authorized requests take a slot, so anonymous
		// traffic cannot starve real readers.
		release, ok := s.acquireRead(w, r)
		if !ok {
			return
		}
		defer release()
		h(w, r, in)
	}
}

// authorize runs the checks every read of an inbox shares: it authenticates the
// caller and finds the inbox only if the caller may read it. It writes the
// response itself when it cannot say yes.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*inbox.Inbox, bool) {
	// Responses carry message content and depend on who is asking. Some of that
	// content is chosen by whoever sent the webhook, so no client may guess a
	// type for it.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	ctx := r.Context()
	principal, err := s.opts.Authenticator.Authenticate(r)
	switch {
	case errors.Is(err, inbox.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="hookinlet"`)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return nil, false
	case err != nil:
		s.internalError(ctx, w, "authenticate", err)
		return nil, false
	}

	in, err := s.opts.Store.GetInboxBySlug(ctx, r.PathValue("slug"))
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return nil, false
	case err != nil:
		s.internalError(ctx, w, "load inbox", err)
		return nil, false
	case !principal.CanReadInbox(in):
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return nil, false
	}
	return in, true
}

func (s *Server) handleInboxInfo(w http.ResponseWriter, r *http.Request, in *inbox.Inbox) {
	if !noParams(w, r) {
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
	values, err := wire.QueryValues(r.URL.RawQuery)
	if err != nil {
		writeParamError(w, err)
		return
	}
	req, err := wire.ParseMessages(values)
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
	if !noParams(w, r) {
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
	if !noParams(w, r) {
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
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
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

// noParams answers a request that carries any query parameter to an endpoint
// that takes none, and reports whether the handler may go on.
func noParams(w http.ResponseWriter, r *http.Request) bool {
	values, err := wire.QueryValues(r.URL.RawQuery)
	if err == nil {
		err = wire.RejectParams(values)
	}
	if err != nil {
		writeParamError(w, err)
		return false
	}
	return true
}

// acquireRead waits for one of the limited read slots. It reports false after
// answering, or after giving up silently because the client has gone away.
func (s *Server) acquireRead(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	release, err := s.readSlot(r.Context())
	switch {
	case err == nil:
		return release, true
	case errors.Is(err, errBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "overloaded", "the server is busy; retry shortly")
	}
	return nil, false
}

// errBusy means no read slot came free within ReadWaitTimeout.
var errBusy = errors.New("server: too many reads in progress")

// readSlot takes one of the limited read slots, waiting up to ReadWaitTimeout.
// It fails with errBusy if none comes free and with the context's error if the
// caller goes away first.
func (s *Server) readSlot(ctx context.Context) (release func(), err error) {
	select {
	case s.reads <- struct{}{}:
		return func() { <-s.reads }, nil
	default:
	}
	wait := time.NewTimer(s.opts.ReadWaitTimeout)
	defer wait.Stop()
	select {
	case s.reads <- struct{}{}:
		return func() { <-s.reads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-wait.C:
		return nil, errBusy
	}
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
