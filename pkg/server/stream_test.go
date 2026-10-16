// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// ---- a small SSE client, parsing the wire format as EventSource does ----

type sseItem struct {
	kind  string // "event", "comment", or "retry"
	id    string
	event string
	data  string
}

type sseClient struct {
	t      *testing.T
	status int
	header http.Header
	items  chan sseItem
	cancel context.CancelFunc
	done   chan struct{} // closed when the stream ends
}

// sseHTTP gives up if the server has not started answering within five seconds,
// so a server that wrongly never responds fails a test instead of hanging it.
var sseHTTP = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}}

// openStream connects and returns once the response headers have arrived.
func openStream(t *testing.T, url, tok string, hdr http.Header) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := sseHTTP.Do(req) //nolint:bodyclose // closed by the reader goroutine below, or when the status is not 200
	if err != nil {
		cancel()
		t.Fatalf("connecting to %s: %v", url, err)
	}
	c := &sseClient{t: t, status: resp.StatusCode, header: resp.Header, items: make(chan sseItem, 4096), cancel: cancel, done: make(chan struct{})}
	t.Cleanup(c.close)
	go func() {
		defer close(c.done)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return
		}
		r := bufio.NewReaderSize(resp.Body, 1<<20)
		var cur sseItem
		var data []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if len(data) > 0 {
					cur.kind, cur.data = "event", strings.Join(data, "\n")
					c.items <- cur
				}
				cur, data = sseItem{}, nil
			case strings.HasPrefix(line, ":"):
				c.items <- sseItem{kind: "comment", data: strings.TrimSpace(line[1:])}
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "id":
					cur.id = value
				case "event":
					cur.event = value
				case "data":
					data = append(data, value)
				case "retry":
					c.items <- sseItem{kind: "retry", data: value}
				}
			}
		}
	}()
	return c
}

func (c *sseClient) close() { c.cancel() }

// next returns the next item, or false if none arrives in time.
func (c *sseClient) next(within time.Duration) (sseItem, bool) {
	select {
	case it := <-c.items:
		return it, true
	case <-time.After(within):
		return sseItem{}, false
	}
}

// nextEvent skips comments and returns the next message event.
func (c *sseClient) nextEvent(within time.Duration) (sseItem, bool) {
	deadline := time.Now().Add(within)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return sseItem{}, false
		}
		it, ok := c.next(left)
		if !ok {
			return sseItem{}, false
		}
		if it.kind == "event" {
			return it, true
		}
	}
}

// events collects message events until none arrives for a while.
func (c *sseClient) events(quiet time.Duration) []sseItem {
	var out []sseItem
	for {
		it, ok := c.nextEvent(quiet)
		if !ok {
			return out
		}
		out = append(out, it)
	}
}

// ended reports whether the stream has finished within the time.
func (c *sseClient) ended(within time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(within):
		return false
	}
}

func idOf(t *testing.T, it sseItem) int64 {
	t.Helper()
	n, err := strconv.ParseInt(it.id, 10, 64)
	if err != nil {
		t.Fatalf("event id %q is not a seq", it.id)
	}
	return n
}

func (ae *agentEnv) streamURL(base, slug, query string) string {
	u := base + "/v1/inboxes/" + slug + "/stream"
	if query != "" {
		u += "?" + query
	}
	return u
}

// streamEnv is an agentEnv with a local notifier it can inspect, served on a socket.
func streamEnv(t *testing.T, mods ...func(*Options)) (*agentEnv, string, *local.Notifier) {
	t.Helper()
	return streamEnvOn(t, net.ListenConfig{}, mods...)
}

// streamEnvOn is streamEnv served on a listener made with lc.
func streamEnvOn(t *testing.T, lc net.ListenConfig, mods ...func(*Options)) (*agentEnv, string, *local.Notifier) {
	t.Helper()
	ae, n := streamEnvUnserved(t, mods...)
	return ae, serveOnWith(t, ae.env, lc), n
}

// streamEnvUnserved is streamEnv before it is served, for a test that serves it itself.
func streamEnvUnserved(t *testing.T, mods ...func(*Options)) (*agentEnv, *local.Notifier) {
	t.Helper()
	n := local.New()
	all := append([]func(*Options){func(o *Options) { o.Notifier = n }}, mods...)
	e := newAgentEnv2(t, all...)
	ae := &agentEnv{env: e.env, a: e.a}
	tokA := e.tokenFor(e.a)
	ae.tokenA = tokA
	b := e.newInbox("agent-b", "trusted", "")
	ae.b = b
	ae.tokenB = e.tokenFor(b)
	return ae, n
}

// ---- format and basics ----

func TestStreamDeliversNewMessagesAsEvents(t *testing.T) {
	ae, base, _ := streamEnv(t)
	ae.post("/in/agent-a", `{"before":"connect"}`) // exists before the stream; must not be replayed

	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	if c.status != http.StatusOK || c.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", c.status, c.header.Get("Content-Type"))
	}
	for _, h := range []string{"Cache-Control:no-store", "X-Content-Type-Options:nosniff", "X-Accel-Buffering:no"} {
		k, v, _ := strings.Cut(h, ":")
		if c.header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, c.header.Get(k), v)
		}
	}
	if it, ok := c.next(2 * time.Second); !ok || it.kind != "retry" || it.data != "3000" {
		t.Fatalf("first item = %+v, want retry: 3000", it)
	}
	if it, ok := c.nextEvent(300 * time.Millisecond); ok {
		t.Fatalf("an event arrived before anything new was posted (no replay by default): %+v", it)
	}

	for i := 1; i <= 3; i++ {
		ae.post("/in/agent-a", fmt.Sprintf(`{"n":%d}`, i), "Content-Type", "application/json")
	}
	for i := 2; i <= 4; i++ { // seq 1 was posted before connecting
		it, ok := c.nextEvent(3 * time.Second)
		if !ok {
			t.Fatalf("event %d never arrived", i)
		}
		if it.event != "message" || idOf(t, it) != int64(i) {
			t.Errorf("event = %+v, want a message with id %d", it, i)
		}
		var m apiMessage
		if err := json.Unmarshal([]byte(it.data), &m); err != nil || m.Seq != int64(i) || m.Inbox != "agent-a" || string(m.Body) != fmt.Sprintf(`{"n":%d}`, i-1) {
			t.Errorf("data = %s (%v)", it.data, err)
		}
	}
}

// The event data is the same JSON the REST API returns for the message.
func TestStreamDataMatchesTheRESTRepresentation(t *testing.T) {
	ae, base, _ := streamEnv(t)
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second) // retry
	ae.do(req{method: http.MethodPut, path: "/in/agent-a/x/y?q=1", body: `{"id":7}`, headers: http.Header{"Content-Type": {"application/json"}, "X-Custom": {"v"}}})
	it, ok := c.nextEvent(3 * time.Second)
	if !ok {
		t.Fatal("no event")
	}
	rest := ae.get("/v1/inboxes/agent-a/messages/1", ae.tokenA)
	var fromStream, fromREST map[string]any
	if err := json.Unmarshal([]byte(it.data), &fromStream); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(rest.Body.Bytes(), &fromREST)
	if a, b := mustJSON(fromStream), mustJSON(fromREST); a != b {
		t.Errorf("stream and REST differ:\n%s\n%s", a, b)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestStreamStartingPoints(t *testing.T) {
	ae, base, _ := streamEnv(t)
	for i := 1; i <= 5; i++ {
		ae.post("/in/agent-a", fmt.Sprintf("m%d", i))
	}
	collect := func(query string, hdr http.Header) []int64 {
		c := openStream(t, ae.streamURL(base, "agent-a", query), ae.tokenA, hdr)
		var ids []int64
		for _, it := range c.events(400 * time.Millisecond) {
			ids = append(ids, idOf(t, it))
		}
		return ids
	}
	same := func(name string, got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	same("no cursor: from now, no replay", collect("", nil))
	same("after=0 replays everything", collect("after=0", nil), 1, 2, 3, 4, 5)
	same("after=3", collect("after=3", nil), 4, 5)
	same("after=5 is caught up", collect("after=5", nil))
	same("Last-Event-ID resumes", collect("", http.Header{"Last-Event-Id": {"2"}}), 3, 4, 5)
	same("Last-Event-ID beats after (a reconnecting client reuses its URL)", collect("after=0", http.Header{"Last-Event-Id": {"4"}}), 5)
	same("an empty Last-Event-ID means none yet", collect("after=3", http.Header{"Last-Event-Id": {""}}), 4, 5)
}

func TestStreamRequestsAreChecked(t *testing.T) {
	ae, base, _ := streamEnv(t)
	for name, tc := range map[string]struct {
		url  string
		tok  string
		hdr  http.Header
		code int
	}{
		"no token":                     {ae.streamURL(base, "agent-a", ""), "", nil, 401},
		"wrong token":                  {ae.streamURL(base, "agent-a", ""), "hki_nope", nil, 401},
		"another inbox's token":        {ae.streamURL(base, "agent-a", ""), ae.tokenB, nil, 404},
		"no such inbox":                {ae.streamURL(base, "ghost", ""), ae.tokenA, nil, 404},
		"a parameter the stream lacks": {ae.streamURL(base, "agent-a", "limit=5"), ae.tokenA, nil, 400},
		"fields":                       {ae.streamURL(base, "agent-a", "fields=summary"), ae.tokenA, nil, 400},
		"repeated after":               {ae.streamURL(base, "agent-a", "after=1&after=2"), ae.tokenA, nil, 400},
		"bad after":                    {ae.streamURL(base, "agent-a", "after=x"), ae.tokenA, nil, 400},
		"bad verified":                 {ae.streamURL(base, "agent-a", "verified=2"), ae.tokenA, nil, 400},
		"malformed query":              {ae.streamURL(base, "agent-a", "after=%zz"), ae.tokenA, nil, 400},
		"bad Last-Event-ID":            {ae.streamURL(base, "agent-a", ""), ae.tokenA, http.Header{"Last-Event-Id": {"abc"}}, 400},
		"negative Last-Event-ID":       {ae.streamURL(base, "agent-a", ""), ae.tokenA, http.Header{"Last-Event-Id": {"-1"}}, 400},
	} {
		c := openStream(t, tc.url, tc.tok, tc.hdr)
		if c.status != tc.code {
			t.Errorf("%s: status %d, want %d", name, c.status, tc.code)
		}
	}
	// The same 404 for another inbox's token and for an inbox that is not there.
	a, _ := do2(t, http.MethodGet, ae.streamURL(base, "agent-a", ""), http.Header{"Authorization": {"Bearer " + ae.tokenB}}, "")
	b, _ := do2(t, http.MethodGet, ae.streamURL(base, "ghost", ""), http.Header{"Authorization": {"Bearer " + ae.tokenB}}, "")
	if a != 404 || b != 404 {
		t.Errorf("statuses %d and %d", a, b)
	}
	// Only GET.
	for _, m := range []string{http.MethodPost, http.MethodHead, http.MethodDelete} {
		status, _ := do2(t, m, ae.streamURL(base, "agent-a", ""), http.Header{"Authorization": {"Bearer " + ae.tokenA}}, "")
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, status)
		}
	}
	// The admin token can follow any inbox.
	if c := openStream(t, ae.streamURL(base, "agent-b", ""), testAdminToken, nil); c.status != 200 {
		t.Errorf("admin: status %d", c.status)
	}
}

func TestStreamFilters(t *testing.T) {
	ae, base, _ := streamEnv(t)
	in := ae.newInbox("filtered", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	_ = in
	send := func(event, secret string, n int) {
		body := fmt.Sprintf(`{"n":%d}`, n)
		ae.post("/in/filtered", body, "X-Hub-Signature-256", githubSig(secret, body), "X-GitHub-Event", event)
	}
	c1 := openStream(t, ae.streamURL(base, "filtered", "type=push&type=issues"), testAdminToken, nil)
	c2 := openStream(t, ae.streamURL(base, "filtered", "verified=false"), testAdminToken, nil)
	c3 := openStream(t, ae.streamURL(base, "filtered", "type=push&verified=true"), testAdminToken, nil)
	c1.next(time.Second)
	c2.next(time.Second)
	c3.next(time.Second)
	send("push", "s", 1)     // seq 1: push, verified
	send("ping", "s", 2)     // seq 2: ping, verified
	send("push", "wrong", 3) // seq 3: unverified (and no event type is trusted)
	send("issues", "s", 4)   // seq 4: issues, verified
	ids := func(c *sseClient) string {
		var out []string
		for _, it := range c.events(500 * time.Millisecond) {
			out = append(out, it.id)
		}
		return strings.Join(out, ",")
	}
	if got := ids(c1); got != "1,4" {
		t.Errorf("type=push|issues: %q, want 1,4", got)
	}
	if got := ids(c2); got != "3" {
		t.Errorf("verified=false: %q, want 3", got)
	}
	if got := ids(c3); got != "1" {
		t.Errorf("type=push&verified=true: %q, want 1", got)
	}
}

// A sender controls the body, headers, and query. None may be able to end an
// event early and make the client see a second one.
func TestStreamCannotBeForgedBySenders(t *testing.T) {
	ae, base, _ := streamEnv(t)
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second)
	nasty := []string{
		"x\n\nid: 999\nevent: message\ndata: {\"seq\":999}\n\n",
		"x\r\n\r\nid: 998\r\n\r\n",
		`{"a":"\n\nid: 997\n\n"}`,
		"data: pretend\n\n",
		"  ",
	}
	for i, body := range nasty {
		ae.do(req{path: "/in/agent-a?q=a%0A%0Aid:+5", body: body, headers: http.Header{
			"X-Evil":       {"a\tb"},
			"Content-Type": {"text/plain"},
		}})
		_ = i
	}
	events := c.events(600 * time.Millisecond)
	if len(events) != len(nasty) {
		t.Fatalf("%d events for %d messages; a sender forged or hid events", len(events), len(nasty))
	}
	for i, it := range events {
		if idOf(t, it) != int64(i+1) {
			t.Errorf("event %d has id %s", i, it.id)
		}
		var m apiMessage
		if err := json.Unmarshal([]byte(it.data), &m); err != nil || m.Seq != int64(i+1) {
			t.Errorf("event %d data: %v %s", i, err, it.data)
		}
	}
}

func TestStreamBodiesSurviveIntact(t *testing.T) {
	ae, base, _ := streamEnv(t)
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second)
	big := `{"pad":"` + strings.Repeat("x", 200<<10) + `"}` // over the 64 KiB inline limit
	ae.do(req{path: "/in/agent-a", body: big, headers: http.Header{"Content-Type": {"application/json"}}})
	ae.do(req{path: "/in/agent-a", body: string([]byte{0xff, 0xfe, 0x00, 0x80})})
	got := c.events(1500 * time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("%d events", len(got))
	}
	var m1, m2 apiMessage
	_ = json.Unmarshal([]byte(got[0].data), &m1)
	_ = json.Unmarshal([]byte(got[1].data), &m2)
	if string(m1.Body) != big {
		t.Errorf("the large JSON body changed (%d bytes back, %d sent)", len(m1.Body), len(big))
	}
	if m2.BodyBase64 == nil || *m2.BodyBase64 != "//4AgA==" {
		t.Errorf("binary body = %v", m2.BodyBase64)
	}
}

// ---- the gate ----

// With a notifier that drops every signal, the recheck interval alone must
// still deliver every message.
func TestStreamSurvivesANotifierThatDropsEverySignal(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) {
		o.Notifier = inbox.NopNotifier{} // never signals: every signal is dropped
		o.SSERecheckInterval = time.Second
	})
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second)
	start := time.Now()
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ae.post("/in/agent-a", fmt.Sprintf("m%d", i)) })
	}
	wg.Wait()
	var ids []int64
	for _, it := range c.events(2500 * time.Millisecond) {
		ids = append(ids, idOf(t, it))
	}
	if len(ids) != n {
		t.Fatalf("received %d of %d messages with every notifier signal dropped", len(ids), n)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("ids = %v: out of order or duplicated", ids)
		}
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("delivery took %v with a 1s recheck", d)
	}
}

func TestWithoutARecheckADroppedSignalMeansNoDelivery(t *testing.T) {
	// The control for the test above: this is why the recheck interval exists.
	ae, base, _ := streamEnv(t, func(o *Options) { o.Notifier = inbox.NopNotifier{} })
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second)
	ae.post("/in/agent-a", "x")
	if it, ok := c.nextEvent(1500 * time.Millisecond); ok {
		t.Errorf("an event arrived with no signal and no recheck: %+v", it)
	}
}

func TestStreamSendsHeartbeats(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.SSEHeartbeat = 100 * time.Millisecond })
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second) // retry
	pings := 0
	deadline := time.Now().Add(1500 * time.Millisecond)
	for pings < 3 && time.Now().Before(deadline) {
		if it, ok := c.next(time.Second); ok && it.kind == "comment" && it.data == "ping" {
			pings++
		}
	}
	if pings < 3 {
		t.Errorf("saw %d heartbeats in 1.5s with a 100ms interval", pings)
	}
	// A heartbeat is a comment, not a message.
	ae.post("/in/agent-a", "x")
	if it, ok := c.nextEvent(2 * time.Second); !ok || idOf(t, it) != 1 {
		t.Errorf("a message after the pings: %+v, %v", it, ok)
	}
}

func TestMessagesDelayHeartbeats(t *testing.T) {
	// The heartbeat is for idle streams: while messages flow, no ping is needed. A message every
	// 100ms against a 1s heartbeat leaves nine tenths of a second for a loaded machine to stall the
	// poster before a ping would be legitimate; the run is long enough for an unreset timer to fire twice.
	ae, base, _ := streamEnv(t, func(o *Options) { o.SSEHeartbeat = time.Second })
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second)
	stop := time.After(2500 * time.Millisecond)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	pings, messages := 0, 0
loop:
	for {
		select {
		case <-tick.C:
			ae.post("/in/agent-a", "x")
		case it := <-c.items:
			switch it.kind {
			case "comment":
				pings++
			case "event":
				messages++
			}
		case <-stop:
			break loop
		}
	}
	if messages == 0 {
		t.Fatal("no message reached the stream, so the test showed nothing")
	}
	if pings != 0 {
		t.Errorf("%d pings while a message arrived every 100ms with a 1s heartbeat", pings)
	}
}

// A client that connects, leaves mid-stream, and reconnects with Last-Event-ID,
// while deliveries keep arriving, ends up with every message exactly once.
func TestReconnectingWithLastEventIDLosesNothing(t *testing.T) {
	ae, base, _ := streamEnv(t)
	const total = 300
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 3 {
		wg.Go(func() {
			for i := range total / 3 {
				ae.post("/in/agent-a", fmt.Sprintf("w%d-%d", w, i))
				select {
				case <-stop:
					return
				case <-time.After(time.Duration(1+i%3) * time.Millisecond):
				}
			}
		})
	}

	var seen []int64
	last := ""
	for round := 0; len(seen) < total && round < 200; round++ {
		hdr := http.Header{}
		if last != "" {
			hdr.Set("Last-Event-ID", last)
		}
		// The first connection replays from the start; later ones resume.
		query := "after=0"
		c := openStream(t, ae.streamURL(base, "agent-a", query), ae.tokenA, hdr)
		// Read a few events, then drop the connection abruptly mid-stream.
		take := 1 + round%17
		for range take {
			it, ok := c.nextEvent(2 * time.Second)
			if !ok {
				break
			}
			id := idOf(t, it)
			seen = append(seen, id)
			last = it.id
		}
		c.close()
		<-c.done
	}
	close(stop)
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("received %d of %d messages across reconnects", len(seen), total)
	}
	for i, id := range seen {
		if id != int64(i+1) {
			t.Fatalf("position %d holds seq %d: a message was lost or repeated across a reconnect (ids %v)", i, id, seen[max(0, i-3):min(len(seen), i+4)])
		}
	}
}

func TestShutdownClosesStreamsWithinTheTimeout(t *testing.T) {
	const streams = 40
	e := newAgentEnv2(t, func(o *Options) { o.ShutdownTimeout = 3 * time.Second })
	tok := e.tokenFor(e.a)

	var lc = func() (string, context.CancelFunc, chan error) {
		l, err := newListener(t)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- e.server.Serve(ctx, l) }()
		return "http://" + l.Addr().String(), cancel, done
	}
	base, shutdown, done := lc()

	clients := make([]*sseClient, 0, streams)
	for range streams {
		c := openStream(t, base+"/v1/inboxes/agent-a/stream", tok, nil)
		if c.status != 200 {
			t.Fatalf("status %d", c.status)
		}
		clients = append(clients, c)
	}
	for _, c := range clients {
		c.next(2 * time.Second) // retry: the stream is established
	}

	begin := time.Now()
	shutdown()
	for i, c := range clients {
		if !c.ended(3 * time.Second) {
			t.Fatalf("stream %d was still open %v after shutdown began", i, time.Since(begin))
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v after a clean shutdown", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not return")
	}
	if d := time.Since(begin); d > 3*time.Second {
		t.Errorf("shutdown took %v with %d idle streams (timeout 3s)", d, streams)
	}
}

// Closing streams must not cut off a delivery that is being stored.
func TestShutdownLetsAnInFlightDeliveryFinish(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	e := newAgentEnv2(t, func(o *Options) { o.ShutdownTimeout = 5 * time.Second })
	e.hooks.before = func(*inbox.Inbox, int64) error {
		entered <- struct{}{}
		<-release
		return nil
	}
	tok := e.tokenFor(e.a)
	l, err := newListener(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.server.Serve(ctx, l) }()
	base := "http://" + l.Addr().String()

	stream := openStream(t, base+"/v1/inboxes/agent-a/stream", tok, nil)
	stream.next(2 * time.Second)

	result := make(chan int, 1)
	go func() {
		status, _ := do2(t, http.MethodPost, base+"/in/agent-a", nil, "slow delivery")
		result <- status
	}()
	<-entered // the delivery is now in flight

	cancel() // start shutting down
	if !stream.ended(3 * time.Second) {
		t.Fatal("the idle stream stayed open during shutdown")
	}
	select {
	case status := <-result:
		t.Fatalf("the in-flight delivery ended early with status %d", status)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if status := <-result; status != http.StatusOK {
		t.Errorf("the in-flight delivery got %d after being released", status)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
	if n := e.count(e.a); n != 1 {
		t.Errorf("%d messages stored; the in-flight delivery was lost", n)
	}
}

// ---- limits and resource handling ----

func TestStreamLimitAndRelease(t *testing.T) {
	ae, base, n := streamEnv(t, func(o *Options) { o.MaxStreams = 2; o.MaxStreamsPerInbox = 2 })
	c1 := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c2 := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	if c1.status != 200 || c2.status != 200 {
		t.Fatalf("statuses %d, %d", c1.status, c2.status)
	}
	c1.next(time.Second)
	c2.next(time.Second)
	if n.Subscribers() != 2 {
		t.Errorf("%d subscriptions for 2 streams", n.Subscribers())
	}

	third := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	if third.status != http.StatusServiceUnavailable || third.header.Get("Retry-After") == "" {
		t.Errorf("a third stream with a limit of 2: status %d, Retry-After %q", third.status, third.header.Get("Retry-After"))
	}
	// An unauthenticated request cannot use up a stream slot.
	if c := openStream(t, ae.streamURL(base, "agent-a", ""), "", nil); c.status != 401 {
		t.Errorf("unauthenticated: status %d", c.status)
	}

	// Leaving frees the slot and the subscription.
	c1.close()
	<-c1.done
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && n.Subscribers() != 1 {
		time.Sleep(20 * time.Millisecond)
	}
	if n.Subscribers() != 1 {
		t.Errorf("%d subscriptions after one client left, want 1", n.Subscribers())
	}
	if c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil); c.status != 200 {
		t.Errorf("after a slot was freed: status %d", c.status)
	}
}

// Streams are long-lived, so they must not use up the slots that bound how many
// pages are built at once.
func TestStreamsDoNotUseReadSlots(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.MaxConcurrentReads = 2 })
	var clients []*sseClient
	for range 10 {
		c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
		if c.status != 200 {
			t.Fatalf("status %d", c.status)
		}
		c.next(time.Second)
		clients = append(clients, c)
	}
	if rec := ae.get("/v1/inboxes/agent-a/messages", ae.tokenA); rec.Code != http.StatusOK {
		t.Errorf("a normal read with 10 streams open and 2 read slots: status %d, %s", rec.Code, rec.Body.String())
	}
	ae.post("/in/agent-a", "hello")
	for i, c := range clients {
		if _, ok := c.nextEvent(3 * time.Second); !ok {
			t.Errorf("stream %d got nothing", i)
		}
	}
}

func TestStreamEndsWhenItsInboxIsDeleted(t *testing.T) {
	ae, base, n := streamEnv(t)
	c := openStream(t, ae.streamURL(base, "agent-b", ""), ae.tokenB, nil)
	c.next(time.Second)
	if err := ae.store.DeleteInbox(t.Context(), ae.b.ID); err != nil {
		t.Fatal(err)
	}
	// Nothing signals a deletion, but the next time the stream looks, the inbox
	// is gone. Poke it the way a real signal would.
	n.Publish(t.Context(), ae.b.ID, 1)
	if !c.ended(3 * time.Second) {
		t.Error("the stream stayed open after its inbox was deleted")
	}
}

func TestReplayingALargeBacklogDeliversEverythingOnce(t *testing.T) {
	ae, base, _ := streamEnv(t)
	const total = 2500 // many pages of 100
	for i := range total {
		if _, _, err := ae.store.Append(t.Context(), ae.a.ID, &inbox.NewMessage{Method: "POST", Verified: true, Body: []byte(fmt.Sprintf("m%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	c := openStream(t, ae.streamURL(base, "agent-a", "after=0"), ae.tokenA, nil)
	var ids []int64
	for _, it := range c.events(1500 * time.Millisecond) {
		ids = append(ids, idOf(t, it))
	}
	if len(ids) != total {
		t.Fatalf("replayed %d of %d", len(ids), total)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("position %d holds %d", i, id)
		}
	}
	// And it keeps following afterwards.
	ae.post("/in/agent-a", "live")
	if it, ok := c.nextEvent(3 * time.Second); !ok || idOf(t, it) != total+1 {
		t.Errorf("the live message after the replay: %+v, %v", it, ok)
	}
}

func TestAFilteredStreamDoesNotRescanOldRows(t *testing.T) {
	// Only one message in a long inbox matches. After delivering it, the stream's
	// cursor jumps to the latest seq, so later wake-ups do not look at old rows.
	ae, base, _ := streamEnv(t)
	for i := range 300 {
		ev := ""
		if i == 10 {
			ev = "rare"
		}
		_, _, _ = ae.store.Append(t.Context(), ae.a.ID, &inbox.NewMessage{Method: "POST", Verified: true, EventType: ev, Body: []byte("x")})
	}
	c := openStream(t, ae.streamURL(base, "agent-a", "after=0&type=rare"), ae.tokenA, nil)
	it, ok := c.nextEvent(3 * time.Second)
	if !ok || idOf(t, it) != 11 {
		t.Fatalf("the rare message: %+v, %v", it, ok)
	}
	for range 5 {
		ae.post("/in/agent-a", "noise")
	}
	ae.do(req{path: "/in/agent-a", body: "rare again", headers: http.Header{}})
	if extra, ok := c.nextEvent(500 * time.Millisecond); ok {
		t.Errorf("an unmatched message was delivered: %+v", extra)
	}
}

func TestStreamsNeverLogTokensOrBodies(t *testing.T) {
	ae, base, _ := streamEnv(t)
	c := openStream(t, ae.streamURL(base, "agent-a", "after=0"), ae.tokenA, nil)
	c.next(time.Second)
	ae.post("/in/agent-a", "BODY-MARKER-4242")
	c.nextEvent(2 * time.Second)
	c.close()
	<-c.done
	time.Sleep(100 * time.Millisecond)
	logs := ae.logs.String()
	for _, secret := range []string{ae.tokenA, ae.tokenB, testAdminToken, "BODY-MARKER-4242"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain %q", secret)
		}
	}
}

var _ = httptest.NewRecorder
