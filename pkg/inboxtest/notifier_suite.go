// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const (
	// signalWait is how long a signal that must arrive is waited for.
	signalWait = 2 * time.Second
	// quiet is how long the absence of a signal is observed.
	quiet = 150 * time.Millisecond
)

// NotifierSuite runs the inbox.Notifier conformance tests. newNotifier must
// return a working Notifier on every call; notifiers returned by one call must
// share state (a Publish on one reaches Subscribe on the same one).
//
// The suite checks the guarantees a consumer relies on, which are the same ones a
// distributed implementation must keep: a Publish after Subscribe returns wakes
// the subscriber, signals coalesce rather than queue, inboxes do not signal each
// other, and cancelling releases everything. It runs one notifier. A notifier whose
// instances reach each other through a backend also needs the cases that run two,
// which NotifierSuiteWith runs when NotifierOptions.Peer is set.
//
// It reads a signal as soon as it can. A notifier whose signals cross a network
// delivers them later and spread out, so the coalescing check can see more than one;
// such a notifier runs NotifierSuiteWith and sets NotifierOptions.Settle.
func NotifierSuite(t *testing.T, newNotifier func(t *testing.T) inbox.Notifier) {
	t.Helper()
	NotifierSuiteWith(t, newNotifier, NotifierOptions{})
}

// NotifierOptions adjusts NotifierSuiteWith to a notifier whose signals cross a
// network. The zero value is NotifierSuite, which is right for an in-process one.
type NotifierOptions struct {
	// Settle is how long the coalescing check waits, after its burst of publishes
	// and before it reads, for signals still on their way to arrive. Signals coalesce
	// only when they arrive before the receiver reads: one that arrives after a read
	// is a new signal, which a networked notifier legitimately delivers when a burst
	// reaches the subscriber spread over time. Set it to the longest a burst of a
	// thousand publishes takes to arrive. The check itself is not relaxed: after
	// Settle the burst must still read as one signal (and at most one more). Zero
	// reads at once.
	Settle time.Duration

	// Peer returns a second Notifier on the same backend as n, as another server of
	// the same deployment would have: a Publish on either reaches the subscribers of
	// both. It is for a notifier whose instances reach each other (over a network,
	// say), where a signal lost between instances goes unnoticed by a suite that runs
	// only one. With it the suite also checks that a Publish on one instance wakes
	// subscribers on the other, in both directions, for every subscriber of the inbox
	// and for no other inbox's, and that cancelling a subscription on one instance
	// leaves the other's. Nil runs only the single-instance cases. Peer is called
	// once per case with the notifier newNotifier returned; it must not return n
	// itself unless the notifier has only one instance, and it releases what it holds
	// with t.Cleanup.
	Peer func(t *testing.T, n inbox.Notifier) inbox.Notifier
}

// NotifierSuiteWith is NotifierSuite with options.
func NotifierSuiteWith(t *testing.T, newNotifier func(t *testing.T) inbox.Notifier, opts NotifierOptions) {
	t.Helper()
	if opts.Settle < 0 {
		t.Fatalf("NotifierOptions.Settle is %v; it must not be negative", opts.Settle)
	}

	signalled := func(ch <-chan struct{}, within time.Duration) bool {
		select {
		case _, ok := <-ch:
			return ok
		case <-time.After(within):
			return false
		}
	}
	drain := func(ch <-chan struct{}) (n int) {
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return n // closed: the signalled checks report it
				}
				n++
			case <-time.After(quiet):
				return n
			}
		}
	}

	// subscribe calls Subscribe, failing the test (rather than hanging it) if an
	// implementation blocks. Subscribe must return at once, even when a backend is down.
	subscribe := func(t *testing.T, n inbox.Notifier, ctx context.Context, id inbox.ID) (<-chan struct{}, func()) {
		t.Helper()
		type pair struct {
			ch     <-chan struct{}
			cancel func()
		}
		got := make(chan pair, 1)
		go func() {
			ch, cancel := n.Subscribe(ctx, id)
			got <- pair{ch, cancel}
		}()
		select {
		case p := <-got:
			if p.ch == nil || p.cancel == nil {
				t.Error("Subscribe returned a nil channel or cancel function")
				return make(chan struct{}), func() {}
			}
			return p.ch, p.cancel
		case <-time.After(2 * time.Second):
			t.Error("Subscribe blocked for 2s; it must return at once, with a silent channel if its backend is unavailable")
			return make(chan struct{}), func() {}
		}
	}

	// publish calls Publish count times, failing the test (rather than hanging it)
	// if an implementation blocks the publisher.
	publish := func(t *testing.T, n inbox.Notifier, id inbox.ID, count int) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := range count {
				n.Publish(t.Context(), id, int64(i+1))
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("Publish blocked: %d publishes did not return in 2s. Publish must never wait for a subscriber.", count)
		}
	}

	t.Run("PublishAfterSubscribeWakesTheSubscriber", func(t *testing.T) {
		n := newNotifier(t)
		ch, cancel := subscribe(t, n, t.Context(), 1)
		defer cancel()
		publish(t, n, 1, 1)
		if !signalled(ch, signalWait) {
			t.Fatal("a Publish after Subscribe did not wake the subscriber")
		}
		// And again: waking once does not use the subscription up.
		publish(t, n, 1, 1)
		if !signalled(ch, signalWait) {
			t.Fatal("the second Publish did not wake the subscriber")
		}
	})

	t.Run("NothingSignalsWithoutAPublish", func(t *testing.T) {
		n := newNotifier(t)
		ch, cancel := subscribe(t, n, t.Context(), 1)
		defer cancel()
		if signalled(ch, quiet) {
			t.Error("a subscription signalled before anything was published")
		}
	})

	t.Run("PublishWithNoSubscribersIsHarmless", func(t *testing.T) {
		n := newNotifier(t)
		publish(t, n, 7, 1000)
	})

	t.Run("SeveralPublishesBeforeAReadCoalesce", func(t *testing.T) {
		n := newNotifier(t)
		ch, cancel := subscribe(t, n, t.Context(), 1)
		defer cancel()
		publish(t, n, 1, 1000)
		time.Sleep(opts.Settle) // let signals still on their way arrive, so that they arrive before the read
		if !signalled(ch, signalWait) {
			t.Fatal("no signal after 1000 publishes")
		}
		// The rest must have been folded into that one, not queued behind it. (One
		// more is tolerated for an implementation whose delivery is asynchronous.)
		if extra := drain(ch); extra > 1 {
			t.Errorf("%d further signals; publishes made before a read should coalesce into one", extra)
		}
		// Reading it does not lose the ability to be signalled again.
		publish(t, n, 1, 1)
		if !signalled(ch, signalWait) {
			t.Error("no signal after draining")
		}
	})

	t.Run("AnInboxDoesNotSignalAnotherInboxsSubscribers", func(t *testing.T) {
		n := newNotifier(t)
		a, cancelA := subscribe(t, n, t.Context(), 1)
		defer cancelA()
		b, cancelB := subscribe(t, n, t.Context(), 2)
		defer cancelB()
		publish(t, n, 2, 1)
		if !signalled(b, signalWait) {
			t.Fatal("inbox 2's subscriber was not signalled")
		}
		if signalled(a, quiet) {
			t.Error("a Publish to inbox 2 woke a subscriber of inbox 1")
		}
	})

	t.Run("EverySubscriberOfAnInboxIsSignalled", func(t *testing.T) {
		n := newNotifier(t)
		var chans []<-chan struct{}
		for range 20 {
			ch, cancel := subscribe(t, n, t.Context(), 1)
			defer cancel()
			chans = append(chans, ch)
		}
		publish(t, n, 1, 1)
		for i, ch := range chans {
			if !signalled(ch, signalWait) {
				t.Errorf("subscriber %d was not signalled", i)
			}
		}
	})

	t.Run("CancellingOneLeavesTheOthers", func(t *testing.T) {
		n := newNotifier(t)
		keep, cancelKeep := subscribe(t, n, t.Context(), 1)
		defer cancelKeep()
		gone, cancelGone := subscribe(t, n, t.Context(), 1)
		cancelGone()
		publish(t, n, 1, 1)
		if !signalled(keep, signalWait) {
			t.Error("cancelling a different subscription stopped this one")
		}
		if signalled(gone, quiet) {
			t.Error("a cancelled subscription was signalled")
		}
	})

	t.Run("CancelIsIdempotentAndTheChannelIsNeverClosed", func(t *testing.T) {
		n := newNotifier(t)
		ch, cancel := subscribe(t, n, t.Context(), 1)
		cancel()
		cancel()
		cancel()
		publish(t, n, 1, 1)
		select {
		case _, ok := <-ch:
			if !ok {
				t.Error("the channel was closed; it must never be closed")
			}
			t.Error("a signal arrived after cancel")
		case <-time.After(quiet):
		}
	})

	t.Run("CancellingTheContextEndsTheSubscription", func(t *testing.T) {
		n := newNotifier(t)
		ctx, cancelCtx := context.WithCancel(t.Context())
		ch, cancel := subscribe(t, n, ctx, 1)
		defer cancel()
		cancelCtx()
		time.Sleep(quiet) // the implementation may notice asynchronously
		publish(t, n, 1, 1)
		select {
		case <-ch:
			t.Error("a subscription whose context was cancelled was signalled")
		case <-time.After(quiet):
		}
	})

	t.Run("ASubscriptionOnACancelledContextNeverSignals", func(t *testing.T) {
		n := newNotifier(t)
		ctx, cancelCtx := context.WithCancel(t.Context())
		cancelCtx()
		ch, cancel := subscribe(t, n, ctx, 1)
		defer cancel()
		time.Sleep(quiet)
		publish(t, n, 1, 1)
		if signalled(ch, quiet) {
			t.Error("signalled on an already-cancelled context")
		}
	})

	t.Run("CancellingReleasesEverythingItHeld", func(t *testing.T) {
		n := newNotifier(t)
		settle := func() { time.Sleep(50 * time.Millisecond); runtime.GC() }
		settle()
		before := runtime.NumGoroutine()

		cancels := make([]func(), 0, 2000)
		ctxs := make([]context.CancelFunc, 0, 1000)
		for i := range 1000 {
			_, cancel := subscribe(t, n, t.Context(), inbox.ID(i%10))
			cancels = append(cancels, cancel)
			ctx, stop := context.WithCancel(t.Context())
			ctxs = append(ctxs, stop)
			_, cancel = subscribe(t, n, ctx, inbox.ID(i%10))
			cancels = append(cancels, cancel)
		}
		for _, c := range cancels[:1000] {
			c()
		}
		for _, stop := range ctxs {
			stop()
		}
		for _, c := range cancels[1000:] {
			c()
		}

		deadline := time.Now().Add(signalWait)
		for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after > before+2 {
			t.Errorf("%d goroutines before, %d after cancelling 2,000 subscriptions: something is leaking", before, after)
		}
		// And a Publish with everyone cancelled is still harmless.
		for i := range 10 {
			publish(t, n, inbox.ID(i), 1)
		}
	})

	// The request that stored a message may be finished, and its context
	// cancelled, by the time the notifier hears about it. The signal still has to
	// go out, or a subscriber waits for the recheck interval for no reason.
	t.Run("PublishWithACancelledContextStillSignals", func(t *testing.T) {
		n := newNotifier(t)
		ch, cancel := subscribe(t, n, t.Context(), 1)
		defer cancel()
		ctx, stop := context.WithCancel(t.Context())
		stop()
		done := make(chan struct{})
		go func() { defer close(done); n.Publish(ctx, 1, 1) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Publish blocked on a cancelled context")
		}
		if !signalled(ch, signalWait) {
			t.Error("Publish with a cancelled context did not signal")
		}
	})

	// Goroutines are not the only thing a notifier can leak: per-inbox state
	// that is never removed grows with every inbox ever listened to.
	t.Run("SubscribingAndCancellingDoesNotGrowMemory", func(t *testing.T) {
		n := newNotifier(t)
		heap := func() uint64 {
			runtime.GC()
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return m.HeapAlloc
		}
		cycle := func(count int) {
			for i := range count {
				ctx, stop := context.WithCancel(t.Context())
				_, cancel := subscribe(t, n, ctx, inbox.ID(i%5000))
				if i%2 == 0 {
					cancel()
				} else {
					stop()
				}
				stop()
				cancel()
			}
		}
		cycle(5000) // warm up allocator and caches
		time.Sleep(50 * time.Millisecond)
		before := heap()
		cycle(200_000)
		time.Sleep(100 * time.Millisecond) // an implementation may clean up asynchronously
		after := heap()
		if after > before && after-before > 8<<20 {
			t.Errorf("the heap grew by %d MiB over 200,000 subscribe/cancel cycles: something is not being released", (after-before)>>20)
		}
	})

	t.Run("PublishDoesNotWaitForSlowSubscribers", func(t *testing.T) {
		n := newNotifier(t)
		for range 5000 {
			_, cancel := subscribe(t, n, t.Context(), 1) // never read from
			defer cancel()
		}
		publish(t, n, 1, 100)
	})

	if opts.Peer != nil {
		for _, c := range []struct {
			name string
			fn   func(testing.TB, inbox.Notifier, inbox.Notifier)
		}{
			{"APublishOnAPeerWakesSubscribersOnBothInstances", checkPeerWakes},
			{"APublishOnAPeerDoesNotWakeAnotherInbox", checkPeerIsolation},
			{"CancellingOnOneInstanceLeavesTheOther", checkPeerCancel},
		} {
			t.Run(c.name, func(t *testing.T) {
				n := newNotifier(t)
				peer := opts.Peer(t, n)
				if peer == nil {
					t.Fatal("NotifierOptions.Peer returned a nil Notifier")
				}
				c.fn(t, n, peer)
			})
		}
	}

	t.Run("ConcurrentUseIsSafe", func(t *testing.T) {
		n := newNotifier(t)
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Go(func() {
				id := inbox.ID(g % 3)
				for i := range 300 {
					ch, cancel := subscribe(t, n, t.Context(), id)
					n.Publish(t.Context(), id, int64(i))
					select {
					case <-ch:
					default:
					}
					cancel()
				}
			})
		}
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent use did not finish: a Publish or Subscribe is blocking")
		}
	})
}

// The checks of a notifier with more than one instance. They report through t
// without stopping the test, so a test of the checks can read what they said.

// subscribeWithin calls Subscribe and gives up if it does not return in time.
func subscribeWithin(t testing.TB, n inbox.Notifier, id inbox.ID) (<-chan struct{}, func(), bool) {
	t.Helper()
	type pair struct {
		ch     <-chan struct{}
		cancel func()
	}
	got := make(chan pair, 1)
	go func() {
		ch, cancel := n.Subscribe(t.Context(), id)
		got <- pair{ch, cancel}
	}()
	select {
	case p := <-got:
		if p.ch == nil || p.cancel == nil {
			t.Error("Subscribe returned a nil channel or cancel function")
			return nil, nil, false
		}
		return p.ch, p.cancel, true
	case <-time.After(signalWait):
		t.Error("Subscribe blocked for 2s; it must return at once")
		return nil, nil, false
	}
}

func wokenWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// settleSignals discards signals that are still arriving, so that a check that
// follows sees only its own.
func settleSignals(chans ...<-chan struct{}) {
	for _, ch := range chans {
		for wokenWithin(ch, quiet) {
		}
	}
}

func checkPeerWakes(t testing.TB, a, b inbox.Notifier) {
	t.Helper()
	onA, cancelA, ok := subscribeWithin(t, a, 1)
	if !ok {
		return
	}
	defer cancelA()
	onB, cancelB, ok := subscribeWithin(t, b, 1)
	if !ok {
		return
	}
	defer cancelB()
	for _, c := range []struct {
		from string
		pub  inbox.Notifier
	}{{"the second instance", b}, {"the first instance", a}} {
		c.pub.Publish(t.Context(), 1, 1)
		woke := wokenWithin(onA, signalWait)
		if !woke {
			t.Errorf("a Publish on %s did not wake a subscriber on the first instance", c.from)
		}
		if !wokenWithin(onB, signalWait) {
			t.Errorf("a Publish on %s did not wake a subscriber on the second instance", c.from)
			woke = false
		}
		if !woke {
			return // the rest would only repeat it, slowly
		}
		settleSignals(onA, onB)
	}
	// Several subscribers on each instance are all woken.
	var chans []<-chan struct{}
	for i := range 10 {
		n := a
		if i%2 == 1 {
			n = b
		}
		ch, cancel, ok := subscribeWithin(t, n, 2)
		if !ok {
			return
		}
		defer cancel()
		chans = append(chans, ch)
	}
	a.Publish(t.Context(), 2, 1)
	for i, ch := range chans {
		if !wokenWithin(ch, signalWait) {
			t.Errorf("subscriber %d (on instance %d) was not woken by a Publish on the first instance", i, i%2+1)
		}
	}
}

func checkPeerIsolation(t testing.TB, a, b inbox.Notifier) {
	t.Helper()
	other, cancelOther, ok := subscribeWithin(t, a, 2)
	if !ok {
		return
	}
	defer cancelOther()
	same, cancelSame, ok := subscribeWithin(t, a, 1)
	if !ok {
		return
	}
	defer cancelSame()
	b.Publish(t.Context(), 1, 1)
	if !wokenWithin(same, signalWait) {
		t.Error("a Publish on the second instance did not wake the first instance's subscriber of that inbox")
	}
	if wokenWithin(other, quiet) {
		t.Error("a Publish to inbox 1 on the second instance woke a subscriber of inbox 2 on the first")
	}
}

func checkPeerCancel(t testing.TB, a, b inbox.Notifier) {
	t.Helper()
	keepA, cancelKeepA, ok := subscribeWithin(t, a, 1)
	if !ok {
		return
	}
	defer cancelKeepA()
	goneB, cancelGoneB, ok := subscribeWithin(t, b, 1)
	if !ok {
		return
	}
	cancelGoneB()
	a.Publish(t.Context(), 1, 1)
	if !wokenWithin(keepA, signalWait) {
		t.Error("cancelling a subscription on the second instance stopped one on the first")
	}
	if wokenWithin(goneB, quiet) {
		t.Error("a subscription cancelled on the second instance was signalled")
	}
	// And the other way: the one that is cancelled is on the first instance.
	keepB, cancelKeepB, ok := subscribeWithin(t, b, 1)
	if !ok {
		return
	}
	defer cancelKeepB()
	settleSignals(keepA)
	cancelKeepA()
	b.Publish(t.Context(), 1, 2)
	if !wokenWithin(keepB, signalWait) {
		t.Error("cancelling a subscription on the first instance stopped one on the second")
	}
	if wokenWithin(keepA, quiet) {
		t.Error("a subscription cancelled on the first instance was signalled")
	}
}
