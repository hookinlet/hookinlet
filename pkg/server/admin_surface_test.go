// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func adminAuth() http.Header { return http.Header{"Authorization": {"Bearer " + testAdminToken}} }

// With its own listener the admin API is on that one and not the public one.
func TestTheAdminAPIOnItsOwnListenerIsNotOnThePublicOne(t *testing.T) {
	e := newEnv(t, withAuth, func(o *Options) {
		o.AdminAddr = "127.0.0.1:1" // never dialled: ServeAdmin is given its own listener
	})
	e.newInbox("surface", "trusted", "")

	var lc net.ListenConfig
	pub, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adm, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.server.ServeAdmin(ctx, pub, adm) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ServeAdmin returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ServeAdmin did not return after shutdown")
		}
	})
	pubURL, admURL := "http://"+pub.Addr().String(), "http://"+adm.Addr().String()

	if status, body := do2(t, "GET", admURL+"/v1/admin/inboxes", adminAuth(), ""); status != 200 || !strings.Contains(body, "surface") {
		t.Errorf("admin listener: %d %s", status, body)
	}
	if status, _ := do2(t, "GET", pubURL+"/v1/admin/inboxes", adminAuth(), ""); status != 404 {
		t.Errorf("the public listener serves the admin API: %d", status)
	}
	if status, _ := do2(t, "POST", pubURL+"/v1/admin/tokens", adminAuth(), `{"name":"x","all_inboxes":true}`); status != 404 {
		t.Errorf("the public listener creates tokens: %d", status)
	}
	// And the admin listener serves nothing else.
	for _, path := range []string{"/healthz", "/v1/inboxes/surface", "/mcp"} {
		if status, _ := do2(t, "GET", admURL+path, adminAuth(), ""); status != 404 {
			t.Errorf("admin listener %s: %d, want 404", path, status)
		}
	}
	if status, _ := do2(t, "POST", admURL+"/in/surface", nil, "x"); status != 404 {
		t.Errorf("ingest on the admin listener: %d", status)
	}
	if status, _ := do2(t, "POST", pubURL+"/in/surface", nil, "x"); status != 200 {
		t.Errorf("ingest on the public listener: %d", status)
	}
	if status, _ := do2(t, "GET", pubURL+"/healthz", nil, ""); status != 200 {
		t.Errorf("healthz on the public listener: %d", status)
	}
}

func TestADisabledAdminAPIIsNotServed(t *testing.T) {
	e := newEnv(t, withAuth, func(o *Options) { o.DisableAdmin = true })
	e.newInbox("noadmin", "trusted", "")
	base := serveOn(t, e)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/admin/inboxes"}, {"POST", "/v1/admin/inboxes"}, {"GET", "/v1/admin/tokens"}, {"GET", "/v1/admin/backup"},
	} {
		if status, _ := do2(t, tc.method, base+tc.path, adminAuth(), ""); status != 404 {
			t.Errorf("%s %s: %d, want 404", tc.method, tc.path, status)
		}
	}
	if status, _ := do2(t, "POST", base+"/in/noadmin", nil, "x"); status != 200 {
		t.Errorf("ingest with the admin API off: %d", status)
	}
}

func TestContradictoryAdminOptionsAreRefused(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"disabled and on its own address": func(o *Options) { o.DisableAdmin, o.AdminAddr = true, "127.0.0.1:9" },
		"the same address as the server":  func(o *Options) { o.AdminAddr = o.Addr },
	} {
		store := newEnv(t).store
		opts := Options{Addr: "127.0.0.1:0", Store: store}
		mod(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// What an admin does is logged with where the request came from.
func TestAdminActionsAreLoggedWithTheCallersAddress(t *testing.T) {
	a := newAdminEnv(t)
	a.mustAdmin("POST", "/v1/admin/tokens", map[string]any{"name": "logged", "all_inboxes": true}, 201)
	logs := a.logs.String()
	var line string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, `"msg":"admin"`) && strings.Contains(l, "create token") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the token creation was not logged:\n%s", logs)
	}
	if !strings.Contains(line, `"client_ip":"127.0.0.1"`) {
		t.Errorf("no client address in %s", line)
	}
	if strings.Contains(logs, `"slug":""`) {
		t.Error("an empty slug was logged")
	}
}

// Serve has no second listener to give the admin API, so it refuses a server that
// has one configured instead of quietly serving admin on the public address.
func TestServeRefusesAServerWithAnAdminAddress(t *testing.T) {
	e := newEnv(t, withAuth, func(o *Options) { o.AdminAddr = "127.0.0.1:1" })
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := e.server.Serve(t.Context(), l); err == nil || !strings.Contains(err.Error(), "ServeAdmin") {
		t.Errorf("Serve with AdminAddr set: %v; want an error pointing at ServeAdmin", err)
	}
	if err := e.server.Serve(t.Context(), nil); err == nil {
		t.Error("Serve accepted a nil listener")
	}
	if err := e.server.ServeAdmin(t.Context(), nil, nil); err == nil {
		t.Error("ServeAdmin accepted nil listeners")
	}
}
