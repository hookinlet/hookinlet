// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	var asked atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Error("the probe sent a credential")
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	if code, out, e := runCmd(t, nil, "healthcheck", "--url", srv.URL); code != 0 || !strings.Contains(out, "/readyz") {
		t.Fatalf("a ready server: %d %q %q", code, out, e)
	}
	if asked.Load() != "/readyz" {
		t.Errorf("asked %v, want /readyz", asked.Load())
	}
	if code, _, _ := runCmd(t, nil, "healthcheck", "--url", srv.URL, "--live"); code != 0 || asked.Load() != "/healthz" {
		t.Errorf("--live: %d, asked %v", code, asked.Load())
	}
	status.Store(http.StatusServiceUnavailable)
	if code, _, e := runCmd(t, nil, "healthcheck", "--url", srv.URL); code != exitRuntime || !strings.Contains(e, "503") {
		t.Errorf("a server that is not ready: %d %q", code, e)
	}
	status.Store(http.StatusFound)
	if code, _, _ := runCmd(t, nil, "healthcheck", "--url", srv.URL); code != exitRuntime {
		t.Errorf("a redirect is not health: %d", code)
	}
	srv.Close()
	if code, _, _ := runCmd(t, nil, "healthcheck", "--url", srv.URL, "--timeout", "500ms"); code != exitRuntime {
		t.Errorf("a server that is not there: %d", code)
	}
	if code, _, _ := runCmd(t, nil, "healthcheck", "--url", "ftp://x"); code != exitUsage {
		t.Errorf("a bad address: %d", code)
	}
}

func TestHealthcheckFindsTheServerFromItsListenAddress(t *testing.T) {
	for listen, want := range map[string]string{
		"":               "http://127.0.0.1:8080",
		":9000":          "http://127.0.0.1:9000",
		"0.0.0.0:9000":   "http://127.0.0.1:9000",
		"[::]:9000":      "http://127.0.0.1:9000",
		"127.0.0.1:7":    "http://127.0.0.1:7",
		"10.1.2.3:8081":  "http://10.1.2.3:8081",
		"not an address": "http://127.0.0.1:8080",
	} {
		if got := localURL(listen); got != want {
			t.Errorf("localURL(%q) = %q, want %q", listen, got, want)
		}
	}
}
