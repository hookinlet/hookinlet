// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// agentHandler serves one authenticated, authorized read of one inbox.
type agentHandler func(w http.ResponseWriter, r *http.Request, in *inbox.Inbox)

// registerAgentAPI mounts the read API. It is only mounted when an Authenticator
// is configured, since there is no safe default for who may read.
func (s *Server) registerAgentAPI(mux *http.ServeMux) {
	h := s.apiHandlers(&readAPI{s: s})
	// GET and HEAD are separate operations in the document.
	byMethod := func(get, head http.HandlerFunc) agentHandler {
		return readHandler(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				head(w, r)
				return
			}
			get(w, r)
		})
	}
	mux.HandleFunc("/v1/inboxes/{slug}", s.agent(byMethod(h.GetInbox, h.HeadGetInbox)))
	mux.HandleFunc("/v1/inboxes/{slug}/messages", s.agent(byMethod(h.ListMessages, h.HeadListMessages)))
	mux.HandleFunc("/v1/inboxes/{slug}/messages/{seq}", s.agent(byMethod(h.GetMessage, h.HeadGetMessage)))
	mux.HandleFunc("/v1/inboxes/{slug}/messages/{seq}/raw", s.agent(byMethod(h.GetMessageRaw, h.HeadGetMessageRaw)))
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
