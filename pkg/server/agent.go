// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"time"
)

// errBusy means no read slot came free within ReadWaitTimeout.
var errBusy = errors.New("server: too many reads in progress")

// holdSlot is readSlot for a handler whose answer is built in memory: the slot is kept
// until the answer has been written to the client (or ResponseTimeout has passed), not
// until the handler returns, so that the slots bound the answers in memory.
func (s *Server) holdSlot(ctx context.Context) (release func(), err error) {
	release, err = s.readSlot(ctx)
	if err != nil {
		return nil, err
	}
	return s.adopt(ctx, release), nil
}

// adopt hands a slot that is already held to the request, to be kept until its answer
// has been written, and starts the response timeout. It returns what the caller should
// still call to release: nothing, if the request took the slot over.
func (s *Server) adopt(ctx context.Context, release func()) func() {
	if rr, ok := ctx.Value(readKey).(*readRequest); ok {
		if rr.deadline != nil {
			rr.deadline()
		}
		if rr.hold == nil {
			rr.hold = release
			return func() {}
		}
	}
	return release
}

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
