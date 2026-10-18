// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func limiter(perSecond float64, burst int) (*RateLimiter, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := NewRateLimiter(perSecond, burst)
	r.now = clk.now
	return r, clk
}

func TestABurstIsAllowedThenRefused(t *testing.T) {
	r, _ := limiter(1, 3)
	in := &inbox.Inbox{ID: 1}
	for i := range 3 {
		if err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in, Size: 10}); err != nil {
			t.Fatalf("delivery %d inside the burst: %v", i+1, err)
		}
	}
	err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in, Size: 10})
	if !errors.Is(err, inbox.ErrRateLimited) {
		t.Fatalf("the delivery after the burst: %v, want ErrRateLimited", err)
	}
}

func TestTokensComeBackWithTime(t *testing.T) {
	r, clk := limiter(2, 2)
	in := &inbox.Inbox{ID: 1}
	for range 2 {
		_ = r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in, Size: 1})
	}
	if err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in, Size: 1}); err == nil {
		t.Fatal("an empty bucket allowed a delivery")
	}
	clk.advance(400 * time.Millisecond) // 0.8 of a token: not enough
	if err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in, Size: 1}); err == nil {
		t.Error("a delivery was allowed with less than one token")
	}
	clk.advance(200 * time.Millisecond) // now one token
	if err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in, Size: 1}); err != nil {
		t.Errorf("after enough time: %v", err)
	}
	clk.advance(time.Hour) // refills to the burst and no further
	allowed := 0
	for range 10 {
		if r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in, Size: 1}) == nil {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("after a long wait %d deliveries were allowed, want the burst of 2", allowed)
	}
}

func TestInboxesDoNotShareABucket(t *testing.T) {
	r, _ := limiter(1, 1)
	a, b := &inbox.Inbox{ID: 1}, &inbox.Inbox{ID: 2}
	if err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: a, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: a, Size: 1}); err == nil {
		t.Error("inbox a was not limited")
	}
	if err := r.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: b, Size: 1}); err != nil {
		t.Errorf("inbox b was limited by inbox a's traffic: %v", err)
	}
}

func TestItIsSafeToCallConcurrently(t *testing.T) {
	r, _ := limiter(1, 50)
	in := &inbox.Inbox{ID: 1}
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Go(func() {
			if r.BeforeIngest(context.Background(), &inbox.IngestRequest{Inbox: in, Size: 1}) == nil {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 50 {
		t.Errorf("%d of 200 simultaneous deliveries were allowed, want exactly the burst of 50", allowed)
	}
}

// The whole program, started the way a user would start it: the hook must turn
// into a 429 at the front door.
func TestTheProgramAnswers429WhenTheLimitIsReached(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cl, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	countsAddr := cl.Addr().String()
	_ = cl.Close()

	env := map[string]string{
		"HOOKINLET_ADMIN_TOKEN": strings.Repeat("a", 40),
		"HOOKINLET_MASTER_KEY":  "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", // 32 bytes
		"HOOKINLET_DATA_DIR":    t.TempDir(),
		"HOOKINLET_LISTEN_ADDR": addr,
		"EXAMPLE_RATE":          "0.001",
		"EXAMPLE_BURST":         "2",
		"EXAMPLE_COUNTS_ADDR":   countsAddr,
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), func(k string) string { return env[k] })
		close(done) // so that a second receive, in the cleanup, does not wait
	}()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("the example stopped with %v", err)
		}
	})

	base := "http://" + addr
	waitUp(t, base+"/healthz", done)
	call := func(method, path, body string, auth bool) (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer "+env["HOOKINLET_ADMIN_TOKEN"])
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	if status, body := call("POST", "/v1/admin/inboxes", `{"slug":"limited-inbox","ingest_key":"limited-inbox","verifier":"none"}`, true); status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var got []int
	for range 4 {
		status, _ := call("POST", "/in/limited-inbox", `{"x":1}`, false)
		got = append(got, status)
	}
	want := []int{200, 200, 429, 429}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
	}
	call("POST", "/in/no-such-inbox", `{}`, false) // no inbox, so not counted

	// The counter saw every delivery to the inbox, the refused ones too.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+countsAddr+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var counts bytes.Buffer
	_, _ = counts.ReadFrom(resp.Body)
	for _, line := range []string{
		`example_deliveries_total{tenant="",inbox="limited-inbox",outcome="stored"} 2`,
		`example_deliveries_total{tenant="",inbox="limited-inbox",outcome="rate_limited"} 2`,
	} {
		if !strings.Contains(counts.String(), line+"\n") {
			t.Errorf("the counts do not say %s:\n%s", line, counts.String())
		}
	}
	if strings.Contains(counts.String(), "no-such-inbox") {
		t.Errorf("a delivery to no inbox was counted:\n%s", counts.String())
	}
}

// waitUp waits for the program to answer, and fails at once if it stops instead
// (for example because another test took the port between the probe and the bind).
func waitUp(t *testing.T, url string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("the program stopped before it was up: %v", err)
		default:
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the program never came up")
}

func TestMisconfigurationIsReportedNotIgnored(t *testing.T) {
	good := map[string]string{
		"HOOKINLET_ADMIN_TOKEN": strings.Repeat("a", 40),
		"HOOKINLET_MASTER_KEY":  "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		"HOOKINLET_DATA_DIR":    t.TempDir(),
	}
	for name, mod := range map[string]func(map[string]string){
		"short admin token": func(e map[string]string) { e["HOOKINLET_ADMIN_TOKEN"] = "short" },
		"bad master key":    func(e map[string]string) { e["HOOKINLET_MASTER_KEY"] = "!!!" },
		"short master key":  func(e map[string]string) { e["HOOKINLET_MASTER_KEY"] = "c2hvcnQ=" },
		"bad rate":          func(e map[string]string) { e["EXAMPLE_RATE"] = "fast" },
		"zero burst":        func(e map[string]string) { e["EXAMPLE_BURST"] = "0" },
		"fractional burst":  func(e map[string]string) { e["EXAMPLE_BURST"] = "0.5" },
		"NaN rate":          func(e map[string]string) { e["EXAMPLE_RATE"] = "NaN" },
		"infinite rate":     func(e map[string]string) { e["EXAMPLE_RATE"] = "Inf" },
	} {
		env := map[string]string{}
		for k, v := range good {
			env[k] = v
		}
		mod(env)
		// A cancelled context: a configuration that is wrongly accepted then ends
		// at once with no error, instead of serving until the test times out.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), func(k string) string { return env[k] })
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
