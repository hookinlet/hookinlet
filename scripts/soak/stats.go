// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build soak

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// stats counts outcomes by "op|tag" and keeps latencies of the successful ones.
// Tags: ok (the expected status), shed503/shed429 (the server said it was
// overloaded), down (a connection error while the server was being restarted),
// and UNEXPECTED_* for everything else.
type stats struct {
	mu     sync.Mutex
	counts map[string]int64
	window map[string][]float64
	all    map[string][]float32
}

func newStats() *stats {
	return &stats{counts: map[string]int64{}, window: map[string][]float64{}, all: map[string][]float32{}}
}

func (s *stats) add(op, tag string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[op+"|"+tag]++
	if tag == "ok" {
		ms := float64(d) / float64(time.Millisecond)
		s.window[op] = append(s.window[op], ms)
		s.all[op] = append(s.all[op], float32(ms))
	}
}

func (s *stats) snapshotCounts() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]int64, len(s.counts))
	for k, v := range s.counts {
		m[k] = v
	}
	return m
}

func pct[T float32 | float64](a []T, p float64) float64 {
	if len(a) == 0 {
		return 0
	}
	b := append([]T(nil), a...)
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	i := int(p * float64(len(b)-1))
	return float64(b[i])
}

// drainLat returns p50, p99 and the count of each op's successful latencies
// since the last call.
func (s *stats) drainLat() map[string][3]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][3]float64{}
	for op, a := range s.window {
		out[op] = [3]float64{pct(a, 0.5), pct(a, 0.99), float64(len(a))}
	}
	s.window = map[string][]float64{}
	return out
}

func (s *stats) allLatency(op string) []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]float32(nil), s.all[op]...)
}

// do runs one request and classifies the outcome against the expected statuses.
func (d *driver) do(op string, hc *http.Client, req *http.Request, expect ...int) (int, []byte, error) {
	g0, down0 := d.srv.downEpoch.Load(), d.srv.down.Load()
	t0 := time.Now()
	resp, err := hc.Do(req)
	var body []byte
	status := 0
	if err == nil {
		status = resp.StatusCode
		body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
	}
	dur := time.Since(t0)
	detail := append([]byte(req.Method+" "+req.URL.Path+" "), body...)
	d.st.add(op, d.classify(op, status, err, detail, down0, g0, expect), dur)
	return status, body, err
}

func (d *driver) classify(op string, status int, err error, body []byte, down0 bool, g0 int64, expect []int) string {
	if err != nil {
		if down0 || d.srv.down.Load() || d.srv.downEpoch.Load() != g0 {
			return "down"
		}
		if strings.Contains(err.Error(), "context canceled") {
			return "cancel"
		}
		d.unexpected(op, "err", err.Error())
		return "UNEXPECTED_err"
	}
	for _, e := range expect {
		if status == e {
			return "ok"
		}
	}
	if status == 503 || status == 429 {
		if !d.storm.Load() {
			d.st.add(op, fmt.Sprintf("overload_outside_storm_%d", status), 0)
		}
		return fmt.Sprintf("shed%d", status)
	}
	d.unexpected(op, fmt.Sprint(status), string(body))
	return fmt.Sprintf("UNEXPECTED_%d", status)
}

var unexpectedSeen sync.Map

func (d *driver) unexpected(op, what, detail string) {
	key := op + "|" + what
	n, _ := unexpectedSeen.LoadOrStore(key, new(int64))
	p := n.(*int64)
	d.vmu.Lock()
	*p++
	c := *p
	d.vmu.Unlock()
	if len(detail) > 200 {
		detail = detail[:200]
	}
	if c <= 5 {
		d.logf("UNEXPECTED %s %s: %s", op, what, detail)
		d.vmu.Lock()
		if len(d.violations) < 300 {
			d.violations = append(d.violations, fmt.Sprintf("[%.0fs] unexpected %s %s: %s", time.Since(d.t0).Seconds(), op, what, detail))
		}
		d.vmu.Unlock()
	}
}

func (d *driver) newReq(method, path, token string, body []byte) *http.Request {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, d.srv.base()+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// get is a GET with a bearer token.
func (d *driver) get(op string, hc *http.Client, path, token string, expect ...int) (int, []byte, error) {
	return d.do(op, hc, d.newReq("GET", path, token, nil), expect...)
}
