// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build soak

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (d *driver) admin(method, path string, obj any, expect ...int) (int, []byte, error) {
	var b []byte
	if obj != nil {
		b, _ = json.Marshal(obj)
	}
	return d.do("admin", d.adminHC, d.newReq(method, path, d.cfg.adminToken, b), expect...)
}

func (d *driver) post(ib *inbox, path string, id uint64, size int, expect int) (int, []byte, error) {
	body := makeBody(id, size)
	req, _ := http.NewRequest("POST", d.srv.base()+path, bytes.NewReader(body))
	sign(ib, req, id, body, false)
	return d.do("ingest_aux", d.ingestHC, req, expect)
}

// watchStream opens a stream and returns a channel that is closed when the
// server ends it.
func (d *driver) watchStream(slug, token string) (ended <-chan struct{}, status int) {
	ch := make(chan struct{})
	req, _ := http.NewRequest("GET", d.srv.base()+"/v1/inboxes/"+slug+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.streamHC.Do(req)
	if err != nil {
		close(ch)
		return ch, 0
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		close(ch)
		return ch, resp.StatusCode
	}
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		close(ch)
	}()
	return ch, 200
}

func (d *driver) adminLoops(ctx context.Context) {
	rot := &inbox{slug: "rot", kind: "none", acked: map[uint64]uint64{}, read: map[uint64]uint64{}}
	tog := &inbox{slug: "toggle", kind: "none", acked: map[uint64]uint64{}, read: map[uint64]uint64{}}
	for _, ib := range []*inbox{rot, tog} {
		if err := d.createInbox(ib, 0); err != nil {
			d.violate("setup", "%v", err)
			return
		}
	}
	var wg sync.WaitGroup
	every := func(period time.Duration, jitter bool, f func(n int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			sleep(ctx, time.Duration(r.Int63n(int64(period))))
			for n := 0; ctx.Err() == nil; n++ {
				if !d.srv.down.Load() {
					f(n)
				}
				p := period
				if jitter {
					p = period/2 + time.Duration(r.Int63n(int64(period)))
				}
				sleep(ctx, p)
			}
		}()
	}

	// Routine administration.
	every(2*time.Second, true, func(n int) {
		d.admin("GET", "/v1/admin/inboxes?limit=100", nil, 200)
		d.admin("GET", "/v1/admin/inboxes/"+d.inboxes[n%len(d.inboxes)].slug, nil, 200)
		d.admin("GET", "/v1/admin/status", nil, 200)
		d.admin("GET", "/v1/admin/tokens?limit=100", nil, 200)
	})
	// Settings changes under load.
	every(10*time.Second, true, func(n int) {
		cnt := 1000 + 100*(n%2)
		d.admin("PATCH", "/v1/admin/inboxes/ret-none", map[string]any{"retention_max_count": cnt}, 200, 409)
		d.admin("PATCH", "/v1/admin/inboxes/aud-none", map[string]any{"name": fmt.Sprintf("audited none %d", n)}, 200, 409)
	})
	every(20*time.Second, true, func(int) {
		d.admin("POST", "/v1/admin/retention/run", nil, 200, 409)
	})
	// Backups: consistent, and openable.
	every(d.cfg.backupEvery, false, func(n int) {
		path := filepath.Join(d.cfg.out, "backup.db")
		t0 := time.Now()
		req := d.newReq("GET", "/v1/admin/backup", d.cfg.adminToken, nil)
		g0, down0 := d.srv.downEpoch.Load(), d.srv.down.Load()
		resp, err := d.adminHC.Do(req)
		if err != nil {
			d.st.add("backup", d.classify("backup", 0, err, nil, down0, g0, nil), 0)
			return
		}
		f, _ := os.Create(path)
		size, cerr := io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()
		d.st.add("backup", d.classify("backup", resp.StatusCode, cerr, nil, down0, g0, []int{200, 409}), time.Since(t0))
		if resp.StatusCode == 200 && cerr == nil {
			out, err := exec.Command("sqlite3", path, "PRAGMA integrity_check").CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "ok" {
				d.violate("backup_corrupt", "integrity_check of a %d byte backup: %v %s", size, err, out)
			}
			d.event("backup", map[string]any{"bytes": size, "ms": time.Since(t0).Milliseconds()})
		}
		_ = os.Remove(path)
	})
	// Delete and re-create inboxes while the others carry on.
	every(60*time.Second, false, func(n int) { d.churnOnce(ctx, n) })
	// Revoke a token that holds an open stream.
	every(90*time.Second, false, func(n int) { d.revokeOnce(ctx) })
	// Rotate an ingest key.
	every(30*time.Second, false, func(n int) { d.rotateOnce(rot) })
	// Disable and enable.
	every(45*time.Second, false, func(n int) { d.toggleOnce(tog) })
	wg.Wait()
}

func (d *driver) churnOnce(ctx context.Context, n int) {
	slug := fmt.Sprintf("churn-%d", n%3)
	ib := &inbox{slug: slug, kind: "none", acked: map[uint64]uint64{}, read: map[uint64]uint64{}}
	// The slug stays taken until the previous delete's cleanup finishes.
	var err error
	for i := 0; i < 60; i++ {
		g0 := d.srv.downEpoch.Load()
		err = d.createInboxQuiet(ib)
		if err == nil || !strings.Contains(err.Error(), "409") || ctx.Err() != nil || d.srv.downEpoch.Load() != g0 {
			break
		}
		sleep(ctx, time.Second)
	}
	if err != nil {
		if !d.srv.down.Load() && ctx.Err() == nil {
			d.violate("churn_create", "%v", err)
		}
		return
	}
	var meta struct {
		Epoch string `json:"epoch"`
	}
	if _, b, err := d.get("read", d.readHC, "/v1/inboxes/"+slug, ib.token, 200); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	if prev, ok := churnEpochs.Load(slug); ok && prev.(string) == meta.Epoch && meta.Epoch != "" {
		d.violate("epoch_reused", "%s re-created with the same epoch %s", slug, meta.Epoch)
	}
	churnEpochs.Store(slug, meta.Epoch)

	ended, st := d.watchStream(slug, ib.token)
	if st != 200 {
		d.st.add("churn", fmt.Sprintf("stream_%d", st), 0)
	}
	// 300 posts, 8 at a time: the sequence numbers must be exactly 1..300.
	var mu sync.Mutex
	seen := map[uint64]bool{}
	var wg sync.WaitGroup
	var next atomic.Uint64
	var failed atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				id := next.Add(1)
				if id > 300 {
					return
				}
				st, b, err := d.post(ib, ib.path, id|1<<40, 300, 200)
				if err != nil || st != 200 {
					failed.Add(1)
					continue
				}
				var a struct {
					Seq uint64 `json:"seq"`
				}
				_ = json.Unmarshal(b, &a)
				mu.Lock()
				if seen[a.Seq] {
					d.violate("churn_dup_seq", "%s: seq %d handed out twice", slug, a.Seq)
				}
				seen[a.Seq] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed.Load() == 0 {
		for s := uint64(1); s <= 300; s++ {
			if !seen[s] {
				d.violate("churn_seq_gap", "%s: seq %d missing from 1..300", slug, s)
				break
			}
		}
	}
	t0 := time.Now()
	d.admin("DELETE", "/v1/admin/inboxes/"+slug, nil, 204)
	// Gone at once, to senders and to readers.
	req, _ := http.NewRequest("POST", d.srv.base()+ib.path, strings.NewReader("{}"))
	d.do("ingest_aux", d.ingestHC, req, 404)
	d.get("read", d.readHC, "/v1/inboxes/"+slug, ib.token, 401, 404)
	select {
	case <-ended:
		d.st.add("churn", "stream_ended", time.Since(t0))
	case <-time.After(45 * time.Second):
		if !d.srv.down.Load() {
			d.violate("delete_stream_open", "%s: a stream was still open 45s after the inbox was deleted", slug)
		}
	case <-ctx.Done():
	}
}

var churnEpochs sync.Map

// createInboxQuiet is createInbox, but a refusal is an error and not a recorded
// unexpected status (a 409 while a deleted slug is still being cleaned up is fine).
func (d *driver) createInboxQuiet(ib *inbox) error {
	b, _ := json.Marshal(map[string]any{"slug": ib.slug, "verifier": "none"})
	st, body, err := d.do("admin", d.adminHC, d.newReq("POST", "/v1/admin/inboxes", d.cfg.adminToken, b), 201, 409)
	if err != nil {
		return err
	}
	if st != 201 {
		return fmt.Errorf("create %s: %d %s", ib.slug, st, body)
	}
	var r struct {
		Token string `json:"agent_token"`
		Inbox struct {
			Path string `json:"ingest_path"`
		} `json:"inbox"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return err
	}
	ib.token, ib.path = r.Token, r.Inbox.Path
	return nil
}

func (d *driver) revokeOnce(ctx context.Context) {
	st, b, err := d.admin("POST", "/v1/admin/tokens", map[string]any{"name": "soak-revoke", "inboxes": []string{"aud-none"}}, 201, 200)
	if err != nil || (st != 201 && st != 200) {
		return
	}
	var r struct {
		Record struct {
			ID int64 `json:"id"`
		} `json:"record"`
		Token string `json:"token"`
	}
	if json.Unmarshal(b, &r) != nil || r.Token == "" {
		d.violate("token_create", "cannot parse: %s", b)
		return
	}
	d.get("read", d.readHC, "/v1/inboxes/aud-none", r.Token, 200)
	ended, sst := d.watchStream("aud-none", r.Token)
	if sst != 200 {
		return
	}
	t0 := time.Now()
	d.admin("DELETE", fmt.Sprintf("/v1/admin/tokens/%d", r.Record.ID), nil, 204)
	d.get("read", d.readHC, "/v1/inboxes/aud-none", r.Token, 401)
	select {
	case <-ended:
		ms := time.Since(t0).Milliseconds()
		for {
			cur := d.revokeLag.Load()
			if ms <= cur || d.revokeLag.CompareAndSwap(cur, ms) {
				break
			}
		}
	case <-time.After(45 * time.Second):
		if !d.srv.down.Load() {
			d.violate("revoke_not_enforced", "stream of a revoked token still open after 45s")
		}
	case <-ctx.Done():
	}
}

func (d *driver) rotateOnce(rot *inbox) {
	st, b, err := d.admin("POST", "/v1/admin/inboxes/rot/rotate-ingest-key", nil, 200)
	if err != nil || st != 200 {
		return
	}
	var r struct {
		Path  string `json:"ingest_path"`
		Inbox struct {
			Path string `json:"ingest_path"`
		} `json:"inbox"`
	}
	_ = json.Unmarshal(b, &r)
	np := r.Path
	if np == "" {
		np = r.Inbox.Path
	}
	if np == "" || np == rot.path {
		d.violate("rotate", "no new ingest path in %s", b)
		return
	}
	old := rot.path
	rot.path = np
	req, _ := http.NewRequest("POST", d.srv.base()+old, strings.NewReader("{}"))
	d.do("ingest_aux", d.ingestHC, req, 404)
	d.post(rot, np, 1, 100, 200)
}

func (d *driver) toggleOnce(tog *inbox) {
	if st, _, err := d.admin("PATCH", "/v1/admin/inboxes/toggle", map[string]any{"disabled": true}, 200); err != nil || st != 200 {
		return
	}
	d.post(tog, tog.path, 2, 100, 404)
	d.get("read", d.readHC, "/v1/inboxes/toggle", tog.token, 200)
	d.admin("PATCH", "/v1/admin/inboxes/toggle", map[string]any{"disabled": false}, 200)
	d.post(tog, tog.path, 3, 100, 200)
}

// streamCapTest opens more streams with one token than the per-credential limit
// allows; the surplus must be refused with 503 and the limit not exceeded.
func (d *driver) streamCapTest(ctx context.Context) {
	ib := &inbox{slug: "capdemo", kind: "none"}
	if err := d.createInboxQuiet(ib); err != nil {
		return
	}
	var ok, shed, other int
	var cancels []context.CancelFunc
	for i := 0; i < 130; i++ {
		cctx, cancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(cctx, "GET", d.srv.base()+"/v1/inboxes/capdemo/stream", nil)
		req.Header.Set("Authorization", "Bearer "+ib.token)
		resp, err := d.streamHC.Do(req)
		switch {
		case err != nil:
			other++
			cancel()
			continue
		case resp.StatusCode == 200:
			ok++
			cancels = append(cancels, func() { resp.Body.Close(); cancel() })
		case resp.StatusCode == 503:
			shed++
			resp.Body.Close()
			cancel()
		default:
			other++
			resp.Body.Close()
			cancel()
		}
	}
	d.event("stream_cap", map[string]any{"opened": ok, "refused_503": shed, "other": other, "attempted": 130})
	if ok > 100 || other > 0 {
		d.violate("stream_cap", "one token held %d streams (limit 100), %d odd outcomes", ok, other)
	}
	sleep(ctx, 3*time.Second)
	for _, c := range cancels {
		c()
	}
	d.admin("DELETE", "/v1/admin/inboxes/capdemo", nil, 204)
}

// trickle is a request body that is sent slowly and, when abortAt is positive,
// fails part way, the way a sender that loses its connection does.
type trickle struct {
	left, abortAt, sent int
	chunk               int
	pause               time.Duration
}

func (t *trickle) Read(p []byte) (int, error) {
	if t.abortAt > 0 && t.sent >= t.abortAt {
		return 0, errors.New("sender gave up")
	}
	if t.left == 0 {
		return 0, io.EOF
	}
	n := t.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > t.left {
		n = t.left
	}
	copy(p[:n], padBytes)
	time.Sleep(t.pause)
	t.left -= n
	t.sent += n
	return n, nil
}

func (d *driver) metricsLines(prefix string) []string {
	req := d.newReq("GET", "/metrics", d.cfg.adminToken, nil)
	resp, err := d.adminHC.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// ingestStorm holds far more request-body bytes in flight than the server's
// memory budget allows, most of them abandoned part way, while the ordinary
// workload carries on.
func (d *driver) ingestStorm(ctx context.Context, dur time.Duration) {
	var bigs []*inbox
	for i := 0; i < 10; i++ {
		ib := &inbox{slug: fmt.Sprintf("big-%d", i), kind: "none"}
		if err := d.createInbox(ib, 10<<20); err != nil {
			d.violate("setup", "%v", err)
			return
		}
		bigs = append(bigs, ib)
	}
	d.storm.Store(true)
	defer d.storm.Store(false)
	sctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var maxUsed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for sctx.Err() == nil {
			if m, err := d.scrapeMetrics(); err == nil {
				v := int64(m["hookinlet_ingest_memory_used_bytes"])
				for cur := maxUsed.Load(); v > cur && !maxUsed.CompareAndSwap(cur, v); cur = maxUsed.Load() {
				}
				if v > int64(m["hookinlet_ingest_memory_budget_bytes"]) {
					d.violate("budget_exceeded", "ingest memory in use %d is over the budget", v)
				}
			}
			sleep(sctx, 500*time.Millisecond)
		}
	}()
	before := d.metricsLines("hookinlet_deliveries_total")
	for w := 0; w < 80; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			hc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			for sctx.Err() == nil {
				ib := bigs[r.Intn(len(bigs))]
				total := 9 << 20
				tr := &trickle{left: total, chunk: 256 << 10, pause: 100 * time.Millisecond}
				if r.Intn(100) != 0 {
					tr.abortAt = 3<<20 + r.Intn(6<<20)
				}
				req, _ := http.NewRequestWithContext(sctx, "POST", d.srv.base()+ib.path, tr)
				req.ContentLength = int64(total)
				req.Header.Set("Content-Type", "application/octet-stream")
				resp, err := hc.Do(req)
				switch {
				case err != nil && sctx.Err() != nil:
				case err != nil:
					d.st.add("storm_ingest", "aborted", 0)
				case resp.StatusCode == 200:
					d.st.add("storm_ingest", "ok", 0)
				case resp.StatusCode == 503 || resp.StatusCode == 429:
					d.st.add("storm_ingest", fmt.Sprintf("shed%d", resp.StatusCode), 0)
				default:
					d.unexpected("storm_ingest", fmt.Sprint(resp.StatusCode), "")
					d.st.add("storm_ingest", fmt.Sprintf("UNEXPECTED_%d", resp.StatusCode), 0)
				}
				if resp != nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(w)
	}
	wg.Wait()
	after := d.metricsLines("hookinlet_deliveries_total")
	d.event("ingest_storm", map[string]any{"max_ingest_mem_used": maxUsed.Load(), "deliveries_before": before, "deliveries_after": after})
	for _, ib := range bigs {
		d.admin("DELETE", "/v1/admin/inboxes/"+ib.slug, nil, 204)
	}
}

// readStorm hammers the read API with large pages, from the admin token, from
// agent tokens, and from clients that ask and never read.
func (d *driver) readStorm(ctx context.Context, dur time.Duration) {
	var slow *inbox
	if st, b, err := d.admin("POST", "/v1/admin/tokens", map[string]any{"name": "soak-slow", "inboxes": []string{"aud-none"}}, 201, 200); err == nil && (st == 201 || st == 200) {
		var r struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(b, &r)
		slow = &inbox{slug: "aud-none", token: r.Token}
	}
	d.storm.Store(true)
	defer d.storm.Store(false)
	sctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < 60; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			for sctx.Err() == nil {
				ib := d.inboxes[r.Intn(5)]
				tok := ib.token
				if w < 40 {
					tok = d.cfg.adminToken
				}
				ib.mu.Lock()
				top := ib.lastSeq
				ib.mu.Unlock()
				after := uint64(0)
				if top > 600 {
					after = uint64(r.Int63n(int64(top - 500)))
				}
				d.get("storm_read", d.readHC, fmt.Sprintf("/v1/inboxes/%s/messages?after=%d&limit=500", ib.slug, after), tok, 200)
			}
		}(w)
	}
	if slow != nil {
		for w := 0; w < 12; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for sctx.Err() == nil {
					req, _ := http.NewRequestWithContext(sctx, "GET", d.srv.base()+"/v1/inboxes/aud-none/messages?after=0&limit=500", nil)
					req.Header.Set("Authorization", "Bearer "+slow.token)
					resp, err := d.readHC.Do(req)
					if err != nil {
						sleep(sctx, 200*time.Millisecond)
						continue
					}
					tag := "held"
					if resp.StatusCode == 503 || resp.StatusCode == 429 {
						tag = fmt.Sprintf("shed%d", resp.StatusCode)
					}
					d.st.add("storm_slowread", tag, 0)
					sleep(sctx, 15*time.Second) // never read it
					resp.Body.Close()
				}
			}()
		}
	}
	wg.Wait()
	d.event("read_storm", nil)
}

// verifyAll waits for the cursor followers to catch up, then compares what was
// acknowledged with what is stored, with an independent full read.
func (d *driver) verifyAll(ctx context.Context) {
	d.srv.setDown(false)
	for _, ib := range d.inboxes {
		if !ib.audited {
			continue
		}
		var latest uint64
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			var m struct {
				Latest uint64 `json:"latest_seq"`
			}
			if _, b, err := d.get("admin", d.adminHC, "/v1/inboxes/"+ib.slug, d.cfg.adminToken, 200); err == nil && json.Unmarshal(b, &m) == nil {
				latest = m.Latest
			}
			ib.mu.Lock()
			caught := ib.lastSeq == latest && latest > 0
			ib.mu.Unlock()
			if caught {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		ib.mu.Lock()
		missing, mismatch := 0, 0
		for id, seq := range ib.acked {
			got, ok := ib.read[id]
			switch {
			case !ok:
				missing++
				if missing <= 3 {
					d.violate("acked_message_lost", "%s: message %d acknowledged as seq %d is not in the cursor follower's view", ib.slug, id, seq)
				}
			case got != seq:
				mismatch++
				if mismatch <= 3 {
					d.violate("acked_seq_mismatch", "%s: message %d acknowledged as seq %d is stored as %d", ib.slug, id, seq, got)
				}
			}
		}
		follSeq, nAcked, nRead := ib.lastSeq, len(ib.acked), len(ib.read)
		ib.mu.Unlock()

		// An independent read from the start.
		scan, scanSeq := d.scan(ib)
		var extra, scanMissing int
		ib.mu.Lock()
		for id, seq := range ib.acked {
			if s, ok := scan[id]; !ok || s != seq {
				scanMissing++
			}
		}
		extra = len(scan) - nAcked
		ib.mu.Unlock()
		if scanMissing > 0 {
			d.violate("acked_message_lost", "%s: %d acknowledged messages missing or moved in a fresh full read", ib.slug, scanMissing)
		}
		if uint64(len(scan)) != scanSeq {
			d.violate("scan_count", "%s: %d messages but latest_seq %d", ib.slug, len(scan), scanSeq)
		}
		d.event("verify", map[string]any{"inbox": ib.slug, "latest_seq": latest, "follower_seq": follSeq, "acked": nAcked,
			"follower_read": nRead, "fresh_scan": len(scan), "stored_not_acked": extra, "acked_missing": missing, "seq_mismatch": mismatch})
	}
}

// scan reads an inbox from the start with the admin token and returns id->seq.
func (d *driver) scan(ib *inbox) (map[uint64]uint64, uint64) {
	out := map[uint64]uint64{}
	next := "/v1/inboxes/" + ib.slug + "/messages?after=0&limit=500"
	var last uint64
	for tries := 0; tries < 20; {
		st, b, err := d.get("read", d.readHC, next, d.cfg.adminToken, 200)
		if err != nil || st != 200 {
			tries++
			time.Sleep(time.Second)
			continue
		}
		var p page
		if json.Unmarshal(b, &p) != nil {
			return out, last
		}
		for _, m := range p.Messages {
			if m.Seq != last+1 {
				d.violate("scan_gap", "%s: seq %d after %d in a fresh full read", ib.slug, m.Seq, last)
			}
			last = m.Seq
			if prev, dup := out[m.Body.N]; dup {
				d.violate("scan_duplicate", "%s: message %d at seq %d and %d", ib.slug, m.Body.N, prev, m.Seq)
			}
			out[m.Body.N] = m.Seq
		}
		if len(p.Messages) == 0 || last >= p.LatestSeq {
			return out, last
		}
		next = p.Next
	}
	return out, last
}
