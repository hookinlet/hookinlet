// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite_test

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/benchseed"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// benchRows is how many messages the query benchmarks run against. The
// Phase 3 gate is a p95 under 20 ms at one million rows; set
// HOOKINLET_BENCH_ROWS to measure a different size.
func benchRows() int {
	if v, err := strconv.Atoi(os.Getenv("HOOKINLET_BENCH_ROWS")); err == nil && v > 0 {
		return v
	}
	return 1_000_000
}

var (
	seedOnce  sync.Once
	seedDir   string
	seedInbox *inbox.Inbox
	seedErr   error
)

// seeded returns a store over a database of benchRows() messages, built once per
// test binary run.
func seeded(tb testing.TB) (*sqlite.Store, *inbox.Inbox) {
	tb.Helper()
	seedOnce.Do(func() {
		seedDir, seedErr = os.MkdirTemp("", "hookinlet-bench-*")
		if seedErr != nil {
			return
		}
		start := time.Now()
		seedInbox, seedErr = benchseed.Seed(tb.Context(), seedDir, "bench", benchRows())
		if seedErr == nil {
			tb.Logf("seeded %d messages in %v", benchRows(), time.Since(start).Round(time.Millisecond))
		}
	})
	if seedErr != nil {
		tb.Fatal(seedErr)
	}
	s, err := sqlite.Open(tb.Context(), seedDir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s, seedInbox
}

func TestMain(m *testing.M) {
	code := m.Run()
	if seedDir != "" {
		_ = os.RemoveAll(seedDir)
	}
	os.Exit(code)
}

// shapes are the query patterns an agent actually uses. Each is built from a
// random position in the inbox, so no run is served from a single warm page.
func shapes(n int64) map[string]func(r *rand.Rand) inbox.Query {
	i64 := func(v int64) *int64 { return &v }
	b := func(v bool) *bool { return &v }
	at := func(seq int64) time.Time { return benchseed.Start.Add(time.Duration(seq) * time.Second) }
	return map[string]func(r *rand.Rand) inbox.Query{
		"latest 50":               func(*rand.Rand) inbox.Query { return inbox.Query{} },
		"poll after near the end": func(r *rand.Rand) inbox.Query { return inbox.Query{After: i64(n - 1 - r.Int64N(500)), Limit: 50} },
		"poll after random":       func(r *rand.Rand) inbox.Query { return inbox.Query{After: i64(r.Int64N(n)), Limit: 50} },
		"page backward random":    func(r *rand.Rand) inbox.Query { return inbox.Query{Before: i64(1 + r.Int64N(n)), Limit: 50} },
		"limit 500":               func(r *rand.Rand) inbox.Query { return inbox.Query{After: i64(r.Int64N(n)), Limit: 500} },
		"one event type, newest":  func(*rand.Rand) inbox.Query { return inbox.Query{Types: []string{"type-7"}} },
		"one event type, random":  func(r *rand.Rand) inbox.Query { return inbox.Query{Types: []string{"type-7"}, After: i64(r.Int64N(n))} },
		"three event types": func(r *rand.Rand) inbox.Query {
			return inbox.Query{Types: []string{"type-1", "type-2", "type-3"}, After: i64(r.Int64N(n))}
		},
		"since a recent time": func(*rand.Rand) inbox.Query { return inbox.Query{Since: at(n - 3600)} },
		"since + until window": func(r *rand.Rand) inbox.Query {
			s := r.Int64N(n - 7200)
			return inbox.Query{Since: at(s), Until: at(s + 3600)}
		},
		"verified=true":         func(r *rand.Rand) inbox.Query { return inbox.Query{Verified: b(true), After: i64(r.Int64N(n))} },
		"verified=false (rare)": func(*rand.Rand) inbox.Query { return inbox.Query{Verified: b(false)} },
		"by delivery id":        func(r *rand.Rand) inbox.Query { return inbox.Query{DeliveryID: fmt.Sprintf("evt_%d", 1+r.Int64N(n))} },
		"summary only (no bodies)": func(r *rand.Rand) inbox.Query {
			return inbox.Query{After: i64(r.Int64N(n)), Limit: 50, BodyLimit: i64(0)}
		},
		"type + time + verified": func(r *rand.Rand) inbox.Query {
			s := r.Int64N(n - 7200)
			return inbox.Query{Types: []string{"type-3"}, Since: at(s), Until: at(s + 7200), Verified: b(true)}
		},
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

// TestQueryLatencyAtScale reports p50/p95/max for every query shape. It runs
// only when asked for, because seeding a million rows takes several seconds:
//
//	HOOKINLET_BENCH=1 go test -run TestQueryLatencyAtScale -v ./pkg/store/sqlite/
//
// and fails if a shape's p95 reaches the 20 ms budget.
func TestQueryLatencyAtScale(t *testing.T) {
	if os.Getenv("HOOKINLET_BENCH") == "" {
		t.Skip("set HOOKINLET_BENCH=1 to measure query latency at scale")
	}
	s, in := seeded(t)
	n := int64(benchRows())
	const iterations = 400
	r := rand.New(rand.NewPCG(1, 2))

	names := make([]string, 0)
	all := shapes(n)
	for name := range all {
		names = append(names, name)
	}
	slices.Sort(names)

	type row struct {
		name          string
		p50, p95, max time.Duration
	}
	var rows []row
	for _, name := range names {
		gen := all[name]
		times := make([]time.Duration, 0, iterations)
		for range iterations {
			q := gen(r)
			start := time.Now()
			if _, err := s.Query(t.Context(), in.ID, q); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			times = append(times, time.Since(start))
		}
		slices.Sort(times)
		rows = append(rows, row{name, percentile(times, 0.50), percentile(times, 0.95), times[len(times)-1]})
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(b.p95, a.p95) })
	t.Logf("%-28s %10s %10s %10s", "query shape ("+strconv.Itoa(int(n))+" rows)", "p50", "p95", "max")
	for _, r := range rows {
		t.Logf("%-28s %10v %10v %10v", r.name, r.p50.Round(time.Microsecond), r.p95.Round(time.Microsecond), r.max.Round(time.Microsecond))
		if r.p95 >= 20*time.Millisecond {
			t.Errorf("p95 of %q is %v, over the 20 ms budget", r.name, r.p95)
		}
	}

	// Other reads an agent makes.
	for name, call := range map[string]func() error{
		"Get one message": func() error { _, err := s.Get(t.Context(), in.ID, 1+r.Int64N(n)); return err },
		"Stats":           func() error { _, err := s.Stats(t.Context(), in.ID); return err },
		"LatestSeq":       func() error { _, err := s.LatestSeq(t.Context(), in.ID); return err },
	} {
		times := make([]time.Duration, 0, iterations)
		for range iterations {
			start := time.Now()
			if err := call(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			times = append(times, time.Since(start))
		}
		slices.Sort(times)
		t.Logf("%-28s %10v %10v %10v", name, percentile(times, 0.50).Round(time.Microsecond), percentile(times, 0.95).Round(time.Microsecond), times[len(times)-1].Round(time.Microsecond))
		if percentile(times, 0.95) >= 20*time.Millisecond {
			t.Errorf("p95 of %q is %v, over the 20 ms budget", name, percentile(times, 0.95))
		}
	}
}

// BenchmarkQuery gives Go's usual ns/op for the most common shapes.
func BenchmarkQuery(b *testing.B) {
	s, in := seeded(b)
	n := int64(benchRows())
	all := shapes(n)
	for _, name := range []string{"latest 50", "poll after near the end", "poll after random", "one event type, newest", "by delivery id"} {
		gen := all[name]
		b.Run(name, func(b *testing.B) {
			r := rand.New(rand.NewPCG(3, 4))
			for b.Loop() {
				if _, err := s.Query(b.Context(), in.ID, gen(r)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
