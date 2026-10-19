// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package servertest

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/server"
)

// seen is a Fallback that records the requests it is given.
type seen struct {
	mu    sync.Mutex
	paths []string
}

func (s *seen) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "fallback")
}

func (s *seen) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.paths
	s.paths = nil
	return p
}

func runFallback(t *testing.T, h Harness) {
	// A path the server has no route for goes to the Fallback; without one it gets the
	// fixed 404.
	t.Run("AnswersPathsTheServerHasNoRouteFor", func(t *testing.T) {
		fb := &seen{}
		f := newFixture(t, h, config{mods: func(o *server.Options) { o.Fallback = fb }})
		for _, c := range []struct{ method, path string }{
			{"GET", "/"},
			{"GET", "/.well-known/oauth-protected-resource"},
			{"GET", "/robots.txt"},
			{"POST", "/hooks-ui/save"},
			{"GET", "/v2/things"},
		} {
			status, body := f.do(c.method, c.path, nil, "")
			if got := fb.take(); status != http.StatusOK || body != "fallback" || len(got) != 1 {
				t.Errorf("%s %s: %d %q, the fallback saw %v", c.method, c.path, status, body, got)
			}
		}
	})

	t.Run("WithoutOneTheAnswerIsTheFixed404", func(t *testing.T) {
		f := newFixture(t, h, config{})
		status, body := f.get("/.well-known/x", nil)
		if status != http.StatusNotFound || body != "{\"error\":{\"code\":\"not_found\",\"message\":\"not found\"}}\n" {
			t.Errorf("an unrouted path: %d %q", status, body)
		}
	})

	// A path whose first segment is one the server uses keeps the answer it has without a
	// Fallback, in any state of the server.
	t.Run("IsNeverReachedForTheServersPaths", func(t *testing.T) {
		fb := &seen{}
		with := newFixture(t, h, config{mods: func(o *server.Options) { o.Fallback = fb }})
		without := newFixture(t, h, config{})
		for _, c := range []struct{ method, path string }{
			{"GET", "/healthz"}, {"POST", "/healthz"}, {"GET", "/healthz/"}, {"GET", "/readyz"}, {"GET", "/metrics"},
			{"POST", "/in/some-key"}, {"GET", "/in/some-key"}, {"POST", "/in/some-key/sub/path"}, {"POST", "/in"}, {"POST", "/in/"},
			{"GET", "/v1/inboxes/x"}, {"GET", "/v1/inboxes"}, {"GET", "/v1/anything"}, {"GET", "/v1"}, {"GET", "/v1/"},
			{"GET", "/mcp/"}, {"GET", "/v1/admin/nothing"}, {"GET", "/admin"}, {"GET", "/admin/"}, {"POST", "/admin/x"},
		} {
			gotStatus, _ := with.do(c.method, c.path, nil, "x")
			wantStatus, _ := without.do(c.method, c.path, nil, "x")
			if p := fb.take(); len(p) != 0 {
				t.Errorf("%s %s reached the fallback: %v", c.method, c.path, p)
			}
			if gotStatus != wantStatus {
				t.Errorf("%s %s: status %d with a fallback, %d without", c.method, c.path, gotStatus, wantStatus)
			}
		}
	})

	// A request the Fallback answers is read with a body limit and runs inside the
	// server's middleware.
	t.Run("BodyIsLimited", func(t *testing.T) {
		read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		})
		for _, c := range []struct {
			name        string
			limit       int64
			size, wants int
		}{
			{"default, under", 0, 1<<20 - 1, 200},
			{"default, over", 0, 1<<20 + 1, 413},
			{"set, over", 10, 11, 413},
			{"set, exactly", 10, 10, 200},
			{"off", -1, 3 << 20, 200},
		} {
			f := newFixture(t, h, config{mods: func(o *server.Options) { o.Fallback = read; o.FallbackMaxBodyBytes = c.limit }})
			if status, _ := f.do(http.MethodPost, "/upload", nil, strings.Repeat("a", c.size)); status != c.wants {
				t.Errorf("%s: status %d, want %d", c.name, status, c.wants)
			}
		}
	})

	t.Run("RunsInsideTheMiddleware", func(t *testing.T) {
		f := newFixture(t, h, config{mods: func(o *server.Options) {
			o.Fallback = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/boom" {
					panic("fallback exploded")
				}
				_, _ = io.WriteString(w, "ok")
			})
		}})
		resp, err := http.Get(f.base + "/page") //nolint:noctx // a plain request to a test server
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.Header.Get("X-Request-Id") == "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers of a fallback answer: %v; want the server's", resp.Header)
		}
		if status, body := f.get("/boom", nil); status != http.StatusInternalServerError || !strings.Contains(body, `"internal"`) {
			t.Errorf("a panic in the fallback: %d %s; want the server's 500", status, body)
		}
	})
}
