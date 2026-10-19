// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// seeingAuth wraps the shipped authenticator and records the AccessInfo that the
// request's context carries when Authenticate is called.
type seeingAuth struct {
	inner inbox.Authenticator
	mu    sync.Mutex
	seen  []seenAccess
}

type seenAccess struct {
	info inbox.AccessInfo
	ok   bool
}

func (a *seeingAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	info, ok := inbox.AccessFrom(r.Context())
	a.mu.Lock()
	a.seen = append(a.seen, seenAccess{info, ok})
	a.mu.Unlock()
	return a.inner.Authenticate(r)
}

func (a *seeingAuth) take() []seenAccess {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.seen
	a.seen = nil
	return s
}

func withSeeingAuth(a *seeingAuth, mods ...func(*Options)) func(*Options) {
	return func(o *Options) {
		a.inner = token.NewAuthenticator(o.Store, testAdminToken)
		o.Authenticator = a
		for _, m := range mods {
			m(o)
		}
	}
}

func TestAuthenticatorSeesTheClientAddress(t *testing.T) {
	proxies := func(o *Options) { o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} }
	for _, c := range []struct {
		name    string
		mods    []func(*Options)
		remote  string
		headers http.Header
		want    string
	}{
		{"no trusted proxies", nil, "198.51.100.9:4000", nil, "198.51.100.9"},
		{"a spoofed X-Forwarded-For from an untrusted peer is ignored", []func(*Options){proxies}, "198.51.100.9:4000",
			http.Header{"X-Forwarded-For": {"192.0.2.1"}}, "198.51.100.9"},
		{"X-Forwarded-For with no trusted proxies is ignored", nil, "198.51.100.9:4000",
			http.Header{"X-Forwarded-For": {"192.0.2.1"}}, "198.51.100.9"},
		{"a trusted proxy's client", []func(*Options){proxies}, "10.0.0.1:4000",
			http.Header{"X-Forwarded-For": {"192.0.2.1"}}, "192.0.2.1"},
		{"an address it cannot read is empty", nil, "not-an-address", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &seeingAuth{}
			ae := &agentEnv{env: newEnv(t, append([]func(*Options){withSeeingAuth(a)}, c.mods...)...)}
			in := ae.newInbox("agent-a", "trusted", "")
			tok, hash, err := token.Generate()
			if err != nil {
				t.Fatal(err)
			}
			if err := ae.store.CreateToken(t.Context(), &inbox.Token{Name: "t", Hash: hash, InboxIDs: []inbox.ID{in.ID}}); err != nil {
				t.Fatal(err)
			}
			h := http.Header{"Authorization": {"Bearer " + tok}}
			for k, v := range c.headers {
				h[k] = v
			}
			rec := ae.do(req{method: "GET", path: "/v1/inboxes/agent-a/messages", headers: h, remote: c.remote})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			seen := a.take()
			if len(seen) != 1 || !seen[0].ok {
				t.Fatalf("Authenticate saw %+v, want one call with an AccessInfo", seen)
			}
			if got := seen[0].info; got.ClientIP != c.want || got.Operation != "listMessages" || got.RequestID != rec.Header().Get("X-Request-Id") {
				t.Errorf("Authenticate saw %+v, want ClientIP %q, operation listMessages, request id %q", got, c.want, rec.Header().Get("X-Request-Id"))
			}
		})
	}
}

func TestAuthenticatorSeesAccessOnEveryEntryPoint(t *testing.T) {
	a := &seeingAuth{}
	ae, base, _ := mcpEnv(t, withSeeingAuth(a, func(o *Options) { o.StreamRevalidateInterval = 50 * time.Millisecond }))
	ae.post("/in/agent-a", "x")
	a.take()

	check := func(t *testing.T, what, op, id string, seen []seenAccess) {
		t.Helper()
		if len(seen) == 0 {
			t.Fatalf("%s: Authenticate was not called", what)
		}
		for _, s := range seen {
			if !s.ok || s.info.Operation != op || s.info.RequestID == "" || s.info.ClientIP != "127.0.0.1" {
				t.Errorf("%s: Authenticate saw %+v (%v), want operation %q, a request id and 127.0.0.1", what, s.info, s.ok, op)
			}
			if id != "" && s.info.RequestID != id {
				t.Errorf("%s: request id %q, the response says %q", what, s.info.RequestID, id)
			}
		}
	}

	t.Run("read API", func(t *testing.T) {
		status, hdr := ae.over(base, "GET", "/v1/inboxes/agent-a")
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		check(t, "getInbox", "getInbox", hdr.Get("X-Request-Id"), a.take())
	})

	t.Run("stream, at the start and at each recheck", func(t *testing.T) {
		c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
		if c.status != http.StatusOK {
			t.Fatalf("status %d", c.status)
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(a.peek()) < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		c.close()
		<-c.done
		seen := a.take()
		if len(seen) < 2 {
			t.Fatalf("stream: Authenticate called %d times, want the first check and a recheck", len(seen))
		}
		check(t, "stream", "streamMessages", c.header.Get("X-Request-Id"), seen)
	})

	t.Run("MCP", func(t *testing.T) {
		cs := connectMCP(t, base, ae.tokenA)
		a.take()
		if o := callTool(t, cs, "list_inboxes", map[string]any{}); o.err != "" {
			t.Fatal(o.err)
		}
		// The credential is checked once for the HTTP request, before the tool is known.
		check(t, "mcp", "mcp", "", a.take())
		// A wait rechecks the credential for the tool call it serves.
		if o := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 1, "timeout_seconds": 1}); o.err != "" {
			t.Fatal(o.err)
		}
		seen := a.take()
		if len(seen) < 2 {
			t.Fatalf("wait: Authenticate called %d times, want the request's check and a recheck", len(seen))
		}
		check(t, "wait", "mcp", "", seen[:1])
		for _, s := range seen[1:] {
			if s.info.Operation != "mcp.wait_for_messages" || s.info.ClientIP != "127.0.0.1" {
				t.Errorf("recheck saw %+v, want operation mcp.wait_for_messages and 127.0.0.1", s.info)
			}
		}
	})

	t.Run("admin API", func(t *testing.T) {
		r, err := http.NewRequestWithContext(t.Context(), "GET", base+"/v1/admin/inboxes", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+testAdminToken)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		check(t, "listInboxes", "listInboxes", resp.Header.Get("X-Request-Id"), a.take())
	})

	t.Run("a request that is refused", func(t *testing.T) {
		r, err := http.NewRequestWithContext(t.Context(), "GET", base+"/v1/inboxes/agent-a", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", resp.StatusCode)
		}
		check(t, "refused", "getInbox", resp.Header.Get("X-Request-Id"), a.take())
	})
}

func (a *seeingAuth) peek() []seenAccess {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]seenAccess(nil), a.seen...)
}
