// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hookinlet/hookinlet/internal/api"
	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// DefaultMaxReadersPerInbox is the default for Options.MaxReadersPerInbox: how many
// named readers one inbox keeps. A reader is a small row, and a place is taken only by
// an explicit acknowledgement from a caller that can read the inbox, so the cap bounds
// what a leaked or careless read credential can fill, not what the Store can hold.
const DefaultMaxReadersPerInbox = 100

// readerAPI serves the operations on an inbox's named readers: how far each has read
// (listReaders), acknowledging a position (ackReader), and forgetting a reader
// (forgetReader). Nothing else records a position: reading an inbox never does, so a
// read stays a read.
//
// They need a Store that implements inbox.ReaderTracker. Without one, or with the
// feature turned off (a negative Options.MaxReadersPerInbox), they answer 400
// "unsupported" rather than accepting a position that is not kept.
type readerAPI struct {
	s *Server
}

const msgReadersUnsupported = "this server does not keep named readers"

func unsupportedReaders(ctx context.Context) api.BadRequestJSONResponse {
	return badRequest(ctx, fail{status: http.StatusBadRequest, code: "unsupported", msg: msgReadersUnsupported})
}

// tracker is the Store's capability, or nil if named readers are not offered.
func (a *readerAPI) tracker() inbox.ReaderTracker {
	if a.s.opts.MaxReadersPerInbox < 0 {
		return nil
	}
	return a.s.readers
}

func readerView(p inbox.ReaderPosition, oldest, latest int64) api.Reader {
	return api.Reader{
		Name: p.Name, Seq: int(p.Seq), UpdatedAt: p.UpdatedAt,
		Lag: int(max(0, latest-p.Seq)), Gap: wire.Gap(p.Seq, oldest, latest),
	}
}

func (a *readerAPI) ListReaders(ctx context.Context, _ api.ListReadersRequestObject) (api.ListReadersResponseObject, error) {
	tr := a.tracker()
	if tr == nil {
		return api.ListReaders400JSONResponse{BadRequestJSONResponse: unsupportedReaders(ctx)}, nil
	}
	release, busy, err := (&readAPI{s: a.s}).slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.ListReaders503JSONResponse{OverloadedJSONResponse: *busy}, nil
	}
	defer release()
	rr := readFrom(ctx)
	st := a.s.opts.Store
	list, err := tr.Readers(ctx, rr.in.ID)
	var stats *inbox.InboxStats
	var cur *inbox.Inbox
	if err == nil {
		stats, err = st.Stats(ctx, rr.in.ID)
	}
	if err == nil {
		cur, err = st.GetInbox(ctx, rr.in.ID)
	}
	if err != nil {
		f := a.s.classifyRead(ctx, "list readers", err)
		if f.status == http.StatusNotFound {
			return api.ListReaders404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.ListReaders500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	out := api.ReaderList{
		Epoch: cur.Epoch, LatestSeq: int(stats.LatestSeq), OldestSeq: int(stats.OldestSeq),
		MaxReaders: a.s.opts.MaxReadersPerInbox, Readers: make([]api.Reader, 0, len(list)),
	}
	for _, p := range list {
		// A position of another epoch would be a number that means something else.
		if p.Epoch == cur.Epoch {
			out.Readers = append(out.Readers, readerView(p, stats.OldestSeq, stats.LatestSeq))
		}
	}
	return api.ListReaders200JSONResponse{Body: out, Headers: api.ListReaders200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *readerAPI) AckReader(ctx context.Context, req api.AckReaderRequestObject) (api.AckReaderResponseObject, error) {
	tr := a.tracker()
	if tr == nil {
		return api.AckReader400JSONResponse{BadRequestJSONResponse: unsupportedReaders(ctx)}, nil
	}
	if req.Body == nil {
		return api.AckReader400JSONResponse{BadRequestJSONResponse: badRequest(ctx, fail{status: http.StatusBadRequest, code: "invalid_request", msg: "the request body is required"})}, nil
	}
	release, busy, err := (&readAPI{s: a.s}).slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.AckReader503JSONResponse{OverloadedJSONResponse: *busy}, nil
	}
	defer release()
	rr := readFrom(ctx)
	pos, err := tr.AckReader(ctx, rr.in.ID, req.Name, req.Body.Epoch, int64(req.Body.Seq), a.s.opts.MaxReadersPerInbox)
	var stats *inbox.InboxStats
	if err == nil {
		stats, err = a.s.opts.Store.Stats(ctx, rr.in.ID)
	}
	if err != nil {
		switch {
		case errors.Is(err, inbox.ErrEpochChanged):
			return api.AckReader409JSONResponse{ReaderConflictJSONResponse: readerConflict(ctx, api.ErrorReaderConflictErrorCodeEpochChanged,
				"the position is for an earlier epoch of this inbox; read the inbox's current epoch and acknowledge again")}, nil
		case errors.Is(err, inbox.ErrConflict):
			return api.AckReader409JSONResponse{ReaderConflictJSONResponse: readerConflict(ctx, api.ErrorReaderConflictErrorCodeTooManyReaders,
				fmt.Sprintf("this inbox already has the most named readers (%d); delete one that is not used", a.s.opts.MaxReadersPerInbox))}, nil
		}
		f := a.s.classifyRead(ctx, "acknowledge reader", err)
		switch f.status {
		case http.StatusBadRequest:
			return api.AckReader400JSONResponse{BadRequestJSONResponse: f.badRequest(ctx)}, nil
		case http.StatusNotFound:
			return api.AckReader404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.AckReader500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	return api.AckReader200JSONResponse{Body: readerView(pos, stats.OldestSeq, stats.LatestSeq), Headers: api.AckReader200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *readerAPI) ForgetReader(ctx context.Context, req api.ForgetReaderRequestObject) (api.ForgetReaderResponseObject, error) {
	tr := a.tracker()
	if tr == nil {
		return api.ForgetReader400JSONResponse{BadRequestJSONResponse: unsupportedReaders(ctx)}, nil
	}
	release, busy, err := (&readAPI{s: a.s}).slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.ForgetReader503JSONResponse{OverloadedJSONResponse: *busy}, nil
	}
	defer release()
	if err := tr.ForgetReader(ctx, readFrom(ctx).in.ID, req.Name); err != nil {
		f := a.s.classifyRead(ctx, "forget reader", err)
		switch f.status {
		case http.StatusBadRequest:
			return api.ForgetReader400JSONResponse{BadRequestJSONResponse: f.badRequest(ctx)}, nil
		case http.StatusNotFound:
			return api.ForgetReader404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.ForgetReader500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	return api.ForgetReader204Response{Headers: api.ForgetReader204ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func readerConflict(ctx context.Context, code api.ErrorReaderConflictErrorCode, msg string) api.ReaderConflictJSONResponse {
	var b api.ErrorReaderConflict
	b.Error.Code, b.Error.Message = code, msg
	return api.ReaderConflictJSONResponse{Body: b, Headers: api.ReaderConflictResponseHeaders{XRequestId: requestIDOf(ctx)}}
}
