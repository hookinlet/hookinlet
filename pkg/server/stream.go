// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const (
	// streamPageLimit and streamPageBudget size the reads a stream makes while
	// catching up. They are smaller than the REST API's, because a client
	// reconnecting to a long backlog should receive it in steady pieces, not make
	// the server build one huge page.
	streamPageLimit  = 100
	streamPageBudget = 2 << 20

	// streamWriteTimeout is how long one write to a client may take. A client that
	// stops reading is disconnected rather than allowed to hold a goroutine for
	// good.
	streamWriteTimeout = 30 * time.Second
)

// errStreamClosed is how a stream learns that the server is shutting down.
var errStreamClosed = errors.New("the server is shutting down")

// handleStream serves GET /v1/inboxes/{slug}/stream as Server-Sent Events.
//
// A stream starts at Last-Event-ID (a reconnect), else the after parameter, else
// the present moment with no replay. It then replays what it missed and follows
// new messages as they arrive. Each message is one event whose id is its seq, so
// a client that reconnects with Last-Event-ID resumes exactly where it stopped.
//
// Waking up is the notifier's job and reading is the Store's. A signal only says
// "look again"; the stream then reads from its own cursor. That is why a dropped
// signal costs latency and never data, and why SSERecheckInterval can paper over
// a notifier that drops them.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint accepts GET")
		return
	}
	in, ok := s.authorize(w, r)
	if !ok {
		return
	}
	values, err := wire.QueryValues(r.URL.RawQuery)
	var req wire.StreamRequest
	if err == nil {
		req, err = wire.ParseStream(values, r.Header.Values("Last-Event-ID"))
	}
	if err != nil {
		writeParamError(w, err)
		return
	}

	// Streams have their own limit. They must not use the read slots, which are
	// held for the length of a request and would cap the server at a handful of
	// open streams.
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "overloaded", "too many streams are open; retry shortly")
		return
	}

	ctx := r.Context()
	// Subscribe before reading anything. A message committed after this point
	// signals us; one committed before it is found by the first read.
	signal, cancel := s.opts.Notifier.Subscribe(ctx, in.ID)
	defer cancel()

	cursor := int64(0)
	if req.After != nil {
		cursor = *req.After
	} else {
		latest, err := s.opts.Store.LatestSeq(ctx, in.ID)
		if err != nil {
			s.readError(w, r, "stream start", err)
			return
		}
		cursor = latest
	}

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no") // tell nginx-style proxies not to hold events back
	w.WriteHeader(http.StatusOK)
	if err := s.writeStream(w, rc, []byte(wire.Retry)); err != nil {
		return
	}

	s.log.LogAttrs(ctx, slog.LevelDebug, "stream opened", slog.String("slug", in.Slug), slog.Int64("from", cursor))
	err = s.follow(ctx, w, rc, in, req, cursor, signal)
	switch {
	case err == nil, errors.Is(err, errStreamClosed), errors.Is(err, context.Canceled):
		s.log.LogAttrs(ctx, slog.LevelDebug, "stream closed", slog.String("slug", in.Slug))
	case errors.Is(err, inbox.ErrNotFound):
		s.log.LogAttrs(ctx, slog.LevelInfo, "stream ended: the inbox was deleted", slog.String("slug", in.Slug))
	default:
		// Most often the client went away mid-write.
		s.log.LogAttrs(ctx, slog.LevelDebug, "stream ended", slog.String("slug", in.Slug), slog.String("err", err.Error()))
	}
}

// follow delivers events until the client leaves, the server shuts down, or
// something fails. Each trip around the loop reads everything after the cursor,
// then waits to be told there may be more.
func (s *Server) follow(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, in *inbox.Inbox,
	req wire.StreamRequest, cursor int64, signal <-chan struct{}) error {

	heartbeat := time.NewTimer(s.opts.SSEHeartbeat)
	defer heartbeat.Stop()
	rearm := func() {
		if !heartbeat.Stop() {
			select {
			case <-heartbeat.C:
			default:
			}
		}
		heartbeat.Reset(s.opts.SSEHeartbeat)
	}
	var recheck <-chan time.Time
	if s.opts.SSERecheckInterval > 0 {
		ticker := time.NewTicker(s.opts.SSERecheckInterval)
		defer ticker.Stop()
		recheck = ticker.C
	}

	read := true
	for {
		if read {
			sent, next, err := s.drain(ctx, w, rc, in, req, cursor)
			cursor = next
			if sent {
				rearm() // a message is as good as a ping
			}
			if err != nil {
				return err
			}
		}
		read = true

		select {
		case <-signal:
		case <-recheck:
		case <-heartbeat.C:
			if err := s.writeStream(w, rc, []byte(wire.Ping)); err != nil {
				return err
			}
			heartbeat.Reset(s.opts.SSEHeartbeat)
			read = false // nothing changed; there is no need to look
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closing:
			return errStreamClosed
		}
	}
}

// drain sends every message after the cursor that matches the stream's filters,
// a page at a time, and returns the new cursor and whether anything was sent.
func (s *Server) drain(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, in *inbox.Inbox,
	req wire.StreamRequest, cursor int64) (sent bool, next int64, err error) {

	for {
		page, err := s.fetchForStream(ctx, in, req, cursor)
		if err != nil {
			return sent, cursor, err
		}
		for _, m := range page.Messages {
			frame, err := wire.Event(m)
			if err != nil {
				// Should be impossible, since JSON never contains a raw line break.
				// A frame that could be forged is dropped, not sent.
				s.log.LogAttrs(ctx, slog.LevelError, "event not sent", slog.String("slug", in.Slug), slog.Int64("seq", m.Seq), slog.String("err", err.Error()))
				continue
			}
			if err := s.writeStream(w, rc, frame); err != nil {
				return sent, cursor, err
			}
			sent = true
		}
		cursor = page.Cursor
		if page.Complete {
			return sent, cursor, nil
		}
		// Replaying a long backlog: stay responsive to shutdown and disconnects.
		select {
		case <-ctx.Done():
			return sent, cursor, ctx.Err()
		case <-s.closing:
			return sent, cursor, errStreamClosed
		default:
		}
	}
}

// fetchForStream reads one page, holding a read slot only while it does, never
// while waiting for new messages or writing to a slow client.
func (s *Server) fetchForStream(ctx context.Context, in *inbox.Inbox, req wire.StreamRequest, cursor int64) (*wire.Page, error) {
	select {
	case s.reads <- struct{}{}:
		defer func() { <-s.reads }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closing:
		return nil, errStreamClosed
	}
	after := cursor
	return wire.FetchPage(ctx, s.opts.Store, in, wire.PageRequest{
		Query: inbox.Query{
			After:    &after,
			Limit:    streamPageLimit,
			Types:    req.Types,
			Verified: req.Verified,
		},
		Limit:  streamPageLimit,
		Budget: streamPageBudget,
	})
}

// writeStream writes and flushes one chunk, giving the client a bounded time to
// accept it.
func (s *Server) writeStream(w http.ResponseWriter, rc *http.ResponseController, data []byte) error {
	// Servers that cannot set a deadline (a test recorder) just do without one.
	_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
