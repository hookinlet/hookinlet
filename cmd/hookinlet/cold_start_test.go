// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build unix

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/benchseed"
)

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
	binaryDir  string
)

// builtBinary builds the command once for every test that runs it as a process,
// and returns its path. Building is the slowest thing these tests do.
func builtBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		binaryDir, binaryErr = os.MkdirTemp("", "hookinlet-test-bin-*")
		if binaryErr != nil {
			return
		}
		binaryPath = filepath.Join(binaryDir, "hookinlet")
		build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", binaryPath, ".") //nolint:gosec,noctx // builds this repository's own command, once
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binaryDir != "" {
		_ = os.RemoveAll(binaryDir)
	}
	os.Exit(code)
}

// timeToReady starts the server on an existing data directory and returns how
// long it takes from exec to the first successful /readyz. It tries again if
// the server exits at once, which is what happens when another test takes the
// port between choosing it and binding it.
func timeToReady(t *testing.T, bin, data string) time.Duration {
	t.Helper()
	var last string
	for range 3 {
		d, stderr, ok := startOnce(t, bin, data)
		if ok {
			return d
		}
		last = stderr
	}
	t.Fatalf("the server kept exiting before it was ready:\n%s", last)
	return 0
}

func startOnce(t *testing.T, bin, data string) (d time.Duration, stderr string, ok bool) {
	t.Helper()
	var lc net.ListenConfig
	probe, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	srv := exec.Command(bin, "serve") //nolint:gosec,noctx // runs the binary this test just built; stopped below
	srv.Env = append(os.Environ(),
		"HOOKINLET_DATA_DIR="+data,
		"HOOKINLET_ADMIN_TOKEN=admin-token-for-the-start-test-0123456789",
		"HOOKINLET_MASTER_KEY="+masterKey(),
		"HOOKINLET_LISTEN_ADDR="+addr,
		"HOOKINLET_LOG_LEVEL=warn",
	)
	var errOut bytes.Buffer
	srv.Stderr = &errOut
	start := time.Now()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = srv.Wait(); close(exited) }()
	defer func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = srv.Process.Kill() // do not leave it running if it will not stop
			<-exited
		}
	}()
	client := &http.Client{Timeout: time.Second}
	for time.Since(start) < 30*time.Second {
		select {
		case <-exited:
			return 0, errOut.String(), false
		default:
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/readyz", nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return time.Since(start), "", true
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the server was not ready after 30 s:\n%s", errOut.String())
	return 0, "", false
}

// The Phase 7 gate: with an existing database the server is ready in under
// 500 ms, and makes no network calls on the way (it has none to make: every
// setting is local). This runs the real binary and measures wall time from exec
// to the first successful /readyz.
//
// An ordinary run uses a small database and a generous limit, which catches a
// start that has become slow for any reason. The figure the gate is about is
// measured on a million messages when asked for:
//
//	HOOKINLET_BENCH=1 go test -run TestColdStart -v ./cmd/hookinlet/
func TestColdStartWithAnExistingDatabaseIsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real binary")
	}
	rows, limit := 20_000, 2*time.Second
	if os.Getenv("HOOKINLET_BENCH") != "" {
		rows, limit = 1_000_000, 500*time.Millisecond
	}

	bin := builtBinary(t)
	data := filepath.Join(t.TempDir(), "data")
	if _, err := benchseed.Seed(t.Context(), data, "cold", rows); err != nil {
		t.Fatal(err)
	}

	// This is a warm restart: the first start is not counted, and leaves the
	// files in the operating system's cache, as a restart finds them. A start on a
	// host with an empty cache, or the time to start a container, is not measured.
	timeToReady(t, bin, data)
	var runs []time.Duration
	for range 7 {
		runs = append(runs, timeToReady(t, bin, data))
	}
	slices.Sort(runs)
	median := runs[len(runs)/2]
	t.Logf("%d messages in the database; ready in: fastest %v, median %v, slowest %v (%d starts)",
		rows, runs[0].Round(time.Millisecond), median.Round(time.Millisecond), runs[len(runs)-1].Round(time.Millisecond), len(runs))
	if median >= limit {
		t.Errorf("median start took %v, over the %v limit", median, limit)
	}
}
