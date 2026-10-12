// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func TestByteBudget(t *testing.T) {
	b := &byteBudget{limit: 100}
	if !b.add(60) {
		t.Fatal("60 of 100 refused")
	}
	if b.add(50) {
		t.Error("110 of 100 granted")
	}
	if !b.add(40) {
		t.Error("exactly the rest refused")
	}
	b.sub(60)
	if !b.add(60) {
		t.Error("the 60 given back were not available")
	}
	if off := (&byteBudget{limit: -1}); !off.add(1 << 40) {
		t.Error("an unlimited budget refused")
	}
	if !(&byteBudget{limit: 5}).add(0) {
		t.Error("nothing was refused")
	}

	// Under contention it never holds more than the limit.
	c := &byteBudget{limit: 1000}
	var most atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 200 {
				if !c.add(100) {
					continue
				}
				c.mu.Lock()
				most.Store(max(most.Load(), c.used))
				c.mu.Unlock()
				c.sub(100)
			}
		})
	}
	wg.Wait()
	if most.Load() > 1000 {
		t.Errorf("%d bytes held at once, limit 1000", most.Load())
	}
	if c.used != 0 {
		t.Errorf("%d bytes still held", c.used)
	}
}

// The bytes are counted as they arrive: a connection that has promised a large body
// and sent nothing holds nothing, and what a delivery held is given back however it
// ended.
func TestTheBudgetCountsBytesNotPromises(t *testing.T) {
	b := &byteBudget{limit: 100}
	br := &budgetReader{r: strings.NewReader(strings.Repeat("x", 80)), b: b}
	if got, err := io.ReadAll(br); err != nil || len(got) != 80 || b.used != 80 {
		t.Fatalf("read %d, used %d, %v", len(got), b.used, err)
	}
	over := &budgetReader{r: strings.NewReader(strings.Repeat("y", 50)), b: b}
	if _, err := io.ReadAll(over); !errors.Is(err, errOverBudget) {
		t.Errorf("a delivery over the budget: %v", err)
	}
	over.release()
	br.release()
	if b.used != 0 {
		t.Errorf("%d bytes still held after both were released", b.used)
	}
	idle := &budgetReader{r: strings.NewReader(""), b: b}
	_, _ = io.ReadAll(idle)
	if b.used != 0 {
		t.Error("a delivery that sent nothing held something")
	}
}

// Deliveries beyond the memory budget are told to come back and nothing is stored
// for them; a promise of a large body that is never kept does not block anyone.
func TestIngestRefusesWhatWouldExceedTheMemoryBudget(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.IngestMemoryBudget = inbox.MaxBodyBytesCeiling })
	in := e.newInbox("budgeted", "trusted", "", func(i *inbox.Inbox) { i.MaxBodyBytes = inbox.MaxBodyBytesCeiling })
	if !e.server.ingestBudget.add(inbox.MaxBodyBytesCeiling - 100) {
		t.Fatal("could not hold the budget")
	}
	rec := e.post("/in/budgeted", strings.Repeat("x", 200))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "overloaded" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over budget: %d %s (Retry-After %q)", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	if n := e.count(in); n != 0 {
		t.Errorf("%d messages stored", n)
	}
	if e.server.ingestBudget.used != inbox.MaxBodyBytesCeiling-100 {
		t.Errorf("the refused delivery kept %d bytes", e.server.ingestBudget.used-(inbox.MaxBodyBytesCeiling-100))
	}
	e.server.ingestBudget.sub(inbox.MaxBodyBytesCeiling - 100)
	if rec := e.post("/in/budgeted", strings.Repeat("x", 200)); rec.Code != http.StatusOK {
		t.Errorf("with the budget free again: %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{"a", strings.Repeat("y", 5000)} {
		e.post("/in/budgeted", body)
	}
	if e.server.ingestBudget.used != 0 {
		t.Errorf("%d bytes of the budget are still held", e.server.ingestBudget.used)
	}
}

func TestABudgetSmallerThanABodyLimitIsRefused(t *testing.T) {
	store := newEnv(t).store
	if _, err := New(Options{Addr: "127.0.0.1:0", Store: store, IngestMemoryBudget: 1000}); err == nil {
		t.Error("a budget smaller than the largest body an inbox may take was accepted")
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

// streamingStore has a Backupper that does not know how big its snapshot is.
type streamingStore struct{ inbox.Store }

func (streamingStore) Backup(context.Context) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader(strings.Repeat("a snapshot of a size nobody knew. ", 10000))), -1, nil
}

func TestABackupThatStreamsHasNoContentLength(t *testing.T) {
	a := newAdminEnv(t, func(o *Options) { o.Store = streamingStore{o.Store} })
	status, hdr, body := a.call("GET", "/v1/admin/backup", nil, testAdminToken)
	if status != http.StatusOK || string(body) != strings.Repeat("a snapshot of a size nobody knew. ", 10000) {
		t.Fatalf("%d, %d bytes", status, len(body))
	}
	// (A body that fits the server's first buffer is sent with a length anyway; one
	// this size is not, which is what streaming is for.)
	if cl := hdr.Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q for a snapshot of unknown size", cl)
	}
	if hdr.Get("Content-Disposition") == "" {
		t.Error("no filename")
	}
}

func TestMetricsCountWhatHappened(t *testing.T) {
	a := newAdminEnv(t)
	a.newInbox("counted", "github", "a-secret")
	a.newInbox("open-counted", "trusted", "")
	a.post("/in/open-counted", "one")
	a.post("/in/open-counted", "two")
	a.post("/in/counted", "unsigned") // rejected: no signature
	a.post("/in/nothing-here", "x")   // not found
	a.get("/v1/inboxes/open-counted", a.tokenA)

	status, _, body := a.call("GET", "/metrics", nil, testAdminToken)
	text := string(body)
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, text)
	}
	for _, want := range []string{
		`hookinlet_deliveries_total{result="stored"} 2`,
		`hookinlet_deliveries_total{result="verification_failed"} 1`,
		`hookinlet_deliveries_total{result="not_found"} 1`,
		`hookinlet_delivery_body_bytes_total `,
		`hookinlet_http_requests_total{route="POST /in/{key}",status="200"} 2`,
		`# TYPE hookinlet_streams_open gauge`,
		`hookinlet_ingest_memory_used_bytes 0`,
		`hookinlet_info{version="dev"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	// Nothing a client chooses becomes a label, and no secret is in the text.
	for _, banned := range []string{"counted", "open-counted", "nothing-here", testAdminToken, "a-secret", "unsigned"} {
		if strings.Contains(text, banned) {
			t.Errorf("the metrics mention %q", banned)
		}
	}
	// Unknown paths do not create series.
	for i := range 20 {
		a.get("/scan/"+strings.Repeat("x", i+1), "")
	}
	_, _, again := a.call("GET", "/metrics", nil, testAdminToken)
	if strings.Count(string(again), "\n") > strings.Count(text, "\n")+4 {
		t.Errorf("scanning added series:\n%s", again)
	}
	if !strings.Contains(string(again), `route="/",status="404"`) {
		t.Error("requests that matched no route are not counted under the catch-all")
	}
	// Admin only.
	for tok, want := range map[string]int{"": 401, a.tokenA: 403} {
		if st, _, _ := a.call("GET", "/metrics", nil, tok); st != want {
			t.Errorf("token %q: %d, want %d", tok, st, want)
		}
	}
}

type breakingStore struct{ inbox.Store }

func (breakingStore) Backup(context.Context) (io.ReadCloser, int64, error) {
	return io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat("good data. ", 30000)), failingReader{})), -1, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the store's stream broke") }

// A streamed backup that fails part way must not look whole to the client: the
// status is already sent, and a clean end would be taken for the end of the file.
func TestAStreamedBackupThatBreaksIsNotACleanEnd(t *testing.T) {
	a := newAdminEnv(t, func(o *Options) { o.Store = breakingStore{o.Store} })
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.base+"/v1/admin/backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("a body of %d bytes that broke part way ended cleanly", len(got))
	}
	if len(got) == 0 {
		t.Error("nothing was sent before the failure; the test proves little")
	}
}

// Calling Shutdown during the drain ends Serve, which must return and not wait for a
// listener that has already reported.
func TestShutdownDuringTheDrainEndsServe(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.DrainDelay = 5 * time.Second })
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.server.Serve(ctx, ln) }()
	time.Sleep(100 * time.Millisecond)
	stop() // told to stop: the drain begins
	time.Sleep(100 * time.Millisecond)
	if err := e.server.Shutdown(t.Context()); err != nil { // an embedder closes it meanwhile
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after Shutdown during the drain")
	}
}

func TestOptionsFollowOneRule(t *testing.T) {
	store := newEnv(t).store
	// Only Store is required; an address is for Run.
	srv, err := New(Options{Store: store})
	if err != nil {
		t.Fatalf("a Server with only a Store: %v", err)
	}
	if err := srv.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "Addr") {
		t.Errorf("Run without an address: %v", err)
	}
	if srv.Handler() == nil {
		t.Error("no handler")
	}
	// A negative number is an error where it means nothing, and is how a bound is
	// turned off where it does.
	for name, o := range map[string]Options{
		"ShutdownTimeout":    {Store: store, ShutdownTimeout: -1},
		"MaxStreams":         {Store: store, MaxStreams: -1},
		"MaxHeaderBytes":     {Store: store, MaxHeaderBytes: -1},
		"SSERecheckInterval": {Store: store, SSERecheckInterval: -1},
		"DrainDelay":         {Store: store, DrainDelay: -1},
		"MCPInlineBodyBytes": {Store: store, MCPInlineBodyBytes: -1},
	} {
		if _, err := New(o); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, o := range map[string]Options{
		"RetentionInterval":  {Store: store, RetentionInterval: -1},
		"IngestMemoryBudget": {Store: store, IngestMemoryBudget: -1},
	} {
		if _, err := New(o); err != nil {
			t.Errorf("%s: a negative value should turn it off: %v", name, err)
		}
	}
}

// An embedder that serves the handler itself has Maintain to run the schedule.
func TestMaintainRunsTheRetentionScheduleWithoutServe(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.RetentionInterval = 50 * time.Millisecond })
	in := e.newInbox("kept-short", "trusted", "", func(i *inbox.Inbox) { i.RetentionMaxCount = 2 })
	for range 6 {
		e.post("/in/kept-short", "x")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.server.Maintain(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.count(in) != 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := e.count(in); n != 2 {
		t.Errorf("%d messages after the schedule ran, want 2", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Maintain did not return when its context was done")
	}
	off := newEnv(t, func(o *Options) { o.RetentionInterval = -1 })
	returned := make(chan struct{})
	go func() { off.server.Maintain(t.Context()); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Error("Maintain with the schedule off did not return at once")
	}
}
