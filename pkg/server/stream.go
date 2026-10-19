// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const (
	// streamPageLimit and streamPageBudget size the reads a stream makes while
	// catching up. The budget is small on purpose: a page is held in memory while
	// it is written to a client that may be slow, and every open stream holds one.
	// A message larger than the budget still goes out, as a page of its own.
	streamPageLimit  = 100
	streamPageBudget = 256 << 10

	// streamWriteTimeout is how long one write to a client may take. A client that
	// stops reading is disconnected rather than allowed to hold a goroutine and a
	// page of memory for good.
	streamWriteTimeout = 30 * time.Second
)

var (
	// errStreamClosed is how a stream learns that the server is shutting down.
	errStreamClosed = errors.New("the server is shutting down")
	// errStreamRevoked means the stream's credential or inbox is no longer valid.
	errStreamRevoked = errors.New("the stream is no longer authorized")
)

// clientGoneError wraps a failure to write to the client, which is the client's
// doing (it left, or stopped reading) and not a fault on our side.
type clientGoneError struct{ err error }

func (e *clientGoneError) Error() string { return "client gone: " + e.err.Error() }
func (e *clientGoneError) Unwrap() error { return e.err }

// takeStream takes a stream slot, both a global one and one of the inbox's share.
// The per-inbox share keeps one token from using them all. It returns the reason
// for refusing, or a release function. Anything that holds a connection open for a
// long time on an inbox's behalf, such as an MCP wait, takes a slot this way, so
// that one token cannot open an unbounded number of them.
//
// The embedder's Admitter, if any, is asked first (kind says whether this is a stream
// or a wait). Its refusal comes back as err, an *admitRefusal, and is not the same
// thing as the server's own limits, which are the refusal string.
func (s *Server) takeStream(ctx context.Context, p inbox.Principal, in *inbox.Inbox, kind inbox.AccessKind, h holder) (release func(), refusal string, err error) {
	admitted, err := s.admit(ctx, p, in, kind)
	if err != nil {
		return nil, "", err
	}
	id := in.ID
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.streamCounts[id] >= s.opts.MaxStreamsPerInbox {
		admitted()
		return nil, "too many streams are open on this inbox; retry shortly", nil
	}
	giveBack, err := s.streamShares.acquire(context.Background(), h, s.opts.MaxStreamsPerCredential, 0)
	if err != nil {
		admitted()
		return nil, "too many streams are open for this credential; retry shortly", nil
	}
	select {
	case s.streams <- struct{}{}:
	default:
		giveBack()
		admitted()
		return nil, "too many streams are open; retry shortly", nil
	}
	s.streamCounts[id]++
	return func() {
		giveBack()
		s.streamMu.Lock()
		defer s.streamMu.Unlock()
		if s.streamCounts[id]--; s.streamCounts[id] <= 0 {
			delete(s.streamCounts, id)
		}
		<-s.streams
		admitted()
	}, "", nil
}

// stillAllowed returns a check, run periodically, that a stream's credential is
// still valid and its inbox still exists and is readable. It reports false only
// when it has been told no. If the answer cannot be had (the Store is having a
// moment) it reports true with the error: not being able to check is not the same
// as being refused, and a passing database error must not disconnect every agent.
func (s *Server) stillAllowed(r *http.Request, id inbox.ID) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		// ctx carries the AccessInfo of the stream or the MCP tool call (see enforce
		// and mcpTools.bind), which is what the Authenticator is told again. A caller
		// that has none still gets the request's own.
		if _, ok := inbox.AccessFrom(ctx); !ok {
			ctx = inbox.WithAccess(ctx, s.accessInfo(r, ""))
		}
		principal, err := s.opts.Authenticator.Authenticate(r.WithContext(ctx))
		switch {
		case errors.Is(err, inbox.ErrUnauthenticated):
			return false, nil
		case err != nil:
			return true, err
		case s.refusesUntenanted(ctx, principal):
			return false, nil
		}
		current, err := s.opts.Store.GetInbox(scopedTo(ctx, principal), id)
		switch {
		case errors.Is(err, inbox.ErrNotFound):
			return false, nil
		case err != nil:
			return true, err
		}
		return principal.CanReadInbox(current), nil
	}
}

// jitter spreads d by up to 10% either way, so that streams opened together do
// not all come due at the same instant forever.
func jitter(d time.Duration) time.Duration {
	return d - d/10 + rand.N(d/5+1) //nolint:gosec // timing noise, not security
}

// follow delivers events until the client leaves, the server shuts down, or
// something fails. Each trip around the loop reads everything after the cursor,
// then waits to be told there may be more.
func (s *Server) follow(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, in *inbox.Inbox,
	req wire.StreamRequest, cursor int64, signal <-chan struct{}, stillAllowed func(context.Context) (bool, error)) error {

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

	// Timers, not tickers, so each interval can be jittered and the first one
	// starts at a random point in it.
	var recheck <-chan time.Time
	var recheckTimer *time.Timer
	if iv := s.opts.SSERecheckInterval; iv > 0 {
		recheckTimer = time.NewTimer(rand.N(iv) + 1) //nolint:gosec // timing noise, not security
		defer recheckTimer.Stop()
		recheck = recheckTimer.C
	}
	revalidateTimer := time.NewTimer(jitter(s.opts.StreamRevalidateInterval))
	defer revalidateTimer.Stop()

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
			recheckTimer.Reset(jitter(s.opts.SSERecheckInterval))
		case <-revalidateTimer.C:
			revalidateTimer.Reset(jitter(s.opts.StreamRevalidateInterval))
			read = false
			ok, err := stillAllowed(ctx)
			if err != nil {
				s.log.LogAttrs(ctx, slog.LevelWarn, "could not recheck a stream's authorization; keeping it open",
					slog.String("slug", in.Slug), slog.String("err", err.Error()))
			}
			if !ok {
				return errStreamRevoked
			}
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

		// Encode the page and let go of the messages: only the encoded bytes are
		// held while writing, which may take a while if the client is slow.
		var frames bytes.Buffer
		for _, m := range page.Messages {
			frame, err := wire.Event(m)
			if err != nil {
				// Should be impossible, since JSON never contains a raw line break.
				// A frame that could be forged is dropped, not sent.
				s.log.LogAttrs(ctx, slog.LevelError, "event not sent", slog.String("slug", in.Slug), slog.Int64("seq", m.Seq), slog.String("err", err.Error()))
				continue
			}
			frames.Write(frame)
		}
		// A page that starts below what is kept says so, however late in the stream
		// that happens: a slow client, or retention running during a backlog.
		if page.Gap {
			if err := s.writeStream(w, rc, wire.GapEvent(cursor, page.OldestSeq, in.Epoch)); err != nil {
				return sent, cursor, err
			}
		}
		// Assign, don't declare: the cursor must carry over to the next page.
		cursor = page.Cursor
		complete := page.Complete
		page.Messages = nil

		if frames.Len() > 0 {
			if err := s.writeStream(w, rc, frames.Bytes()); err != nil {
				return sent, cursor, err
			}
			sent = true
		}
		if complete {
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
// accept it. A failure is the client's: it left or stopped reading.
func (s *Server) writeStream(w http.ResponseWriter, rc *http.ResponseController, data []byte) error {
	// Servers that cannot set a deadline (a test recorder) just do without one.
	_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	// Checked after the deadline is set: shutdown sets the deadline to "now", and
	// if that lands before ours it also closes this channel, so we see it here.
	select {
	case <-s.closing:
		return errStreamClosed
	default:
	}
	if _, err := w.Write(data); err != nil {
		return &clientGoneError{err}
	}
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return &clientGoneError{err}
	}
	return nil
}
