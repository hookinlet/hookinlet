// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// metrics are the counters behind GET /metrics, which writes them in the Prometheus
// text exposition format. They are kept by hand, in memory, from nothing but the
// standard library: a handful of counters does not need a client library, and
// nothing here has a label whose values a client can choose (there is no inbox
// label, no path, no query), so the series count is fixed by the code and the
// document.
type metrics struct {
	deliveries sync.Map // result string -> *atomic.Int64
	bodyBytes  atomic.Int64
	requests   sync.Map // "route\x00status" -> *atomic.Int64
	retention  atomic.Int64
	sweeps     atomic.Int64
}

func (m *metrics) count(table *sync.Map, key string, n int64) {
	v, ok := table.Load(key)
	if !ok {
		v, _ = table.LoadOrStore(key, new(atomic.Int64))
	}
	v.(*atomic.Int64).Add(n)
}

// delivery notes how a delivery ended: one of a fixed set of results.
func (m *metrics) delivery(result string) { m.count(&m.deliveries, result, 1) }

// request notes a finished request by the route pattern that served it and its
// status. A path that matches no route is served by the catch-all, whose pattern is
// "/", so scanning cannot add series; a request with no pattern at all (which the
// router does not produce) is counted as "unmatched".
func (m *metrics) request(route string, status int) {
	if route == "" {
		route = "unmatched"
	}
	m.count(&m.requests, fmt.Sprintf("%s\x00%d", route, status), 1)
}

// deliveryResult names how an ingest ended, for the metrics.
func deliveryResult(res ingestResult) string {
	switch {
	case res.status == 200 && res.challenge != nil:
		return "handshake"
	case res.status == 200 && res.duplicate:
		return "duplicate"
	case res.status == 200:
		return "stored"
	case res.status == 401:
		return "verification_failed"
	case res.status == 402 || res.status == 403 || res.status == 429:
		return "refused_by_hook"
	case res.status == 404:
		return "not_found"
	case res.status == 413:
		return "too_large"
	case res.status == 503 && res.code == "storage_full":
		return "storage_full"
	case res.status == 503:
		return "overloaded"
	case res.status >= 500:
		return "error"
	default:
		return "rejected"
	}
}

// write renders the metrics. The order is fixed, so two scrapes diff cleanly.
func (s *Server) writeMetrics(w io.Writer) error {
	m := &s.metrics
	var b strings.Builder
	family := func(name, help, kind string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	sorted := func(table *sync.Map) []string {
		var keys []string
		table.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true })
		sort.Strings(keys)
		return keys
	}
	val := func(table *sync.Map, key string) int64 {
		v, _ := table.Load(key)
		return v.(*atomic.Int64).Load()
	}

	family("hookinlet_info", "The server's version.", "gauge")
	fmt.Fprintf(&b, "hookinlet_info{version=%q} 1\n", s.opts.Version)

	family("hookinlet_deliveries_total", "Deliveries received, by how they ended.", "counter")
	for _, k := range sorted(&m.deliveries) {
		fmt.Fprintf(&b, "hookinlet_deliveries_total{result=%q} %d\n", k, val(&m.deliveries, k))
	}
	family("hookinlet_delivery_body_bytes_total", "Bytes of request bodies read from deliveries.", "counter")
	fmt.Fprintf(&b, "hookinlet_delivery_body_bytes_total %d\n", m.bodyBytes.Load())

	family("hookinlet_http_requests_total", "Requests served, by route and status.", "counter")
	for _, k := range sorted(&m.requests) {
		route, status, _ := strings.Cut(k, "\x00")
		fmt.Fprintf(&b, "hookinlet_http_requests_total{route=%q,status=%q} %d\n", route, status, val(&m.requests, k))
	}

	family("hookinlet_ingest_memory_used_bytes", "Bytes of the ingest memory budget held by deliveries being received.", "gauge")
	s.ingestBudget.mu.Lock()
	used := s.ingestBudget.used
	s.ingestBudget.mu.Unlock()
	fmt.Fprintf(&b, "hookinlet_ingest_memory_used_bytes %d\n", used)
	family("hookinlet_ingest_memory_budget_bytes", "The ingest memory budget (-1: unbounded).", "gauge")
	fmt.Fprintf(&b, "hookinlet_ingest_memory_budget_bytes %d\n", s.ingestBudget.limit)

	family("hookinlet_reads_in_flight", "Read API requests being served.", "gauge")
	fmt.Fprintf(&b, "hookinlet_reads_in_flight %d\n", len(s.reads))
	family("hookinlet_streams_open", "Event streams and MCP waits open.", "gauge")
	fmt.Fprintf(&b, "hookinlet_streams_open %d\n", len(s.streams))

	family("hookinlet_retention_sweeps_total", "Retention sweeps run.", "counter")
	fmt.Fprintf(&b, "hookinlet_retention_sweeps_total %d\n", m.sweeps.Load())
	family("hookinlet_retention_deleted_messages_total", "Messages removed by retention.", "counter")
	fmt.Fprintf(&b, "hookinlet_retention_deleted_messages_total %d\n", m.retention.Load())

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	family("hookinlet_go_goroutines", "Goroutines.", "gauge")
	fmt.Fprintf(&b, "hookinlet_go_goroutines %d\n", runtime.NumGoroutine())
	family("hookinlet_go_heap_alloc_bytes", "Bytes of heap in use.", "gauge")
	fmt.Fprintf(&b, "hookinlet_go_heap_alloc_bytes %d\n", ms.HeapAlloc)

	_, err := io.WriteString(w, b.String())
	return err
}
