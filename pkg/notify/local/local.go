// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package local is the default inbox.Notifier: an in-process broadcast with no
// payloads.
//
// Single-node assumption: signals reach only subscribers in this process. That
// is enough when one process owns the data directory, which is how the SQLite
// store is deployed. A multi-node deployment needs a notifier that crosses
// processes.
//
// Stability: this package is pre-1.0. Its API can still change, and every change is
// listed in CHANGELOG.md; after v1.0.0, semantic versioning applies.
package local

import (
	"context"
	"sync"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// Notifier implements inbox.Notifier. The zero value is not usable; call New.
//
// It never drops a signal while a subscriber is subscribed: a subscriber that has
// not yet read its last signal simply sees one signal for any number of
// publishes (they coalesce). It creates no goroutines, and a cancelled
// subscription leaves nothing behind.
type Notifier struct {
	mu   sync.Mutex
	subs map[inbox.ID]map[*subscription]struct{}
}

var _ inbox.Notifier = (*Notifier)(nil)

type subscription struct {
	// ch holds at most one pending signal. A publisher that finds it full has
	// nothing to do: the subscriber is already going to be woken.
	ch chan struct{}
}

// New returns an empty Notifier.
func New() *Notifier {
	return &Notifier{subs: make(map[inbox.ID]map[*subscription]struct{})}
}

// Publish implements inbox.Notifier. It never blocks and ignores seq: the
// notifier says that something changed, not what.
func (n *Notifier) Publish(_ context.Context, id inbox.ID, _ int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for s := range n.subs[id] {
		select {
		case s.ch <- struct{}{}:
		default: // already signalled and not yet read: coalesce
		}
	}
}

// Subscribe implements inbox.Notifier.
func (n *Notifier) Subscribe(ctx context.Context, id inbox.ID) (<-chan struct{}, func()) {
	s := &subscription{ch: make(chan struct{}, 1)}
	n.mu.Lock()
	set := n.subs[id]
	if set == nil {
		set = make(map[*subscription]struct{})
		n.subs[id] = set
	}
	set[s] = struct{}{}
	n.mu.Unlock()

	// Cancelling ctx ends the subscription too. AfterFunc starts nothing until
	// ctx is actually cancelled, so a subscription on a context that never ends
	// costs no goroutine.
	stop := context.AfterFunc(ctx, func() { n.remove(id, s) })
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			stop()
			n.remove(id, s)
		})
	}
	return s.ch, cancel
}

// remove drops a subscription, and the inbox's entry once nobody is left.
func (n *Notifier) remove(id inbox.ID, s *subscription) {
	n.mu.Lock()
	defer n.mu.Unlock()
	set := n.subs[id]
	delete(set, s)
	if len(set) == 0 {
		delete(n.subs, id)
	}
}

// Subscribers reports how many subscriptions are active, for tests and metrics.
func (n *Notifier) Subscribers() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for _, set := range n.subs {
		total += len(set)
	}
	return total
}
