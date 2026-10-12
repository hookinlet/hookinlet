// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"errors"
	"io"
	"sync"
)

// byteBudget counts bytes held, never more than limit at once, without ever making
// anyone wait: an addition that does not fit is refused, and the caller tells its
// client to come back. A limit below zero allows anything.
type byteBudget struct {
	limit int64

	mu   sync.Mutex
	used int64
}

// add holds n more bytes, or refuses.
func (b *byteBudget) add(n int64) bool {
	if b.limit < 0 || n <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return false
	}
	b.used += n
	return true
}

// sub gives n bytes back.
func (b *byteBudget) sub(n int64) {
	if b.limit < 0 || n <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
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
	r    io.Reader
	b    *byteBudget
	held int64
}

func (br *budgetReader) Read(p []byte) (int, error) {
	n, err := br.r.Read(p)
	if n > 0 {
		if !br.b.add(int64(n)) {
			return 0, errOverBudget
		}
		br.held += int64(n)
	}
	return n, err
}

// release gives back everything held. Call it when the delivery is done with the body.
func (br *budgetReader) release() {
	br.b.sub(br.held)
	br.held = 0
}
