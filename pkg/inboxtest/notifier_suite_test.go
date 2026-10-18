// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"strings"
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

func newRelayedNotifier(t *testing.T) *relayedNotifier { return newRelayedOn(t, local.New()) }

// newRelayedPeer is a second instance on the same backend as n: it shares n's
// subscribers (the backend) and has a relay of its own.
func newRelayedPeer(t *testing.T, n inbox.Notifier) *relayedNotifier {
	return newRelayedOn(t, n.(*relayedNotifier).Notifier)
}

func newRelayedOn(t *testing.T, backend *local.Notifier) *relayedNotifier {
	n := &relayedNotifier{
		Notifier: backend,
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

// With a second instance on the same backend the peer cases run too, on a notifier
// whose signals take a moment to arrive.
func TestNotifierSuiteWithAPeerOnARelayedNotifier(t *testing.T) {
	NotifierSuiteWith(t, func(t *testing.T) inbox.Notifier { return newRelayedNotifier(t) }, NotifierOptions{
		Settle: time.Second,
		Peer:   func(t *testing.T, n inbox.Notifier) inbox.Notifier { return newRelayedPeer(t, n) },
	})
}

// The core's own notifier has one instance, so its peer is itself. This runs the cases;
// it cannot show that they would catch a lost wake-up, which the next test does.
func TestNotifierSuiteWithAPeerOnTheLocalNotifier(t *testing.T) {
	NotifierSuiteWith(t, func(*testing.T) inbox.Notifier { return local.New() }, NotifierOptions{
		Peer: func(_ *testing.T, n inbox.Notifier) inbox.Notifier { return n },
	})
}

// lossyPeer is a notifier whose Publish reaches only the subscribers of its own
// instance, which is what a networked notifier with a lost wake-up does.
type lossyPeer struct{ *local.Notifier }

// sharedCancel is a backend whose subscriptions, when one is cancelled, are all
// cancelled for that inbox, on every instance: the mistake of releasing a shared
// subscription by name instead of by use.
type sharedCancel struct {
	backend *local.Notifier
	mu      sync.Mutex
	cancels map[inbox.ID][]func()
}

func newSharedCancel() *sharedCancel {
	return &sharedCancel{backend: local.New(), cancels: map[inbox.ID][]func(){}}
}

type sharedCancelInstance struct{ *sharedCancel }

func (s *sharedCancel) instance() inbox.Notifier { return sharedCancelInstance{s} }

func (s sharedCancelInstance) Publish(ctx context.Context, id inbox.ID, seq int64) {
	s.backend.Publish(ctx, id, seq)
}

func (s sharedCancelInstance) Subscribe(ctx context.Context, id inbox.ID) (<-chan struct{}, func()) {
	ch, cancel := s.backend.Subscribe(ctx, id)
	s.mu.Lock()
	s.cancels[id] = append(s.cancels[id], cancel)
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		all := s.cancels[id]
		s.cancels[id] = nil
		s.mu.Unlock()
		for _, c := range all {
			c()
		}
	}
}

func TestThePeerChecksCatchALostWakeUp(t *testing.T) {
	good := local.New()
	shared := newSharedCancel()
	sharedA, sharedB := shared.instance(), shared.instance()
	for _, c := range []struct {
		name  string
		check func(testing.TB, inbox.Notifier, inbox.Notifier)
		a, b  inbox.Notifier
		want  string
	}{
		{"wakes, sharing a backend", checkPeerWakes, good, lossyPeer{good}, ""},
		{"wakes, instances that do not reach each other", checkPeerWakes, local.New(), local.New(), "did not wake a subscriber on the"},
		{"isolation, sharing a backend", checkPeerIsolation, good, lossyPeer{good}, ""},
		{"isolation, instances that do not reach each other", checkPeerIsolation, local.New(), local.New(), "did not wake the first instance's subscriber"},
		{"cancel, sharing a backend", checkPeerCancel, good, lossyPeer{good}, ""},
		{"cancel, instances that do not reach each other", checkPeerCancel, newSharedCancel().instance(), newSharedCancel().instance(), ""},
		{"cancel, a cancel on one instance that ends the other's", checkPeerCancel, sharedA, sharedB, "stopped one on the"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &recordingTB{TB: t}
			c.check(rec, c.a, c.b)
			got := strings.Join(rec.errs, "\n")
			switch {
			case c.want == "" && got != "":
				t.Errorf("unexpected failure: %s", got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Errorf("failures %q do not mention %q", rec.errs, c.want)
			}
		})
	}
}
