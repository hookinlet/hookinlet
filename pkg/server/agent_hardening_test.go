// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// serveOn runs the env's server on a real socket and returns its base URL.
func serveOn(t *testing.T, e *env) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.server.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return "http://" + ln.Addr().String()
}

// do2 sends a request to a running server and returns the status and body.
func do2(t *testing.T, method, url string, hdr http.Header, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// Every header a sender provides is stored and later handed to agents, so their
// total size is bounded at the door. Go's default is 1 MiB per request, which let
// a single sender plant messages that made later pages cost gigabytes.
func TestHeaderSizeIsLimited(t *testing.T) {
	e := newAgentEnv(t)
	base := serveOn(t, e.env)

	big := http.Header{"X-Big": {strings.Repeat("h", 200<<10)}}
	if status, _ := do2(t, http.MethodPost, base+"/in/agent-a", big, "x"); status != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("200 KB of headers: status %d, want 431", status)
	}
	// Ordinary headers, even generous ones, are fine.
	ok := http.Header{"X-Reasonable": {strings.Repeat("h", 8<<10)}, "Content-Type": {"application/json"}}
	if status, body := do2(t, http.MethodPost, base+"/in/agent-a", ok, `{"a":1}`); status != http.StatusOK {
		t.Errorf("8 KB of headers: status %d, %s", status, body)
	}
	if n := e.count(e.a); n != 1 {
		t.Errorf("%d messages stored, want only the one with ordinary headers", n)
	}
	// The limit is configurable.
	e2 := newAgentEnv2(t, func(o *Options) { o.MaxHeaderBytes = 2 << 10 })
	base2 := serveOn(t, e2.env)
	// net/http allows 4 KiB of read-buffer slack on top of the configured limit.
	if status, _ := do2(t, http.MethodPost, base2+"/in/agent-a", http.Header{"X-Pad": {strings.Repeat("h", 10<<10)}}, "x"); status != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("10 KB with a 2 KB limit: status %d", status)
	}
	if status, _ := do2(t, http.MethodPost, base2+"/in/agent-a", http.Header{"X-Pad": {strings.Repeat("h", 1<<10)}}, "x"); status != http.StatusOK {
		t.Errorf("1 KB with a 2 KB limit: status %d", status)
	}
}

func newAgentEnv2(t *testing.T, mods ...func(*Options)) *agentEnv {
	t.Helper()
	e := newEnv(t, append([]func(*Options){withAuth}, mods...)...)
	ae := &agentEnv{env: e}
	a := e.newInbox("agent-a", "none", "")
	ae.a = a
	return ae
}

func TestMalformedQueryStringsAreAnErrorEverywhere(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "x")
	for _, path := range []string{
		"/v1/inboxes/agent-a/messages?after=5;&limit=2",
		"/v1/inboxes/agent-a/messages?after=%zz",
		"/v1/inboxes/agent-a/messages?%",
		"/v1/inboxes/agent-a?;x",
		"/v1/inboxes/agent-a/messages/1?;x",
		"/v1/inboxes/agent-a/messages/1/raw?%zz",
	} {
		rec := e.get(path, e.tokenA)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_parameter" {
			t.Errorf("%s: status %d, %s", path, rec.Code, rec.Body.String())
		}
	}
	// A hostile parameter name is not echoed in full.
	rec := e.get("/v1/inboxes/agent-a?"+strings.Repeat("x", 5000)+"=1", e.tokenA)
	if rec.Code != http.StatusBadRequest || rec.Body.Len() > 400 {
		t.Errorf("status %d, response of %d bytes", rec.Code, rec.Body.Len())
	}
	rec = e.get("/v1/inboxes/agent-a/messages/1/raw?"+strings.Repeat("y", 5000)+"=1", e.tokenA)
	if rec.Body.Len() > 400 {
		t.Errorf("the raw endpoint echoed a hostile parameter name: %d bytes", rec.Body.Len())
	}
}

// blockingStore holds every page query until released, so a test can pin a read
// in flight.
type blockingStore struct {
	inbox.Store
	entered chan struct{}
	release chan struct{}
}

func (b blockingStore) Query(ctx context.Context, id inbox.ID, q inbox.Query) (*inbox.Page, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return b.Store.Query(ctx, id, q)
}

// Building a page can take tens of megabytes. Without a limit, a handful of
// agents polling at once could exhaust memory, so reads queue for a slot and give
// up with 503 and a Retry-After rather than piling up.
func TestConcurrentReadsAreLimited(t *testing.T) {
	bs := blockingStore{entered: make(chan struct{}, 8), release: make(chan struct{})}
	e := newEnv(t,
		func(o *Options) { bs.Store = o.Store; o.Store = bs },
		withAuth,
		func(o *Options) { o.MaxConcurrentReads = 1; o.ReadWaitTimeout = 100 * time.Millisecond },
	)
	in := e.newInbox("limited-reads", "none", "")
	_ = in

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- e.get("/v1/inboxes/limited-reads/messages", testAdminToken) }()
	select {
	case <-bs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first read never reached the store")
	}

	// Without a limit this request would also enter the store and block, so give
	// it a deadline and fail clearly instead of hanging the test.
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- e.get("/v1/inboxes/limited-reads/messages", testAdminToken) }()
	select {
	case rec := <-secondDone:
		if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "overloaded" || rec.Header().Get("Retry-After") == "" {
			t.Errorf("second read while the slot is taken: status %d, %s, Retry-After %q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
		}
	case <-time.After(3 * time.Second):
		close(bs.release) // unblock whatever is stuck so the test can end
		t.Fatal("the second read was not turned away while the only slot was taken")
	}
	// Unauthenticated requests never take a slot, so they cannot starve readers.
	if rec := e.get("/v1/inboxes/limited-reads/messages", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request while the slot is taken: status %d", rec.Code)
	}

	close(bs.release)
	if rec := <-first; rec.Code != http.StatusOK {
		t.Errorf("the first read: status %d", rec.Code)
	}
	if rec := e.get("/v1/inboxes/limited-reads/messages", testAdminToken); rec.Code != http.StatusOK {
		t.Errorf("a read after the slot was freed: status %d, %s", rec.Code, rec.Body.String())
	}
}

func TestAbandonedReadsGiveUpTheirPlaceInTheQueue(t *testing.T) {
	bs := blockingStore{entered: make(chan struct{}, 8), release: make(chan struct{})}
	e := newEnv(t,
		func(o *Options) { bs.Store = o.Store; o.Store = bs },
		withAuth,
		func(o *Options) { o.MaxConcurrentReads = 1; o.ReadWaitTimeout = 30 * time.Second },
	)
	e.newInbox("queue-box", "none", "")
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- e.get("/v1/inboxes/queue-box/messages", testAdminToken) }()
	<-bs.entered

	// A second reader waits, then its client goes away. It must return promptly.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/inboxes/queue-box/messages", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	done := make(chan struct{})
	go func() { defer close(done); e.server.Handler().ServeHTTP(httptest.NewRecorder(), req) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read whose client went away kept waiting for a slot")
	}
	close(bs.release)
	<-first
}

func TestRequestFailuresAreLoggedHonestly(t *testing.T) {
	e := newAgentEnv(t)

	// A failing store is our problem and says so, as a read, not as an ingest.
	_ = e.store.Close()
	e.get("/v1/inboxes/agent-a/messages", testAdminToken)
	logs := e.logs.String()
	if !strings.Contains(logs, `"level":"ERROR"`) || strings.Contains(logs, "ingest failed") {
		t.Errorf("a read failure was logged as: %s", logs)
	}

	// A client that disconnects mid-request is not an error on our side.
	e2 := newAgentEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/inboxes/agent-a", nil)
	req.Header.Set("Authorization", "Bearer "+e2.tokenA)
	e2.server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(e2.logs.String(), `"level":"ERROR"`) {
		t.Errorf("a cancelled request was logged as an error: %s", e2.logs.String())
	}
}

func TestRawResponseIsKeptSameOrigin(t *testing.T) {
	e := newAgentEnv(t)
	e.do(req{path: "/in/agent-a", body: "<html>", headers: http.Header{"Content-Type": {"text/html"}}})
	rec := e.get("/v1/inboxes/agent-a/messages/1/raw", e.tokenA)
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Errorf("Cross-Origin-Resource-Policy = %q", got)
	}
}

// tokenFor creates an agent token for the inbox and returns it.
func (ae *agentEnv) tokenFor(in *inbox.Inbox) string {
	ae.t.Helper()
	tok, hash, err := token.Generate()
	if err != nil {
		ae.t.Fatal(err)
	}
	upd := *in
	upd.AgentTokenHash = hash
	if err := ae.store.UpdateInbox(ae.t.Context(), &upd); err != nil {
		ae.t.Fatal(err)
	}
	*in = upd
	return tok
}

func newListener(t *testing.T) (net.Listener, error) {
	t.Helper()
	var lc net.ListenConfig
	return lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
}
