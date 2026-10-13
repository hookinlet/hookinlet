// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build soak

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// server owns the hookinlet process under test: it starts it, stops it with a
// chosen signal, and tells the rest of the driver when it is expected to be
// unreachable.
type server struct {
	cfg *config

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	pid     atomic.Int64
	started time.Time

	onDeath   func(status string) // called when the process exits while the driver did not ask it to
	down      atomic.Bool
	downEpoch atomic.Int64 // bumped whenever down changes
	restarts  atomic.Int64
}

func (s *server) setDown(v bool) {
	s.down.Store(v)
	s.downEpoch.Add(1)
}

func (s *server) base() string { return "http://" + s.cfg.addr() }

// start launches the binary (through sh, only to raise the descriptor limit;
// exec keeps the pid) and waits until /readyz answers 200. It returns how long
// that took.
func (s *server) start(ctx context.Context) (time.Duration, error) {
	logf, err := os.OpenFile(filepath.Join(s.cfg.out, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command("sh", "-c", "ulimit -n 10240; exec "+shellQuote(s.cfg.bin)+" serve")
	cmd.Env = append(os.Environ(),
		"HOOKINLET_ADMIN_TOKEN="+s.cfg.adminToken,
		"HOOKINLET_MASTER_KEY="+s.cfg.masterKey,
		"HOOKINLET_DATA_DIR="+s.cfg.dataDir,
		"HOOKINLET_LISTEN_ADDR="+s.cfg.addr(),
		"HOOKINLET_LOG_LEVEL=warn",
		"HOOKINLET_RETENTION_INTERVAL=1s",
	)
	cmd.Stdout = logf
	cmd.Stderr = logf
	fmt.Fprintf(logf, "=== driver: starting server at %s\n", time.Now().Format(time.RFC3339))
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		if !s.down.Load() && s.onDeath != nil {
			s.onDeath(cmd.ProcessState.String())
		}
		logf.Close()
		close(exited)
	}()
	s.mu.Lock()
	s.cmd, s.exited, s.started = cmd, exited, time.Now()
	s.mu.Unlock()
	s.pid.Store(int64(cmd.Process.Pid))

	hc := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return 0, fmt.Errorf("server exited during start (see server.log)")
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		resp, err := hc.Get(s.base() + "/readyz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return time.Since(t0), nil
			}
		}
		if time.Since(t0) > 60*time.Second {
			return 0, fmt.Errorf("server not ready after 60s")
		}
	}
}

// stop sends sig and waits for the process. It returns the time taken and the
// exit status description.
func (s *server) stop(sig syscall.Signal, wait time.Duration) (time.Duration, string, error) {
	s.mu.Lock()
	cmd, exited := s.cmd, s.exited
	s.mu.Unlock()
	t0 := time.Now()
	if err := cmd.Process.Signal(sig); err != nil {
		return 0, "", err
	}
	select {
	case <-exited:
	case <-time.After(wait):
		_ = cmd.Process.Kill()
		<-exited
		return time.Since(t0), "TIMEOUT-killed", fmt.Errorf("server ignored %v for %v", sig, wait)
	}
	return time.Since(t0), cmd.ProcessState.String(), nil
}

// check runs `hookinlet check` on the data directory.
func (s *server) check(quick bool) (string, error) {
	args := []string{"check"}
	if quick {
		args = append(args, "--quick")
	}
	cmd := exec.Command(s.cfg.bin, args...)
	cmd.Env = append(os.Environ(), "HOOKINLET_DATA_DIR="+s.cfg.dataDir, "HOOKINLET_MASTER_KEY="+s.cfg.masterKey)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sample is one row of the 10-second time series.
type sample struct {
	T          float64               `json:"t"` // seconds since the run began
	Pid        int64                 `json:"pid"`
	Incarn     int64                 `json:"incarnation"`
	RSSKB      int64                 `json:"rss_kb"`
	CPU        float64               `json:"cpu_pct"`
	FDs        int                   `json:"fds"`
	FDSockets  int                   `json:"fds_tcp"`
	FDFiles    int                   `json:"fds_reg"`
	DB         int64                 `json:"db_bytes"`
	WAL        int64                 `json:"wal_bytes"`
	SHM        int64                 `json:"shm_bytes"`
	Goroutines int                   `json:"goroutines"`
	HeapAlloc  int64                 `json:"heap_alloc"`
	Streams    int                   `json:"streams_open"`
	Reads      int                   `json:"reads_in_flight"`
	IngestMem  int64                 `json:"ingest_mem_used"`
	Down       bool                  `json:"down"`
	Counts     map[string]int64      `json:"counts"`
	Lat        map[string][3]float64 `json:"lat_ms"` // op -> p50, p99, n over the window
	Bounded    map[string]int64      `json:"retained_counts,omitempty"`
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func (d *driver) takeSample() sample {
	pid := d.srv.pid.Load()
	sm := sample{T: time.Since(d.t0).Seconds(), Pid: pid, Incarn: d.srv.restarts.Load(), Down: d.srv.down.Load()}
	if !sm.Down {
		if out, err := exec.Command("ps", "-o", "rss=,%cpu=", "-p", strconv.FormatInt(pid, 10)).Output(); err == nil {
			f := strings.Fields(string(out))
			if len(f) == 2 {
				sm.RSSKB, _ = strconv.ParseInt(f[0], 10, 64)
				sm.CPU, _ = strconv.ParseFloat(f[1], 64)
			}
		}
		if out, err := exec.Command("lsof", "-b", "-w", "-n", "-P", "-p", strconv.FormatInt(pid, 10)).Output(); err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for i, l := range lines {
				if i == 0 {
					continue
				}
				f := strings.Fields(l)
				if len(f) < 5 {
					continue
				}
				sm.FDs++
				switch f[4] {
				case "IPv4", "IPv6":
					sm.FDSockets++
				case "REG":
					sm.FDFiles++
				}
			}
		}
	}
	db := filepath.Join(d.cfg.dataDir, "hookinlet.db")
	sm.DB, sm.WAL, sm.SHM = fileSize(db), fileSize(db+"-wal"), fileSize(db+"-shm")
	if !sm.Down {
		if m, err := d.scrapeMetrics(); err == nil {
			sm.Goroutines = int(m["hookinlet_go_goroutines"])
			sm.HeapAlloc = int64(m["hookinlet_go_heap_alloc_bytes"])
			sm.Streams = int(m["hookinlet_streams_open"])
			sm.Reads = int(m["hookinlet_reads_in_flight"])
			sm.IngestMem = int64(m["hookinlet_ingest_memory_used_bytes"])
		}
	}
	sm.Counts = d.st.snapshotCounts()
	sm.Lat = d.st.drainLat()
	return sm
}

// scrapeMetrics returns the unlabelled series of /metrics.
func (d *driver) scrapeMetrics() (map[string]float64, error) {
	req, _ := http.NewRequest("GET", d.srv.base()+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+d.cfg.adminToken)
	resp, err := d.adminHC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("metrics: %d", resp.StatusCode)
	}
	out := map[string]float64{}
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || l[0] == '#' || strings.Contains(l, "{") {
			continue
		}
		f := strings.Fields(l)
		if len(f) == 2 {
			v, _ := strconv.ParseFloat(f[1], 64)
			out[f[0]] = v
		}
	}
	return out, nil
}

func (d *driver) sampler(ctx context.Context, f *os.File) {
	tk := time.NewTicker(10 * time.Second)
	defer tk.Stop()
	enc := json.NewEncoder(f)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		sm := d.takeSample()
		sm.Bounded = d.retainedCounts()
		_ = enc.Encode(sm)
		d.logf("sample t=%4.0fs rss=%dMB fds=%d gor=%d streams=%d db=%dMB wal=%dMB ingest_ok=%d viol=%d",
			sm.T, sm.RSSKB/1024, sm.FDs, sm.Goroutines, sm.Streams, sm.DB>>20, sm.WAL>>20,
			sm.Counts["ingest|ok"], d.violationCount())
	}
}
