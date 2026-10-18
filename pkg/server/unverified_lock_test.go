// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// The count and the append of an unverified delivery are made under a lock held
// for its inbox alone: deliveries to one inbox cannot overshoot its cap, and
// deliveries to different inboxes do not wait for each other.

func TestManyConcurrentForgedDeliveriesStoreExactlyTheCap(t *testing.T) {
	const maxUnverified, senders = 10, 64
	e := newEnv(t, func(o *Options) { o.MaxUnverified = maxUnverified })
	in := e.newInbox("flood", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	codes := make(chan *httptest.ResponseRecorder, senders)
	var start sync.WaitGroup
	start.Add(1)
	for range senders {
		go func() {
			start.Wait()
			codes <- e.post("/in/flood", `{"n":1}`, "X-Hub-Signature-256", githubSig("wrong", `{"n":1}`))
		}()
	}
	start.Done()
	var ok, refused int
	for range senders {
		switch rec := <-codes; {
		case rec.Code == http.StatusOK:
			ok++
		case rec.Code == http.StatusTooManyRequests && errorCode(t, rec) == "rate_limited":
			refused++
		default:
			t.Errorf("status %d, %s", rec.Code, rec.Body.String())
		}
	}
	if ok != maxUnverified || refused != senders-maxUnverified || e.count(in) != maxUnverified {
		t.Errorf("%d accepted, %d refused and %d stored; want %d, %d and %d",
			ok, refused, e.count(in), maxUnverified, senders-maxUnverified, maxUnverified)
	}
	// An inbox at its cap refuses every delivery, and the line is written once a minute.
	if got := strings.Count(e.logs.String(), "too many unverified messages"); got != 1 {
		t.Errorf("%d refusal lines logged for %d refusals, want 1", got, senders-maxUnverified)
	}
	if n := e.server.unverifiedLocks.len(); n != 0 {
		t.Errorf("%d inbox locks left once every delivery ended, want 0", n)
	}
}

// len is how many inboxes have a lock that someone holds or waits for.
func (l *inboxLocks) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// waiting is how many hold or wait for id's lock.
func (l *inboxLocks) waiting(id inbox.ID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[id]; e != nil {
		return e.users
	}
	return 0
}

// slowCountStore takes hold to count one inbox's unverified messages, as a remote
// database under load might. It forwards inbox.UnverifiedCounter, so the cap holds.
type slowCountStore struct {
	inbox.Store
	hold    time.Duration
	slow    atomic.Int64 // the inbox ID that is slow to count
	entered chan struct{}
}

func (s *slowCountStore) CountUnverified(ctx context.Context, id inbox.ID, limit int64) (int64, error) {
	if int64(id) == s.slow.Load() {
		s.entered <- struct{}{}
		time.Sleep(s.hold)
	}
	return s.Store.(inbox.UnverifiedCounter).CountUnverified(ctx, id, limit)
}

func TestASlowInboxDoesNotHoldUpDeliveriesToAnother(t *testing.T) {
	const hold, queued, others = 50 * time.Millisecond, 16, 5
	ss := &slowCountStore{hold: hold, entered: make(chan struct{}, queued)}
	e := newEnv(t, func(o *Options) { o.MaxUnverified = 100; ss.Store = o.Store; o.Store = ss })
	slow := e.newInbox("slow", "none", "")
	e.newInbox("fast", "none", "")
	ss.slow.Store(int64(slow.ID))

	var wg sync.WaitGroup
	for range queued {
		wg.Go(func() {
			if code := e.post("/in/slow", `{}`).Code; code != http.StatusOK {
				t.Errorf("a delivery to the slow inbox: status %d", code)
			}
		})
	}
	<-ss.entered // one of them is counting, and the rest wait for its inbox's lock
	begin := time.Now()
	for range others {
		if code := e.post("/in/fast", `{}`).Code; code != http.StatusOK {
			t.Errorf("a delivery to the other inbox: status %d", code)
		}
	}
	took := time.Since(begin)
	wg.Wait()
	// Taken in turn, the slow inbox's deliveries hold its lock for queued*hold.
	if took >= 4*hold {
		t.Errorf("%d deliveries to another inbox took %v while %d to a slow inbox (%v each) were queued: they waited for its lock",
			others, took.Round(time.Millisecond), queued, hold)
	}
}

func TestInboxLocks(t *testing.T) {
	ctx := t.Context()
	lockOrFail := func(l *inboxLocks, id inbox.ID) func() {
		t.Helper()
		unlock, err := l.lock(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return unlock
	}

	t.Run("different inboxes do not wait for each other", func(t *testing.T) {
		var l inboxLocks
		unlockA := lockOrFail(&l, 1)
		got := make(chan struct{})
		go func() { lockOrFail(&l, 2)(); close(got) }()
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("locking inbox 2 waited for inbox 1")
		}
		unlockA()
	})

	t.Run("one inbox is taken in turn", func(t *testing.T) {
		var l inboxLocks
		unlock := lockOrFail(&l, 1)
		got := make(chan struct{})
		go func() { lockOrFail(&l, 1)(); close(got) }()
		select {
		case <-got:
			t.Fatal("the lock was taken twice")
		case <-time.After(20 * time.Millisecond):
		}
		unlock()
		<-got
	})

	t.Run("a waiter whose request is gone stops waiting", func(t *testing.T) {
		var l inboxLocks
		unlock := lockOrFail(&l, 1)
		gone, cancel := context.WithCancel(ctx)
		waited := make(chan error)
		go func() {
			u, err := l.lock(gone, 1)
			if err == nil {
				u()
			}
			waited <- err
		}()
		for l.waiting(1) != 2 { // the holder and the waiter
			time.Sleep(time.Millisecond)
		}
		cancel()
		if err := <-waited; !errors.Is(err, context.Canceled) {
			t.Errorf("err %v, want context.Canceled", err)
		}
		if n := l.len(); n != 1 {
			t.Errorf("%d locks while one is held, want 1", n)
		}
		unlock()
		if n := l.len(); n != 0 {
			t.Errorf("%d locks once none is held, want 0", n)
		}
		// The lock still works for whoever comes next.
		lockOrFail(&l, 1)()
	})

	t.Run("the table empties when no one holds or waits", func(t *testing.T) {
		var l inboxLocks
		var wg sync.WaitGroup
		var inside [4]atomic.Int32
		for i := range 200 {
			wg.Go(func() {
				id := inbox.ID(i % len(inside))
				unlock := lockOrFail(&l, id)
				if inside[id].Add(1) != 1 {
					t.Error("two holders of one inbox's lock")
				}
				time.Sleep(50 * time.Microsecond)
				inside[id].Add(-1)
				unlock()
			})
		}
		wg.Wait()
		if n := l.len(); n != 0 {
			t.Errorf("%d locks left, want 0", n)
		}
	})
}

// BenchmarkIngestUnverifiedManyInboxes measures ingest of unverified deliveries,
// with the cap on, sent concurrently to 1 inbox or spread over 64, on the SQLite
// store. Each inbox takes at most perInbox deliveries before the next is used, so
// every delivery is stored (none is refused) and every count reads at most
// perInbox rows, whichever the spread.
//
//	go test -run '^$' -bench IngestUnverifiedManyInboxes -benchtime 20000x ./pkg/server/
func BenchmarkIngestUnverifiedManyInboxes(b *testing.B) {
	const perInbox = 500
	for _, spread := range []int{1, 64} {
		b.Run(fmt.Sprintf("inboxes=%d", spread), func(b *testing.B) {
			store, err := sqlite.Open(b.Context(), b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			srv, err := New(Options{
				Addr: "127.0.0.1:0", Store: store, Logger: slog.New(slog.DiscardHandler),
				MaxUnverified: perInbox + 1,
			})
			if err != nil {
				b.Fatal(err)
			}
			h := srv.Handler()
			rounds := (b.N + spread*perInbox - 1) / (spread * perInbox)
			for i := range rounds * spread {
				uid, err := inbox.NewUID()
				if err != nil {
					b.Fatal(err)
				}
				slug := fmt.Sprintf("bench-%d", i)
				if err := store.CreateInbox(b.Context(), &inbox.Inbox{Slug: slug, UID: uid, IngestKey: slug, Name: slug, Verifier: "none"}); err != nil {
					b.Fatal(err)
				}
			}
			var next atomic.Int64
			b.SetParallelism(16) // deliveries wait on storage, not on the CPU
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					n := int(next.Add(1) - 1)
					path := fmt.Sprintf("/in/bench-%d", n/(spread*perInbox)*spread+n%spread)
					r := httptest.NewRequestWithContext(b.Context(), http.MethodPost, path, strings.NewReader(`{}`))
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != http.StatusOK {
						b.Errorf("POST %s: status %d, %s", path, w.Code, w.Body.String())
						return
					}
				}
			})
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "deliveries/s")
		})
	}
}
