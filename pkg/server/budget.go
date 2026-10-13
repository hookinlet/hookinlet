// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"errors"
	"io"
	"sync"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// byteBudget counts bytes held, never more than limit at once, without ever making
// anyone wait: an addition that does not fit is refused, and the caller tells its
// client to come back. A limit below zero allows anything.
type byteBudget struct {
	limit int64

	mu      sync.Mutex
	used    int64
	byInbox map[inbox.ID]int64
}

// perInbox is the most of the budget one inbox's deliveries may hold at once: an eighth of
// it, but never less than its largest body (or one delivery at that size could not be
// received at all). Whoever has an inbox's key can send slow bodies, and without a share of
// its own that one inbox could fill the whole budget and turn every other inbox's deliveries
// away. With a budget only a little larger than the largest bodies the share is large too,
// since one such delivery needs it.
func (b *byteBudget) perInbox(maxBody int64) int64 {
	return max(b.limit/8, maxBody)
}

// add holds n more bytes for an inbox, or refuses: because the budget is full, or because
// the inbox already holds its share (share, from perInbox).
func (b *byteBudget) add(id inbox.ID, share, n int64) bool {
	if b.limit < 0 || n <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit || b.byInbox[id]+n > share {
		return false
	}
	if b.byInbox == nil {
		b.byInbox = make(map[inbox.ID]int64)
	}
	b.used += n
	b.byInbox[id] += n
	return true
}

// sub gives n bytes back.
func (b *byteBudget) sub(id inbox.ID, n int64) {
	if b.limit < 0 || n <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	if b.byInbox[id] -= n; b.byInbox[id] <= 0 {
		delete(b.byInbox, id)
	}
	b.mu.Unlock()
}

// errOverBudget is what a budgetReader returns when the budget has no room for what
// it just read.
var errOverBudget = errors.New("server: the memory for deliveries being received is in use")

// budgetReader counts the bytes read through it against a byteBudget, as they
// arrive, and holds them until release. What is counted is what is held in memory,
// not what a sender says it is going to send: a connection that has promised a large
// body and sent none costs the budget nothing, so a few idle connections cannot use
// it up.
type budgetReader struct {
	r     io.Reader
	b     *byteBudget
	id    inbox.ID // whose delivery it is
	share int64    // the most this inbox may hold (byteBudget.perInbox)
	held  int64
}

func (br *budgetReader) Read(p []byte) (int, error) {
	n, err := br.r.Read(p)
	if n > 0 {
		if !br.b.add(br.id, br.share, int64(n)) {
			return 0, errOverBudget
		}
		br.held += int64(n)
	}
	return n, err
}

// release gives back everything held. Call it when the delivery is done with the body.
func (br *budgetReader) release() {
	br.b.sub(br.id, br.held)
	br.held = 0
}
