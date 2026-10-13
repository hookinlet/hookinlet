// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// One credential cannot use every stream slot: a token that can read several inboxes is held to
// its own share, and other credentials (and the admin token) still get streams.
func TestOneCredentialCannotHoldEveryStreamSlot(t *testing.T) {
	a := newAdminEnv(t, func(o *Options) {
		o.MaxStreams, o.MaxStreamsPerInbox, o.MaxStreamsPerCredential = 10, 10, 2
	})
	a.newInbox("agent-b", "trusted", "")
	made := unjson[struct {
		Token string `json:"token"`
	}](t, a.mustAdmin("POST", "/v1/admin/tokens", map[string]any{"name": "all", "all_inboxes": true}, http.StatusCreated))
	wide := made.Token

	first := openStream(t, a.base+"/v1/inboxes/agent-a/stream", wide, nil)
	second := openStream(t, a.base+"/v1/inboxes/agent-b/stream", wide, nil)
	third := openStream(t, a.base+"/v1/inboxes/agent-a/stream", wide, nil)
	if first.status != 200 || second.status != 200 || third.status != http.StatusServiceUnavailable {
		t.Fatalf("one token's streams: %d %d %d, want 200 200 503", first.status, second.status, third.status)
	}
	if other := openStream(t, a.base+"/v1/inboxes/agent-a/stream", a.tokenA, nil); other.status != 200 {
		t.Errorf("another credential's stream while the first holds its share: %d", other.status)
	}
	for i := range 3 { // the administrator is not held to a credential's share
		if adm := openStream(t, a.base+"/v1/inboxes/agent-a/stream", testAdminToken, nil); adm.status != 200 {
			t.Errorf("the admin token's stream %d: %d", i, adm.status)
		}
	}
	first.close() // a slot comes back
	deadline := time.Now().Add(3 * time.Second)
	status := 0
	for time.Now().Before(deadline) {
		c := openStream(t, a.base+"/v1/inboxes/agent-a/stream", wide, nil)
		if status = c.status; status == 200 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != 200 {
		t.Errorf("after one of its streams closed: %d, want 200", status)
	}
}

// One credential cannot hold every read slot by not reading its answers: its second request waits
// for its own share and is turned away, while other credentials and the admin token are served.
func TestOneCredentialCannotHoldEveryReadSlot(t *testing.T) {
	e := newAgentEnv2(t, func(o *Options) {
		o.MaxConcurrentReads, o.MaxReadsPerCredential = 4, 1
		o.ReadWaitTimeout = 200 * time.Millisecond
		o.ResponseTimeout = 3 * time.Second
	})
	e.tokenA = e.tokenFor(e.a)
	b := e.newInbox("agent-b", "trusted", "")
	tokenB := e.tokenFor(b)
	base := serveOn(t, e.env)
	big := strings.Repeat("x", 400<<10)
	for range 24 {
		e.post("/in/agent-a", big)
	}

	dialer := net.Dialer{Control: func(_, _ string, c rawConn) error { return smallReceiveBuffer(c) }}
	conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET /v1/inboxes/agent-a/messages?after=0&limit=100 HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", e.tokenA); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the server is stuck writing the page

	hdr := func(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }
	if status, _ := do2(t, "GET", base+"/v1/inboxes/agent-a", hdr(e.tokenA), ""); status != http.StatusServiceUnavailable {
		t.Errorf("the same credential's second read while its first is not being read: %d, want 503", status)
	}
	if status, _ := do2(t, "GET", base+"/v1/inboxes/agent-b", hdr(tokenB), ""); status != http.StatusOK {
		t.Errorf("another credential's read: %d, want 200", status)
	}
	if status, _ := do2(t, "GET", base+"/v1/inboxes/agent-a", hdr(testAdminToken), ""); status != http.StatusOK {
		t.Errorf("the admin token's read: %d, want 200", status)
	}
}

// An inbox's key holder can send slow bodies, but one inbox cannot fill the memory budget for
// deliveries and turn every other inbox's senders away.
func TestOneInboxCannotUseUpTheIngestMemoryBudget(t *testing.T) {
	e := newAgentEnv2(t, func(o *Options) { o.IngestMemoryBudget = 16 << 20 })
	e.newInbox("other", "trusted", "")
	base := serveOn(t, e.env)
	addr := strings.TrimPrefix(base, "http://")

	// Thirty senders to one inbox each send most of a 1 MiB body and then stop: together far
	// more than the whole budget, if one inbox could hold it all.
	part := strings.Repeat("A", 900<<10)
	var conns []net.Conn
	for range 30 {
		c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		if _, err := fmt.Fprintf(c, "POST /in/agent-a HTTP/1.1\r\nHost: x\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", 1<<20, part); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	time.Sleep(700 * time.Millisecond) // the server has read what was sent

	e.server.ingestBudget.mu.Lock()
	used := e.server.ingestBudget.used
	e.server.ingestBudget.mu.Unlock()
	if used > 3<<20 {
		t.Errorf("one inbox's stalled deliveries hold %d bytes of a %d byte budget; its share is 2 MiB", used, 16<<20)
	}
	if status, body := do2(t, "POST", base+"/in/other", nil, "hello"); status != http.StatusOK {
		t.Errorf("a delivery to another inbox while the first is stalled: %d %s", status, body)
	}
}
