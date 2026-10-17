// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// accessCall is one hook call and the AccessInfo its context carried.
type accessCall struct {
	hook string
	info inbox.AccessInfo
	ok   bool
}

// accessHook records the AccessInfo of every hook call. admitErr and beforeErr are
// what Admit and BeforeIngest return.
type accessHook struct {
	inbox.NopHooks
	mu        sync.Mutex
	calls     []accessCall
	admitErr  error
	beforeErr error
}

func (h *accessHook) record(ctx context.Context, hook string) {
	info, ok := inbox.AccessFrom(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, accessCall{hook, info, ok})
}

func (h *accessHook) Admit(ctx context.Context, _ inbox.Principal, _ *inbox.Inbox, _ inbox.AccessKind) (func(), error) {
	h.record(ctx, "Admit")
	h.mu.Lock()
	defer h.mu.Unlock()
	return nil, h.admitErr
}

func (h *accessHook) BeforeIngest(ctx context.Context, _ *inbox.IngestRequest) error {
	h.record(ctx, "BeforeIngest")
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.beforeErr
}

func (h *accessHook) AfterIngest(ctx context.Context, _ *inbox.Inbox, _ *inbox.Message, _ bool) {
	h.record(ctx, "AfterIngest")
}

func (h *accessHook) ObserveIngest(ctx context.Context, _ *inbox.IngestEvent) {
	h.record(ctx, "ObserveIngest")
}

func (h *accessHook) MaxUnverified(ctx context.Context, _ *inbox.Inbox) int {
	h.record(ctx, "MaxUnverified")
	return 0
}

func (h *accessHook) take() []accessCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.calls
	h.calls = nil
	return c
}

func (h *accessHook) set(admit, before error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.admitErr, h.beforeErr = admit, before
}

// accessLogged reports whether the access log has a line for a request with this id
// on this route. A stream's line is written when it ends, so it is waited for.
func accessLogged(t *testing.T, logs *syncBuffer, id, route string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, line := range strings.Split(logs.String(), "\n") {
			var l struct {
				Msg       string `json:"msg"`
				RequestID string `json:"request_id"`
				Route     string `json:"route"`
			}
			if json.Unmarshal([]byte(line), &l) != nil {
				continue
			}
			if l.Msg == "request" && l.RequestID == id && l.Route == route {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkCalls fails unless calls are exactly the hooks named, each with op and a request
// id the access log has for route (and, if id is set, equal to it).
func checkCalls(t *testing.T, logs *syncBuffer, calls []accessCall, hooks []string, op, id, route string) {
	t.Helper()
	var names []string
	for _, c := range calls {
		names = append(names, c.hook)
	}
	if strings.Join(names, ",") != strings.Join(hooks, ",") {
		t.Fatalf("%s: hook calls %+v, want %v", op, calls, hooks)
	}
	for _, c := range calls {
		if !c.ok || c.info.Operation != op || c.info.RequestID == "" {
			t.Errorf("%s: %s saw %+v (%v), want operation %q and a request id", op, c.hook, c.info, c.ok, op)
			continue
		}
		if id != "" && c.info.RequestID != id {
			t.Errorf("%s: %s saw request id %q, the response says %q", op, c.hook, c.info.RequestID, id)
		}
		if !accessLogged(t, logs, c.info.RequestID, route) {
			t.Errorf("%s: %s saw request id %q, which no access log line for %s has", op, c.hook, c.info.RequestID, route)
		}
	}
}

func TestHooksSeeTheOperationAndRequestID(t *testing.T) {
	h := &accessHook{}
	ae, base, _ := mcpEnv(t, func(o *Options) { o.Hooks = h })
	ae.post("/in/agent-a", "x")
	h.take()

	t.Run("REST", func(t *testing.T) {
		for _, c := range []struct{ method, path, op, route string }{
			{"GET", "/v1/inboxes/agent-a", "getInbox", "GET /v1/inboxes/{slug}"},
			{"HEAD", "/v1/inboxes/agent-a", "headGetInbox", "HEAD /v1/inboxes/{slug}"},
			{"GET", "/v1/inboxes/agent-a/messages", "listMessages", "GET /v1/inboxes/{slug}/messages"},
			{"HEAD", "/v1/inboxes/agent-a/messages", "headListMessages", "HEAD /v1/inboxes/{slug}/messages"},
			{"GET", "/v1/inboxes/agent-a/messages/1", "getMessage", "GET /v1/inboxes/{slug}/messages/{seq}"},
			{"HEAD", "/v1/inboxes/agent-a/messages/1", "headGetMessage", "HEAD /v1/inboxes/{slug}/messages/{seq}"},
			{"GET", "/v1/inboxes/agent-a/messages/1/raw", "getMessageRaw", "GET /v1/inboxes/{slug}/messages/{seq}/raw"},
			{"HEAD", "/v1/inboxes/agent-a/messages/1/raw", "headGetMessageRaw", "HEAD /v1/inboxes/{slug}/messages/{seq}/raw"},
		} {
			rec := ae.do(req{method: c.method, path: c.path, headers: http.Header{"Authorization": {"Bearer " + ae.tokenA}}})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: status %d", c.method, c.path, rec.Code)
			}
			checkCalls(t, ae.logs, h.take(), []string{"Admit"}, c.op, rec.Header().Get("X-Request-Id"), c.route)
		}
	})

	t.Run("stream", func(t *testing.T) {
		c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
		if c.status != http.StatusOK {
			t.Fatalf("status %d", c.status)
		}
		c.close()
		<-c.done
		checkCalls(t, ae.logs, h.take(), []string{"Admit"}, "streamMessages", c.header.Get("X-Request-Id"), "GET /v1/inboxes/{slug}/stream")
	})

	t.Run("an id the client sent", func(t *testing.T) {
		rec := ae.do(req{method: "GET", path: "/v1/inboxes/agent-a", headers: http.Header{
			"Authorization": {"Bearer " + ae.tokenA}, "X-Request-Id": {"client-chosen-id"},
		}})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		checkCalls(t, ae.logs, h.take(), []string{"Admit"}, "getInbox", "client-chosen-id", "GET /v1/inboxes/{slug}")
	})

	t.Run("ingest", func(t *testing.T) {
		for _, c := range []struct{ method, path, op, route string }{
			{"POST", "/in/agent-a", "ingest", "POST /in/{key}"},
			{"PUT", "/in/agent-a", "ingestPut", "PUT /in/{key}"},
			{"POST", "/in/agent-a/p", "ingestWithPath", "POST /in/{key}/{rest...}"},
			{"PUT", "/in/agent-a/p", "ingestPutWithPath", "PUT /in/{key}/{rest...}"},
		} {
			rec := ae.do(req{method: c.method, path: c.path, body: "x"})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: status %d", c.method, c.path, rec.Code)
			}
			// The trusted verifier stores verified messages, so MaxUnverified is not asked.
			checkCalls(t, ae.logs, h.take(), []string{"BeforeIngest", "AfterIngest", "ObserveIngest"}, c.op, rec.Header().Get("X-Request-Id"), c.route)
		}
	})

	t.Run("MCP", func(t *testing.T) {
		cs := connectMCP(t, base, ae.tokenA)
		h.take()
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"list_inboxes", map[string]any{}},
			{"query_messages", map[string]any{"inbox": "agent-a"}},
			{"get_message", map[string]any{"inbox": "agent-a", "seq": 1}},
			{"wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 0}},
			{"wait_for_messages", map[string]any{"inbox": "agent-a", "after": 1, "timeout_seconds": 1}},
		} {
			if o := callTool(t, cs, c.tool, c.args); o.err != "" {
				t.Fatalf("%s: %s", c.tool, o.err)
			}
			checkCalls(t, ae.logs, h.take(), []string{"Admit"}, "mcp."+c.tool, "", "POST /mcp")
		}
	})
}

func TestUnverifiedLimiterSeesTheOperation(t *testing.T) {
	h := &accessHook{}
	e := newEnv(t, func(o *Options) { o.Hooks = h })
	e.newInbox("open", "none", "")
	rec := e.post("/in/open", "x")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got []string
	for _, c := range h.take() {
		got = append(got, c.hook)
		if !c.ok || c.info.Operation != "ingest" || c.info.RequestID != rec.Header().Get("X-Request-Id") {
			t.Errorf("%s saw %+v (%v)", c.hook, c.info, c.ok)
		}
	}
	if strings.Join(got, ",") != "BeforeIngest,MaxUnverified,AfterIngest,ObserveIngest" {
		t.Errorf("hooks called: %v", got)
	}
}

func TestRetryAfterOf(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{inbox.ErrRateLimited, ""},
		{fmt.Errorf("plain: %w", inbox.ErrQuotaExceeded), ""},
		{inbox.RefuseAfter(inbox.ErrRateLimited, 0), ""},
		{inbox.RefuseAfter(inbox.ErrRateLimited, -time.Second), ""},
		{inbox.RefuseAfter(inbox.ErrRateLimited, time.Nanosecond), "1"},
		{inbox.RefuseAfter(inbox.ErrRateLimited, time.Second), "1"},
		{inbox.RefuseAfter(inbox.ErrRateLimited, 1500*time.Millisecond), "2"},
		{inbox.RefuseAfter(inbox.ErrRateLimited, 90*time.Second), "90"},
		{fmt.Errorf("wrapped: %w", inbox.RefuseAfter(inbox.ErrForbidden, 90*time.Second)), "90"},
		{inbox.RefuseAfter(inbox.ErrQuotaExceeded, maxRetryAfter), "86400"},
		{inbox.RefuseAfter(inbox.ErrQuotaExceeded, maxRetryAfter+time.Nanosecond), "86400"},
		{inbox.RefuseAfter(inbox.ErrQuotaExceeded, 30*24*time.Hour), "86400"},
		{inbox.RefuseAfter(inbox.ErrQuotaExceeded, math.MaxInt64), "86400"},
	} {
		if got := retryAfterOf(c.err); got != c.want {
			t.Errorf("retryAfterOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestBeforeIngestRefusalsCarryRetryAfter(t *testing.T) {
	for _, r := range refusals {
		t.Run(r.code, func(t *testing.T) {
			e := newEnv(t)
			in := e.newInbox("gated", "trusted", "")
			for _, c := range []struct {
				name string
				err  error
				want string
			}{
				{"with a delay", inbox.RefuseAfter(r.err, 90*time.Second), "90"},
				{"wrapped", fmt.Errorf("plan: %w", inbox.RefuseAfter(r.err, 1500*time.Millisecond)), "2"},
				{"without one", fmt.Errorf("plan: %w", r.err), ""},
				{"zero", inbox.RefuseAfter(r.err, 0), ""},
				{"negative", inbox.RefuseAfter(r.err, -time.Minute), ""},
			} {
				e.hooks.mu.Lock()
				e.hooks.before = func(*inbox.Inbox, int64) error { return c.err }
				e.hooks.mu.Unlock()
				for _, m := range []string{"POST", "PUT"} {
					for _, path := range []string{"/in/gated", "/in/gated/p"} {
						rec := e.do(req{method: m, path: path, body: "x"})
						if rec.Code != r.status || errorCode(t, rec) != r.code {
							t.Fatalf("%s %s %s: status %d, %s", c.name, m, path, rec.Code, rec.Body.String())
						}
						if got := rec.Header().Get("Retry-After"); got != c.want {
							t.Errorf("%s %s %s: Retry-After %q, want %q", c.name, m, path, got, c.want)
						}
						if _, sent := rec.Header()["Retry-After"]; c.want == "" && sent {
							t.Errorf("%s %s %s: an empty Retry-After was sent", c.name, m, path)
						}
					}
				}
			}
			if e.count(in) != 0 {
				t.Error("a refused delivery was stored")
			}
		})
	}
}

func TestOtherRefusalsSendNoHookRetryAfter(t *testing.T) {
	// A hook error that is not a refusal is a 500, whatever it says about retrying.
	e := newEnv(t)
	e.newInbox("gated", "trusted", "")
	e.hooks.before = func(*inbox.Inbox, int64) error { return inbox.RefuseAfter(errors.New("db down"), time.Minute) }
	rec := e.post("/in/gated", "x")
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Retry-After") != "" {
		t.Errorf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// So is an Admit error that is not one.
	h := &accessHook{admitErr: inbox.RefuseAfter(errors.New("db down"), time.Minute)}
	ae, _, _ := mcpEnv(t, func(o *Options) { o.Hooks = h })
	rec = ae.get("/v1/inboxes/agent-a/messages", ae.tokenA)
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Retry-After") != "" {
		t.Errorf("admit: status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// over makes a request with token A over the socket, as a client would (a recorder
// would keep a body that a HEAD response is not sent with), and checks the answer
// against the document.
func (ae *agentEnv) over(base, method, path string) (int, http.Header) {
	ae.t.Helper()
	r, err := http.NewRequestWithContext(ae.t.Context(), method, base+path, nil)
	if err != nil {
		ae.t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+ae.tokenA)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		ae.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		ae.t.Fatal(err)
	}
	checkAgainstSpec(ae.t, method, path, resp.StatusCode, resp.Header, body)
	return resp.StatusCode, resp.Header
}

func TestAdmitRefusalsCarryRetryAfter(t *testing.T) {
	for _, r := range refusals {
		t.Run(r.code, func(t *testing.T) {
			h := &accessHook{}
			ae, base, _ := mcpEnv(t, func(o *Options) { o.Hooks = h })
			ae.post("/in/agent-a", "x")
			for _, c := range []struct {
				name string
				err  error
				want string
			}{
				{"with a delay", fmt.Errorf("plan: %w", inbox.RefuseAfter(r.err, 90*time.Second)), "90"},
				{"without one", fmt.Errorf("plan: %w", r.err), ""},
				{"zero", inbox.RefuseAfter(r.err, 0), ""},
			} {
				h.set(c.err, nil)
				for _, m := range []string{"GET", "HEAD"} {
					for _, path := range []string{"/v1/inboxes/agent-a", "/v1/inboxes/agent-a/messages", "/v1/inboxes/agent-a/messages/1", "/v1/inboxes/agent-a/messages/1/raw"} {
						status, hdr := ae.over(base, m, path)
						if status != r.status {
							t.Fatalf("%s %s %s: status %d", c.name, m, path, status)
						}
						if got := hdr.Get("Retry-After"); got != c.want {
							t.Errorf("%s %s %s: Retry-After %q, want %q", c.name, m, path, got, c.want)
						}
					}
				}
				s := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
				if s.status != r.status || s.header.Get("Retry-After") != c.want {
					t.Errorf("%s stream: status %d, Retry-After %q; want %d %q", c.name, s.status, s.header.Get("Retry-After"), r.status, c.want)
				}
				s.close()
			}
		})
	}
}

func TestMCPAdmitRefusalsNameTheConditionAndTheRetry(t *testing.T) {
	for _, r := range refusals {
		t.Run(r.code, func(t *testing.T) {
			h := &accessHook{}
			ae, base, _ := mcpEnv(t, func(o *Options) { o.Hooks = h })
			cs := connectMCP(t, base, ae.tokenA)
			tools := map[string]map[string]any{
				"list_inboxes":      {},
				"query_messages":    {"inbox": "agent-a"},
				"get_message":       {"inbox": "agent-a", "seq": 1},
				"wait_for_messages": {"inbox": "agent-a", "after": 0, "timeout_seconds": 5},
			}
			h.set(fmt.Errorf("tenant 42 is over plan secret-plan-name: %w", inbox.RefuseAfter(r.err, 1500*time.Millisecond)), nil)
			for tool, args := range tools {
				o := callTool(t, cs, tool, args)
				if !strings.HasPrefix(o.err, r.code+": ") || !strings.HasSuffix(o.err, "; retry after 2 s") {
					t.Errorf("%s: err = %q, want the condition %s and the retry time", tool, o.err, r.code)
				}
				if strings.Contains(o.err, "secret-plan-name") || strings.Contains(o.err, "tenant 42") {
					t.Errorf("%s: the embedder's reason reached the agent: %q", tool, o.err)
				}
			}
			h.set(fmt.Errorf("no delay known: %w", r.err), nil)
			for tool, args := range tools {
				o := callTool(t, cs, tool, args)
				if !strings.HasPrefix(o.err, r.code+": ") || strings.Contains(o.err, "retry after") || strings.Contains(o.err, "no delay known") {
					t.Errorf("%s: err = %q, want the condition %s and no retry time", tool, o.err, r.code)
				}
			}
		})
	}
}
