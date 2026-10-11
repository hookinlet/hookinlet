// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// syncBuffer is a bytes.Buffer safe for concurrent use by the logger.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestServer(t *testing.T) (*Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	store, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(Options{
		Addr:   "127.0.0.1:0",
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		Store:  store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, logs
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func listen(t *testing.T) (net.Listener, error) {
	t.Helper()
	var lc net.ListenConfig
	return lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
}

func TestNewValidatesOptions(t *testing.T) {
	store, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := New(Options{Store: store}); err == nil {
		t.Error("want an error for a missing Addr")
	}
	if _, err := New(Options{Addr: "127.0.0.1:0"}); err == nil {
		t.Error("want an error for a missing Store")
	}
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(t)
	rec := do(t, s.Handler(), http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Errorf("body %q, err %v", rec.Body.String(), err)
	}
}

func TestUnknownRoutesReturnJSONErrors(t *testing.T) {
	s, _ := newTestServer(t)
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/nope"},
		{http.MethodGet, "/healthz/extra"},
	} {
		rec := do(t, s.Handler(), tt.method, tt.path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d", tt.method, tt.path, rec.Code)
		}
		var body struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "not_found" {
			t.Errorf("%s %s: body %q, err %v", tt.method, tt.path, rec.Body.String(), err)
		}
	}
}

// A method a path does not take is a 405 with the methods the document gives it.
func TestAWrongMethodIsAJSON405WithAnAllowHeader(t *testing.T) {
	s, _ := newTestServer(t)
	for _, tt := range []struct{ method, path, allow string }{
		{http.MethodPost, "/healthz", "GET, HEAD"},
		{http.MethodDelete, "/readyz", "GET, HEAD"},
		{http.MethodGet, "/in/some-inbox", "POST, PUT"},
		{http.MethodHead, "/in/some-inbox/sub", "POST, PUT"},
	} {
		rec := do(t, s.Handler(), tt.method, tt.path, nil)
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != tt.allow ||
			(tt.method != http.MethodHead && (json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Error.Code != "method_not_allowed")) {
			t.Errorf("%s %s: %d, Allow %q, body %q", tt.method, tt.path, rec.Code, rec.Header().Get("Allow"), rec.Body.String())
		}
	}
}

func TestRequestID(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	generated := do(t, h, http.MethodGet, "/healthz", nil).Header().Get("X-Request-Id")
	if len(generated) != 16 {
		t.Errorf("generated id %q", generated)
	}
	if got := do(t, h, http.MethodGet, "/healthz", map[string]string{"X-Request-Id": "abc-123_x.y"}).Header().Get("X-Request-Id"); got != "abc-123_x.y" {
		t.Errorf("well-formed id not honored: %q", got)
	}
	for _, bad := range []string{"has space", "semi;colon", strings.Repeat("a", 65), "new\tline"} {
		got := do(t, h, http.MethodGet, "/healthz", map[string]string{"X-Request-Id": bad}).Header().Get("X-Request-Id")
		if got == bad || len(got) != 16 {
			t.Errorf("malformed id %q was not replaced: %q", bad, got)
		}
	}
}

func TestAccessLogUsesRoutePatternNotPath(t *testing.T) {
	s, logs := newTestServer(t)
	do(t, s.Handler(), http.MethodGet, "/healthz?token=secret-in-query", nil)
	do(t, s.Handler(), http.MethodGet, "/unknown/credential-like-segment", nil)

	out := logs.String()
	if !strings.Contains(out, `"route":"GET /healthz"`) {
		t.Errorf("route pattern not logged: %s", out)
	}
	for _, leak := range []string{"secret-in-query", "credential-like-segment"} {
		if strings.Contains(out, leak) {
			t.Errorf("access log leaked %q: %s", leak, out)
		}
	}
}

func TestPanicBecomesJSON500AndIsLogged(t *testing.T) {
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	h := requestID(accessLog(log, recoverer(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))

	rec := do(t, h, http.MethodGet, "/x", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Errorf("body %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("panic value leaked to client: %q", rec.Body.String())
	}
	if out := logs.String(); !strings.Contains(out, "boom") || !strings.Contains(out, `"status":500`) {
		t.Errorf("panic not logged with status 500: %s", out)
	}
}

func TestPanicAfterWriteKeepsOriginalResponse(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := accessLog(log, recoverer(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late")
	})))
	if rec := do(t, h, http.MethodGet, "/x", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 preserved", rec.Code)
	}
}

func TestStatusWriterUnwrapsForResponseController(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Fatalf("Flush through statusWriter: %v", err)
	}
}

func TestServeAndGracefulShutdown(t *testing.T) {
	s, _ := newTestServer(t)
	ln, err := listen(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz") //nolint:noctx // test against a local listener
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil { //nolint:noctx,bodyclose // expecting failure
		t.Error("server still accepting connections after shutdown")
	}
}

func TestShutdownWaitsForInFlightRequest(t *testing.T) {
	s, _ := newTestServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	ln, err := listen(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	bodyCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/") //nolint:noctx // test
		if err != nil {
			bodyCh <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		bodyCh <- string(b)
	}()

	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("Serve returned while a request was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if got := <-bodyCh; got != "finished" {
		t.Errorf("in-flight request got %q", got)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

func TestShutdownTimeoutForcesClose(t *testing.T) {
	logs := &syncBuffer{}
	store, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(Options{
		Addr:            "127.0.0.1:0",
		Logger:          slog.New(slog.NewJSONHandler(logs, nil)),
		ShutdownTimeout: 50 * time.Millisecond,
		Store:           store,
	})
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	s.http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	ln, _ := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/") //nolint:noctx // test
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("want a shutdown error when in-flight work outlives the timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the shutdown timeout")
	}
}
