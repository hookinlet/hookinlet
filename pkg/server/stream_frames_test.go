// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/specvalidate"
)

// The frames of an event stream are described in the document (`x-frames` on the
// stream's response): the reconnection delay, the heartbeat, and for each kind of
// event its name, what its id is, and the schema of its data. What the server
// writes is held to that.
func TestStreamFramesAreWhatTheDocumentDescribes(t *testing.T) {
	sp := loadSpec(t)
	frames := get(sp.root, "paths", "/v1/inboxes/{slug}/stream", "get", "responses", "200", "content", "text/event-stream", "x-frames").(map[string]any)
	events := frames["events"].([]any)
	if len(events) != 2 || frames["heartbeat"] != "ping" {
		t.Fatalf("the test knows a message and a gap event and a ping heartbeat, the document has %v", frames)
	}
	ev := events[0].(map[string]any)
	gapEv := events[1].(map[string]any)
	if ev["event"] != "message" || gapEv["event"] != "gap" {
		t.Fatalf("events in the document: %v", events)
	}

	ae, base, _ := streamEnv(t, func(o *Options) { o.SSEHeartbeat = 100 * time.Millisecond })
	ae.post("/in/agent-a", `{"a":1,"html":"<b>&</b>"}`)
	ae.post("/in/agent-a", "plain text")
	c := openStream(t, ae.streamURL(base, "agent-a", "after=0"), ae.tokenA, nil)
	var items []sseItem
	for deadline := time.Now().Add(450 * time.Millisecond); time.Now().Before(deadline); {
		if it, ok := c.next(time.Until(deadline)); ok {
			items = append(items, it)
		}
	}

	var messages, pings int
	for i, it := range items {
		switch it.kind {
		case "retry":
			if i != 0 || strings.TrimSpace(it.data) != strconv.Itoa(int(frames["retry"].(float64))) {
				t.Errorf("retry hint at %d: %+v, the document says %v", i, it, frames["retry"])
			}
		case "comment":
			if strings.TrimSpace(it.data) != frames["heartbeat"] {
				t.Errorf("comment %q is not the heartbeat the document describes", it.data)
			}
			pings++
		case "event":
			messages++
			if it.event == "gap" {
				continue // checked in TestAGapIsAnnouncedOnAStream
			}
			if it.event != ev["event"] {
				t.Errorf("event name %q, the document says %q", it.event, ev["event"])
			}
			var data any
			if err := json.Unmarshal([]byte(it.data), &data); err != nil {
				t.Errorf("data is not JSON: %q", it.data)
				continue
			}
			for _, e := range sp.doc.Check(ev["data"], data, specvalidate.Options{ClosedObjects: true}) {
				t.Errorf("data breaks its schema: %s", e)
			}
			if seq := data.(map[string]any)["seq"].(float64); strconv.Itoa(int(seq)) != it.id {
				t.Errorf("id %q is not the message's %s %v", it.id, ev["id"], seq)
			}
		default:
			t.Errorf("a frame of kind %q is not in the document", it.kind)
		}
	}
	if messages != 2 || pings == 0 {
		t.Errorf("saw %d messages and %d heartbeats", messages, pings)
	}
}
