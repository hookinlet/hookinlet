// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var files = fstest.MapFS{
	"index.html":         {Data: []byte("<!doctype html><title>console</title>")},
	"assets/app-1a2b.js": {Data: []byte("console.log(1)")},
}

func get(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(files).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	return rec
}

func TestEveryAnswerCarriesTheSecurityHeaders(t *testing.T) {
	for _, target := range []string{"/admin/", "/admin/assets/app-1a2b.js", "/admin/nothing.js", "/admin/inboxes/x"} {
		rec := get(t, http.MethodGet, target)
		h := rec.Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") || strings.Contains(h.Get("Content-Security-Policy"), "unsafe") {
			t.Errorf("%s: CSP %q", target, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", target, h)
		}
	}
}

func TestAScreenGetsTheIndexAndAMissingFileDoesNot(t *testing.T) {
	for _, target := range []string{"/admin/", "/admin/inboxes", "/admin/inboxes/stripe-prod/messages"} {
		rec := get(t, http.MethodGet, target)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>console</title>") {
			t.Errorf("%s: %d %q", target, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: the index must be revalidated, got %q", target, rec.Header().Get("Cache-Control"))
		}
	}
	if rec := get(t, http.MethodGet, "/admin/assets/"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "app-1a2b.js") {
		t.Errorf("a directory listing: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(t, http.MethodGet, "/admin/assets/gone.js"); rec.Code != http.StatusNotFound {
		t.Errorf("a missing script: %d", rec.Code)
	}
}

func TestHashedAssetsAreCachedForever(t *testing.T) {
	rec := get(t, http.MethodGet, "/admin/assets/app-1a2b.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("%d, cache %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestOnlyReadsAreAllowed(t *testing.T) {
	if rec := get(t, http.MethodPost, "/admin/"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Errorf("POST: %d", rec.Code)
	}
	if rec := get(t, http.MethodHead, "/admin/"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d with %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestPathsCannotEscapeTheFiles(t *testing.T) {
	rec := get(t, http.MethodGet, "/admin/../../etc/passwd")
	if strings.Contains(rec.Body.String(), "root:") {
		t.Fatal("served a file from outside the console")
	}
}

// The files that are embedded in the binary are the real build.
func TestTheEmbeddedBuildHasAnIndexAndScripts(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="app"`) {
		t.Fatalf("the embedded index: %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Error("the index has an inline script, which the content security policy would refuse")
	}
}
