// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// relayedNotifier stands for a notifier whose signals cross a network: Publish
// queues the signal and returns, and a relay delivers the queue in order, a few at
// a time, as messages from a broker arrive. Signals published together therefore
// reach a subscriber spread over a while, and one that reads as soon as the first
// arrives sees later ones as new signals.
type relayedNotifier struct {
	*local.Notifier
	mu    sync.Mutex
	queue []inbox.ID
	wake  chan struct{}
	stop  chan struct{}
	done  chan struct{}
}

func newRelayedNotifier(t *testing.T) *relayedNotifier {
	n := &relayedNotifier{
		Notifier: local.New(),
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go n.relay()
	t.Cleanup(func() { close(n.stop); <-n.done })
	return n
}

func (n *relayedNotifier) Publish(_ context.Context, id inbox.ID, _ int64) {
	n.mu.Lock()
	n.queue = append(n.queue, id)
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// relay delivers 25 queued signals every millisecond.
func (n *relayedNotifier) relay() {
	defer close(n.done)
	for {
		n.mu.Lock()
		batch := n.queue[:min(25, len(n.queue))]
		n.queue = n.queue[len(batch):]
		n.mu.Unlock()
		for _, id := range batch {
			n.Notifier.Publish(context.Background(), id, 0)
		}
		if len(batch) > 0 {
			select {
			case <-n.stop:
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}
		select {
		case <-n.stop:
			return
		case <-n.wake:
		}
	}
}

// A burst reaches the relayed notifier's subscriber over about 40ms. With Settle the
// suite waits for it, and the burst reads as one signal, as it must. (NotifierSuite,
// which reads at once, fails it: it sees a stream of later signals, which is how such
// a notifier is meant to behave.)
func TestNotifierSuiteWithSettleOnARelayedNotifier(t *testing.T) {
	NotifierSuiteWith(t, func(t *testing.T) inbox.Notifier { return newRelayedNotifier(t) }, NotifierOptions{Settle: time.Second})
}
