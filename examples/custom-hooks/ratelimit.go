// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// RateLimiter is an inbox.Hooks that limits how fast each inbox accepts
// deliveries, with a token bucket per inbox: it holds up to burst tokens, gains
// perSecond tokens a second, and spends one on every delivery.
//
// A warning for anyone copying this: BeforeIngest runs before the signature is
// checked, so this counts unverified requests too. Someone who knows an inbox's
// URL can spend its tokens with unsigned posts, and a real sender then gets 429.
// It is kept this simple to show the shape of a hook; see docs/extending.md for
// what to do instead.
//
// It embeds inbox.NopHooks, so it overrides BeforeIngest and inherits a
// do-nothing AfterIngest. That is the pattern for a hook that cares about one
// side only.
type RateLimiter struct {
	inbox.NopHooks

	perSecond float64
	burst     float64
	now       func() time.Time // replaced in tests

	mu      sync.Mutex
	buckets map[inbox.ID]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows perSecond deliveries a second to each inbox, in bursts
// of up to burst.
func NewRateLimiter(perSecond float64, burst int) *RateLimiter {
	return &RateLimiter{
		perSecond: perSecond,
		burst:     float64(burst),
		now:       time.Now,
		buckets:   make(map[inbox.ID]*bucket),
	}
}

// BeforeIngest implements inbox.Hooks. It runs before the delivery is verified
// or stored, so a refused delivery costs the server almost nothing. Wrapping
// inbox.ErrRateLimited makes the server answer 429.
//
// The hook is called concurrently, from every request, so it locks.
func (r *RateLimiter) BeforeIngest(_ context.Context, in *inbox.Inbox, _ int64) error {
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[in.ID]
	if !ok {
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[in.ID] = b
	}
	// Refill for the time since the last delivery, up to a full bucket.
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Seconds()*r.perSecond)
	b.last = now
	if b.tokens < 1 {
		return fmt.Errorf("%w: more than %g a second", inbox.ErrRateLimited, r.perSecond)
	}
	b.tokens--
	return nil
}
