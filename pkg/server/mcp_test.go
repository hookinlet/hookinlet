// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// bearer adds a token to every request, as an MCP client's config does.
type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.tok != "" {
		r.Header.Set("Authorization", "Bearer "+b.tok)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// connectMCP opens an SDK client session against the server.
func connectMCP(t *testing.T, base, tok string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             base + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{tok}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// toolOut is what a tool call returned: its structured content, or the error
// text the agent would see.
type toolOut struct {
	raw  json.RawMessage
	text string
	err  string // non-empty when the tool reported an error
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) toolOut {
	t.Helper()
	return callToolCtx(t, t.Context(), cs, name, args)
}

func callToolCtx(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) toolOut {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var out toolOut
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out.text += tc.Text
		}
	}
	if res.IsError {
		out.err = out.text
		return out
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	out.raw = b
	return out
}

type mcpPage struct {
	Messages   []map[string]any `json:"messages"`
	NextAfter  *int64           `json:"next_after"`
	NextBefore *int64           `json:"next_before"`
	LatestSeq  int64            `json:"latest_seq"`
}

func (o toolOut) page(t *testing.T) mcpPage {
	t.Helper()
	if o.err != "" {
		t.Fatalf("tool error: %s", o.err)
	}
	var p mcpPage
	if err := json.Unmarshal(o.raw, &p); err != nil {
		t.Fatalf("%v: %s", err, o.raw)
	}
	return p
}

func seqsOf(ms []map[string]any) []int64 {
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		out = append(out, int64(m["seq"].(float64)))
	}
	return out
}

// mcpEnv is an agentEnv served on a socket with an inspectable notifier.
func mcpEnv(t *testing.T, mods ...func(*Options)) (*agentEnv, string, *local.Notifier) {
	t.Helper()
	return streamEnv(t, mods...)
}

// ---- the tools return what the REST API returns ----

func TestToolsReturnWhatTheRESTAPIReturns(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	bodies := []struct{ ct, body string }{
		{"application/json", `{"type":"a.created","n":1,"html":"<b>&</b>"}`},
		{"text/plain", "plain text, not json"},
		{"application/octet-stream", "\xff\xfe binary"},
		{"application/json", `[1,2,3]`},
		{"application/json", `{"type":"b","nested":{"x":[{"y":1}]}}`},
	}
	for _, b := range bodies {
		ae.post("/in/agent-a", b.body, "Content-Type", b.ct)
	}
	cs := connectMCP(t, base, ae.tokenA)

	// query_messages equals GET /messages, message for message.
	got := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "after": 0, "limit": 100, "bodies": true}).page(t)
	got.Messages = flattenUntrusted(got.Messages)
	rest := ae.get("/v1/inboxes/agent-a/messages?after=0&limit=100", ae.tokenA)
	var want struct {
		Messages  []map[string]any `json:"messages"`
		LatestSeq int64            `json:"latest_seq"`
	}
	if err := json.Unmarshal(rest.Body.Bytes(), &want); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != len(bodies) {
		t.Fatalf("got %d messages, want %d", len(got.Messages), len(bodies))
	}
	if !reflect.DeepEqual(got.Messages, want.Messages) {
		t.Errorf("MCP and REST pages differ:\nmcp:  %v\nrest: %v", got.Messages, want.Messages)
	}
	if got.LatestSeq != want.LatestSeq || got.LatestSeq != int64(len(bodies)) {
		t.Errorf("latest_seq = %d, REST says %d", got.LatestSeq, want.LatestSeq)
	}
	if got.NextAfter == nil || *got.NextAfter != int64(len(bodies)) {
		t.Errorf("next_after = %v, want %d", got.NextAfter, len(bodies))
	}

	// get_message equals GET /messages/{seq}.
	for seq := 1; seq <= len(bodies); seq++ {
		one := callTool(t, cs, "get_message", map[string]any{"inbox": "agent-a", "seq": seq})
		if one.err != "" {
			t.Fatal(one.err)
		}
		var g map[string]any
		_ = json.Unmarshal(one.raw, &g)
		g = flattenUntrusted([]map[string]any{g})[0]
		var w map[string]any
		_ = json.Unmarshal(ae.get(fmt.Sprintf("/v1/inboxes/agent-a/messages/%d", seq), ae.tokenA).Body.Bytes(), &w)
		if !reflect.DeepEqual(g, w) {
			t.Errorf("seq %d: get_message differs from REST:\nmcp:  %v\nrest: %v", seq, g, w)
		}
	}

	// A JSON body arrives as JSON. The text form carries the sender's characters
	// as they were; the structured content is the same JSON value, which the wire
	// encoding may escape (<, >, &) without changing what it means.
	first := callTool(t, cs, "get_message", map[string]any{"inbox": "agent-a", "seq": 1})
	if !strings.Contains(first.text, `"html":"<b>&</b>"`) {
		t.Errorf("the body's characters were rewritten: %s", first.text)
	}

	// list_inboxes shows the inbox this token can read.
	list := callTool(t, cs, "list_inboxes", nil)
	var l struct {
		Inboxes []struct {
			Slug      string `json:"slug"`
			LatestSeq int64  `json:"latest_seq"`
		} `json:"inboxes"`
	}
	if err := json.Unmarshal(list.raw, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Inboxes) != 1 || l.Inboxes[0].Slug != "agent-a" || l.Inboxes[0].LatestSeq != int64(len(bodies)) {
		t.Errorf("list_inboxes = %s", list.raw)
	}
}

func TestQueryFiltersAndDirections(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) {})
	for i := 1; i <= 7; i++ {
		ae.post("/in/agent-a", fmt.Sprintf(`{"n":%d}`, i), "Content-Type", "application/json")
	}
	cs := connectMCP(t, base, ae.tokenA)

	// Default: newest first, with a cursor that continues backward.
	p := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "limit": 3}).page(t)
	if got := seqsOf(p.Messages); !reflect.DeepEqual(got, []int64{7, 6, 5}) {
		t.Fatalf("newest first = %v", got)
	}
	if p.NextAfter != nil || p.NextBefore == nil || *p.NextBefore != 5 {
		t.Errorf("cursors = after %v, before %v", p.NextAfter, p.NextBefore)
	}
	p = callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "limit": 10, "before": *p.NextBefore}).page(t)
	if got := seqsOf(p.Messages); !reflect.DeepEqual(got, []int64{4, 3, 2, 1}) {
		t.Fatalf("backward page = %v", got)
	}
	if p.NextBefore != nil {
		t.Errorf("next_before must be null once the oldest message is returned, got %d", *p.NextBefore)
	}

	// Forward, oldest first, in pieces, visiting each message once.
	var seen []int64
	after := int64(0)
	for range 10 {
		p = callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "after": after, "limit": 3}).page(t)
		seen = append(seen, seqsOf(p.Messages)...)
		if p.NextAfter == nil {
			t.Fatal("a forward read must always give next_after")
		}
		if *p.NextAfter == after {
			break
		}
		after = *p.NextAfter
	}
	if !reflect.DeepEqual(seen, []int64{1, 2, 3, 4, 5, 6, 7}) {
		t.Errorf("forward walk = %v", seen)
	}

	// Messages come without bodies unless asked.
	p = callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "limit": 1}).page(t)
	u, ok := p.Messages[0]["untrusted"].(map[string]any)
	if !ok {
		t.Fatalf("no untrusted object: %v", p.Messages[0])
	}
	if _, has := u["body"]; has {
		t.Errorf("a default query carried a body: %v", p.Messages[0])
	}
}

func TestToolArgumentsAreChecked(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	ae.post("/in/agent-a", "x")
	cs := connectMCP(t, base, ae.tokenA)
	for name, c := range map[string]struct {
		args map[string]any
		want string // the error must mention this
	}{
		"limit zero":        {map[string]any{"inbox": "agent-a", "limit": 0}, "limit"},
		"negative after":    {map[string]any{"inbox": "agent-a", "after": -1}, "after"},
		"after and before":  {map[string]any{"inbox": "agent-a", "after": 1, "before": 5}, "before"},
		"bad time":          {map[string]any{"inbox": "agent-a", "since": "yesterday"}, "since"},
		"misspelled arg":    {map[string]any{"inbox": "agent-a", "limt": 5}, "limt"},
		"missing inbox":     {map[string]any{"limit": 5}, "inbox"},
		"inbox wrong type":  {map[string]any{"inbox": 7}, "inbox"},
		"empty type filter": {map[string]any{"inbox": "agent-a", "types": []string{""}}, "type"},
	} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "query_messages", Arguments: c.args})
		var msg string
		switch {
		case err != nil:
			msg = err.Error()
		case res.IsError:
			for _, ct := range res.Content {
				if tc, ok := ct.(*mcp.TextContent); ok {
					msg += tc.Text
				}
			}
		default:
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: the error %q does not mention %q", name, msg, c.want)
		}
	}
	if o := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 61}); o.err == "" {
		t.Error("a 61 second wait was accepted")
	}
	if o := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": -1}); o.err == "" {
		t.Error("a negative wait was accepted")
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"inbox": "agent-a"}})
	if err == nil && !res.IsError {
		t.Error("wait_for_messages without after was accepted")
	}
	// A limit above the maximum is clamped, as in the REST API.
	for i := 0; i < 130; i++ {
		ae.post("/in/agent-a", "x")
	}
	if p := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "limit": 500}).page(t); len(p.Messages) != mcpMaxLimit {
		t.Errorf("limit 500 returned %d messages, want %d", len(p.Messages), mcpMaxLimit)
	}
}

// ---- bodies ----

func TestBodiesOverTheInlineLimitAreTruncatedAndFetchableWhole(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) { o.MCPInlineBodyBytes = 200 })
	big := `{"data":"` + strings.Repeat("z", 5000) + `"}`
	ae.post("/in/agent-a", `{"small":1}`, "Content-Type", "application/json")
	ae.post("/in/agent-a", big, "Content-Type", "application/json")
	cs := connectMCP(t, base, ae.tokenA)

	for _, tool := range []string{"query", "wait"} {
		var p mcpPage
		if tool == "query" {
			p = callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "after": 0, "bodies": true}).page(t)
		} else {
			p = callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 0, "bodies": true}).page(t)
		}
		p.Messages = flattenUntrusted(p.Messages)
		if len(p.Messages) != 2 {
			t.Fatalf("%s: %d messages", tool, len(p.Messages))
		}
		small, large := p.Messages[0], p.Messages[1]
		if small["body_truncated"] != nil || small["body"] == nil {
			t.Errorf("%s: a small body must be inline: %v", tool, small)
		}
		if large["body_truncated"] != true {
			t.Errorf("%s: a large body must be marked body_truncated: %v", tool, large)
		}
		for _, k := range []string{"body", "body_text", "body_base64"} {
			if _, has := large[k]; has {
				t.Errorf("%s: the large body leaked through as %s", tool, k)
			}
		}
		if int64(large["size"].(float64)) != int64(len(big)) {
			t.Errorf("%s: size = %v, want %d", tool, large["size"], len(big))
		}
	}

	whole := callTool(t, cs, "get_message", map[string]any{"inbox": "agent-a", "seq": 2})
	var m struct {
		BodyTruncated bool `json:"body_truncated"`
		Untrusted     struct {
			Body map[string]string `json:"body"`
		} `json:"untrusted"`
	}
	if err := json.Unmarshal(whole.raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.BodyTruncated || m.Untrusted.Body["data"] != strings.Repeat("z", 5000) {
		t.Errorf("get_message must return the full body (truncated=%v, %d bytes)", m.BodyTruncated, len(m.Untrusted.Body["data"]))
	}
}

// ---- wait_for_messages ----

func TestWaitReturnsPromptlyOnIngest(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	cs := connectMCP(t, base, ae.tokenA)

	type result struct {
		p   mcpPage
		at  time.Time
		err string
	}
	done := make(chan result, 1)
	go func() {
		o := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 20})
		if o.err != "" {
			done <- result{err: o.err, at: time.Now()}
			return
		}
		done <- result{p: o.page(t), at: time.Now()}
	}()
	time.Sleep(300 * time.Millisecond) // let the wait begin
	sent := time.Now()
	ae.post("/in/agent-a", `{"hello":"agent"}`, "Content-Type", "application/json")
	select {
	case r := <-done:
		if r.err != "" {
			t.Fatal(r.err)
		}
		if lag := r.at.Sub(sent); lag > 100*time.Millisecond {
			t.Errorf("wait returned %v after the ingest, want under 100ms", lag)
		}
		if got := seqsOf(r.p.Messages); !reflect.DeepEqual(got, []int64{1}) {
			t.Errorf("messages = %v", got)
		}
		if r.p.NextAfter == nil || *r.p.NextAfter != 1 {
			t.Errorf("next_after = %v", r.p.NextAfter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after an ingest")
	}
}

func TestWaitReturnsEmptyAtItsTimeout(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	ae.post("/in/agent-a", "old")
	cs := connectMCP(t, base, ae.tokenA)
	start := time.Now()
	p := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 1, "timeout_seconds": 1}).page(t)
	took := time.Since(start)
	if took < 900*time.Millisecond || took > 2500*time.Millisecond {
		t.Errorf("a 1 second wait took %v", took)
	}
	if p.Messages == nil || len(p.Messages) != 0 {
		t.Errorf("messages = %v, want an empty list", p.Messages)
	}
	if p.NextAfter == nil || *p.NextAfter != 1 || p.LatestSeq != 1 {
		t.Errorf("next_after = %v, latest_seq = %d; want 1 and 1", p.NextAfter, p.LatestSeq)
	}
}

func TestWaitWithTimeoutZeroChecksOnce(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	cs := connectMCP(t, base, ae.tokenA)
	start := time.Now()
	p := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 0}).page(t)
	if time.Since(start) > time.Second || len(p.Messages) != 0 {
		t.Errorf("a zero wait took %v and returned %d messages", time.Since(start), len(p.Messages))
	}
}

func TestWaitStillWorksWhenTheNotifierDropsEverySignal(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) {
		o.Notifier = inbox.NopNotifier{}
		o.SSERecheckInterval = 300 * time.Millisecond
	})
	cs := connectMCP(t, base, ae.tokenA)
	done := make(chan mcpPage, 1)
	go func() {
		done <- callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 20}).page(t)
	}()
	time.Sleep(200 * time.Millisecond)
	ae.post("/in/agent-a", "x")
	select {
	case p := <-done:
		if got := seqsOf(p.Messages); !reflect.DeepEqual(got, []int64{1}) {
			t.Errorf("messages = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("with a dropping notifier and a recheck interval, the wait never saw the message")
	}
}

func TestWaitWithoutARecheckAndNoSignalSleepsToItsTimeout(t *testing.T) {
	// The control for the test above.
	ae, base, _ := mcpEnv(t, func(o *Options) { o.Notifier = inbox.NopNotifier{} })
	cs := connectMCP(t, base, ae.tokenA)
	done := make(chan mcpPage, 1)
	go func() {
		done <- callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 1}).page(t)
	}()
	time.Sleep(200 * time.Millisecond)
	ae.post("/in/agent-a", "x")
	p := <-done
	if len(p.Messages) != 0 {
		t.Errorf("a message arrived with no signal and no recheck: %v", seqsOf(p.Messages))
	}
}

func TestWaitIgnoresMessagesThatDoNotMatchAndSkipsPastThem(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	upd := *ae.a
	upd.EventTypePointer = "/type"
	if err := ae.store.UpdateInbox(t.Context(), &upd); err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, base, ae.tokenA)
	done := make(chan toolOut, 1)
	go func() {
		done <- callTool(t, cs, "wait_for_messages", map[string]any{
			"inbox": "agent-a", "after": 0, "types": []string{"wanted"}, "timeout_seconds": 20,
		})
	}()
	time.Sleep(200 * time.Millisecond)
	for range 3 {
		ae.post("/in/agent-a", `{"type":"other"}`)
		time.Sleep(30 * time.Millisecond)
	}
	select {
	case o := <-done:
		t.Fatalf("returned for messages that do not match: %s %s", o.raw, o.err)
	case <-time.After(300 * time.Millisecond):
	}
	ae.post("/in/agent-a", `{"type":"wanted"}`)
	select {
	case o := <-done:
		p := o.page(t)
		if got := seqsOf(p.Messages); !reflect.DeepEqual(got, []int64{4}) {
			t.Errorf("messages = %v, want only seq 4", got)
		}
		if p.NextAfter == nil || *p.NextAfter != 4 {
			t.Errorf("next_after = %v", p.NextAfter)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a matching message did not end the wait")
	}
}

// Looping on next_after while senders are busy sees every message once.
func TestWaitLoopSeesEveryMessageExactlyOnce(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	cs := connectMCP(t, base, ae.tokenA)
	const total = 120
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			for j := 0; j < total/6; j++ {
				ae.post("/in/agent-a", fmt.Sprintf(`{"w":%d,"j":%d}`, i, j))
				time.Sleep(time.Millisecond)
			}
		})
	}
	var seen []int64
	after := int64(0)
	deadline := time.Now().Add(30 * time.Second)
	for len(seen) < total && time.Now().Before(deadline) {
		p := callTool(t, cs, "wait_for_messages", map[string]any{
			"inbox": "agent-a", "after": after, "limit": 7, "timeout_seconds": 5,
		}).page(t)
		seen = append(seen, seqsOf(p.Messages)...)
		after = *p.NextAfter
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("saw %d of %d messages", len(seen), total)
	}
	for i, s := range seen {
		if s != int64(i+1) {
			t.Fatalf("seqs = %v: a gap, repeat, or reordering", seen)
		}
	}
}

func TestAbandonedWaitsReleaseTheirSubscription(t *testing.T) {
	ae, base, n := mcpEnv(t)
	cs := connectMCP(t, base, ae.tokenA)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name: "wait_for_messages", Arguments: map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 60},
		})
		errc <- err
	}()
	waitFor(t, "the wait to subscribe", func() bool { return n.Subscribers() == 1 })
	cancel()
	<-errc
	waitFor(t, "the abandoned wait to unsubscribe", func() bool { return n.Subscribers() == 0 })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestShutdownReleasesPendingWaits(t *testing.T) {
	const waits = 12
	// The timeout is longer than the 5 seconds net/http gives a connection that
	// was opened but never used (a client dials ahead of its request) before it
	// counts it as idle. What matters here is that the waits end at once.
	ae := newAgentEnv2(t, func(o *Options) { o.ShutdownTimeout = 8 * time.Second; o.Notifier = local.New() })
	tok := ae.tokenFor(ae.a)
	l, err := newListener(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, shutdown := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- ae.server.Serve(ctx, l) }()
	base := "http://" + l.Addr().String()

	var started sync.WaitGroup
	returned := make(chan time.Time, waits)
	for range waits {
		started.Add(1)
		go func() {
			cs := connectMCP(t, base, tok)
			started.Done()
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "wait_for_messages", Arguments: map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 60},
			})
			if err != nil || res.IsError {
				t.Errorf("a released wait must end cleanly, got %v %+v", err, res)
			}
			returned <- time.Now()
		}()
	}
	started.Wait()
	time.Sleep(300 * time.Millisecond)

	begin := time.Now()
	shutdown()
	for i := range waits {
		select {
		case at := <-returned:
			if at.Sub(begin) > 3*time.Second {
				t.Errorf("wait %d returned %v after shutdown began", i, at.Sub(begin))
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("wait %d was still pending after shutdown", i)
		}
	}
	if err := <-served; err != nil {
		t.Errorf("Serve returned %v", err)
	}
}

func TestWaitEndsWhenTheTokenIsRevoked(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) { o.StreamRevalidateInterval = 200 * time.Millisecond })
	cs := connectMCP(t, base, ae.tokenA)
	done := make(chan toolOut, 1)
	go func() {
		done <- callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 60})
	}()
	time.Sleep(300 * time.Millisecond)
	rec, err := ae.store.GetTokenByHash(t.Context(), token.Hash(ae.tokenA))
	if err != nil {
		t.Fatal(err)
	}
	if err := ae.store.DeleteToken(t.Context(), rec.ID); err != nil { // the token no longer works
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.err != "not found" {
			t.Errorf("the wait ended with %q %s, want not found", o.err, o.raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wait outlived its token")
	}
}

// ---- access ----

func TestMCPRequiresAuthentication(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for name, hdr := range map[string]http.Header{
		"no token":     {},
		"wrong token":  {"Authorization": {"Bearer nope"}},
		"other scheme": {"Authorization": {"Basic " + ae.tokenA}},
	} {
		h := hdr.Clone()
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "application/json, text/event-stream")
		status, resp := do2(t, http.MethodPost, base+"/mcp", h, body)
		if status != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, status)
		}
		var e struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal([]byte(resp), &e); err != nil || e.Error.Code != "unauthenticated" {
			t.Errorf("%s: body %q", name, resp)
		}
		if strings.Contains(resp, ae.tokenA) || strings.Contains(resp, "nope") {
			t.Errorf("%s: the response echoes a credential", name)
		}
	}
	// GET is unauthenticated too: nothing about the endpoint is open.
	status, _ := do2(t, http.MethodGet, base+"/mcp", http.Header{}, "")
	if status != http.StatusUnauthorized {
		t.Errorf("GET without a token: status %d", status)
	}
}

func TestATokenSeesNoTraceOfAnotherInbox(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	ae.post("/in/agent-a", `{"mine":true}`)
	ae.post("/in/agent-b", `{"secret":"inbox-b"}`)
	csA := connectMCP(t, base, ae.tokenA)

	// list_inboxes shows only its own.
	list := callTool(t, csA, "list_inboxes", nil)
	if strings.Contains(string(list.raw), "agent-b") {
		t.Errorf("list_inboxes mentions another inbox: %s", list.raw)
	}

	// Every tool answers for inbox B exactly as it does for an inbox that does not exist.
	probes := map[string]func(slug string) toolOut{
		"query_messages": func(s string) toolOut { return callTool(t, csA, "query_messages", map[string]any{"inbox": s}) },
		"get_message":    func(s string) toolOut { return callTool(t, csA, "get_message", map[string]any{"inbox": s, "seq": 1}) },
		"wait_for_messages": func(s string) toolOut {
			return callTool(t, csA, "wait_for_messages", map[string]any{"inbox": s, "after": 0, "timeout_seconds": 0})
		},
	}
	for name, probe := range probes {
		real, ghost := probe("agent-b"), probe("no-such-inbox")
		if real.err == "" || ghost.err == "" {
			t.Errorf("%s: a foreign or unknown inbox was readable: %q / %q", name, real.err, ghost.err)
			continue
		}
		if real.err != ghost.err {
			t.Errorf("%s: a foreign inbox is distinguishable from a missing one: %q vs %q", name, real.err, ghost.err)
		}
		if strings.Contains(real.err, "inbox-b") {
			t.Errorf("%s: the error leaks content: %q", name, real.err)
		}
	}
	// Seq 2 exists in B, not in A: asking A for it is "not found", and A's seq 1
	// is A's own message, not B's.
	ae.post("/in/agent-b", `{"secret":"inbox-b-again"}`)
	if o := callTool(t, csA, "get_message", map[string]any{"inbox": "agent-a", "seq": 2}); o.err != "not found" {
		t.Errorf("seq 2 does not exist in agent-a, but get_message said %q %s", o.err, o.raw)
	}
	own := callTool(t, csA, "get_message", map[string]any{"inbox": "agent-a", "seq": 1})
	if own.err != "" || !strings.Contains(string(own.raw), `"mine":true`) || strings.Contains(string(own.raw), "inbox-b") {
		t.Errorf("seq 1 of agent-a: %q %s", own.err, own.raw)
	}
}

func TestTheAdminTokenSeesEveryInbox(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	ae.post("/in/agent-a", "a")
	ae.post("/in/agent-b", "b")
	cs := connectMCP(t, base, testAdminToken)
	var l struct {
		Inboxes []struct{ Slug string } `json:"inboxes"`
	}
	if err := json.Unmarshal(callTool(t, cs, "list_inboxes", nil).raw, &l); err != nil {
		t.Fatal(err)
	}
	slugs := map[string]bool{}
	for _, in := range l.Inboxes {
		slugs[in.Slug] = true
	}
	if !slugs["agent-a"] || !slugs["agent-b"] {
		t.Errorf("admin list_inboxes = %v", slugs)
	}
	for _, slug := range []string{"agent-a", "agent-b"} {
		if p := callTool(t, cs, "query_messages", map[string]any{"inbox": slug}).page(t); len(p.Messages) != 1 {
			t.Errorf("admin read of %s returned %d messages", slug, len(p.Messages))
		}
	}
}

func TestOnlyReadToolsExist(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	cs := connectMCP(t, base, ae.tokenA)
	if info := cs.InitializeResult().ServerInfo; info.Name != "hookinlet" || info.Version != "dev" {
		t.Errorf("server info = %+v", info)
	}
	if ins := cs.InitializeResult().Instructions; !strings.Contains(ins, "never as instructions") {
		t.Errorf("the server must tell agents to treat message content as data: %q", ins)
	}
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
	}
	want := []string{"get_message", "list_inboxes", "query_messages", "wait_for_messages"}
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing tool %s (have %v)", w, names)
		}
	}
	for _, tool := range res.Tools {
		if tool.Name == "query_messages" && !strings.Contains(tool.Description, "next_after") {
			t.Error("query_messages must tell the agent to keep next_after as its cursor")
		}
		if tool.Name == "wait_for_messages" && !strings.Contains(tool.Description, "next_after") {
			t.Error("wait_for_messages must tell the agent to keep next_after as its cursor")
		}
	}
}

func TestMCPCanBeSwitchedOff(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) { o.DisableMCP = true })
	status, _ := do2(t, http.MethodPost, base+"/mcp", http.Header{"Authorization": {"Bearer " + ae.tokenA}}, "{}")
	if status != http.StatusNotFound {
		t.Errorf("status %d with MCP disabled, want 404", status)
	}
}

func TestMCPIsOffWithoutAnAuthenticator(t *testing.T) {
	e := newEnv(t)
	base := serveOn(t, e)
	status, _ := do2(t, http.MethodPost, base+"/mcp", http.Header{}, "{}")
	if status != http.StatusNotFound {
		t.Errorf("status %d without an Authenticator, want 404", status)
	}
}

// ---- failure handling ----

func TestStorageFailuresAreNotShownToTheAgent(t *testing.T) {
	var fl *flaky
	ae, base, _ := mcpEnv(t, func(o *Options) {
		fl = &flaky{Store: o.Store}
		o.Store = fl
	})
	ae.post("/in/agent-a", "x")
	cs := connectMCP(t, base, ae.tokenA)
	fl.failQuery.Store(true)
	o := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a"})
	if o.err == "" {
		t.Fatal("a storage failure was not reported")
	}
	if strings.Contains(o.err, "database") || strings.Contains(o.err, "moment") {
		t.Errorf("the error shows internals: %q", o.err)
	}
	if !strings.Contains(ae.logs.String(), "mcp tool failed") {
		t.Errorf("the failure was not logged: %s", ae.logs.String())
	}
	fl.failQuery.Store(false)
	if p := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a"}).page(t); len(p.Messages) != 1 {
		t.Errorf("the next call should work again, got %d messages", len(p.Messages))
	}
}

func TestMCPLogsDoNotContainBodiesOrTokens(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	ae.post("/in/agent-a", `{"password":"hunter2-body"}`)
	cs := connectMCP(t, base, ae.tokenA)
	callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a"})
	callTool(t, cs, "get_message", map[string]any{"inbox": "agent-a", "seq": 1})
	logs := ae.logs.String()
	for _, secret := range []string{"hunter2-body", ae.tokenA} {
		if bytes.Contains([]byte(logs), []byte(secret)) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

func TestConcurrentToolCallsShareTheReadLimit(t *testing.T) {
	bs := blockingStore{entered: make(chan struct{}, 8), release: make(chan struct{})}
	ae, base, _ := mcpEnv(t, func(o *Options) {
		bs.Store = o.Store
		o.Store = bs
		o.MaxConcurrentReads = 1
		o.ReadWaitTimeout = 300 * time.Millisecond
	})
	cs := connectMCP(t, base, ae.tokenA)
	first := make(chan toolOut, 1)
	go func() { first <- callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a"}) }()
	<-bs.entered
	o := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a"})
	if !strings.Contains(o.err, "busy") {
		t.Errorf("a second read with no slot free got %q, want a busy error", o.err)
	}
	close(bs.release)
	if r := <-first; r.err != "" {
		t.Errorf("the first read failed: %s", r.err)
	}
}

// afterRecorder records the position each page query starts from.
type afterRecorder struct {
	inbox.Store
	mu     sync.Mutex
	afters []int64
}

func (r *afterRecorder) Query(ctx context.Context, id inbox.ID, q inbox.Query) (*inbox.Page, error) {
	if q.After != nil {
		r.mu.Lock()
		r.afters = append(r.afters, *q.After)
		r.mu.Unlock()
	}
	return r.Store.Query(ctx, id, q)
}

func (r *afterRecorder) last() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.afters) == 0 {
		return -1
	}
	return r.afters[len(r.afters)-1]
}

// A wait that keeps finding nothing it wants must not re-read the same rows
// each time something else arrives.
func TestAWaitDoesNotRescanWhatItHasRuledOut(t *testing.T) {
	rec := &afterRecorder{}
	ae, base, _ := mcpEnv(t, func(o *Options) { rec.Store = o.Store; o.Store = rec })
	upd := *ae.a
	upd.EventTypePointer = "/type"
	if err := ae.store.UpdateInbox(t.Context(), &upd); err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, base, ae.tokenA)
	done := make(chan toolOut, 1)
	go func() {
		done <- callTool(t, cs, "wait_for_messages", map[string]any{
			"inbox": "agent-a", "after": 0, "types": []string{"wanted"}, "timeout_seconds": 20,
		})
	}()
	time.Sleep(200 * time.Millisecond)
	for range 5 {
		ae.post("/in/agent-a", `{"type":"other"}`)
		time.Sleep(30 * time.Millisecond)
	}
	// Each look starts where the previous one ended, so the look that found the
	// fifth message began after the fourth.
	waitFor(t, "the wait to have read past the first four messages", func() bool { return rec.last() == 4 })
	ae.post("/in/agent-a", `{"type":"wanted"}`)
	<-done
}

// ---- what the endpoint refuses before the SDK sees it ----

func mcpPost(t *testing.T, base, tok, body string, extra ...string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, resp.Header, buf.String()
}

func errorCodeOf(t *testing.T, body string) string {
	t.Helper()
	var e struct{ Error struct{ Code string } }
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not a JSON error: %q", body)
	}
	return e.Error.Code
}

// A batch is run by the SDK as many concurrent calls, with every result held in
// memory until all are done: one small request could start thousands of waits.
func TestJSONRPCBatchesAreRefused(t *testing.T) {
	ae, base, n := mcpEnv(t)
	call := `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"wait_for_messages","arguments":{"inbox":"agent-a","after":0,"timeout_seconds":60}}}`
	var parts []string
	for i := range 50 {
		parts = append(parts, fmt.Sprintf(call, i))
	}
	for _, prefix := range []string{"", "  \n\t"} {
		status, _, body := mcpPost(t, base, ae.tokenA, prefix+"["+strings.Join(parts, ",")+"]")
		if status != http.StatusBadRequest || errorCodeOf(t, body) != "invalid_request" || !strings.Contains(body, "batches") {
			t.Errorf("batch (prefix %q): status %d %s", prefix, status, body)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n.Subscribers() != 0 {
		t.Errorf("a refused batch left %d subscriptions", n.Subscribers())
	}
	// The same calls one at a time are fine.
	status, _, _ := mcpPost(t, base, ae.tokenA, fmt.Sprintf(strings.Replace(call, `"timeout_seconds":60`, `"timeout_seconds":0`, 1), 1), "MCP-Protocol-Version", "2025-06-18")
	if status != http.StatusOK {
		t.Errorf("a single call: status %d", status)
	}
}

// Newer protocol versions add methods, such as subscriptions/listen, that hold a
// request open for as long as the client likes. Only what the tools need is served.
func TestMethodsOutsideTheToolsAreRefused(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	for _, m := range []string{"subscriptions/listen", "resources/list", "prompts/list", "logging/setLevel", "sampling/createMessage", ""} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, m)
		status, _, resp := mcpPost(t, base, ae.tokenA, body, "MCP-Protocol-Version", "2026-07-28")
		if status != http.StatusBadRequest || errorCodeOf(t, resp) != "invalid_request" {
			t.Errorf("%q: status %d %s", m, status, resp)
		}
	}
	for _, m := range []string{"initialize", "ping", "tools/list"} {
		status, _, resp := mcpPost(t, base, ae.tokenA, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, m))
		if status != http.StatusOK {
			t.Errorf("%s: status %d %s", m, status, resp)
		}
	}
}

func TestMCPTransportErrorsAreJSONAndNeverCached(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	good := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	status, hdr, _ := mcpPost(t, base, ae.tokenA, good)
	var body string
	if status != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("a normal reply: status %d, Cache-Control %q", status, hdr.Get("Cache-Control"))
	}
	status, hdr, body = mcpPost(t, base, ae.tokenA, "{not json")
	if status != http.StatusBadRequest || errorCodeOf(t, body) != "invalid_request" || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("malformed JSON: status %d %q, Cache-Control %q", status, body, hdr.Get("Cache-Control"))
	}
	status, _, body = mcpPost(t, base, ae.tokenA, good, "Content-Type", "text/plain")
	if status != http.StatusUnsupportedMediaType || errorCodeOf(t, body) != "unsupported_media_type" {
		t.Errorf("wrong content type: status %d %q", status, body)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req, _ := http.NewRequestWithContext(t.Context(), method, base+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+ae.tokenA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" || errorCodeOf(t, b.String()) != "method_not_allowed" {
			t.Errorf("%s: status %d, Allow %q, %q", method, resp.StatusCode, resp.Header.Get("Allow"), b.String())
		}
	}
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` + strings.Repeat("a", 2<<20) + `"}}`
	status, _, body = mcpPost(t, base, ae.tokenA, huge)
	if status != http.StatusRequestEntityTooLarge || errorCodeOf(t, body) != "payload_too_large" {
		t.Errorf("a 2 MiB request: status %d %q", status, body)
	}

	// The tools' own results carry message content and are not cached either.
	ae.post("/in/agent-a", `{"x":1}`)
	status, hdr, _ = mcpPost(t, base, ae.tokenA, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_message","arguments":{"inbox":"agent-a","seq":1}}}`)
	if status != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("a tool result: status %d, Cache-Control %q", status, hdr.Get("Cache-Control"))
	}
}

// ---- limits ----

func TestWaitsAreLimitedPerInboxAndReleased(t *testing.T) {
	ae, base, n := mcpEnv(t, func(o *Options) { o.MaxStreamsPerInbox = 2 })
	cs := connectMCP(t, base, ae.tokenA)
	csB := connectMCP(t, base, ae.tokenB)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wait := func(cs *mcp.ClientSession, slug string, secs int) {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"inbox": slug, "after": 0, "timeout_seconds": secs}})
	}
	go wait(cs, "agent-a", 30)
	go wait(cs, "agent-a", 30)
	waitFor(t, "two waits", func() bool { return n.Subscribers() == 2 })

	o := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 30})
	if !strings.Contains(o.err, "too many waits") {
		t.Errorf("a third wait on the same inbox got %q %s, want a refusal", o.err, o.raw)
	}
	// Another inbox has its own share, and a check without waiting needs none.
	if o := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 0}); o.err != "" {
		t.Errorf("a zero-second wait was refused: %s", o.err)
	}
	if o := callTool(t, csB, "wait_for_messages", map[string]any{"inbox": "agent-b", "after": 0, "timeout_seconds": 0}); o.err != "" {
		t.Errorf("another inbox was refused: %s", o.err)
	}
	// Ending the waits gives the slots back.
	cancel()
	waitFor(t, "the waits to end", func() bool { return n.Subscribers() == 0 })
	var again toolOut
	waitFor(t, "a slot to come free", func() bool {
		again = callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 0})
		return again.err == ""
	})
}

func TestResultsAreBoundedByWhatIsSentNotWhatIsStored(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) { o.MCPInlineBodyBytes = 16 << 10 })
	// Characters that grow the most when escaped: the SDK writes < as six bytes,
	// in two copies of the data, one of them inside a string.
	body := strings.Repeat("<", 15<<10)
	const total = 30
	for range total {
		ae.post("/in/agent-a", body, "Content-Type", "text/plain")
	}
	call := func(after int64) (int, mcpPage) {
		status, _, resp := mcpPost(t, base, ae.tokenA, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_messages","arguments":{"inbox":"agent-a","after":%d,"limit":100,"bodies":true}}}`, after))
		if status != http.StatusOK {
			t.Fatalf("status %d: %.200s", status, resp)
		}
		var env struct {
			Result struct {
				StructuredContent mcpPage `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(resp), &env); err != nil {
			t.Fatal(err)
		}
		return len(resp), env.Result.StructuredContent
	}

	var seen []int64
	after := int64(0)
	for range 40 {
		size, p := call(after)
		if size > mcpResultBudget+64<<10 {
			t.Fatalf("a result of %d bytes, over the %d byte budget", size, mcpResultBudget)
		}
		seen = append(seen, seqsOf(p.Messages)...)
		if len(p.Messages) == 0 {
			break
		}
		after = *p.NextAfter
	}
	if len(seen) != total {
		t.Fatalf("saw %d of %d messages", len(seen), total)
	}
	for i, sq := range seen {
		if sq != int64(i+1) {
			t.Fatalf("seqs = %v: a gap or repeat when results were trimmed", seen)
		}
	}
}

func TestAMessageTooBigToReturnIsFetchedOverREST(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	ae.post("/in/agent-a", strings.Repeat("<", 900<<10), "Content-Type", "text/plain")
	ae.post("/in/agent-a", `{"small":true}`, "Content-Type", "application/json")
	cs := connectMCP(t, base, ae.tokenA)
	o := callTool(t, cs, "get_message", map[string]any{"inbox": "agent-a", "seq": 1})
	if !strings.Contains(o.err, "too large") || !strings.Contains(o.err, "/v1/inboxes/agent-a/messages/1/raw") {
		t.Errorf("get_message of a huge message: %q", o.err)
	}
	if o := callTool(t, cs, "get_message", map[string]any{"inbox": "agent-a", "seq": 2}); o.err != "" {
		t.Errorf("a small message was refused: %s", o.err)
	}
	// It is still in a page, as the only message that fits, and with bodies it is
	// marked as too big to include (it is over the inline limit).
	p := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "after": 0, "bodies": true}).page(t)
	if len(p.Messages) == 0 || seqsOf(p.Messages)[0] != 1 {
		t.Fatalf("the first message must still be deliverable in a page: %v", seqsOf(p.Messages))
	}
	if p.Messages[0]["body_truncated"] != true {
		t.Errorf("a body over the inline limit must be flagged: %v", p.Messages[0])
	}
}

func TestInlineLimitIsExactAndCannotExceedWhatRESTLoads(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) { o.MCPInlineBodyBytes = 100 })
	exact := `{"d":"` + strings.Repeat("a", 100-8) + `"}` // exactly 100 bytes
	if len(exact) != 100 {
		t.Fatalf("test body is %d bytes", len(exact))
	}
	ae.post("/in/agent-a", exact, "Content-Type", "application/json")
	ae.post("/in/agent-a", exact+" ", "Content-Type", "application/json") // 101
	cs := connectMCP(t, base, ae.tokenA)
	p := callTool(t, cs, "query_messages", map[string]any{"inbox": "agent-a", "after": 0, "bodies": true}).page(t)
	p.Messages = flattenUntrusted(p.Messages)
	if p.Messages[0]["body_truncated"] != nil || p.Messages[0]["body"] == nil {
		t.Errorf("a body of exactly the limit must be inline: %v", p.Messages[0])
	}
	if p.Messages[1]["body_truncated"] != true {
		t.Errorf("a body one byte over the limit must be truncated: %v", p.Messages[1])
	}

	big, err := New(Options{Addr: "x", Store: ae.store, MCPInlineBodyBytes: 10 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := big.opts.MCPInlineBodyBytes; got != 64<<10 {
		t.Errorf("MCPInlineBodyBytes = %d, want it limited to the REST inline size", got)
	}
}

// ---- list_inboxes ----

type listCounter struct {
	inbox.Store
	lists atomic.Int64
}

func (l *listCounter) ListInboxes(ctx context.Context, p inbox.ListParams) ([]*inbox.Inbox, error) {
	l.lists.Add(1)
	return l.Store.ListInboxes(ctx, p)
}

func TestListInboxesForAnAgentTokenDoesNotSearchOrRevealOthers(t *testing.T) {
	lc := &listCounter{}
	ae, base, _ := mcpEnv(t, func(o *Options) { lc.Store = o.Store; o.Store = lc })
	for i := range 120 {
		ae.newInbox(fmt.Sprintf("other-%03d", i), "trusted", "")
	}
	ae.post("/in/agent-a", "x")
	cs := connectMCP(t, base, ae.tokenA)
	o := callTool(t, cs, "list_inboxes", nil)
	if lc.lists.Load() != 0 {
		t.Errorf("an agent token's list_inboxes searched the inboxes (%d listings)", lc.lists.Load())
	}
	var l listInboxesOutput
	if err := json.Unmarshal(o.raw, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Inboxes) != 1 || l.Inboxes[0].Slug != "agent-a" || l.Inboxes[0].LatestSeq != 1 || l.Truncated {
		t.Errorf("list_inboxes = %s", o.raw)
	}

	// The admin sees up to the limit and is told there are more.
	admin := connectMCP(t, base, testAdminToken)
	o = callTool(t, admin, "list_inboxes", nil)
	l = listInboxesOutput{}
	if err := json.Unmarshal(o.raw, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Inboxes) != mcpMaxInboxes || !l.Truncated {
		t.Errorf("admin list_inboxes returned %d inboxes, truncated=%v", len(l.Inboxes), l.Truncated)
	}
	if lc.lists.Load() == 0 {
		t.Error("the admin's list did not read the inboxes")
	}
}

// ---- the other tools against REST ----

func TestWaitMatchesRESTAndFiltersApply(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	upd := *ae.a
	upd.EventTypePointer = "/type"
	if err := ae.store.UpdateInbox(t.Context(), &upd); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		ty := "a"
		if i%2 == 0 {
			ty = "b"
		}
		ae.post("/in/agent-a", fmt.Sprintf(`{"type":%q,"n":%d}`, ty, i), "Content-Type", "application/json")
	}
	cs := connectMCP(t, base, ae.tokenA)

	var rest struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(ae.get("/v1/inboxes/agent-a/messages?after=1&type=b&limit=20", ae.tokenA).Body.Bytes(), &rest)
	w := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 1, "types": []string{"b"}, "timeout_seconds": 0, "bodies": true}).page(t)
	w.Messages = flattenUntrusted(w.Messages)
	if !reflect.DeepEqual(w.Messages, rest.Messages) || !reflect.DeepEqual(seqsOf(w.Messages), []int64{2, 4, 6}) {
		t.Errorf("wait %v differs from REST %v", seqsOf(w.Messages), seqsOf(rest.Messages))
	}
	if *w.NextAfter != 6 {
		t.Errorf("next_after = %d", *w.NextAfter)
	}

	for name, c := range map[string]struct {
		args map[string]any
		want []int64
	}{
		"types":      {map[string]any{"types": []string{"a"}, "after": 0}, []int64{1, 3, 5}},
		"verified":   {map[string]any{"verified": true, "after": 0, "limit": 2}, []int64{1, 2}},
		"unverified": {map[string]any{"verified": false, "after": 0}, nil},
		"since":      {map[string]any{"since": "2999-01-01T00:00:00Z", "after": 0}, nil},
		"until":      {map[string]any{"until": "2000-01-01T00:00:00Z", "after": 0}, nil},
		"window":     {map[string]any{"since": "2000-01-01T00:00:00Z", "until": "2999-01-01T00:00:00Z", "after": 4}, []int64{5, 6}},
	} {
		c.args["inbox"] = "agent-a"
		got := seqsOf(callTool(t, cs, "query_messages", c.args).page(t).Messages)
		if len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

// A principal that can read several inboxes, but is not the admin.
type multiAuth struct{ inner inbox.Authenticator }

type multiPrincipal struct{ slugs map[string]bool }

func (p multiPrincipal) CanReadInbox(in *inbox.Inbox) bool { return in != nil && p.slugs[in.Slug] }
func (multiPrincipal) CanAdmin() bool                      { return false }

func (m multiAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	if r.Header.Get("Authorization") == "Bearer multi" {
		return multiPrincipal{slugs: map[string]bool{"inbox-02": true, "inbox-09": true}}, nil
	}
	return m.inner.Authenticate(r)
}

// Running out of scan budget must not be reported: it would tell a caller how
// many inboxes the server holds.
func TestListInboxesDoesNotRevealHowManyInboxesExist(t *testing.T) {
	old := mcpListScan
	mcpListScan = 5
	t.Cleanup(func() { mcpListScan = old })
	ae, base, _ := mcpEnv(t, func(o *Options) { o.Authenticator = multiAuth{o.Authenticator} })
	for i := 1; i <= 10; i++ {
		ae.newInbox(fmt.Sprintf("inbox-%02d", i), "trusted", "")
	}
	cs := connectMCP(t, base, "multi")
	var l listInboxesOutput
	if err := json.Unmarshal(callTool(t, cs, "list_inboxes", nil).raw, &l); err != nil {
		t.Fatal(err)
	}
	if l.Truncated {
		t.Error("truncated was set because the scan budget ran out, which says how many inboxes exist")
	}
	if len(l.Inboxes) != 1 || l.Inboxes[0].Slug != "inbox-02" {
		t.Errorf("list_inboxes = %+v", l.Inboxes)
	}
}

// flattenUntrusted lifts what is under "untrusted" up beside the rest, giving the
// shape the REST API uses, so the two can be compared field for field.
func flattenUntrusted(ms []map[string]any) []map[string]any {
	out := make([]map[string]any, len(ms))
	for i, m := range ms {
		flat := map[string]any{}
		for k, v := range m {
			if k != "untrusted" {
				flat[k] = v
			}
		}
		if u, ok := m["untrusted"].(map[string]any); ok {
			for k, v := range u {
				if _, clash := flat[k]; clash {
					panic("a field is both trusted and untrusted: " + k)
				}
				flat[k] = v
			}
		}
		out[i] = flat
	}
	return out
}

// Reading backward is trimmed to the same budget, and the cursor to continue from
// is the oldest message kept.
func TestABackwardReadIsTrimmedToTheBudgetToo(t *testing.T) {
	ae, base, _ := mcpEnv(t, func(o *Options) { o.MCPInlineBodyBytes = 16 << 10 })
	body := strings.Repeat("<", 15<<10)
	const total = 30
	for range total {
		ae.post("/in/agent-a", body, "Content-Type", "text/plain")
	}
	status, _, resp := mcpPost(t, base, ae.tokenA, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_messages","arguments":{"inbox":"agent-a","limit":100,"bodies":true}}}`)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if len(resp) > mcpResultBudget+(64<<10) {
		t.Errorf("a result of %d bytes, over the %d byte budget", len(resp), mcpResultBudget)
	}
	var env struct {
		Result struct {
			StructuredContent mcpPage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(resp), &env); err != nil {
		t.Fatal(err)
	}
	p := env.Result.StructuredContent
	if len(p.Messages) == 0 || len(p.Messages) >= total {
		t.Fatalf("%d messages: it was not trimmed, or nothing is left", len(p.Messages))
	}
	oldest := seqsOf(p.Messages)[len(p.Messages)-1]
	if p.NextBefore == nil || *p.NextBefore != oldest {
		t.Errorf("next_before = %v, want the oldest message kept (%d)", p.NextBefore, oldest)
	}
}
