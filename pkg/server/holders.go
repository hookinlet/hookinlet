// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// holder says who is asking, for the limits that stop one credential from using up what
// every caller shares (read slots, stream slots). An administrator is not limited by
// them: the console is one person, and the credential is the one that can fix a server
// that is being leaned on.
type holder struct {
	key   string
	admin bool
}

// holderOf identifies the credential a request was made with. The key is a digest of the
// Authorization header, kept only in memory, so a token is never stored or logged.
func holderOf(r *http.Request, p inbox.Principal) holder {
	if p != nil && p.CanAdmin() {
		return holder{admin: true}
	}
	sum := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return holder{key: hex.EncodeToString(sum[:8])}
}

// shares limits how many of something one credential may hold at once. A credential that
// holds its limit waits for one of its own to come back (up to a time the caller names) or
// is refused at once; other credentials are not affected either way.
type shares struct {
	mu sync.Mutex
	m  map[string]*share
}

type share struct {
	slots chan struct{}
	users int // callers that have this entry, waiting or holding
}

// acquire takes one for h, waiting up to wait for one to free (no wait at all if wait is
// zero or less), and returns what gives it back. It fails with errBusy when none came
// free, and with the context's error if the caller went away first.
func (s *shares) acquire(ctx context.Context, h holder, limit int, wait time.Duration) (release func(), err error) {
	if h.admin || limit <= 0 {
		return func() {}, nil
	}
	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[string]*share)
	}
	e := s.m[h.key]
	if e == nil {
		e = &share{slots: make(chan struct{}, limit)}
		s.m[h.key] = e
	}
	e.users++
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		if e.users--; e.users <= 0 {
			delete(s.m, h.key)
		}
		s.mu.Unlock()
	}
	select {
	case e.slots <- struct{}{}:
		return func() { <-e.slots; drop() }, nil
	default:
	}
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case e.slots <- struct{}{}:
			return func() { <-e.slots; drop() }, nil
		case <-ctx.Done():
			drop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	drop()
	return nil, errBusy
}
