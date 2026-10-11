// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/server"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

type fixture struct {
	url, token string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	box, _ := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	srv, err := server.New(server.Options{
		Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: store, SecretBox: box,
		Notifier: local.New(), Authenticator: token.NewAuthenticator(store, strings.Repeat("a", 40)), RetentionInterval: -1,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	tok, hash, _ := token.Generate()
	if err := store.CreateInbox(t.Context(), &inbox.Inbox{Slug: "agent-test", Name: "t", Verifier: "none", AgentTokenHash: hash}); err != nil {
		t.Fatal(err)
	}
	return &fixture{url: ts.URL, token: tok}
}

func (f *fixture) send(t *testing.T, body string) {
	t.Helper()
	resp, err := http.Post(f.url+"/in/agent-test", "application/json", strings.NewReader(body)) //nolint:gosec,noctx // a local test server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

type collector struct {
	mu   sync.Mutex
	seqs []int64
}

func (c *collector) handle(m Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seqs = append(c.seqs, m.Seq)
}

func (c *collector) got() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.seqs...)
}

func TestPollingSeesEveryMessageOnceInOrder(t *testing.T) {
	f := newFixture(t)
	for i := range 120 { // more than a page
		f.send(t, `{"n":`+string(rune('0'+i%10))+`}`)
	}
	var c collector
	a := &Agent{URL: f.url, Token: f.token, Inbox: "agent-test", Handle: c.handle, Interval: 10 * time.Millisecond, Limit: 120}
	if err := a.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := c.got()
	if len(got) != 120 {
		t.Fatalf("got %d messages", len(got))
	}
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("seqs[%d] = %d: a gap, repeat, or reordering", i, s)
		}
	}
}

func TestPollingWaitsForNewMessagesAndStartsWhereToldTo(t *testing.T) {
	f := newFixture(t)
	f.send(t, `{"old":1}`)
	f.send(t, `{"old":2}`)
	var c collector
	a := &Agent{URL: f.url, Token: f.token, Inbox: "agent-test", After: 2, Handle: c.handle, Interval: 10 * time.Millisecond, Limit: 1}
	done := make(chan error, 1)
	go func() { done <- a.Poll(t.Context()) }()
	time.Sleep(100 * time.Millisecond)
	f.send(t, `{"new":3}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("polling never saw the new message")
	}
	if got := c.got(); len(got) != 1 || got[0] != 3 {
		t.Errorf("got %v, want [3]", got)
	}
}

func TestStreamingReplaysThenFollowsLive(t *testing.T) {
	f := newFixture(t)
	f.send(t, `{"n":1}`)
	f.send(t, `{"n":2}`)
	var c collector
	a := &Agent{URL: f.url, Token: f.token, Inbox: "agent-test", Handle: c.handle, Limit: 3}
	done := make(chan error, 1)
	go func() { done <- a.Stream(t.Context()) }()
	time.Sleep(200 * time.Millisecond)
	f.send(t, `{"n":3}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the stream delivered only %v", c.got())
	}
	if got := c.got(); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("got %v, want [1 2 3]", got)
	}
}

func TestAMissingInboxEndsAStreamInsteadOfRetryingForever(t *testing.T) {
	f := newFixture(t)
	a := &Agent{URL: f.url, Token: f.token, Inbox: "no-such-inbox", Handle: func(Message) {}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := a.Stream(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Stream of a missing inbox: %v", err)
	}
}

func TestAServerThatIsDownIsRetriedNotFatal(t *testing.T) {
	f := newFixture(t)
	f.send(t, `{"n":1}`)
	var down sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.TryLock() {
			down.Unlock()
		} else {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, f.url+r.URL.RequestURI(), nil) //nolint:gosec // a test proxy to a test server
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // a test proxy to a test server
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	down.Lock() // the server is down
	var c collector
	a := &Agent{URL: proxy.URL, Token: f.token, Inbox: "agent-test", Handle: c.handle, Interval: 20 * time.Millisecond, Limit: 1}
	done := make(chan error, 1)
	go func() { done <- a.Poll(t.Context()) }()
	time.Sleep(150 * time.Millisecond)
	down.Unlock() // and comes back
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("polling gave up on a server that came back: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("polling never recovered")
	}
	if got := c.got(); len(got) != 1 || got[0] != 1 {
		t.Errorf("got %v", got)
	}
}

func TestAWrongTokenIsAnErrorNotAHang(t *testing.T) {
	f := newFixture(t)
	a := &Agent{URL: f.url, Token: "hki_wrong", Inbox: "agent-test", Handle: func(Message) {}, Interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := a.Poll(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("Poll with a bad token: %v", err)
	}
}
