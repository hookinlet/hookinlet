// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func TestByteBudget(t *testing.T) {
	b := &byteBudget{limit: 100}
	r1, ok := b.take(60)
	if !ok {
		t.Fatal("60 of 100 refused")
	}
	if _, ok := b.take(50); ok {
		t.Error("110 of 100 granted")
	}
	r2, ok := b.take(40)
	if !ok {
		t.Error("exactly the rest refused")
	}
	r1()
	r1() // giving back twice gives back once
	if _, ok := b.take(60); !ok {
		t.Error("the 60 given back were not available")
	}
	r2()

	// A request larger than the whole budget gets the whole budget, when it is free.
	big := &byteBudget{limit: 10}
	if _, ok := big.take(1000); !ok {
		t.Error("a large request on an idle budget was refused")
	}
	if _, ok := big.take(1); ok {
		t.Error("the large request did not hold the whole budget")
	}
	// Negative turns it off; zero-sized requests cost nothing.
	off := &byteBudget{limit: -1}
	if _, ok := off.take(1 << 40); !ok {
		t.Error("an unlimited budget refused")
	}
	if _, ok := (&byteBudget{limit: 5}).take(0); !ok {
		t.Error("an empty request was refused")
	}

	// Under contention it never grants more than the limit.
	c := &byteBudget{limit: 1000}
	var mu sync.Mutex
	var held, most int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 200 {
				release, ok := c.take(100)
				if !ok {
					continue
				}
				mu.Lock()
				held += 100
				most = max(most, held)
				mu.Unlock()
				mu.Lock()
				held -= 100
				mu.Unlock()
				release()
			}
		})
	}
	wg.Wait()
	if most > 1000 {
		t.Errorf("%d bytes held at once, limit 1000", most)
	}
}

// Deliveries beyond the memory budget are told to come back, before their bodies
// are read, and nothing is stored for them.
func TestIngestRefusesWhatWouldExceedTheMemoryBudget(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.IngestMemoryBudget = 1000 })
	in := e.newInbox("budgeted", "trusted", "")
	hold, ok := e.server.ingestBudget.take(900)
	if !ok {
		t.Fatal("could not take the budget")
	}
	rec := e.post("/in/budgeted", strings.Repeat("x", 200))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "overloaded" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over budget: %d %s (Retry-After %q)", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	if n := e.count(in); n != 0 {
		t.Errorf("%d messages stored", n)
	}
	hold()
	if rec := e.post("/in/budgeted", strings.Repeat("x", 200)); rec.Code != http.StatusOK {
		t.Errorf("with the budget free again: %d %s", rec.Code, rec.Body)
	}
	// And the budget is given back after every delivery, however it ended.
	for _, body := range []string{"a", strings.Repeat("y", 5000)} {
		e.post("/in/budgeted", body)
	}
	if e.server.ingestBudget.used != 0 {
		t.Errorf("%d bytes of the budget are still held", e.server.ingestBudget.used)
	}
}

type fullStore struct{ inbox.Store }

func (fullStore) Append(context.Context, inbox.ID, *inbox.NewMessage) (*inbox.Message, bool, error) {
	return nil, false, errors.Join(inbox.ErrStorageFull, errors.New("disk is full"))
}

// A delivery that cannot be stored is not acknowledged; it is a 503 the sender will
// retry, and nothing about the disk is said to the sender.
func TestAFullDiskIsA503ForTheSender(t *testing.T) {
	e := newEnv(t)
	e.newInbox("nospace", "trusted", "")
	e.server.opts.Store = fullStore{e.store}
	rec := e.post("/in/nospace", "x")
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "storage_full" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "disk") {
		t.Errorf("the response describes the server's disk: %s", rec.Body)
	}
	if got := len(e.hooks.after()); got != 0 {
		t.Errorf("AfterIngest ran %d times for a delivery that was not stored", got)
	}
}

// BeforeIngest is told where the delivery came from and what it carried.
func TestBeforeIngestSeesTheSourceAndTheHeaders(t *testing.T) {
	e := newEnv(t)
	e.newInbox("described", "trusted", "")
	e.post("/in/described/deep/path", "hello", "X-Custom", "yes")
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	if len(e.hooks.requests) != 1 {
		t.Fatalf("%d requests seen", len(e.hooks.requests))
	}
	r := e.hooks.requests[0]
	if r.Inbox.Slug != "described" || r.Size != 5 || r.Method != http.MethodPost || r.PathSuffix != "deep/path" || r.Headers.Get("X-Custom") != "yes" {
		t.Errorf("the request: %+v", r)
	}
	if r.SourceIP == "" {
		t.Error("no source address")
	}
}

// Told to stop, the server reports not ready for DrainDelay while it still serves,
// and only then closes.
func TestADrainingServerReportsNotReadyBeforeItCloses(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.DrainDelay = 600 * time.Millisecond })
	e.newInbox("draining", "trusted", "")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.server.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	if status, _ := do2(t, "GET", base+"/readyz", nil, ""); status != http.StatusOK {
		t.Fatalf("before: %d", status)
	}
	begin := time.Now()
	stop()
	deadline := time.Now().Add(400 * time.Millisecond)
	var sawNotReady bool
	for time.Now().Before(deadline) {
		status, body := do2(t, "GET", base+"/readyz", nil, "")
		if status == http.StatusServiceUnavailable && strings.Contains(body, "shutting down") {
			sawNotReady = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawNotReady {
		t.Fatal("/readyz did not report the shutdown during the delay")
	}
	// Still serving: liveness and ingest work while the balancer catches up.
	if status, _ := do2(t, "GET", base+"/healthz", nil, ""); status != http.StatusOK {
		t.Errorf("/healthz while draining: %d", status)
	}
	if status, _ := do2(t, "POST", base+"/in/draining", nil, "x"); status != http.StatusOK {
		t.Errorf("ingest while draining: %d", status)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
		if time.Since(begin) < 500*time.Millisecond {
			t.Errorf("closed after %v, before the delay was over", time.Since(begin))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}
