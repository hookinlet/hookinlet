// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// seriesNames lists the metric families in an exposition, which do not change between
// scrapes, unlike some of their values.
func seriesNames(body string) []string {
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^# TYPE (\S+) `).FindAllStringSubmatch(body, -1) {
		names = append(names, m[1])
	}
	return names
}

// An embedder serves the metrics where it chooses, without the admin API, and the
// handler needs no credential: where it is mounted is the embedder's to protect.
func TestMetricsHandlerServesTheAdminEndpointsSeries(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "one")
	e.post("/in/agent-a", "two")

	rec := httptest.NewRecorder()
	e.server.MetricsHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the metrics may be cached")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hookinlet_deliveries_total{result="stored"} 2`) {
		t.Errorf("the two deliveries are not counted:\n%s", body)
	}

	admin := e.get("/metrics", testAdminToken)
	if admin.Code != http.StatusOK {
		t.Fatalf("/metrics: %d", admin.Code)
	}
	want, got := strings.Join(seriesNames(admin.Body.String()), ","), strings.Join(seriesNames(body), ",")
	if want != got {
		t.Errorf("the handler serves different series from /metrics:\n got  %s\n want %s", got, want)
	}
}

func TestMetricsHandlerAnswersHeadAndRefusesOtherMethods(t *testing.T) {
	e := newAgentEnv(t)
	h := e.server.MetricsHandler()

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD: %d with a body of %d bytes", head.Code, head.Body.Len())
	}
	if head.Header().Get("Content-Length") == "" || head.Header().Get("Content-Type") != get.Header().Get("Content-Type") {
		t.Errorf("HEAD headers %v differ from GET's %v", head.Header(), get.Header())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/", strings.NewReader("x")))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d, Allow %q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
}
