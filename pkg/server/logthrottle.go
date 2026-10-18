// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"sync"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// logThrottle lets a log line that can repeat for every request out once in a while
// for each inbox, and says how many were left out since the last one. Without it a
// sender holding an inbox's ingest key can fill the log, one line per request, with
// something the operator needs to be told once.
type logThrottle struct {
	every time.Duration
	now   func() time.Time // replaced in tests

	mu   sync.Mutex
	seen map[inbox.ID]*throttled
}

type throttled struct {
	last       time.Time
	suppressed int64
}

// throttleKeep bounds how many inboxes a logThrottle remembers. Past it, those that
// have not logged for a whole interval are forgotten, which loses only the count of
// what was left out.
const throttleKeep = 4096

func newLogThrottle(every time.Duration) *logThrottle {
	return &logThrottle{every: every, now: time.Now, seen: map[inbox.ID]*throttled{}}
}

// allow reports whether to write the line for id now. When it does, skipped is how
// many were not written since the line before it; when it does not, the occurrence
// is counted for the next one.
func (t *logThrottle) allow(id inbox.ID) (write bool, skipped int64) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.seen[id]
	if e == nil {
		if len(t.seen) >= throttleKeep {
			for k, v := range t.seen {
				if now.Sub(v.last) >= t.every {
					delete(t.seen, k)
				}
			}
		}
		t.seen[id] = &throttled{last: now}
		return true, 0
	}
	if now.Sub(e.last) < t.every {
		e.suppressed++
		return false, 0
	}
	skipped = e.suppressed
	e.last, e.suppressed = now, 0
	return true, skipped
}
