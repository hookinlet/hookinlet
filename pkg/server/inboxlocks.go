// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"sync"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// inboxLocks hands out one lock per inbox, so that work which must not overlap
// within an inbox does not hold up any other inbox. A lock exists only while
// someone holds it or waits for it, so the table does not grow with the number of
// inboxes ever seen. The zero value is ready to use.
//
// mu is held only to find, create or drop an entry, never while waiting for an
// inbox's lock or while holding one, so it is never held together with anything
// else.
type inboxLocks struct {
	mu sync.Mutex
	m  map[inbox.ID]*inboxLock
}

type inboxLock struct {
	held  chan struct{} // holds a token while the lock is held
	users int           // holders and waiters; guarded by inboxLocks.mu
}

// lock takes id's lock, waiting while someone else holds it, and returns the
// function that gives it back. If ctx ends first it stops waiting and returns
// ctx's error, so a delivery whose client has gone does not queue behind a slow
// one.
func (l *inboxLocks) lock(ctx context.Context, id inbox.ID) (unlock func(), err error) {
	l.mu.Lock()
	e := l.m[id]
	if e == nil {
		if l.m == nil {
			l.m = make(map[inbox.ID]*inboxLock)
		}
		e = &inboxLock{held: make(chan struct{}, 1)}
		l.m[id] = e
	}
	e.users++
	l.mu.Unlock()

	select {
	case e.held <- struct{}{}:
		return func() { <-e.held; l.leave(id, e) }, nil
	case <-ctx.Done():
		l.leave(id, e)
		return nil, ctx.Err()
	}
}

// leave drops one user of e, and e itself once no one holds or waits for it.
func (l *inboxLocks) leave(id inbox.ID, e *inboxLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.users--; e.users == 0 {
		delete(l.m, id)
	}
}
