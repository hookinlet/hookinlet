// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
)

type admitCall struct {
	kind inbox.AccessKind
	slug string // empty when the inbox is nil
	p    inbox.Principal
}

// admitHook is Hooks that also implements inbox.Admitter. It records what it is asked
// and how many admitted accesses are still open.
type admitHook struct {
	inbox.NopHooks
	mu       sync.Mutex
	calls    []admitCall
	open     int
	err      error // what Admit returns, if set
	noRetire bool  // return a nil release
}

func (h *admitHook) Admit(_ context.Context, p inbox.Principal, in *inbox.Inbox, kind inbox.AccessKind) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := admitCall{kind: kind, p: p}
	if in != nil {
		c.slug = in.Slug
	}
	h.calls = append(h.calls, c)
	if h.err != nil {
		return nil, h.err
	}
	if h.noRetire {
		return nil, nil
	}
	h.open++
	var once sync.Once
	return func() { once.Do(func() { h.mu.Lock(); h.open--; h.mu.Unlock() }) }, nil
}

func (h *admitHook) setErr(err error) { h.mu.Lock(); h.err = err; h.mu.Unlock() }
func (h *admitHook) reset()           { h.mu.Lock(); h.calls = nil; h.mu.Unlock() }

func (h *admitHook) snapshot() ([]admitCall, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]admitCall(nil), h.calls...), h.open
}

func (h *admitHook) waitOpen(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, open := h.snapshot(); open == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, open := h.snapshot()
	t.Fatalf("%d admitted accesses open, want %d", open, want)
}

func (h *admitHook) only(t *testing.T, kind inbox.AccessKind, slug string) {
	t.Helper()
	calls, _ := h.snapshot()
	if len(calls) != 1 || calls[0].kind != kind || calls[0].slug != slug || calls[0].p == nil {
		t.Fatalf("Admit calls = %+v, want one %s on %q with a principal", calls, kind, slug)
	}
}

func withAdmit(h *admitHook) func(*Options) { return func(o *Options) { o.Hooks = h } }

var refusals = []struct {
	err    error
	status int
	code   string
}{
	{inbox.ErrRateLimited, http.StatusTooManyRequests, "rate_limited"},
	{inbox.ErrQuotaExceeded, http.StatusPaymentRequired, "quota_exceeded"},
	{inbox.ErrForbidden, http.StatusForbidden, "forbidden"},
}

func TestAdmitReadIsAskedAndReleased(t *testing.T) {
	h := &admitHook{}
	ae, _, _ := mcpEnv(t, withAdmit(h))
	h.reset()
	if rec := ae.get("/v1/inboxes/agent-a/messages", ae.tokenA); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	h.only(t, inbox.AccessRead, "agent-a")
	h.waitOpen(t, 0)

	// Anonymous and unauthorized requests are not asked.
	h.reset()
	ae.get("/v1/inboxes/agent-a/messages", "")
	ae.get("/v1/inboxes/agent-a/messages", ae.tokenB)
	if calls, _ := h.snapshot(); len(calls) != 0 {
		t.Errorf("Admit was asked about requests that were refused earlier: %+v", calls)
	}
}

func TestAdmitReadRefusalsBecomeTheirStatus(t *testing.T) {
	for _, r := range refusals {
		t.Run(r.code, func(t *testing.T) {
			h := &admitHook{err: r.err}
			ae, _, _ := mcpEnv(t, withAdmit(h))
			rec := ae.get("/v1/inboxes/agent-a/messages", ae.tokenA)
			if rec.Code != r.status || !strings.Contains(rec.Body.String(), r.code) {
				t.Errorf("status %d, body %s; want %d %s", rec.Code, rec.Body.String(), r.status, r.code)
			}
			// A refused read took no slot of the server's.
			if len(ae.server.reads) != 0 {
				t.Errorf("%d read slots held after a refusal", len(ae.server.reads))
			}
		})
	}
}

func TestAdmitFailureIsAnInternalError(t *testing.T) {
	h := &admitHook{err: errors.New("the limits database is down")}
	ae, _, _ := mcpEnv(t, withAdmit(h))
	rec := ae.get("/v1/inboxes/agent-a/messages", ae.tokenA)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database") {
		t.Errorf("status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestAdmitMayReturnANilRelease(t *testing.T) {
	h := &admitHook{noRetire: true}
	ae, base, _ := mcpEnv(t, withAdmit(h))
	if rec := ae.get("/v1/inboxes/agent-a/messages", ae.tokenA); rec.Code != http.StatusOK {
		t.Errorf("read: status %d", rec.Code)
	}
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	if c.status != http.StatusOK {
		t.Errorf("stream: status %d", c.status)
	}
	c.close()
}

func TestAdmitStreamIsHeldUntilTheStreamEnds(t *testing.T) {
	h := &admitHook{}
	ae, base, _ := streamEnv(t, withAdmit(h))
	h.reset()
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	if c.status != http.StatusOK {
		t.Fatalf("status %d", c.status)
	}
	h.only(t, inbox.AccessStream, "agent-a")
	h.waitOpen(t, 1)
	c.close()
	<-c.done
	h.waitOpen(t, 0)
}

func TestAdmitStreamRefusalsBecomeTheirStatus(t *testing.T) {
	for _, r := range refusals {
		t.Run(r.code, func(t *testing.T) {
			h := &admitHook{err: r.err}
			ae, base, n := streamEnv(t, withAdmit(h), func(o *Options) { o.MaxStreams = 1; o.MaxStreamsPerInbox = 1 })
			c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
			if c.status != r.status {
				t.Fatalf("status %d, want %d", c.status, r.status)
			}
			if n.Subscribers() != 0 {
				t.Errorf("%d subscriptions after a refusal", n.Subscribers())
			}
			// The refusal took none of the server's own stream slots.
			h.setErr(nil)
			c = openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
			if c.status != http.StatusOK {
				t.Errorf("after admission was allowed: status %d", c.status)
			}
			c.close()
		})
	}
}

func TestServerLimitsStillApplyAfterAdmission(t *testing.T) {
	h := &admitHook{}
	ae, base, _ := streamEnv(t, withAdmit(h), func(o *Options) { o.MaxStreams = 1; o.MaxStreamsPerInbox = 1 })
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	defer c.close()
	if c.status != http.StatusOK {
		t.Fatalf("status %d", c.status)
	}
	h.waitOpen(t, 1)
	if second := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil); second.status != http.StatusServiceUnavailable {
		t.Errorf("second stream: status %d", second.status)
	}
	// The stream the server turned away was admitted and then released.
	h.waitOpen(t, 1)
}

func TestAdmitMCPKinds(t *testing.T) {
	h := &admitHook{}
	ae, base, _ := mcpEnv(t, withAdmit(h))
	ae.post("/in/agent-a", `{"n":1}`, "Content-Type", "application/json")
	cs := connectMCP(t, base, ae.tokenA)

	cases := []struct {
		name string
		tool string
		args map[string]any
		kind inbox.AccessKind
		slug string
	}{
		{"list", "list_inboxes", map[string]any{}, inbox.AccessRead, ""},
		{"query", "query_messages", map[string]any{"inbox": "agent-a"}, inbox.AccessRead, "agent-a"},
		{"get", "get_message", map[string]any{"inbox": "agent-a", "seq": 1}, inbox.AccessRead, "agent-a"},
		{"wait of zero", "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 0}, inbox.AccessRead, "agent-a"},
		{"wait", "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 1, "timeout_seconds": 1}, inbox.AccessWait, "agent-a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.reset()
			if o := callTool(t, cs, c.tool, c.args); o.err != "" {
				t.Fatal(o.err)
			}
			h.only(t, c.kind, c.slug)
			h.waitOpen(t, 0)
		})
	}
}

func TestAdmitMCPWaitIsHeldWhileWaiting(t *testing.T) {
	h := &admitHook{}
	ae, base, _ := mcpEnv(t, withAdmit(h))
	cs := connectMCP(t, base, ae.tokenA)
	done := make(chan toolOut, 1)
	go func() {
		done <- callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 20})
	}()
	h.waitOpen(t, 1)
	ae.post("/in/agent-a", `{"n":1}`, "Content-Type", "application/json")
	if o := <-done; o.err != "" {
		t.Fatal(o.err)
	}
	h.waitOpen(t, 0)
}

func TestAdmitMCPRefusals(t *testing.T) {
	for _, r := range refusals {
		t.Run(r.code, func(t *testing.T) {
			h := &admitHook{err: r.err}
			ae, base, _ := mcpEnv(t, withAdmit(h))
			cs := connectMCP(t, base, ae.tokenA)
			for tool, args := range map[string]map[string]any{
				"list_inboxes":      {},
				"query_messages":    {"inbox": "agent-a"},
				"get_message":       {"inbox": "agent-a", "seq": 1},
				"wait_for_messages": {"inbox": "agent-a", "after": 0, "timeout_seconds": 5},
			} {
				o := callTool(t, cs, tool, args)
				if o.err == "" || o.err == "internal error" {
					t.Errorf("%s: err = %q, want a refusal the agent can read", tool, o.err)
				}
			}
			h.waitOpen(t, 0)
		})
	}
}

func TestHooksWithoutAnAdmitterAdmitEverything(t *testing.T) {
	ae, base, _ := mcpEnv(t) // the default hooks
	if rec := ae.get("/v1/inboxes/agent-a/messages", ae.tokenA); rec.Code != http.StatusOK {
		t.Errorf("read: status %d", rec.Code)
	}
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	defer c.close()
	if c.status != http.StatusOK {
		t.Errorf("stream: status %d", c.status)
	}
}

// capAdmitter is a small reference Admitter: at most max accesses in flight, per kind.
type capAdmitter struct {
	inbox.NopHooks
	mu   sync.Mutex
	max  int
	held map[inbox.AccessKind]int
}

func (c *capAdmitter) Admit(_ context.Context, _ inbox.Principal, _ *inbox.Inbox, kind inbox.AccessKind) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held == nil {
		c.held = map[inbox.AccessKind]int{}
	}
	if c.held[kind] >= c.max {
		return nil, fmt.Errorf("%d %ss at once: %w", c.max, kind, inbox.ErrQuotaExceeded)
	}
	c.held[kind]++
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.held[kind]--; c.mu.Unlock() }) }, nil
}

func TestAdmitterSuiteAcceptsAReferenceAdmitter(t *testing.T) {
	inboxtest.AdmitterSuite(t, func(*testing.T) inbox.Admitter { return &capAdmitter{max: 3} })
}
