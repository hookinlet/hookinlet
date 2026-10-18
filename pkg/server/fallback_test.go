// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fallbackSeen is a Fallback that records the requests it is given.
type fallbackSeen struct{ paths []string }

func (f *fallbackSeen) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "fallback")
}

func serveOnce(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func withFallback(f http.Handler) func(*Options) {
	return func(o *Options) { o.Fallback = f }
}

func TestFallbackAnswersPathsTheServerHasNoRouteFor(t *testing.T) {
	f := &fallbackSeen{}
	e := newEnv(t, withAuth, withFallback(f))
	for _, tc := range []struct{ method, path string }{
		{"GET", "/"},
		{"GET", "/.well-known/oauth-protected-resource"},
		{"GET", "/robots.txt"},
		{"HEAD", "/robots.txt"},
		{"POST", "/hooks-ui/save"},
		{"GET", "/v2/things"},
		{"GET", "/healthz/"}, // not the route's path, and under a segment the server uses
	} {
		f.paths = nil
		rec := serveOnce(e.server.Handler(), tc.method, tc.path, "")
		owned := tc.path == "/healthz/"
		if owned {
			if rec.Code != http.StatusNotFound || len(f.paths) != 0 {
				t.Errorf("%s %s: status %d, fallback saw %v; a path under a server segment keeps its 404", tc.method, tc.path, rec.Code, f.paths)
			}
			continue
		}
		if rec.Code != 200 || len(f.paths) != 1 {
			t.Errorf("%s %s: status %d, fallback saw %v", tc.method, tc.path, rec.Code, f.paths)
		}
		if tc.method != "HEAD" && rec.Body.String() != "fallback" {
			t.Errorf("%s %s: body %q", tc.method, tc.path, rec.Body.String())
		}
	}
}

// Nothing the server routes, or would route in another configuration, reaches the
// Fallback; each keeps exactly the answer it has without one.
func TestFallbackIsNeverReachedForTheServersPaths(t *testing.T) {
	paths := []struct{ method, path string }{
		{"GET", "/healthz"}, {"POST", "/healthz"}, {"GET", "/readyz"}, {"GET", "/metrics"},
		{"POST", "/in/some-key"}, {"GET", "/in/some-key"}, {"POST", "/in/some-key/sub/path"}, {"POST", "/in"}, {"POST", "/in/"},
		{"GET", "/v1/inboxes/x"}, {"GET", "/v1/inboxes/x/messages"}, {"GET", "/v1/inboxes/x/stream"}, {"GET", "/v1/inboxes"},
		{"POST", "/mcp"}, {"GET", "/mcp"}, {"GET", "/mcp/"},
		{"GET", "/v1/admin/inboxes"}, {"POST", "/v1/admin/inboxes"}, {"GET", "/v1/admin/tokens"}, {"GET", "/v1/admin/backup"},
		{"GET", "/v1/admin/status"}, {"GET", "/v1/admin/nothing"}, {"GET", "/v1/anything"}, {"GET", "/v1"}, {"GET", "/v1/"},
		{"GET", "/admin"}, {"GET", "/admin/"}, {"GET", "/admin/assets/app.js"}, {"POST", "/admin/x"},
		{"GET", "/%69n/some-key"}, {"GET", "/v1%2Fadmin/status"},
	}
	configs := map[string]func(*Options){
		"defaults":          func(*Options) {},
		"authenticated":     withAuth,
		"admin API off":     func(o *Options) { withAuth(o); o.DisableAdmin = true },
		"console off":       func(o *Options) { withAuth(o); o.DisableUI = true },
		"MCP off":           func(o *Options) { withAuth(o); o.DisableMCP = true },
		"admin API and MCP": func(o *Options) { withAuth(o); o.DisableAdmin, o.DisableMCP = true, true },
	}
	for name, mod := range configs {
		f := &fallbackSeen{}
		with := newEnv(t, mod, withFallback(f))
		without := newEnv(t, mod)
		for _, p := range paths {
			got := serveOnce(with.server.Handler(), p.method, p.path, "x")
			want := serveOnce(without.server.Handler(), p.method, p.path, "x")
			if len(f.paths) != 0 {
				t.Errorf("%s: %s %s reached the fallback", name, p.method, p.path)
				f.paths = nil
			}
			if got.Code != want.Code {
				t.Errorf("%s: %s %s: status %d with a fallback, %d without", name, p.method, p.path, got.Code, want.Code)
			}
		}
	}
}

// With the admin API off its paths answer the 404 they always did, not the Fallback.
func TestFallbackDoesNotAnswerForADisabledAdminAPI(t *testing.T) {
	f := &fallbackSeen{}
	e := newEnv(t, withAuth, withFallback(f), func(o *Options) { o.DisableAdmin = true })
	for _, path := range []string{"/v1/admin/inboxes", "/v1/admin/tokens", "/v1/admin/status", "/v1/admin/backup", "/v1/admin/inboxes/x/secret", "/admin/", "/admin"} {
		rec := serveOnce(e.server.Handler(), "GET", path, "")
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) || strings.Contains(rec.Body.String(), "fallback") {
			t.Errorf("%s: status %d, body %q", path, rec.Code, rec.Body.String())
		}
	}
	if len(f.paths) != 0 {
		t.Errorf("the fallback saw %v", f.paths)
	}
	if rec := serveOnce(e.server.Handler(), "GET", "/.well-known/x", ""); rec.Body.String() != "fallback" {
		t.Errorf("an unknown path: %q", rec.Body.String())
	}
}

// A nil Fallback leaves the 404 exactly as it was: same status, headers (but the
// request ID) and body.
func TestWithoutAFallbackTheUnroutedAnswerIsTheFixed404(t *testing.T) {
	e := newEnv(t, withAuth)
	rec := serveOnce(e.server.Handler(), "GET", "/.well-known/x", "")
	if rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
	const want = "{\"error\":{\"code\":\"not_found\",\"message\":\"not found\"}}\n"
	if rec.Body.String() != want {
		t.Errorf("body %q, want %q", rec.Body.String(), want)
	}
	h := rec.Header().Clone()
	h.Del("X-Request-Id")
	if h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", h)
	}

	// And a server with a Fallback answers a path the server owns byte for byte as one without.
	with := newEnv(t, withAuth, withFallback(&fallbackSeen{}))
	for _, path := range []string{"/v1/admin/nothing", "/admin/nothing", "/in"} {
		a, b := serveOnce(e.server.Handler(), "GET", path, ""), serveOnce(with.server.Handler(), "GET", path, "")
		a.Header().Del("X-Request-Id")
		b.Header().Del("X-Request-Id")
		if a.Code != b.Code || a.Body.String() != b.Body.String() || !equalHeaders(a.Header(), b.Header()) {
			t.Errorf("%s: %d %v %q without a fallback, %d %v %q with one", path, a.Code, a.Header(), a.Body.String(), b.Code, b.Header(), b.Body.String())
		}
	}
}

func equalHeaders(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if strings.Join(av, "\x00") != strings.Join(b[k], "\x00") {
			return false
		}
	}
	return true
}

// The Fallback runs inside the server's middleware.
func TestFallbackRunsInsideTheMiddleware(t *testing.T) {
	e := newEnv(t, withFallback(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("fallback exploded")
		}
		_, _ = io.WriteString(w, "ok")
	})))
	rec := serveOnce(e.server.Handler(), "GET", "/page", "")
	if rec.Header().Get("X-Request-Id") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", rec.Header())
	}
	rec = serveOnce(e.server.Handler(), "GET", "/boom", "")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Errorf("a panic in the fallback: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(e.logs.String(), "access") && !strings.Contains(e.logs.String(), "/page") {
		t.Errorf("the request was not logged: %s", e.logs.String())
	}
}

func TestFallbackBodyIsLimited(t *testing.T) {
	read := func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case err != nil:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = io.WriteString(w, "read "+strconv.Itoa(len(b)))
		}
	}
	for _, tc := range []struct {
		name  string
		limit int64
		size  int
		want  int
	}{
		{"default, under", 0, 1<<20 - 1, 200},
		{"default, exactly", 0, 1 << 20, 200},
		{"default, over", 0, 1<<20 + 1, 413},
		{"set, over", 10, 11, 413},
		{"set, under", 10, 10, 200},
		{"off", -1, 3 << 20, 200},
	} {
		e := newEnv(t, withFallback(http.HandlerFunc(read)), func(o *Options) { o.FallbackMaxBodyBytes = tc.limit })
		rec := serveOnce(e.server.Handler(), "POST", "/upload", strings.Repeat("a", tc.size))
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// On a server with an admin listener of its own, the Fallback answers on the public
// one and the admin one keeps its fixed 404.
func TestFallbackIsOnlyOnThePublicListener(t *testing.T) {
	f := &fallbackSeen{}
	e := newEnv(t, withAuth, withFallback(f), func(o *Options) { o.AdminAddr = "127.0.0.1:1" })
	if rec := serveOnce(e.server.Handler(), "GET", "/.well-known/x", ""); rec.Body.String() != "fallback" {
		t.Errorf("public: %d %q", rec.Code, rec.Body.String())
	}
	if rec := serveOnce(e.server.AdminHandler(), "GET", "/.well-known/x", ""); rec.Code != 404 || strings.Contains(rec.Body.String(), "fallback") {
		t.Errorf("admin: %d %q", rec.Code, rec.Body.String())
	}
	if rec := serveOnce(e.server.Handler(), "GET", "/v1/admin/status", ""); rec.Code != 404 || strings.Contains(rec.Body.String(), "fallback") {
		t.Errorf("admin path on the public listener: %d %q", rec.Code, rec.Body.String())
	}
}

// Serve hands the Fallback requests like any other, and shutdown waits for one that is
// running.
func TestFallbackWorksWithServeAndShutdownWaitsForIt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	e := newEnv(t, withAuth, withFallback(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-release
		}
		_, _ = io.WriteString(w, "served")
	})))
	base := serveOn(t, e)
	if status, body := do2(t, "GET", base+"/.well-known/x", nil, ""); status != 200 || body != "served" {
		t.Fatalf("over a listener: %d %q", status, body)
	}
	if status, _ := do2(t, "GET", base+"/v1/admin/status", nil, ""); status != 401 {
		t.Errorf("a routed path over a listener: %d, want 401", status)
	}

	got := make(chan string, 1)
	go func() {
		_, body := do2(t, "GET", base+"/slow", nil, "")
		got <- body
	}()
	<-started
	shut := make(chan error, 1)
	go func() { shut <- e.server.Shutdown(t.Context()) }()
	select {
	case err := <-shut:
		t.Fatalf("Shutdown returned (%v) while the fallback was answering", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-shut; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if body := <-got; body != "served" {
		t.Errorf("the request in flight got %q", body)
	}
}
