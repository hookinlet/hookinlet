// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import "sync"

// byteBudget hands out a number of bytes, never more than limit at once, without
// ever making anyone wait: a request that does not fit is refused, and the caller
// tells its client to come back. A limit below zero hands out anything.
type byteBudget struct {
	limit int64

	mu   sync.Mutex
	used int64
}

// take reserves n bytes and returns the function that gives them back. A request
// larger than the whole budget is given the whole budget when nothing else holds
// any, so that a limit smaller than one inbox's body limit slows large deliveries
// down and does not forbid them.
func (b *byteBudget) take(n int64) (release func(), ok bool) {
	if b.limit < 0 || n <= 0 {
		return func() {}, true
	}
	n = min(n, b.limit)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return nil, false
	}
	b.used += n
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.used -= n
			b.mu.Unlock()
		})
	}, true
}
