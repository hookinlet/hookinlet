// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build soak

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// inbox is one inbox the driver created, with what it needs to send to it, read
// it, and judge what came back.
type inbox struct {
	slug     string
	kind     string // none, github, stripe, token
	secret   string
	audited  bool // no retention: every acknowledged message must be readable
	maxCount int
	onStore  bool // on_verify_fail=store: a bad signature is stored, unverified
	weight   int

	token string // the inbox's agent token

	mu       sync.Mutex
	path     string            // /in/<key>
	acked    map[uint64]uint64 // message id -> seq, as acknowledged to the sender
	read     map[uint64]uint64 // message id -> seq, as seen by the cursor follower
	lastSeq  uint64            // follower position
	maxAcked uint64
	epoch    string
}

func (ib *inbox) ingestPath() string {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.path
}

func (d *driver) ackedMaxSeqs() map[string]uint64 {
	out := map[string]uint64{}
	for _, ib := range d.inboxes {
		ib.mu.Lock()
		out[ib.slug] = ib.maxAcked
		ib.mu.Unlock()
	}
	return out
}

func (d *driver) ackedTotal() int {
	n := 0
	for _, ib := range d.inboxes {
		ib.mu.Lock()
		n += len(ib.acked)
		ib.mu.Unlock()
	}
	return n
}

func (d *driver) retainedCounts() map[string]int64 {
	out := map[string]int64{}
	for _, ib := range d.inboxes {
		if ib.maxCount == 0 {
			continue
		}
		var m struct {
			Count int64 `json:"message_count"`
		}
		if st, b, err := d.get("admin", d.adminHC, "/v1/inboxes/"+ib.slug, d.cfg.adminToken, 200); err == nil && st == 200 {
			_ = json.Unmarshal(b, &m)
			out[ib.slug] = m.Count
			// The sweep runs every second, so allow a second of arrivals, and a restart.
			if m.Count > int64(ib.maxCount)+int64(4*d.cfg.rate) {
				d.violate("retention_unbounded", "%s holds %d messages with max count %d", ib.slug, m.Count, ib.maxCount)
			}
		}
	}
	return out
}

// setup creates the inboxes.
func (d *driver) setup(ctx context.Context) error {
	specs := []*inbox{
		{slug: "aud-none", kind: "none", audited: true, weight: 18},
		{slug: "aud-github", kind: "github", audited: true, weight: 18},
		{slug: "aud-stripe", kind: "stripe", audited: true, weight: 18},
		{slug: "aud-token", kind: "token", audited: true, weight: 18},
		{slug: "aud-ghstore", kind: "github", audited: true, onStore: true, weight: 8},
		{slug: "ret-none", kind: "none", maxCount: 1000, weight: 10},
		{slug: "ret-github", kind: "github", maxCount: 500, weight: 10},
	}
	for _, ib := range specs {
		ib.acked, ib.read = map[uint64]uint64{}, map[uint64]uint64{}
		if ib.kind != "none" {
			ib.secret = "whsec_" + randB64(24)
		}
		if err := d.createInbox(ib, 0); err != nil {
			return err
		}
		d.inboxes = append(d.inboxes, ib)
	}
	return nil
}

// createInbox creates ib (maxBody 0: the default) and fills in its token and path.
func (d *driver) createInbox(ib *inbox, maxBody int) error {
	req := map[string]any{"slug": ib.slug, "verifier": ib.kind}
	if ib.secret != "" {
		req["verifier_secret"] = ib.secret
	}
	if ib.maxCount > 0 {
		req["retention_max_count"] = ib.maxCount
	}
	if ib.onStore {
		req["on_verify_fail"] = "store"
	}
	if maxBody > 0 {
		req["max_body_bytes"] = maxBody
	}
	b, _ := json.Marshal(req)
	st, body, err := d.do("admin", d.adminHC, d.newReq("POST", "/v1/admin/inboxes", d.cfg.adminToken, b), 201)
	if err != nil || st != 201 {
		return fmt.Errorf("create %s: %d %v %s", ib.slug, st, err, body)
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

// payload builds a JSON body of about size bytes whose id is n.
var padBytes = func() []byte {
	b := make([]byte, 3<<20)
	r := rand.New(rand.NewSource(7))
	for i := range b {
		b[i] = 'a' + byte(r.Intn(26))
	}
	return b
}()

func makeBody(n uint64, size int) []byte {
	head := fmt.Sprintf(`{"n":%d,"pad":"`, n)
	pad := size - len(head) - 2
	if pad < 0 {
		pad = 0
	}
	var buf bytes.Buffer
	buf.Grow(size + 8)
	buf.WriteString(head)
	buf.Write(padBytes[:pad])
	buf.WriteString(`"}`)
	return buf.Bytes()
}

func sizeFor(r *rand.Rand, audited bool) int {
	x := r.Float64()
	if audited {
		switch {
		case x < 0.92:
			return 150 + r.Intn(3850)
		case x < 0.995:
			return 8<<10 + r.Intn(40<<10)
		default:
			return 100<<10 + r.Intn(100<<10)
		}
	}
	switch {
	case x < 0.85:
		return 150 + r.Intn(3850)
	case x < 0.97:
		return 8<<10 + r.Intn(56<<10)
	case x < 0.995:
		return 128<<10 + r.Intn(384<<10)
	default:
		return 800<<10 + r.Intn(200<<10) // just under the 1 MiB default limit
	}
}

func hmacHex(secret string, parts ...[]byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	for _, p := range parts {
		m.Write(p)
	}
	return hex.EncodeToString(m.Sum(nil))
}

// sign sets the headers the inbox's verifier wants. With bad, the signature is
// wrong.
func sign(ib *inbox, req *http.Request, id uint64, body []byte, bad bool) {
	req.Header.Set("Content-Type", "application/json")
	switch ib.kind {
	case "github":
		sig := hmacHex(ib.secret, body)
		if bad {
			sig = hmacHex(ib.secret+"x", body)
		}
		req.Header.Set("X-Hub-Signature-256", "sha256="+sig)
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("soak-%d", id))
	case "stripe":
		ts := fmt.Sprint(time.Now().Unix())
		sig := hmacHex(ib.secret, []byte(ts+"."), body)
		if bad {
			sig = hmacHex(ib.secret+"x", []byte(ts+"."), body)
		}
		req.Header.Set("Stripe-Signature", "t="+ts+",v1="+sig)
	case "token":
		tok := ib.secret
		if bad {
			tok += "x"
		}
		req.Header.Set("X-Inbox-Token", tok)
	}
}

func (ib *inbox) pickWeighted(r *rand.Rand, all []*inbox) *inbox {
	total := 0
	for _, x := range all {
		total += x.weight
	}
	n := r.Intn(total)
	for _, x := range all {
		if n < x.weight {
			return x
		}
		n -= x.weight
	}
	return all[0]
}

// sendOne performs one randomly chosen ingest request.
func (d *driver) sendOne(r *rand.Rand) {
	roll := r.Intn(1000)
	switch {
	case roll < 10: // oversize: more than the 1 MiB default
		ib := d.inboxes[r.Intn(len(d.inboxes))]
		body := make([]byte, 1100<<10+r.Intn(900<<10))
		copy(body, padBytes)
		req, _ := http.NewRequest("POST", d.srv.base()+ib.ingestPath(), bytes.NewReader(body))
		sign(ib, req, 0, body, false)
		d.do("ingest_oversize", d.ingestHC, req, 413)
		return
	case roll < 20: // not an inbox, or not a method it takes
		if r.Intn(2) == 0 {
			req, _ := http.NewRequest("POST", d.srv.base()+"/in/nonexistent"+fmt.Sprint(r.Intn(100)), bytes.NewReader([]byte("{}")))
			d.do("ingest_junk", d.ingestHC, req, 404)
		} else {
			req, _ := http.NewRequest("GET", d.srv.base()+d.inboxes[0].ingestPath(), nil)
			d.do("ingest_junk", d.ingestHC, req, 405)
		}
		return
	}
	ib := (*inbox)(nil).pickWeighted(r, d.inboxes)
	id := d.nextID.Add(1)
	body := makeBody(id, sizeFor(r, ib.audited))
	bad := ib.kind != "none" && r.Intn(100) < 3
	req, _ := http.NewRequest("POST", d.srv.base()+ib.ingestPath(), bytes.NewReader(body))
	sign(ib, req, id, body, bad)
	if r.Intn(10) == 0 {
		req.URL.Path += "/extra/path"
	}
	if bad && !ib.onStore {
		d.do("ingest_badsig", d.ingestHC, req, 401)
		return
	}
	st, resp, err := d.do("ingest", d.ingestHC, req, 200)
	if err != nil || st != 200 {
		return
	}
	var ack struct {
		Seq uint64 `json:"seq"`
	}
	if json.Unmarshal(resp, &ack) != nil || ack.Seq == 0 {
		d.violate("ack_malformed", "%s: %s", ib.slug, resp)
		return
	}
	if ib.audited {
		ib.mu.Lock()
		ib.acked[id] = ack.Seq
		if ack.Seq > ib.maxAcked {
			ib.maxAcked = ack.Seq
		}
		ib.mu.Unlock()
	}
}

// ingestLoad offers rate requests per second through a pool of senders.
func (d *driver) ingestLoad(ctx context.Context) {
	const workers = 128
	jobs := make(chan struct{}, 1)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w) + time.Now().UnixNano()))
			for range jobs {
				d.sendOne(r)
			}
		}(w)
	}
	tick := 10 * time.Millisecond
	tk := time.NewTicker(tick)
	defer tk.Stop()
	var credit float64
	perTick := float64(d.cfg.rate) * tick.Seconds()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tk.C:
		}
		credit += perTick
		for credit >= 1 {
			credit--
			select {
			case jobs <- struct{}{}:
			default:
				d.shedLoad.Add(1)
			}
		}
	}
	close(jobs)
	wg.Wait()
}
