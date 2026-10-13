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
	// One credential may hold only some of the slots: a client that asks for big pages
	// and never reads them keeps its slots until ResponseTimeout, and without this it
	// could hold every one, so that every other inbox's readers are turned away.
	if rr, ok := ctx.Value(readKey).(*readRequest); ok && rr.r != nil {
		started := time.Now()
		giveBack, err := s.readShares.acquire(ctx, holderOf(rr.r, rr.principal), s.opts.MaxReadsPerCredential, s.opts.ReadWaitTimeout)
		if err != nil {
			return nil, err
		}
		// ReadWaitTimeout bounds the whole wait, for the credential's share and the shared slot together.
		slot, err := s.globalReadSlot(ctx, max(0, s.opts.ReadWaitTimeout-time.Since(started)))
		if err != nil {
			giveBack()
			return nil, err
		}
		return func() { slot(); giveBack() }, nil
	}
	return s.globalReadSlot(ctx, s.opts.ReadWaitTimeout)
}

// globalReadSlot takes one of the shared slots, waiting up to wait.
func (s *Server) globalReadSlot(ctx context.Context, wait time.Duration) (release func(), err error) {
	select {
	case s.reads <- struct{}{}:
		return func() { <-s.reads }, nil
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case s.reads <- struct{}{}:
		return func() { <-s.reads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errBusy
	}
}
