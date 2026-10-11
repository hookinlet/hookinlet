// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hookinlet/hookinlet/internal/api"
	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// streamAPI serves GET /v1/inboxes/{slug}/stream through the generated interface.
//
// What the document lists for the operation is what can happen before the stream
// starts (a bad parameter, an inbox that is gone, too many streams open), and
// those are typed responses like any other. Once it starts the answer is 200 and
// the body never ends; that is the sseBody below.
type streamAPI struct {
	s *Server
}

// StreamMessages starts a stream. It follows Last-Event-ID (a reconnect), else the
// after parameter, else the present moment with no replay. It then replays what it
// missed and follows new messages as they arrive. Each message is one event whose
// id is its seq, so a client that reconnects with Last-Event-ID resumes exactly
// where it stopped.
//
// Waking up is the notifier's job and reading is the Store's. A signal only says
// "look again"; the stream then reads from its own cursor. That is why a dropped
// signal costs latency and never data, and why SSERecheckInterval can paper over a
// notifier that drops them.
func (a *streamAPI) StreamMessages(ctx context.Context, request api.StreamMessagesRequestObject) (api.StreamMessagesResponseObject, error) {
	s := a.s
	rr := readFrom(ctx)
	id := requestIDOf(ctx)
	sp := request.Params

	req, err := wire.NewStreamRequest(streamParams(sp))
	if err != nil {
		return api.StreamMessages400JSONResponse{BadRequestJSONResponse: badRequest(ctx, fail{status: 400, code: "invalid_parameter", msg: err.Error()})}, nil
	}

	// A cursor from another epoch of the inbox is refused before anything is sent.
	if err := wire.CheckEpoch(rr.in, req.Epoch); err != nil {
		f := s.classifyRead(ctx, "stream start", err)
		return api.StreamMessages409JSONResponse{EpochChangedJSONResponse: f.epochChanged(ctx)}, nil
	}

	// Streams have their own limits. They must not use the read slots, which are
	// held for the length of a request and would cap the server at a handful of
	// open streams.
	release, refusal := s.takeStream(rr.in.ID)
	if refusal != "" {
		var b api.ErrorOverloaded
		b.Error.Code, b.Error.Message = api.ErrorOverloadedErrorCodeOverloaded, refusal
		return api.StreamMessages503JSONResponse{OverloadedJSONResponse: api.OverloadedJSONResponse{
			Body: b, Headers: api.OverloadedResponseHeaders{RetryAfter: "5", XRequestId: id},
		}}, nil
	}

	// Subscribe before reading anything. A message committed after this point
	// signals us; one committed before it is found by the first read.
	signal, cancel := s.opts.Notifier.Subscribe(ctx, rr.in.ID)
	latest, err := s.opts.Store.LatestSeq(ctx, rr.in.ID)
	if err != nil {
		cancel()
		release()
		switch f := s.classifyRead(ctx, "stream start", err); f.status {
		case http.StatusBadRequest:
			return api.StreamMessages400JSONResponse{BadRequestJSONResponse: f.badRequest(ctx)}, nil
		case http.StatusNotFound:
			return api.StreamMessages404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.StreamMessages500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	cursor, ahead := latest, req.After != nil && *req.After > latest
	var oldest int64
	var gap bool
	if req.After != nil {
		cursor = *req.After
		if cursor > 0 && !ahead {
			st, err := s.opts.Store.Stats(ctx, rr.in.ID)
			if err != nil {
				cancel()
				release()
				switch f := s.classifyRead(ctx, "stream start", err); f.status {
				case http.StatusNotFound:
					return api.StreamMessages404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
				}
				return api.StreamMessages500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
			}
			oldest, gap = st.OldestSeq, wire.Gap(cursor, st.OldestSeq, st.LatestSeq)
		}
	}

	rr.w.Header().Set("X-Accel-Buffering", "no") // tell nginx-style proxies not to hold events back
	return api.StreamMessages200TexteventStreamResponse{
		Body: &sseBody{s: s, ctx: ctx, rr: rr, req: req, cursor: cursor, latest: latest, ahead: ahead, gap: gap, oldest: oldest,
			signal: signal, release: func() { cancel(); release() }},
		Headers: api.StreamMessages200ResponseHeaders{XRequestId: id},
	}, nil
}

// sseBody is an open stream. The generated response copies it to the client with
// WriteTo, which is where the stream runs until the client leaves, the server shuts
// down, or something fails; Read is never used.
type sseBody struct {
	s       *Server
	ctx     context.Context
	rr      *readRequest
	req     wire.StreamRequest
	cursor  int64
	latest  int64
	ahead   bool
	gap     bool  // messages after the starting cursor were removed
	oldest  int64 // the lowest seq stored when the stream opened
	signal  <-chan struct{}
	release func()
}

func (b *sseBody) Read([]byte) (int, error) {
	return 0, errors.New("server: an event stream is written with WriteTo")
}

func (b *sseBody) Close() error {
	b.release()
	b.release = func() {}
	return nil
}

func (b *sseBody) WriteTo(w io.Writer) (int64, error) {
	s, ctx, in := b.s, b.ctx, b.rr.in
	rw := b.rr.w
	rc := http.NewResponseController(rw)

	// If the server starts shutting down while a write is blocked on a client that
	// has stopped reading, abort that write now. Waiting out the write timeout
	// would hold shutdown past its deadline, and the fallback for that closes
	// connections that have deliveries mid-store.
	stopAbort := context.AfterFunc(s.closeCtx, func() { _ = rc.SetWriteDeadline(time.Now()) })
	defer stopAbort()

	if err := s.writeStream(rw, rc, []byte(wire.Retry)); err != nil {
		return 0, nil
	}
	if b.gap {
		if err := s.writeStream(rw, rc, wire.GapEvent(b.cursor, b.oldest, in.Epoch)); err != nil {
			return 0, nil
		}
	}
	if b.ahead {
		// Typical after an inbox is recreated: the client remembers a position the
		// new inbox has not reached. Nothing is wrong, but nothing will arrive until
		// the sequence catches up, which looks exactly like a hung stream.
		note := fmt.Sprintf(": the requested position (%d) is ahead of the newest message (%d); waiting for new messages\n\n", b.cursor, b.latest)
		if err := s.writeStream(rw, rc, []byte(note)); err != nil {
			return 0, nil
		}
	}

	s.log.LogAttrs(ctx, slog.LevelDebug, "stream opened", slog.String("slug", in.Slug), slog.Int64("from", b.cursor))
	err := s.follow(ctx, rw, rc, in, b.req, b.cursor, b.signal, s.stillAllowed(b.rr.r, in.ID))

	var gone *clientGoneError
	switch {
	case err == nil, errors.Is(err, errStreamClosed), errors.Is(err, context.Canceled):
		s.log.LogAttrs(ctx, slog.LevelDebug, "stream closed", slog.String("slug", in.Slug))
	case errors.Is(err, errStreamRevoked):
		s.log.LogAttrs(ctx, slog.LevelInfo, "stream ended: no longer authorized", slog.String("slug", in.Slug))
	case errors.Is(err, inbox.ErrNotFound):
		s.log.LogAttrs(ctx, slog.LevelInfo, "stream ended: the inbox was deleted", slog.String("slug", in.Slug))
	case errors.As(err, &gone):
		// The client left or stopped reading.
		s.log.LogAttrs(ctx, slog.LevelDebug, "stream ended by the client", slog.String("slug", in.Slug), slog.String("err", err.Error()))
	default:
		s.log.LogAttrs(ctx, slog.LevelError, "stream failed", slog.String("slug", in.Slug), slog.String("err", err.Error()))
	}
	// Whatever ended it, the response is over: the status is sent and a stream has
	// no result to report.
	return 0, nil
}

// streamParams reads the generated parameters of the stream. A test sets every
// field of the generated struct and fails if one is not read here.
func streamParams(p api.StreamMessagesParams) wire.StreamParams {
	return wire.StreamParams{After: p.After, Types: deref(p.Type), Verified: p.Verified, Epoch: p.Epoch, LastEventID: p.LastEventID}
}
