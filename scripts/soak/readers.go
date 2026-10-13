// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build soak

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type page struct {
	Epoch     string `json:"epoch"`
	Gap       bool   `json:"gap"`
	LatestSeq uint64 `json:"latest_seq"`
	Next      string `json:"next"`
	Messages  []struct {
		Seq  uint64 `json:"seq"`
		Body struct {
			N uint64 `json:"n"`
		} `json:"body"`
	} `json:"messages"`
}

// follower reads an inbox from the start by following `next`, the way an agent
// does, and checks what comes back: strictly increasing seq, no gaps on an
// inbox without retention, and no message twice.
func (d *driver) follower(ctx context.Context, ib *inbox) {
	next := "/v1/inboxes/" + ib.slug + "/messages?after=0&limit=200"
	for ctx.Err() == nil {
		st, body, err := d.get("read", d.readHC, next, ib.token, 200)
		if err != nil || st != 200 {
			if st == 409 {
				d.violate("epoch_changed", "%s: cursor refused with 409: %s", ib.slug, body)
			}
			sleep(ctx, 300*time.Millisecond)
			continue
		}
		var p page
		if err := json.Unmarshal(body, &p); err != nil {
			d.violate("bad_json", "%s page: %v", ib.slug, err)
			sleep(ctx, time.Second)
			continue
		}
		ib.mu.Lock()
		for _, m := range p.Messages {
			switch {
			case m.Seq <= ib.lastSeq:
				d.violate("rest_not_increasing", "%s: seq %d after %d", ib.slug, m.Seq, ib.lastSeq)
			case ib.audited && m.Seq != ib.lastSeq+1 && !p.Gap:
				d.violate("rest_gap", "%s: seq %d after %d with no gap flag", ib.slug, m.Seq, ib.lastSeq)
			}
			if ib.audited {
				if prev, dup := ib.read[m.Body.N]; dup {
					d.violate("rest_duplicate", "%s: message %d at seq %d and %d", ib.slug, m.Body.N, prev, m.Seq)
				}
				ib.read[m.Body.N] = m.Seq
			}
			if m.Seq > ib.lastSeq {
				ib.lastSeq = m.Seq
			}
		}
		ib.epoch = p.Epoch
		ib.mu.Unlock()
		if p.Next != "" {
			next = p.Next
		}
		if len(p.Messages) == 0 {
			sleep(ctx, 120*time.Millisecond)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// sseClient follows an inbox over SSE. Auditors stay connected (reconnecting
// only when the server goes away); chaos clients drop at random and resume with
// Last-Event-ID. Both check that every message arrives exactly once, in order.
func (d *driver) sseClient(ctx context.Context, ib *inbox, auditor bool, seed int64) {
	r := rand.New(rand.NewSource(seed))
	var cursor uint64
	first := true
	if !auditor {
		// Start from "now" rather than replaying a growing inbox.
		for ctx.Err() == nil {
			st, b, err := d.get("read", d.readHC, "/v1/inboxes/"+ib.slug, ib.token, 200)
			var m struct {
				Latest uint64 `json:"latest_seq"`
			}
			if err == nil && st == 200 && json.Unmarshal(b, &m) == nil {
				cursor = m.Latest
				break
			}
			sleep(ctx, time.Second)
		}
	}
	for ctx.Err() == nil {
		cctx, cancel := context.WithCancel(ctx)
		var life time.Duration
		stalled := false
		switch {
		case auditor:
			life = 24 * time.Hour
		case r.Intn(100) < 4:
			stalled, life = true, 35*time.Second
		default:
			life = 500*time.Millisecond + time.Duration(r.ExpFloat64()*float64(15*time.Second))
			if life > 90*time.Second {
				life = 90 * time.Second
			}
		}
		timer := time.AfterFunc(life, cancel)
		cursor = d.streamOnce(cctx, ib, cursor, first, stalled)
		first = false
		timer.Stop()
		cancel()
		if !auditor {
			sleep(ctx, time.Duration(r.Intn(400))*time.Millisecond)
		} else {
			sleep(ctx, 300*time.Millisecond)
		}
	}
}

// streamOnce opens one stream from cursor, reads until it ends, and returns the
// new cursor. The first connection uses ?after=; later ones send Last-Event-ID,
// as a reconnecting EventSource would.
func (d *driver) streamOnce(ctx context.Context, ib *inbox, cursor uint64, first, stalled bool) uint64 {
	url := d.srv.base() + "/v1/inboxes/" + ib.slug + "/stream"
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+ib.token)
	if first {
		req.URL.RawQuery = "after=" + strconv.FormatUint(cursor, 10)
	} else {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(cursor, 10))
	}
	g0, down0 := d.srv.downEpoch.Load(), d.srv.down.Load()
	t0 := time.Now()
	resp, err := d.streamHC.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			d.st.add("sse_connect", d.classify("sse_connect", 0, err, nil, down0, g0, nil), 0)
			sleep(ctx, 500*time.Millisecond)
		}
		return cursor
	}
	defer resp.Body.Close()
	tag := d.classify("sse_connect", resp.StatusCode, nil, nil, down0, g0, []int{200})
	d.st.add("sse_connect", tag, time.Since(t0))
	if resp.StatusCode != 200 {
		sleep(ctx, time.Second)
		return cursor
	}
	if stalled { // a client that stops reading: the server must cope and eventually let go
		<-ctx.Done()
		return cursor
	}
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	var event string
	var id uint64
	gapPending := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return cursor
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event == "message" && id != 0 {
				d.sseMsgs.Add(1)
				switch {
				case id <= cursor:
					d.violate("sse_duplicate_or_reorder", "%s: id %d after %d", ib.slug, id, cursor)
				case ib.audited && id != cursor+1 && !gapPending:
					d.violate("sse_skip", "%s: id %d after %d without a gap event", ib.slug, id, cursor)
				}
				if id > cursor {
					cursor = id
				}
				gapPending = false
			} else if event == "gap" {
				gapPending = true
				if ib.audited {
					d.violate("sse_unexpected_gap", "%s: gap event on an inbox without retention", ib.slug)
				}
			}
			event, id = "", 0
		case strings.HasPrefix(line, ":"):
			d.ssePings.Add(1)
		case strings.HasPrefix(line, "id: "):
			id, _ = strconv.ParseUint(line[4:], 10, 64)
		case strings.HasPrefix(line, "event: "):
			event = line[7:]
		}
	}
}

// pollers are agents that poll the retained inboxes and read around the API.
func (d *driver) readMix(ctx context.Context, seed int64) {
	r := rand.New(rand.NewSource(seed))
	for ctx.Err() == nil {
		ib := d.inboxes[r.Intn(len(d.inboxes))]
		base := "/v1/inboxes/" + ib.slug
		switch r.Intn(6) {
		case 0:
			d.get("read", d.readHC, base+"/messages?limit=20", ib.token, 200)
		case 1:
			d.get("read", d.readHC, base+"/messages?limit=100&fields=summary", ib.token, 200)
		case 2:
			d.get("read", d.readHC, base, ib.token, 200)
		case 3:
			d.get("read", d.readHC, base+"/messages?limit=50&verified=false", ib.token, 200)
		case 4, 5:
			var m struct {
				Latest uint64 `json:"latest_seq"`
			}
			if st, b, err := d.get("read", d.readHC, base, ib.token, 200); err == nil && st == 200 && json.Unmarshal(b, &m) == nil && m.Latest > 0 {
				seq := 1 + uint64(r.Int63n(int64(m.Latest)))
				suffix := ""
				if r.Intn(2) == 0 {
					suffix = "/raw"
				}
				// Retained inboxes may have pruned it: 404 is right there.
				if ib.audited {
					d.get("read", d.readHC, fmt.Sprintf("%s/messages/%d%s", base, seq, suffix), ib.token, 200)
				} else {
					d.get("read", d.readHC, fmt.Sprintf("%s/messages/%d%s", base, seq, suffix), ib.token, 200, 404)
				}
			}
		}
		sleep(ctx, time.Duration(20+r.Intn(150))*time.Millisecond)
	}
}

func (d *driver) mcpCall(token, tool string, args map[string]any) (map[string]any, error) {
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}}
	b, _ := json.Marshal(req)
	r := d.newReq("POST", "/mcp", token, b)
	r.Header.Set("Accept", "application/json, text/event-stream")
	st, body, err := d.do("mcp", d.readHC, r, 200)
	if err != nil || st != 200 {
		return nil, err
	}
	var out struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		d.violate("mcp_bad_json", "%s: %v", tool, err)
		return nil, err
	}
	if out.Result.IsError && len(out.Result.Content) > 0 && strings.Contains(out.Result.Content[0].Text, "busy") {
		// The tool's way of saying 503: overload, not a fault.
		d.st.add("mcp", "shed_busy", 0)
		return nil, fmt.Errorf("busy")
	}
	if out.Error != nil || out.Result.IsError {
		d.violate("mcp_error", "%s %v: %s", tool, args, body)
		return nil, fmt.Errorf("mcp error")
	}
	return out.Result.StructuredContent, nil
}

func (d *driver) mcpLoop(ctx context.Context, seed int64) {
	r := rand.New(rand.NewSource(seed))
	for ctx.Err() == nil {
		ib := d.inboxes[r.Intn(len(d.inboxes))]
		switch r.Intn(4) {
		case 0:
			d.mcpCall(ib.token, "list_inboxes", map[string]any{})
		case 1:
			d.mcpCall(ib.token, "query_messages", map[string]any{"inbox": ib.slug, "limit": 20})
		case 2:
			if res, err := d.mcpCall(ib.token, "query_messages", map[string]any{"inbox": ib.slug, "limit": 5}); err == nil {
				if ms, _ := res["messages"].([]any); len(ms) > 0 {
					if m, _ := ms[0].(map[string]any); m != nil {
						if seq, ok := m["seq"].(float64); ok {
							d.mcpCall(ib.token, "get_message", map[string]any{"inbox": ib.slug, "seq": int64(seq)})
						}
					}
				}
			}
		case 3:
			// Wait from the current end: returns when something arrives or the time is up.
			if res, err := d.mcpCall(ib.token, "query_messages", map[string]any{"inbox": ib.slug, "limit": 1}); err == nil {
				after, _ := res["latest_seq"].(float64)
				d.mcpCall(ib.token, "wait_for_messages", map[string]any{"inbox": ib.slug, "after": int64(after), "timeout_seconds": 1 + r.Intn(4)})
			}
		}
		sleep(ctx, time.Duration(50+r.Intn(300))*time.Millisecond)
	}
}

// startLoad launches the steady workload.
func (d *driver) startLoad(wctx, fctx context.Context, spawn func(*sync.WaitGroup, context.Context, func(context.Context)), wwg, fwg *sync.WaitGroup) {
	spawn(wwg, wctx, d.ingestLoad)
	for _, ib := range d.inboxes {
		ib := ib
		spawn(fwg, fctx, func(c context.Context) { d.follower(c, ib) })
		if ib.audited {
			spawn(fwg, fctx, func(c context.Context) { d.sseClient(c, ib, true, 1) })
		}
	}
	for i := 0; i < d.cfg.sse; i++ {
		i := i
		ib := d.inboxes[i%len(d.inboxes)]
		spawn(fwg, fctx, func(c context.Context) {
			sleep(c, time.Duration(i)*20*time.Millisecond) // ramp the connections
			d.sseClient(c, ib, false, int64(1000+i))
		})
	}
	for i := 0; i < 4; i++ {
		i := i
		spawn(fwg, fctx, func(c context.Context) { d.readMix(c, int64(i)) })
	}
	for i := 0; i < 6; i++ {
		i := i
		spawn(fwg, fctx, func(c context.Context) { d.mcpLoop(c, int64(50+i)) })
	}
	spawn(fwg, fctx, d.adminLoops)
}
