// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

type epochPage struct {
	Epoch     string           `json:"epoch"`
	Gap       bool             `json:"gap"`
	Next      *string          `json:"next"`
	LatestSeq int              `json:"latest_seq"`
	Messages  []map[string]any `json:"messages"`
}

// Everything that hands out a cursor carries the inbox's epoch.
func TestEpochIsCarriedEverywhereACursorIs(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "one")
	want := e.a.Epoch
	if want == "" {
		t.Fatal("the inbox has no epoch")
	}
	info := decode[map[string]any](t, e.get("/v1/inboxes/agent-a", e.tokenA))
	if info["epoch"] != want {
		t.Errorf("inbox info epoch = %v, want %v", info["epoch"], want)
	}
	pg := decode[epochPage](t, e.get("/v1/inboxes/agent-a/messages?after=0", e.tokenA))
	if pg.Epoch != want || len(pg.Messages) != 1 || pg.Messages[0]["epoch"] != want {
		t.Errorf("page epoch %q, message epoch %v, want %q", pg.Epoch, pg.Messages[0]["epoch"], want)
	}
	if pg.Next == nil || !strings.Contains(*pg.Next, "epoch="+want) {
		t.Errorf("the next link does not carry the epoch: %v", pg.Next)
	}
	one := decode[map[string]any](t, e.get("/v1/inboxes/agent-a/messages/1", e.tokenA))
	if one["epoch"] != want {
		t.Errorf("a single message's epoch = %v", one["epoch"])
	}
	// Following the link is accepted: it names the epoch it came from.
	if rec := e.get(*pg.Next, e.tokenA); rec.Code != http.StatusOK {
		t.Errorf("following next: %d %s", rec.Code, rec.Body)
	}
}

// A cursor from another epoch is refused, with a status and a code of its own.
func TestACursorFromAnotherEpochIsRefused(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "one")
	for _, path := range []string{
		"/v1/inboxes/agent-a/messages?after=0&epoch=0123456789abcdef",
		"/v1/inboxes/agent-a/messages?epoch=0123456789abcdef",
		"/v1/inboxes/agent-a/messages?after=1&epoch=" + e.a.Epoch + "x",
	} {
		rec := e.get(path, e.tokenA)
		if rec.Code != http.StatusConflict || errorCode(t, rec) != "epoch_changed" {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := e.do(req{method: http.MethodHead, path: "/v1/inboxes/agent-a/messages?epoch=abc", headers: http.Header{"Authorization": {"Bearer " + e.tokenA}}}); rec.Code != http.StatusConflict {
		t.Errorf("HEAD: %d", rec.Code)
	}
	// The right epoch, and no epoch, are both fine.
	for _, path := range []string{"/v1/inboxes/agent-a/messages?after=0&epoch=" + e.a.Epoch, "/v1/inboxes/agent-a/messages?after=0"} {
		if rec := e.get(path, e.tokenA); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	// An epoch that is not even shaped like one is a plain bad request.
	if rec := e.get("/v1/inboxes/agent-a/messages?epoch=a%20b", e.tokenA); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed epoch: %d", rec.Code)
	}
}

// An inbox made again under the same slug is a new life: the old cursor says so.
func TestARecreatedInboxHasANewEpoch(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "one")
	e.post("/in/agent-a", "two")
	old := e.a.Epoch
	cursor := "/v1/inboxes/agent-a/messages?after=2&epoch=" + old
	if rec := e.get(cursor, e.tokenA); rec.Code != http.StatusOK {
		t.Fatalf("before: %d", rec.Code)
	}
	if err := e.store.DeleteInbox(t.Context(), e.a.ID); err != nil {
		t.Fatal(err)
	}
	again := e.newInbox("agent-a", "trusted", "")
	e.tokenFor(again)
	if again.Epoch == old {
		t.Fatal("the new inbox has the old epoch")
	}
	tok := e.tokenFor(again)
	rec := e.get(cursor, tok)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "epoch_changed" {
		t.Errorf("the old cursor on the new inbox: %d %s", rec.Code, rec.Body)
	}
	// Without the epoch the number alone would have passed as a cursor: this is the
	// quiet failure the epoch exists to prevent.
	if rec := e.get("/v1/inboxes/agent-a/messages?after=2", tok); rec.Code != http.StatusOK {
		t.Errorf("a bare cursor: %d", rec.Code)
	}
}

// A reader whose cursor is older than the oldest message kept is told.
func TestAGapIsReported(t *testing.T) {
	e := newAgentEnv(t)
	in := e.newInbox("gappy", "trusted", "", func(i *inbox.Inbox) { i.RetentionMaxCount = 2 })
	tok := e.tokenFor(in)
	for range 5 {
		e.post("/in/gappy", "x")
	}
	if _, err := e.store.Prune(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// Seqs 4 and 5 remain.
	for _, tc := range []struct {
		after int
		gap   bool
	}{{0, false}, {1, true}, {2, true}, {3, false}, {4, false}, {5, false}} {
		pg := decode[epochPage](t, e.get("/v1/inboxes/gappy/messages?after="+strconv.Itoa(tc.after), tok))
		if pg.Gap != tc.gap {
			t.Errorf("after=%d: gap = %v, want %v", tc.after, pg.Gap, tc.gap)
		}
	}
	// Reading backward has no cursor to have outlived.
	if pg := decode[epochPage](t, e.get("/v1/inboxes/gappy/messages", tok)); pg.Gap {
		t.Error("a backward read reported a gap")
	}
	// Everything removed is a gap too.
	empty := e.newInbox("emptied", "trusted", "", func(i *inbox.Inbox) { i.RetentionMaxCount = 1 })
	et := e.tokenFor(empty)
	for range 3 {
		e.post("/in/emptied", "x")
	}
	if _, err := e.store.Prune(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if pg := decode[epochPage](t, e.get("/v1/inboxes/emptied/messages?after=1", et)); !pg.Gap {
		t.Error("after=1 with only seq 3 left should report a gap")
	}
}

// A stream refuses a cursor from another epoch before it sends anything, and says
// so once at the start when its cursor is behind what is kept.
func TestStreamsKnowAboutEpochsAndGaps(t *testing.T) {
	ae, base, _ := streamEnv(t)
	in := ae.newInbox("streamy", "trusted", "", func(i *inbox.Inbox) { i.RetentionMaxCount = 2 })
	tok := ae.tokenFor(in)
	for range 5 {
		ae.post("/in/streamy", "x")
	}
	if _, err := ae.store.Prune(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if c := openStream(t, ae.streamURL(base, "streamy", "after=0&epoch=deadbeef"), tok, nil); c.status != http.StatusConflict {
		t.Errorf("a stream with another epoch: status %d, want 409", c.status)
	}
	if c := openStream(t, ae.streamURL(base, "streamy", "after=4&epoch="+in.Epoch), tok, nil); c.status != http.StatusOK {
		t.Errorf("a stream with the right epoch: status %d", c.status)
	}

	// A cursor of 1 with seqs 4 and 5 left: a gap event, then the two messages.
	c := openStream(t, ae.streamURL(base, "streamy", "after=1"), tok, nil)
	var kinds []string
	var gap map[string]any
	for len(kinds) < 3 {
		it, ok := c.next(2 * time.Second)
		if !ok {
			break
		}
		if it.kind == "retry" {
			continue
		}
		kinds = append(kinds, it.event)
		if it.event == "gap" {
			if it.id != "" {
				t.Errorf("the gap event has an id (%q), which would move Last-Event-ID", it.id)
			}
			if err := json.Unmarshal([]byte(it.data), &gap); err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.Join(kinds, ",") != "gap,message,message" {
		t.Fatalf("events: %v", kinds)
	}
	if gap["after"] != 1.0 || gap["oldest_seq"] != 4.0 || gap["epoch"] != in.Epoch {
		t.Errorf("gap event data: %v", gap)
	}

	// No gap, no event.
	d := openStream(t, ae.streamURL(base, "streamy", "after=3"), tok, nil)
	for {
		it, ok := d.next(500 * time.Millisecond)
		if !ok {
			break
		}
		if it.event == "gap" {
			t.Error("a gap was announced to a reader that missed nothing")
		}
	}
}

// The MCP tools take and return the epoch.
func TestMCPToolsKnowAboutEpochsAndGaps(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	in := ae.newInbox("mcp-epoch", "trusted", "", func(i *inbox.Inbox) { i.RetentionMaxCount = 2 })
	tok := ae.tokenFor(in)
	for range 4 {
		ae.post("/in/mcp-epoch", "x")
	}
	if _, err := ae.store.Prune(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, base, tok)

	list := callTool(t, cs, "list_inboxes", map[string]any{})
	if !strings.Contains(string(list.raw), in.Epoch) {
		t.Errorf("list_inboxes does not show the epoch: %s", list.raw)
	}
	var page struct {
		Epoch string `json:"epoch"`
		Gap   bool   `json:"gap"`
	}
	for _, tc := range []struct {
		after int
		gap   bool
	}{{0, false}, {1, true}, {2, false}} {
		out := callTool(t, cs, "query_messages", map[string]any{"inbox": "mcp-epoch", "after": tc.after, "epoch": in.Epoch})
		if out.err != "" {
			t.Fatalf("query_messages: %s", out.err)
		}
		if err := json.Unmarshal(out.raw, &page); err != nil {
			t.Fatal(err)
		}
		if page.Epoch != in.Epoch || page.Gap != tc.gap {
			t.Errorf("after=%d: epoch %q gap %v, want %q %v", tc.after, page.Epoch, page.Gap, in.Epoch, tc.gap)
		}
	}
	for name, args := range map[string]map[string]any{
		"query_messages":    {"inbox": "mcp-epoch", "after": 0, "epoch": "someotherepoch"},
		"wait_for_messages": {"inbox": "mcp-epoch", "after": 0, "epoch": "someotherepoch", "timeout_seconds": 0},
	} {
		out := callTool(t, cs, name, args)
		if !strings.HasPrefix(out.err, "epoch_changed") {
			t.Errorf("%s with another epoch: %q", name, out.err)
		}
	}
	// A wait whose cursor is behind what is kept returns at once, not after its timeout.
	begin := time.Now()
	out := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "mcp-epoch", "after": 1, "timeout_seconds": 20, "epoch": in.Epoch})
	if time.Since(begin) > 5*time.Second || out.err != "" {
		t.Errorf("a wait behind the oldest message: %v, %q", time.Since(begin), out.err)
	}
}
