// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// TestOpenAPIMatchesTheServer calls every operation, makes the unusual responses
// happen in turn, and then requires that every response the document lists was
// seen and that nothing was seen that it does not list.
func TestOpenAPIMatchesTheServer(t *testing.T) {
	c := newContract(t)
	t.Run("ordinary use", func(t *testing.T) { ordinaryUse(t, c) })
	t.Run("hooks refuse deliveries", func(t *testing.T) { hooksRefuse(t, c) })
	t.Run("the store fails", func(t *testing.T) { storeFails(t, c) })
	t.Run("the server is full", func(t *testing.T) { serverFull(t, c) })
	t.Run("work is already running", func(t *testing.T) { alreadyRunning(t, c) })
	t.Run("the server cannot do it", func(t *testing.T) { cannotDoIt(t, c) })
	t.Run("the host serves no org", func(t *testing.T) { unroutedHostScenario(t, c) })
	t.Run("the HTTP stack answers", func(t *testing.T) { httpStack(t, c) })
	c.requireCoverage(t)
}

func ordinaryUse(t *testing.T, c *contract) {
	c.t = t
	a := newAdminEnv(t, func(o *Options) { o.PublicBaseURL = "https://hooks.example.com" })
	cl := c.on(a.base)
	admin, agent := testAdminToken, a.tokenA
	addr := strings.TrimPrefix(a.base, "http://")

	// Inboxes to deliver to: one that checks nothing, one that checks GitHub's signature.
	cl.call("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "signed-inbox", "ingest_key": "signed-inbox", "verifier": "github", "verifier_secret": "s3"}, admin)

	cl.call("healthz", "GET", "/healthz", nil, "")
	cl.call("healthz", "POST", "/healthz", nil, "")
	cl.call("headHealthz", "HEAD", "/healthz", nil, "")
	cl.call("readyz", "GET", "/readyz", nil, "")
	cl.call("readyz", "DELETE", "/readyz", nil, "")
	cl.call("headReadyz", "HEAD", "/readyz", nil, "")

	cl.call("ingest", "POST", "/in/agent-a", `{"hello":"world"}`, "", "Content-Type", "application/json")
	cl.call("ingestPut", "PUT", "/in/agent-a", `{"hello":"world"}`, "", "Content-Type", "application/json")
	cl.call("ingest", "POST", "/in/agent-a", "plain text", "", "Content-Type", "text/plain")
	cl.call("ingestPut", "PUT", "/in/agent-a", "plain text", "", "Content-Type", "text/plain")
	cl.call("ingest", "POST", "/in/signed-inbox", `{"x":1}`, "", "Content-Type", "application/json")   // unsigned: 401
	cl.call("ingestPut", "PUT", "/in/signed-inbox", `{"x":1}`, "", "Content-Type", "application/json") // unsigned: 401
	cl.call("ingest", "POST", "/in/no-such-inbox", "x", "")
	cl.call("ingestPut", "PUT", "/in/no-such-inbox", "x", "")
	cl.call("ingest", "GET", "/in/agent-a", nil, "")
	cl.call("ingestPut", "GET", "/in/agent-a", nil, "")
	cl.call("ingestWithPath", "POST", "/in/agent-a/some/sub/path?q=1", `{"n":2}`, "", "Content-Type", "application/json")
	cl.call("ingestPutWithPath", "PUT", "/in/agent-a/some/sub/path?q=1", `{"n":2}`, "", "Content-Type", "application/json")
	cl.call("ingestWithPath", "GET", "/in/agent-a/x", nil, "")
	cl.call("ingestPutWithPath", "GET", "/in/agent-a/x", nil, "")
	cl.call("ingestWithPath", "POST", "/in/signed-inbox/sub", `{"x":1}`, "", "Content-Type", "application/json")   // unsigned: 401
	cl.call("ingestPutWithPath", "PUT", "/in/signed-inbox/sub", `{"x":1}`, "", "Content-Type", "application/json") // unsigned: 401
	cl.call("ingestWithPath", "POST", "/in/no-such-inbox/sub", "x", "")
	cl.call("ingestPutWithPath", "PUT", "/in/no-such-inbox/sub", "x", "")
	a.mustAdmin("PATCH", "/v1/admin/inboxes/agent-a", map[string]any{"max_body_bytes": 64}, 200)
	cl.call("ingest", "POST", "/in/agent-a", strings.Repeat("x", 200), "")
	cl.call("ingestPut", "PUT", "/in/agent-a", strings.Repeat("x", 200), "")
	cl.call("ingestWithPath", "POST", "/in/agent-a/big", strings.Repeat("x", 200), "")
	cl.call("ingestPutWithPath", "PUT", "/in/agent-a/big", strings.Repeat("x", 200), "")
	a.mustAdmin("PATCH", "/v1/admin/inboxes/agent-a", map[string]any{"max_body_bytes": 1048576}, 200)
	// A body that stops before its declared length.
	st, h, b := rawBrokenBody(t, addr, "/in/agent-a", "", 100, "short")
	c.record("ingest", st, h, b, false, false)
	st, h, b = rawBrokenBody(t, addr, "/in/agent-a/sub", "", 100, "short")
	c.record("ingestWithPath", st, h, b, false, false)
	st, h, b = rawBrokenBodyAs(t, addr, "PUT", "/in/agent-a", "", 100, "short")
	c.record("ingestPut", st, h, b, false, false)
	st, h, b = rawBrokenBodyAs(t, addr, "PUT", "/in/agent-a/sub", "", 100, "short")
	c.record("ingestPutWithPath", st, h, b, false, false)

	for _, m := range []string{"GET", "HEAD"} {
		pre := ""
		if m == "HEAD" {
			pre = "head"
		}
		id := func(op string) string {
			if pre == "" {
				return op
			}
			return pre + strings.ToUpper(op[:1]) + op[1:]
		}
		cl.call(id("getInbox"), m, "/v1/inboxes/agent-a", nil, agent)
		cl.call(id("getInbox"), m, "/v1/inboxes/agent-a", nil, "")
		cl.call(id("getInbox"), m, "/v1/inboxes/no-such-inbox", nil, agent)
		cl.call(id("getInbox"), m, "/v1/inboxes/agent-a?x=1", nil, agent)
		cl.call(id("listMessages"), m, "/v1/inboxes/agent-a/messages", nil, agent)
		cl.call(id("listMessages"), m, "/v1/inboxes/agent-a/messages?after=0&limit=2&fields=summary&type=a&type=b&verified=true", nil, agent)
		cl.call(id("listMessages"), m, "/v1/inboxes/agent-a/messages?limt=2", nil, agent)
		cl.call(id("listMessages"), m, "/v1/inboxes/agent-a/messages?after=0&epoch=0badc0de", nil, agent) // another epoch
		cl.call(id("listMessages"), m, "/v1/inboxes/agent-a/messages", nil, "")
		cl.call(id("listMessages"), m, "/v1/inboxes/no-such-inbox/messages", nil, agent)
		cl.call(id("getMessage"), m, "/v1/inboxes/agent-a/messages/1", nil, agent)
		cl.call(id("getMessage"), m, "/v1/inboxes/agent-a/messages/999", nil, agent)
		cl.call(id("getMessage"), m, "/v1/inboxes/agent-a/messages/1?x=1", nil, agent)
		cl.call(id("getMessage"), m, "/v1/inboxes/agent-a/messages/1?epoch=0badc0de", nil, agent) // another epoch
		cl.call(id("getMessage"), m, "/v1/inboxes/agent-a/messages/1", nil, "")
		cl.call(id("getMessageRaw"), m, "/v1/inboxes/agent-a/messages/1/raw", nil, agent)
		cl.call(id("getMessageRaw"), m, "/v1/inboxes/agent-a/messages/999/raw", nil, agent)
		cl.call(id("getMessageRaw"), m, "/v1/inboxes/agent-a/messages/1/raw?x=1", nil, agent)
		cl.call(id("getMessageRaw"), m, "/v1/inboxes/agent-a/messages/1/raw?epoch=0badc0de", nil, agent) // another epoch
		cl.call(id("getMessageRaw"), m, "/v1/inboxes/agent-a/messages/1/raw", nil, "")
	}
	cl.call("getInbox", "POST", "/v1/inboxes/agent-a", nil, agent)
	cl.call("listMessages", "POST", "/v1/inboxes/agent-a/messages", nil, agent)
	cl.call("getMessage", "POST", "/v1/inboxes/agent-a/messages/1", nil, agent)
	cl.call("getMessageRaw", "POST", "/v1/inboxes/agent-a/messages/1/raw", nil, agent)

	// The stream: only the start of it is read.
	sctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	req, _ := http.NewRequestWithContext(sctx, "GET", a.base+"/v1/inboxes/agent-a/stream?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+agent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 64)
	n, _ := resp.Body.Read(first)
	cancel()
	_ = resp.Body.Close()
	c.record("streamMessages", resp.StatusCode, resp.Header, first[:n], false, false)
	cl.call("streamMessages", "GET", "/v1/inboxes/agent-a/stream", nil, "")
	cl.call("streamMessages", "GET", "/v1/inboxes/agent-a/stream?bogus=1", nil, agent)
	cl.call("streamMessages", "GET", "/v1/inboxes/agent-a/stream?epoch=0badc0de", nil, agent) // another epoch
	cl.call("streamMessages", "GET", "/v1/inboxes/no-such-inbox/stream", nil, agent)
	cl.call("streamMessages", "POST", "/v1/inboxes/agent-a/stream", nil, agent)

	accept := []string{"Accept", "application/json, text/event-stream"}
	cl.call("mcp", "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, agent, accept...)
	cl.call("mcp", "POST", "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, agent, accept...)
	cl.call("mcp", "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "", accept...)
	cl.call("mcp", "POST", "/mcp", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, agent, accept...)
	cl.call("mcp", "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, agent) // no Accept: the SDK refuses as plain text
	cl.call("mcp", "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, agent, append([]string{"Content-Type", "text/plain"}, accept...)...)
	cl.call("mcp", "POST", "/mcp", strings.Repeat("a", 2<<20), agent, accept...)
	cl.call("mcp", "GET", "/mcp", nil, agent)
	st, h, b = rawBrokenBody(t, addr, "/mcp", agent, 100, "{")
	c.record("mcp", st, h, b, false, false)

	created := cl.call2("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "contract-test", "verifier": "github", "verifier_secret": "s"}, admin)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "contract-test", "verifier": "none"}, admin)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "No", "verifier": "none"}, admin)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", `{"slug": "contract-two", "verifier": "none"} {"x":1}`, admin)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "contract-two", "verifier": "none"}, agent)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "contract-two", "verifier": "none"}, "")
	cl.call("createInbox", "PUT", "/v1/admin/inboxes", nil, admin)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", `{"slug":"contract-three","verifier":"none"}`, admin, "Content-Type", "text/plain")
	cl.call("createInbox", "POST", "/v1/admin/inboxes", `{"slug":"contract-three","verifier":"none","name":"`+strings.Repeat("n", 100<<10)+`"}`, admin)
	st, h, b = rawBrokenBody(t, addr, "/v1/admin/inboxes", admin, 100, `{"slug":`)
	c.record("createInbox", st, h, b, false, false)
	if created == "" {
		t.Fatal("no token from createInbox")
	}
	cl.call("listInboxes", "GET", "/v1/admin/inboxes?limit=1", nil, admin)
	cl.call("listInboxes", "GET", "/v1/admin/inboxes?bogus=1", nil, admin)
	cl.call("listInboxes", "GET", "/v1/admin/inboxes", nil, agent)
	cl.call("listInboxes", "GET", "/v1/admin/inboxes", nil, "")
	cl.call("listInboxes", "DELETE", "/v1/admin/inboxes", nil, admin)
	cl.call("showInbox", "GET", "/v1/admin/inboxes/contract-test", nil, admin)
	cl.call("showInbox", "GET", "/v1/admin/inboxes/contract-test?bogus=1", nil, admin)
	cl.call("showInbox", "GET", "/v1/admin/inboxes/nope-nope", nil, admin)
	cl.call("showInbox", "GET", "/v1/admin/inboxes/contract-test", nil, agent)
	cl.call("showInbox", "GET", "/v1/admin/inboxes/contract-test", nil, "")
	cl.call("showInbox", "PUT", "/v1/admin/inboxes/contract-test", nil, admin)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/contract-test", map[string]any{"name": "Renamed", "retention_days": 7}, admin)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/contract-test", map[string]any{"slug": "other", "verifier": "none"}, admin)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/nope-nope", map[string]any{"name": "x"}, admin)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/contract-test", `{"name":"x"}`, admin, "Content-Type", "text/plain")
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/contract-test", `{"name":"`+strings.Repeat("n", 100<<10)+`"}`, admin)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/contract-test", map[string]any{"name": "x"}, agent)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/contract-test", map[string]any{"name": "x"}, "")
	st, h, b = rawBrokenBody(t, addr, "/v1/admin/inboxes/contract-test", admin, 100, `{"name":`) // POST: 405 for this path
	c.record("updateInbox", st, h, b, false, false)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/contract-test/secret", map[string]any{"secret": "new"}, admin)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/contract-test/secret", map[string]any{"secret": ""}, admin)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/nope-nope/secret", map[string]any{"secret": "x"}, admin)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/contract-test/secret", `{"secret":"x"}`, admin, "Content-Type", "text/plain")
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/contract-test/secret", `{"secret":"`+strings.Repeat("n", 100<<10)+`"}`, admin)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/contract-test/secret", map[string]any{"secret": "x"}, agent)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/contract-test/secret", map[string]any{"secret": "x"}, "")
	cl.call("setVerifierSecret", "GET", "/v1/admin/inboxes/contract-test/secret", nil, admin)
	_, _, made := cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "t1", "inboxes": []string{"contract-test"}}, admin)
	var madeTok struct {
		Record struct {
			ID int64 `json:"id"`
		} `json:"record"`
	}
	_ = json.Unmarshal(made, &madeTok)
	cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "t2", "all_inboxes": true}, admin)
	cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "t3"}, admin)
	cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "t4", "inboxes": []string{"nope-nope"}}, admin)
	cl.call("createToken", "POST", "/v1/admin/tokens?bogus=1", map[string]any{"name": "t5", "all_inboxes": true}, admin)
	cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "t6", "all_inboxes": true}, agent)
	cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "t7", "all_inboxes": true}, "")
	cl.call("createToken", "POST", "/v1/admin/tokens", `{"name":"t8","all_inboxes":true}`, admin, "Content-Type", "text/plain")
	cl.call("createToken", "POST", "/v1/admin/tokens", `{"name":"`+strings.Repeat("n", 100<<10)+`"}`, admin)
	cl.call("createToken", "PUT", "/v1/admin/tokens", nil, admin)
	st, h, b = rawBrokenBody(t, addr, "/v1/admin/tokens", admin, 100, `{"name":`)
	c.record("createToken", st, h, b, false, false)
	cl.call("listTokens", "GET", "/v1/admin/tokens", nil, admin)
	cl.call("listTokens", "GET", "/v1/admin/tokens?limit=1&inbox=contract-test", nil, admin)
	cl.call("listTokens", "GET", "/v1/admin/tokens?bogus=1", nil, admin)
	cl.call("listTokens", "GET", "/v1/admin/tokens?inbox=nope-nope", nil, admin)
	cl.call("listTokens", "GET", "/v1/admin/tokens", nil, agent)
	cl.call("listTokens", "GET", "/v1/admin/tokens", nil, "")
	cl.call("listTokens", "PUT", "/v1/admin/tokens/1", nil, admin)                                                        // 405 on the id path
	cl.call("revokeToken", "DELETE", "/v1/admin/tokens/"+strconv.FormatInt(madeTok.Record.ID, 10)+"?bogus=1", nil, admin) // refused, so nothing is revoked
	cl.call("revokeToken", "DELETE", "/v1/admin/tokens/"+strconv.FormatInt(madeTok.Record.ID, 10), nil, admin)
	cl.call("revokeToken", "DELETE", "/v1/admin/tokens/999999", nil, admin)
	cl.call("revokeToken", "DELETE", "/v1/admin/tokens/abc", nil, admin)
	cl.call("revokeToken", "DELETE", "/v1/admin/tokens/1", nil, agent)
	cl.call("revokeToken", "DELETE", "/v1/admin/tokens/1", nil, "")
	cl.call("revokeToken", "GET", "/v1/admin/tokens/1", nil, admin)
	cl.call("metrics", "GET", "/metrics", nil, admin)
	cl.call("metrics", "GET", "/metrics?bogus=1", nil, admin)
	cl.call("metrics", "GET", "/metrics", nil, agent)
	cl.call("metrics", "GET", "/metrics", nil, "")
	cl.call("metrics", "POST", "/metrics", nil, admin)
	cl.call("rotateIngestKey", "POST", "/v1/admin/inboxes/contract-test/rotate-ingest-key", nil, admin)
	cl.call("rotateIngestKey", "POST", "/v1/admin/inboxes/contract-test/rotate-ingest-key?bogus=1", nil, admin)
	cl.call("rotateIngestKey", "POST", "/v1/admin/inboxes/nope-nope/rotate-ingest-key", nil, admin)
	cl.call("rotateIngestKey", "POST", "/v1/admin/inboxes/contract-test/rotate-ingest-key", nil, agent)
	cl.call("rotateIngestKey", "POST", "/v1/admin/inboxes/contract-test/rotate-ingest-key", nil, "")
	cl.call("rotateIngestKey", "GET", "/v1/admin/inboxes/contract-test/rotate-ingest-key", nil, admin)
	cl.call("runRetention", "POST", "/v1/admin/retention/run", nil, admin)
	cl.call("runRetention", "POST", "/v1/admin/retention/run?bogus=1", nil, admin)
	cl.call("runRetention", "GET", "/v1/admin/retention/run", nil, admin)
	cl.call("runRetention", "POST", "/v1/admin/retention/run", nil, agent)
	cl.call("runRetention", "POST", "/v1/admin/retention/run", nil, "")
	cl.call("adminStatus", "GET", "/v1/admin/status", nil, admin)
	cl.call("adminStatus", "POST", "/v1/admin/status", nil, admin)
	cl.call("adminStatus", "GET", "/v1/admin/status", nil, agent)
	cl.call("adminStatus", "GET", "/v1/admin/status", nil, "")
	cl.call("backup", "GET", "/v1/admin/backup", nil, admin)
	cl.call("backup", "GET", "/v1/admin/backup?bogus=1", nil, admin)
	cl.call("backup", "GET", "/v1/admin/backup", nil, agent)
	cl.call("backup", "GET", "/v1/admin/backup", nil, "")
	cl.call("backup", "POST", "/v1/admin/backup", nil, admin)
	cl.call("deleteInbox", "DELETE", "/v1/admin/inboxes/contract-test?bogus=1", nil, admin) // refused, so nothing is deleted
	cl.call("deleteInbox", "DELETE", "/v1/admin/inboxes/contract-test", nil, admin)
	cl.call("deleteInbox", "DELETE", "/v1/admin/inboxes/contract-test", nil, admin)
	cl.call("deleteInbox", "DELETE", "/v1/admin/inboxes/signed-inbox", nil, agent)
	cl.call("deleteInbox", "DELETE", "/v1/admin/inboxes/signed-inbox", nil, "")
	cl.call("deleteInbox", "POST", "/v1/admin/inboxes/signed-inbox", nil, admin) // the path takes GET, PATCH, DELETE
}

// call2 is call that returns the agent token from a create response.
func (cl *client) call2(opID, method, path string, body any, tok string) string {
	_, _, data := cl.call(opID, method, path, body, tok)
	var v struct {
		AgentToken string `json:"agent_token"`
	}
	_ = json.Unmarshal(data, &v)
	return v.AgentToken
}

func hooksRefuse(t *testing.T, c *contract) {
	c.t = t
	a := newAdminEnv(t)
	cl := c.on(a.base)
	for _, refusal := range []error{inbox.ErrQuotaExceeded, inbox.ErrForbidden, inbox.ErrRateLimited} {
		a.hooks.mu.Lock()
		// With a time to retry, so that the Retry-After it sends is checked against the document.
		a.hooks.before = func(*inbox.Inbox, int64) error {
			return inbox.RefuseAfter(fmt.Errorf("%w: test", refusal), 30*time.Second)
		}
		a.hooks.mu.Unlock()
		cl.call("ingest", "POST", "/in/agent-a", "x", "")
		cl.call("ingestPut", "PUT", "/in/agent-a", "x", "")
		cl.call("ingestWithPath", "POST", "/in/agent-a/p", "x", "")
		cl.call("ingestPutWithPath", "PUT", "/in/agent-a/p", "x", "")

		// The same refusals, from an Admitter, on reads and streams.
		a.hooks.mu.Lock()
		a.hooks.admitErr = inbox.RefuseAfter(fmt.Errorf("%w: test", refusal), 30*time.Second)
		a.hooks.mu.Unlock()
		for _, m := range []string{"GET", "HEAD"} {
			pre := ""
			if m == "HEAD" {
				pre = "head"
			}
			id := func(op string) string {
				if pre == "" {
					return op
				}
				return pre + strings.ToUpper(op[:1]) + op[1:]
			}
			cl.call(id("getInbox"), m, "/v1/inboxes/agent-a", nil, a.tokenA)
			cl.call(id("listMessages"), m, "/v1/inboxes/agent-a/messages", nil, a.tokenA)
			cl.call(id("getMessage"), m, "/v1/inboxes/agent-a/messages/1", nil, a.tokenA)
			cl.call(id("getMessageRaw"), m, "/v1/inboxes/agent-a/messages/1/raw", nil, a.tokenA)
		}
		cl.call("streamMessages", "GET", "/v1/inboxes/agent-a/stream", nil, a.tokenA)
		a.hooks.mu.Lock()
		a.hooks.admitErr = nil
		a.hooks.mu.Unlock()
	}
}

func storeFails(t *testing.T, c *contract) {
	c.t = t
	var f *faulty
	a := newAdminEnv(t, func(o *Options) {
		f = &faulty{Store: o.Store}
		inner := o.Authenticator
		o.Store = f
		o.Authenticator = &faultyAuth{inner: inner, f: f}
	})
	cl := c.on(a.base)
	admin, agent := testAdminToken, a.tokenA
	accept := []string{"Accept", "application/json, text/event-stream"}

	// A conflict that cannot be settled.
	f.set(false, true, false)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/agent-a", map[string]any{"name": "x"}, admin)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/agent-a/secret", map[string]any{"secret": "x"}, admin)
	cl.call("rotateIngestKey", "POST", "/v1/admin/inboxes/agent-a/rotate-ingest-key", nil, admin)

	f.set(false, false, true)
	cl.call("readyz", "GET", "/readyz", nil, "")
	cl.call("headReadyz", "HEAD", "/readyz", nil, "")

	// No room to store a delivery: a 503 the sender retries.
	f.setNoRoom(true)
	cl.call("ingest", "POST", "/in/agent-a", "x", "")
	cl.call("ingestPut", "PUT", "/in/agent-a", "x", "")
	cl.call("ingestWithPath", "POST", "/in/agent-a/p", "x", "")
	cl.call("ingestPutWithPath", "PUT", "/in/agent-a/p", "x", "")
	f.setNoRoom(false)

	f.set(true, false, false)
	cl.call("ingest", "POST", "/in/agent-a", "x", "")
	cl.call("ingestPut", "PUT", "/in/agent-a", "x", "")
	cl.call("ingestWithPath", "POST", "/in/agent-a/p", "x", "")
	cl.call("ingestPutWithPath", "PUT", "/in/agent-a/p", "x", "")
	for _, p := range []struct{ id, path string }{
		{"getInbox", "/v1/inboxes/agent-a"}, {"listMessages", "/v1/inboxes/agent-a/messages"},
		{"getMessage", "/v1/inboxes/agent-a/messages/1"}, {"getMessageRaw", "/v1/inboxes/agent-a/messages/1/raw"},
		{"streamMessages", "/v1/inboxes/agent-a/stream"},
	} {
		cl.call(p.id, "GET", p.path, nil, agent)
	}
	cl.call("mcp", "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, agent, accept...)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "never-made", "verifier": "none"}, admin)
	cl.call("listInboxes", "GET", "/v1/admin/inboxes", nil, admin)
	cl.call("showInbox", "GET", "/v1/admin/inboxes/agent-a", nil, admin)
	cl.call("updateInbox", "PATCH", "/v1/admin/inboxes/agent-a", map[string]any{"name": "x"}, admin)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/agent-a/secret", map[string]any{"secret": "x"}, admin)
	cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "x", "all_inboxes": true}, admin)
	cl.call("createToken", "POST", "/v1/admin/tokens", map[string]any{"name": "x", "inboxes": []string{"agent-a"}}, admin)
	cl.call("listTokens", "GET", "/v1/admin/tokens", nil, admin)
	cl.call("revokeToken", "DELETE", "/v1/admin/tokens/1", nil, admin)
	cl.call("rotateIngestKey", "POST", "/v1/admin/inboxes/agent-a/rotate-ingest-key", nil, admin)
	cl.call("deleteInbox", "DELETE", "/v1/admin/inboxes/agent-a", nil, admin)
	cl.call("runRetention", "POST", "/v1/admin/retention/run", nil, admin) // Prune is not failing: this one succeeds
	cl.call("adminStatus", "GET", "/v1/admin/status", nil, admin)
	cl.call("backup", "GET", "/v1/admin/backup", nil, admin)
	f.set(false, false, false)
}

// faultyAuth makes the Authenticator fail with an error that is not "unauthenticated".
type faultyAuth struct {
	inner inbox.Authenticator
	f     *faulty
}

func (a *faultyAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	if a.f.failing() {
		return nil, errBoom
	}
	return a.inner.Authenticate(r)
}

func serverFull(t *testing.T, c *contract) {
	c.t = t
	bs := blockingStore{entered: make(chan struct{}, 8), release: make(chan struct{})}
	a := newAdminEnv(t, func(o *Options) {
		bs.Store = o.Store
		o.Store = bs
		o.MaxConcurrentReads = 1
		o.ReadWaitTimeout = 50 * time.Millisecond
		o.MaxStreams = 1
		o.MaxStreamsPerInbox = 1
		o.Notifier = local.New()
	})
	cl := c.on(a.base)
	agent := a.tokenA
	t.Cleanup(func() { close(bs.release) })

	// One read holds the only slot.
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", a.base+"/v1/inboxes/agent-a/messages", nil)
		req.Header.Set("Authorization", "Bearer "+agent)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-bs.entered
	for _, p := range []struct{ id, path string }{
		{"getInbox", "/v1/inboxes/agent-a"}, {"listMessages", "/v1/inboxes/agent-a/messages"},
		{"getMessage", "/v1/inboxes/agent-a/messages/1"}, {"getMessageRaw", "/v1/inboxes/agent-a/messages/1/raw"},
	} {
		cl.call(p.id, "GET", p.path, nil, agent)
	}

	// One stream holds the only stream slot.
	sctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, "GET", a.base+"/v1/inboxes/agent-a/stream", nil)
	req.Header.Set("Authorization", "Bearer "+agent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("the first stream: %v %v", resp, err)
	}
	defer func() { _ = resp.Body.Close() }()
	cl.call("streamMessages", "GET", "/v1/inboxes/agent-a/stream", nil, agent)
}

func alreadyRunning(t *testing.T, c *contract) {
	c.t = t
	bp := &blockedPrune{entered: make(chan struct{}, 4), release: make(chan struct{})}
	a := newAdminEnv(t, func(o *Options) { bp.Store = o.Store; o.Store = bp; o.RetentionInterval = -1 })
	cl := c.on(a.base)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(t.Context(), "POST", a.base+"/v1/admin/retention/run", nil)
		req.Header.Set("Authorization", "Bearer "+testAdminToken)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-bp.entered
	cl.call("runRetention", "POST", "/v1/admin/retention/run", nil, testAdminToken)
	close(bp.release)
	<-done

	sb := slowBackup{entered: make(chan struct{}, 2), release: make(chan struct{})}
	b := newAdminEnv(t, func(o *Options) { sb.Store = o.Store; o.Store = sb })
	cl = c.on(b.base)
	first := make(chan struct{})
	go func() {
		defer close(first)
		req, _ := http.NewRequestWithContext(t.Context(), "GET", b.base+"/v1/admin/backup", nil)
		req.Header.Set("Authorization", "Bearer "+testAdminToken)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-sb.entered
	cl.call("backup", "GET", "/v1/admin/backup", nil, testAdminToken)
	close(sb.release)
	<-first
}

func cannotDoIt(t *testing.T, c *contract) {
	c.t = t
	a := newAdminEnv(t, func(o *Options) { o.SecretBox = nil }) // no master key
	cl := c.on(a.base)
	cl.call("createInbox", "POST", "/v1/admin/inboxes", map[string]any{"slug": "needs-key", "verifier": "github", "verifier_secret": "s"}, testAdminToken)
	cl.call("setVerifierSecret", "PUT", "/v1/admin/inboxes/agent-a/secret", map[string]any{"secret": "s"}, testAdminToken)

	b := newAdminEnv(t, func(o *Options) { o.Store = storeOnly{o.Store} }) // a store that cannot make backups
	c.on(b.base).call("backup", "GET", "/v1/admin/backup", nil, testAdminToken)
}

// The HTTP stack answers some things before the server's code is reached, in
// plain text. The document's `default` response says so.
func httpStack(t *testing.T, c *contract) {
	c.t = t
	a := newAdminEnv(t)
	cl := c.on(a.base)
	cl.callDefault("getInbox", "GET", "/v1/inboxes/agent-a", nil, a.tokenA, "X-Padding", strings.Repeat("p", 80<<10))
}
