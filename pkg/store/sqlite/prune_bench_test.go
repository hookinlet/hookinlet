// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite_test

import (
	"bytes"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/benchseed"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// These measure how long Store.Append waits while a large delete runs, which is
// the prune-latency budget (p99 under 50 ms). They run only when asked for, because they
// seed a lot of data:
//
//	HOOKINLET_BENCH=1 go test -run 'WhileA|WhileLarge' -v -timeout 20m ./pkg/store/sqlite/
//
// What is measured is Store.Append, not the HTTP request around it (which adds
// verification and encoding, and no wait on the writer). Senders are closed
// loops with a 2 ms pause, so a sender stuck behind a batch sends nothing while
// it waits; with more senders the queue behind each batch is longer. Only
// appends made while the operation runs are counted.

type senderGroup struct {
	name      string
	target    *inbox.Inbox
	senders   int
	mu        sync.Mutex
	latencies []time.Duration
}

// whileRunning runs op while the groups append, and reports their latencies.
func whileRunning(t *testing.T, s *sqlite.Store, groups []*senderGroup, op func()) {
	t.Helper()
	var (
		measuring = make(chan struct{})
		stop      = make(chan struct{})
		wg        sync.WaitGroup
	)
	for _, g := range groups {
		for range g.senders {
			wg.Go(func() {
				counting := false
				for {
					select {
					case <-stop:
						return
					default:
					}
					select {
					case <-measuring:
						counting = true
					default:
					}
					begin := time.Now()
					_, _, err := s.Append(t.Context(), g.target.ID, &inbox.NewMessage{Method: "POST", Body: []byte(`{"during":"op"}`), Verified: true})
					d := time.Since(begin)
					if err == nil && counting {
						g.mu.Lock()
						g.latencies = append(g.latencies, d)
						g.mu.Unlock()
					}
					time.Sleep(2 * time.Millisecond)
				}
			})
		}
	}
	time.Sleep(300 * time.Millisecond) // warm up; these appends are not counted
	close(measuring)
	op()
	close(stop)
	wg.Wait()

	for _, g := range groups {
		slices.Sort(g.latencies)
		if len(g.latencies) == 0 {
			t.Errorf("%s: no appends completed during the operation", g.name)
			continue
		}
		pct := func(p float64) time.Duration { return percentile(g.latencies, p) }
		t.Logf("%s: %d appends: p50 %v, p95 %v, p99 %v, max %v", g.name, len(g.latencies), pct(0.5), pct(0.95), pct(0.99), g.latencies[len(g.latencies)-1])
		if pct(0.99) >= 50*time.Millisecond {
			t.Errorf("%s: append p99 was %v, over the 50 ms budget", g.name, pct(0.99))
		}
	}
}

func TestIngestLatencyWhileAMillionRowsArePruned(t *testing.T) {
	if os.Getenv("HOOKINLET_BENCH") == "" {
		t.Skip("set HOOKINLET_BENCH=1 to measure append latency during a large prune")
	}
	dir := t.TempDir()
	start := time.Now()
	victim, err := benchseed.Seed(t.Context(), dir, "victim", benchRows())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d messages in %v", benchRows(), time.Since(start).Round(time.Millisecond))
	s := openAt(t, dir)
	defer func() { _ = s.Close() }()
	other := newInbox("bystander")
	if err := s.CreateInbox(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	victim.RetentionMaxCount = 1 // keep one message: everything else is deleted
	if err := s.UpdateInbox(t.Context(), victim); err != nil {
		t.Fatal(err)
	}

	var deleted int64
	var took time.Duration
	whileRunning(t, s, []*senderGroup{
		{name: "appends to another inbox", target: other, senders: 2},
		{name: "appends to the inbox being pruned", target: victim, senders: 2},
	}, func() {
		begin := time.Now()
		var err error
		if deleted, err = s.Prune(t.Context(), benchseed.Start.Add(10*365*24*time.Hour)); err != nil {
			t.Error(err)
		}
		took = time.Since(begin)
	})
	t.Logf("pruned %d rows in %v", deleted, took.Round(time.Millisecond))
	if deleted < int64(benchRows())-1 {
		t.Errorf("the prune deleted only %d rows", deleted)
	}
}

func TestIngestLatencyWhileALargeInboxIsDeleted(t *testing.T) {
	if os.Getenv("HOOKINLET_BENCH") == "" {
		t.Skip("set HOOKINLET_BENCH=1 to measure append latency while an inbox is deleted")
	}
	dir := t.TempDir()
	victim, err := benchseed.Seed(t.Context(), dir, "victim", benchRows())
	if err != nil {
		t.Fatal(err)
	}
	s := openAt(t, dir)
	defer func() { _ = s.Close() }()
	other := newInbox("bystander")
	if err := s.CreateInbox(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	var took time.Duration
	whileRunning(t, s, []*senderGroup{{name: "appends to another inbox", target: other, senders: 4}}, func() {
		begin := time.Now()
		if err := s.DeleteInbox(t.Context(), victim.ID); err != nil {
			t.Error(err)
		}
		took = time.Since(begin)
	})
	t.Logf("deleted a %d-message inbox in %v", benchRows(), took.Round(time.Millisecond))
	if _, err := s.GetInbox(t.Context(), victim.ID); err == nil {
		t.Error("the inbox is still there")
	}
}

// Batches of large messages take far longer than batches of small ones, so the
// batch is bounded by bytes too.
func TestIngestLatencyWhileLargeMessagesArePruned(t *testing.T) {
	if os.Getenv("HOOKINLET_BENCH") == "" {
		t.Skip("set HOOKINLET_BENCH=1 to measure append latency while large messages are pruned")
	}
	s := openAt(t, t.TempDir())
	defer func() { _ = s.Close() }()
	victim := newInbox("big-victim")
	victim.MaxBodyBytes = 1 << 20
	other := newInbox("bystander")
	for _, in := range []*inbox.Inbox{victim, other} {
		if err := s.CreateInbox(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	}
	body := bytes.Repeat([]byte("x"), 512<<10)
	const rows = 1500 // about 750 MiB
	for range rows {
		if _, _, err := s.Append(t.Context(), victim.ID, &inbox.NewMessage{Method: "POST", Body: body, Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	victim.RetentionMaxCount = 1
	if err := s.UpdateInbox(t.Context(), victim); err != nil {
		t.Fatal(err)
	}
	var deleted int64
	var took time.Duration
	whileRunning(t, s, []*senderGroup{{name: "appends to another inbox", target: other, senders: 4}}, func() {
		begin := time.Now()
		var err error
		if deleted, err = s.Prune(t.Context(), time.Now()); err != nil {
			t.Error(err)
		}
		took = time.Since(begin)
	})
	t.Logf("pruned %d messages of 512 KiB in %v", deleted, took.Round(time.Millisecond))
}
