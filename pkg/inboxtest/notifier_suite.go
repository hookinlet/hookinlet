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
// other, and cancelling releases everything.
func NotifierSuite(t *testing.T, newNotifier func(t *testing.T) inbox.Notifier) {
	t.Helper()

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
			case <-ch:
				n++
			case <-time.After(quiet):
				return n
			}
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
		ch, cancel := n.Subscribe(t.Context(), 1)
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
		ch, cancel := n.Subscribe(t.Context(), 1)
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
		ch, cancel := n.Subscribe(t.Context(), 1)
		defer cancel()
		publish(t, n, 1, 1000)
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
		a, cancelA := n.Subscribe(t.Context(), 1)
		defer cancelA()
		b, cancelB := n.Subscribe(t.Context(), 2)
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
			ch, cancel := n.Subscribe(t.Context(), 1)
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
		keep, cancelKeep := n.Subscribe(t.Context(), 1)
		defer cancelKeep()
		gone, cancelGone := n.Subscribe(t.Context(), 1)
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
		ch, cancel := n.Subscribe(t.Context(), 1)
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
		ch, cancel := n.Subscribe(ctx, 1)
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
		ch, cancel := n.Subscribe(ctx, 1)
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
			_, cancel := n.Subscribe(t.Context(), inbox.ID(i%10))
			cancels = append(cancels, cancel)
			ctx, stop := context.WithCancel(t.Context())
			ctxs = append(ctxs, stop)
			_, cancel = n.Subscribe(ctx, inbox.ID(i%10))
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
		ch, cancel := n.Subscribe(t.Context(), 1)
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
				_, cancel := n.Subscribe(ctx, inbox.ID(i%5000))
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
			_, cancel := n.Subscribe(t.Context(), 1) // never read from
			defer cancel()
		}
		publish(t, n, 1, 100)
	})

	t.Run("ConcurrentUseIsSafe", func(t *testing.T) {
		n := newNotifier(t)
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Go(func() {
				id := inbox.ID(g % 3)
				for i := range 300 {
					ch, cancel := n.Subscribe(t.Context(), id)
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
