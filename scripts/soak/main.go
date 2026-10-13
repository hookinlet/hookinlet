// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build soak

// Command soak is a soak and load driver for the real hookinlet binary. It
// starts `hookinlet serve` itself, creates inboxes of every verifier kind,
// drives a mixed workload for a while (ingest with bad signatures and oversize
// bodies, cursor-following readers, SSE streams that drop and resume, admin and
// MCP calls, retention), restarts the server with SIGTERM and once with
// SIGKILL, samples its resource use every 10 seconds, and checks invariants at
// the end: nothing acknowledged is lost or duplicated, sequence numbers are
// contiguous and strictly increasing, and `hookinlet check` passes.
//
// It is excluded from normal builds and tests by the soak build tag:
//
//	go build -o bin/hookinlet ./cmd/hookinlet
//	go run -tags soak ./scripts/soak -bin bin/hookinlet -duration 35m -out /tmp/soak
//
// A short smoke run is -duration 2m. The output directory receives
// samples.jsonl (one row per 10 seconds), events.jsonl, server.log and
// summary.json.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	bin         string
	out         string
	dataDir     string
	port        int
	duration    time.Duration
	cooldown    time.Duration
	rate        int
	sse         int
	plain       bool
	backupEvery time.Duration
	adminToken  string
	masterKey   string
}

func (c *config) addr() string { return fmt.Sprintf("127.0.0.1:%d", c.port) }

type driver struct {
	cfg *config
	srv *server
	st  *stats
	t0  time.Time

	ingestHC, readHC, streamHC, adminHC *http.Client

	inboxes []*inbox

	storm     atomic.Bool
	sseMsgs   atomic.Int64
	ssePings  atomic.Int64
	nextID    atomic.Uint64
	shedLoad  atomic.Int64 // offered requests dropped because every sender was busy
	revokeLag atomic.Int64 // worst milliseconds from revoke to stream end

	vmu        sync.Mutex
	violations []string
	vcount     map[string]int

	evmu   sync.Mutex
	events *os.File

	logmu sync.Mutex
}

func (d *driver) logf(format string, a ...any) {
	d.logmu.Lock()
	defer d.logmu.Unlock()
	fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
}

// event records something that happened to the server or the run.
func (d *driver) event(kind string, kv map[string]any) {
	if kv == nil {
		kv = map[string]any{}
	}
	kv["kind"] = kind
	kv["t"] = time.Since(d.t0).Seconds()
	d.evmu.Lock()
	defer d.evmu.Unlock()
	_ = json.NewEncoder(d.events).Encode(kv)
	d.logf("event %s %v", kind, kv)
}

// violate records a broken invariant. These are what the run exists to find.
func (d *driver) violate(kind, format string, a ...any) {
	d.vmu.Lock()
	defer d.vmu.Unlock()
	d.vcount[kind]++
	if len(d.violations) < 300 {
		d.violations = append(d.violations, fmt.Sprintf("[%.0fs] %s: %s", time.Since(d.t0).Seconds(), kind, fmt.Sprintf(format, a...)))
	}
	if d.vcount[kind] <= 3 {
		d.logf("VIOLATION %s: %s", kind, fmt.Sprintf(format, a...))
	}
}

func (d *driver) violationCount() int {
	d.vmu.Lock()
	defer d.vmu.Unlock()
	n := 0
	for _, c := range d.vcount {
		n += c
	}
	return n
}

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.bin, "bin", "bin/hookinlet", "hookinlet binary to test")
	flag.StringVar(&cfg.out, "out", "", "output directory (required; the data directory is created inside it)")
	flag.IntVar(&cfg.port, "port", 18437, "port for the server under test")
	flag.DurationVar(&cfg.duration, "duration", 35*time.Minute, "length of the loaded phase")
	flag.DurationVar(&cfg.cooldown, "cooldown", 90*time.Second, "idle time after the load, to see resources settle")
	flag.IntVar(&cfg.rate, "rate", 300, "target ingest requests per second")
	flag.IntVar(&cfg.sse, "sse", 250, "SSE streams that drop and reconnect at random")
	flag.BoolVar(&cfg.plain, "plain", false, "steady load only: no restarts, storms or kill (for baselines)")
	flag.DurationVar(&cfg.backupEvery, "backup-every", 4*time.Minute, "how often to take and check a backup through the admin API")
	flag.Parse()
	if cfg.out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	var err error
	if cfg.bin, err = filepath.Abs(cfg.bin); err != nil {
		panic(err)
	}
	if cfg.out, err = filepath.Abs(cfg.out); err != nil {
		panic(err)
	}
	cfg.dataDir = filepath.Join(cfg.out, "data")
	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		panic(err)
	}
	cfg.adminToken, cfg.masterKey = randB64(32), randB64(32)

	d := &driver{cfg: cfg, st: newStats(), t0: time.Now(), vcount: map[string]int{}}
	d.srv = &server{cfg: cfg}
	d.srv.onDeath = func(status string) {
		d.violate("server_died", "the server exited on its own: %s", status)
		d.event("server_died", map[string]any{"status": status})
	}
	d.events, err = os.Create(filepath.Join(cfg.out, "events.jsonl"))
	if err != nil {
		panic(err)
	}
	mk := func(maxConns int) *http.Client {
		return &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: maxConns, MaxIdleConns: maxConns, IdleConnTimeout: 30 * time.Second}}
	}
	d.ingestHC = mk(256)
	d.readHC = mk(64)
	d.adminHC = mk(16)
	d.streamHC = &http.Client{Transport: &http.Transport{MaxIdleConns: 4}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := d.run(ctx)
	os.Exit(code)
}

func (d *driver) run(ctx context.Context) int {
	if _, err := d.srv.start(ctx); err != nil {
		d.logf("cannot start server: %v", err)
		return 1
	}
	d.t0 = time.Now()
	d.event("start", map[string]any{"pid": d.srv.pid.Load(), "duration": d.cfg.duration.String(), "rate": d.cfg.rate, "sse": d.cfg.sse})
	if err := d.setup(ctx); err != nil {
		d.logf("setup: %v", err)
		_, _, _ = d.srv.stop(syscall.SIGKILL, 10*time.Second)
		return 1
	}

	sf, _ := os.Create(filepath.Join(d.cfg.out, "samples.jsonl"))
	sctx, sampStop := context.WithCancel(context.Background())
	var swg sync.WaitGroup
	swg.Add(1)
	go func() { defer swg.Done(); d.sampler(sctx, sf) }()

	// Load. wctx ends the senders; fctx (followers, auditors) outlives it so
	// the readers can drain before the final comparison.
	wctx, wstop := context.WithCancel(ctx)
	fctx, fstop := context.WithCancel(context.Background())
	var wwg, fwg sync.WaitGroup
	spawn := func(wg *sync.WaitGroup, c context.Context, f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(c) }()
	}
	d.startLoad(wctx, fctx, spawn, &wwg, &fwg)

	killed := d.timeline(ctx)

	// Quiesce: stop writers, let readers catch up, then compare.
	d.event("load_stop", nil)
	wstop()
	wwg.Wait()
	d.verifyAll(fctx)
	fstop()
	fwg.Wait()

	if d.cfg.cooldown > 0 {
		d.event("cooldown_start", nil)
		select {
		case <-time.After(d.cfg.cooldown):
		case <-ctx.Done():
		}
	}
	sampStop()
	swg.Wait()
	final := d.takeSample()
	d.event("final_sample", map[string]any{"rss_kb": final.RSSKB, "fds": final.FDs, "goroutines": final.Goroutines, "wal": final.WAL, "db": final.DB, "streams": final.Streams})

	// Clean shutdown, then a full check of the data directory.
	d.srv.setDown(true)
	dur, st, err := d.srv.stop(syscall.SIGTERM, 30*time.Second)
	d.event("final_sigterm", map[string]any{"took_ms": dur.Milliseconds(), "status": st, "err": fmt.Sprint(err)})
	if err != nil || !strings.Contains(st, "exit status 0") && !strings.Contains(st, "exited") {
		d.violate("shutdown", "final SIGTERM: %v %s", err, st)
	}
	out, cerr := d.srv.check(false)
	d.event("final_check", map[string]any{"out": out, "err": fmt.Sprint(cerr)})
	if cerr != nil {
		d.violate("check", "final check failed: %v %s", cerr, out)
	}
	d.writeSummary(killed, final)
	if d.violationCount() > 0 {
		return 1
	}
	return 0
}

// timeline performs the restarts, the storms and the SIGKILL at fixed
// fractions of the run and returns when the loaded phase is over.
func (d *driver) timeline(ctx context.Context) bool {
	at := func(f float64) time.Duration { return time.Duration(f * float64(d.cfg.duration)) }
	type step struct {
		f    float64
		name string
		do   func()
	}
	steps := []step{
		{0.03, "stream_cap", func() { d.streamCapTest(ctx) }},
		{0.12, "sigterm", func() { d.restart(ctx, syscall.SIGTERM) }},
		{0.30, "ingest_storm", func() { d.ingestStorm(ctx, 45*time.Second) }},
		{0.38, "sigterm", func() { d.restart(ctx, syscall.SIGTERM) }},
		{0.45, "read_storm", func() { d.readStorm(ctx, 30*time.Second) }},
		{0.55, "sigkill", func() { d.restart(ctx, syscall.SIGKILL) }},
		{0.68, "sigterm", func() { d.restart(ctx, syscall.SIGTERM) }},
		{0.80, "sigterm", func() { d.restart(ctx, syscall.SIGTERM) }},
	}
	if d.cfg.plain {
		steps = nil
	}
	killed := false
	end := time.NewTimer(d.cfg.duration)
	defer end.Stop()
	for _, s := range steps {
		wait := time.Until(d.t0.Add(at(s.f)))
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-end.C:
				return killed
			case <-ctx.Done():
				return killed
			}
		}
		if ctx.Err() != nil {
			return killed
		}
		d.logf("timeline: %s", s.name)
		s.do()
		killed = killed || s.name == "sigkill"
	}
	select {
	case <-end.C:
	case <-ctx.Done():
	}
	return killed
}

// restart stops the server with sig and starts it again, under load.
func (d *driver) restart(ctx context.Context, sig syscall.Signal) {
	name := sig.String()
	pre := d.ackedMaxSeqs()
	d.srv.setDown(true)
	took, status, err := d.srv.stop(sig, 40*time.Second)
	ev := map[string]any{"signal": name, "stop_ms": took.Milliseconds(), "status": status, "err": fmt.Sprint(err)}
	if sig == syscall.SIGTERM {
		if err != nil {
			d.violate("shutdown", "SIGTERM: %v", err)
		}
		if took > 12*time.Second {
			d.violate("shutdown_slow", "SIGTERM took %v with streams open (limit is 10s)", took)
		}
	}
	if sig == syscall.SIGKILL {
		out, cerr := d.srv.check(false)
		ev["check_after_kill"] = out
		ev["check_err"] = fmt.Sprint(cerr)
		if cerr != nil {
			d.violate("check", "hookinlet check after SIGKILL failed: %v %s", cerr, out)
		}
	}
	d.srv.restarts.Add(1)
	ready, serr := d.srv.start(ctx)
	if serr != nil {
		d.violate("restart", "server did not come back: %v", serr)
		d.event("restart_failed", ev)
		return
	}
	ev["ready_ms"] = ready.Milliseconds()
	time.Sleep(500 * time.Millisecond)
	d.srv.setDown(false)
	// Everything acknowledged before the signal must still be there.
	for _, ib := range d.inboxes {
		if !ib.audited {
			continue
		}
		var meta struct {
			LatestSeq uint64 `json:"latest_seq"`
		}
		if st, body, err := d.get("admin", d.adminHC, "/v1/inboxes/"+ib.slug, d.cfg.adminToken, 200); err == nil && st == 200 {
			_ = json.Unmarshal(body, &meta)
			if meta.LatestSeq < pre[ib.slug] {
				d.violate("lost_after_restart", "%s: latest_seq %d < acked-before-%s %d", ib.slug, meta.LatestSeq, name, pre[ib.slug])
			}
		}
	}
	d.event("restart", ev)
}

func (d *driver) writeSummary(killed bool, final sample) {
	d.vmu.Lock()
	vc := map[string]int{}
	for k, v := range d.vcount {
		vc[k] = v
	}
	vs := append([]string(nil), d.violations...)
	d.vmu.Unlock()
	counts := d.st.snapshotCounts()
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	all := map[string]any{}
	for _, op := range []string{"ingest", "read", "sse_connect", "admin", "mcp"} {
		a := d.st.allLatency(op)
		if len(a) > 0 {
			all[op] = map[string]float64{"p50_ms": pct(a, 0.5), "p90_ms": pct(a, 0.9), "p99_ms": pct(a, 0.99), "max_ms": pct(a, 1), "n": float64(len(a))}
		}
	}
	sum := map[string]any{
		"duration_s":                    time.Since(d.t0).Seconds(),
		"sigkill_done":                  killed,
		"violations":                    vc,
		"violation_samples":             vs,
		"counts":                        counts,
		"latency_overall":               all,
		"sse_messages":                  d.sseMsgs.Load(),
		"sse_pings":                     d.ssePings.Load(),
		"offered_but_shed_by_driver":    d.shedLoad.Load(),
		"revoke_to_stream_end_worst_ms": d.revokeLag.Load(),
		"messages_acked":                d.ackedTotal(),
		"final":                         final,
	}
	b, _ := json.MarshalIndent(sum, "", " ")
	_ = os.WriteFile(filepath.Join(d.cfg.out, "summary.json"), b, 0o644)
	fmt.Println(string(b))
}
