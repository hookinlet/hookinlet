// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"cmp"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/benchseed"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

func benchRows() int {
	if v, err := strconv.Atoi(os.Getenv("HOOKINLET_BENCH_ROWS")); err == nil && v > 0 {
		return v
	}
	return 1_000_000
}

func pct(sorted []time.Duration, p float64) time.Duration {
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

// TestAPILatencyAtScale measures what an agent experiences: the whole request,
// including authentication, the inbox lookup, the query, and the JSON encoding,
// against a million messages. It runs only when asked for:
//
//	HOOKINLET_BENCH=1 go test -run TestAPILatencyAtScale -v -timeout 20m ./pkg/server/
//
// and fails if any request shape's p95 reaches the 20 ms budget.
func TestAPILatencyAtScale(t *testing.T) {
	if os.Getenv("HOOKINLET_BENCH") == "" {
		t.Skip("set HOOKINLET_BENCH=1 to measure API latency at scale")
	}
	n := int64(benchRows())
	dir := t.TempDir()
	start := time.Now()
	if _, err := benchseed.Seed(t.Context(), dir, "bench", int(n)); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d messages in %v", n, time.Since(start).Round(time.Millisecond))

	store, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	srv, err := New(Options{
		Addr: "127.0.0.1:0", Store: store, Logger: slog.New(slog.DiscardHandler),
		Authenticator: token.NewAuthenticator(store, testAdminToken),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	at := func(seq int64) string {
		return benchseed.Start.Add(time.Duration(seq) * time.Second).Format(time.RFC3339)
	}
	r := rand.New(rand.NewPCG(7, 8)) //nolint:gosec // a fixed-seed generator for repeatable benchmark inputs
	base := "/v1/inboxes/bench"
	shapes := map[string]func() string{
		"inbox info":              func() string { return base },
		"latest 50":               func() string { return base + "/messages" },
		"poll after near the end": func() string { return fmt.Sprintf("%s/messages?after=%d", base, n-1-r.Int64N(500)) },
		"poll after random":       func() string { return fmt.Sprintf("%s/messages?after=%d", base, r.Int64N(n)) },
		"page backward random":    func() string { return fmt.Sprintf("%s/messages?before=%d", base, 1+r.Int64N(n)) },
		"limit 500":               func() string { return fmt.Sprintf("%s/messages?after=%d&limit=500", base, r.Int64N(n)) },
		"one event type":          func() string { return fmt.Sprintf("%s/messages?after=%d&type=type-7", base, r.Int64N(n)) },
		"verified=false (rare)":   func() string { return base + "/messages?verified=false" },
		"since a recent time":     func() string { return base + "/messages?since=" + at(n-3600) },
		"time window": func() string {
			s := r.Int64N(n - 7200)
			return base + "/messages?since=" + at(s) + "&until=" + at(s+3600)
		},
		"by delivery id":   func() string { return fmt.Sprintf("%s/messages?delivery_id=evt_%d", base, 1+r.Int64N(n)) },
		"summary":          func() string { return fmt.Sprintf("%s/messages?after=%d&fields=summary", base, r.Int64N(n)) },
		"one message":      func() string { return fmt.Sprintf("%s/messages/%d", base, 1+r.Int64N(n)) },
		"one message, raw": func() string { return fmt.Sprintf("%s/messages/%d/raw", base, 1+r.Int64N(n)) },
		"type + window + verified": func() string {
			s := r.Int64N(n - 7200)
			return base + "/messages?type=type-3&verified=true&since=" + at(s) + "&until=" + at(s+7200)
		},
	}

	const iterations = 400
	type row struct {
		name          string
		p50, p95, max time.Duration
	}
	var rows []row
	for name, path := range shapes {
		times := make([]time.Duration, 0, iterations)
		for range iterations {
			p := path()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, p, nil)
			req.Header.Set("Authorization", "Bearer "+testAdminToken)
			rec := httptest.NewRecorder()
			begin := time.Now()
			h.ServeHTTP(rec, req)
			times = append(times, time.Since(begin))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: GET %s = %d, %s", name, p, rec.Code, rec.Body.String())
			}
		}
		slices.Sort(times)
		rows = append(rows, row{name, pct(times, 0.50), pct(times, 0.95), times[len(times)-1]})
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(b.p95, a.p95) })
	t.Logf("%-28s %10s %10s %10s", fmt.Sprintf("request (%d rows)", n), "p50", "p95", "max")
	for _, r := range rows {
		t.Logf("%-28s %10v %10v %10v", r.name, r.p50.Round(time.Microsecond), r.p95.Round(time.Microsecond), r.max.Round(time.Microsecond))
		if r.p95 >= 20*time.Millisecond {
			t.Errorf("p95 of %q is %v, over the 20 ms budget", r.name, r.p95)
		}
	}
}
