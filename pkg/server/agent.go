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
