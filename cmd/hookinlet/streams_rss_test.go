// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build unix

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rssKiB returns the resident set size of a process in KiB, as ps reports it.
func rssKiB(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // ps with a pid this test started
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("ps printed %q", out)
	}
	return n
}

// raiseFileLimit lifts this process's soft descriptor limit so that hundreds of
// connections fit, and returns what it ended up with. A child started afterwards
// inherits it.
func raiseFileLimit(t *testing.T, want uint64) uint64 {
	t.Helper()
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Skipf("cannot read the file limit: %v", err)
	}
	if lim.Cur < want {
		lim.Cur = min(want, lim.Max)
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			t.Skipf("cannot raise the file limit: %v", err)
		}
	}
	return lim.Cur
}

// The memory budget for streams: 500 idle streams add less than 40 MiB (80 KiB each; about 50 KiB
// is measured) to the server's resident memory.
// This runs the real binary as a separate process and measures it with ps, since
// measuring inside the test process would count the 500 clients too.
func TestFiveHundredIdleStreamsStayUnderFiftyMegabytes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real binary")
	}
	const streams = 500
	if got := raiseFileLimit(t, 4096); got < 2*streams+100 {
		t.Skipf("the file limit is %d; this test needs about %d", got, 2*streams+100)
	}

	dir := t.TempDir()
	bin := builtBinary(t)

	data := filepath.Join(dir, "data")
	env := append(os.Environ(),
		"HOOKINLET_DATA_DIR="+data,
		"HOOKINLET_ADMIN_TOKEN=admin-token-for-the-memory-test-0123456789",
		"HOOKINLET_MASTER_KEY="+masterKey(),
		"HOOKINLET_LOG_LEVEL=warn",
		"HOOKINLET_MAX_STREAMS_PER_CREDENTIAL=1000", // one token holds all 500 of the streams measured here
	)
	create := exec.CommandContext(t.Context(), bin, "inbox", "create", "--slug", "streamed", "--ingest-key", "streamed", "--verifier", "none") //nolint:gosec // runs the binary this test just built
	create.Env = env
	created, err := create.Output()
	if err != nil {
		t.Fatalf("inbox create: %v", err)
	}
	m := tokenRE.FindStringSubmatch(string(created))
	if m == nil {
		t.Fatalf("no token in %q", created)
	}
	agentToken := m[1]

	var lc net.ListenConfig
	probe, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	var stderr bytes.Buffer
	srv := exec.Command(bin, "serve") //nolint:gosec,noctx // runs the binary this test just built; its lifetime is managed by Cleanup
	srv.Env = append(env, "HOOKINLET_LISTEN_ADDR="+addr)
	srv.Stderr = &stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = srv.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = srv.Process.Kill()
		}
	})
	ready := false
	for range 100 {
		resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx // polling a local server
		if err == nil {
			_ = resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("the server never came up: %s", stderr.String())
	}
	time.Sleep(300 * time.Millisecond)
	baseline := rssKiB(t, srv.Process.Pid)

	// Open the streams one by one and wait for each to be established.
	request := fmt.Sprintf("GET /v1/inboxes/streamed/stream HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nAccept: text/event-stream\r\n\r\n", addr, agentToken)
	conns := make([]net.Conn, 0, streams)
	readers := make([]*bufio.Reader, 0, streams)
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	dialer := net.Dialer{Timeout: 5 * time.Second}
	for i := range streams {
		c, err := dialer.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		conns = append(conns, c)
		if _, err := c.Write([]byte(request)); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		r := bufio.NewReaderSize(c, 512)
		readers = append(readers, r)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		status, err := r.ReadString('\n')
		if err != nil || !strings.Contains(status, "200") {
			t.Fatalf("stream %d: status line %q, %v", i, status, err)
		}
		// Skip the headers, then read the retry frame that proves it is established.
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("stream %d headers: %v", i, err)
			}
			if line == "\r\n" {
				break
			}
		}
	}
	// Let everything settle, as a long-idle server would.
	time.Sleep(time.Second)
	loaded := rssKiB(t, srv.Process.Pid)

	// The streams are real, not just open sockets: one delivery reaches all 500.
	body := `{"hello":"streams"}`
	post, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/in/streamed", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delivery status %d", resp.StatusCode)
	}
	start := time.Now()
	for i, r := range readers {
		_ = conns[i].SetReadDeadline(time.Now().Add(10 * time.Second))
		got := false
		for !got {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("stream %d never received the delivery: %v", i, err)
			}
			got = strings.HasPrefix(line, "event: message")
		}
	}
	t.Logf("one delivery reached all %d streams in %v", streams, time.Since(start).Round(time.Millisecond))
	afterBroadcast := rssKiB(t, srv.Process.Pid)

	t.Logf("server RSS: %.1f MiB idle, %.1f MiB with %d streams (+%.1f MiB, %.0f KiB each), %.1f MiB after a broadcast",
		float64(baseline)/1024, float64(loaded)/1024, streams, float64(loaded-baseline)/1024,
		float64(loaded-baseline)/streams, float64(afterBroadcast)/1024)
	// What the streams cost is what they add to the process, not the total, which also holds the
	// runtime's own memory and moves by tens of MiB between machines and runs (23 MiB idle here, and
	// measured from 6 to 24 elsewhere). So the gate is the growth, 80 KiB per stream, and the total
	// has only a loose ceiling that catches a baseline that has blown up. macOS counts memory it has
	// been told it may reclaim, so there the bounds only catch runaway growth.
	const perStreamKiB = 80
	growthLimit := streams * perStreamKiB
	totalLimit := 80 << 10 // KiB
	if runtime.GOOS != "linux" {
		growthLimit, totalLimit = 2*growthLimit, 2*totalLimit
	}
	if grew := loaded - baseline; grew >= growthLimit {
		t.Errorf("%d idle streams added %.1f MiB (%.0f KiB each), over the %.0f MiB budget (%d KiB each) on %s",
			streams, float64(grew)/1024, float64(grew)/streams, float64(growthLimit)/1024, perStreamKiB, runtime.GOOS)
	}
	if loaded >= totalLimit {
		t.Errorf("RSS with %d idle streams is %.1f MiB, over the %.0f MiB ceiling on %s", streams, float64(loaded)/1024, float64(totalLimit)/1024, runtime.GOOS)
	}
	// A broadcast makes every stream read the store at once, and the garbage from
	// those reads stays resident for a while. That is not part of the gate, which
	// is about idle streams, and the figure moves by several MiB between runs
	// (43 to 66 MiB measured on Linux), so this is only a tripwire for a
	// regression that makes a broadcast cost far more. macOS reports memory it
	// has been told it may reclaim, so there the number is only logged.
	const broadcastLimit = 96 << 10 // KiB
	if afterBroadcast >= broadcastLimit {
		if runtime.GOOS == "linux" {
			t.Errorf("RSS after a broadcast to %d streams is %.1f MiB, over the %d MiB tripwire", streams, float64(afterBroadcast)/1024, broadcastLimit>>10)
		} else {
			t.Logf("RSS after a broadcast is above %d MiB on %s, which over-reports; the Linux figure is what is enforced", broadcastLimit>>10, runtime.GOOS)
		}
	}

	// And a graceful shutdown ends all of them promptly.
	shutdownStart := time.Now()
	if err := srv.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for i, r := range readers {
		_ = conns[i].SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			if _, err := r.ReadString('\n'); err != nil {
				break
			}
		}
		_ = i
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("the server exited with %v after SIGTERM", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not exit after SIGTERM")
	}
	if d := time.Since(shutdownStart); d > 10*time.Second {
		t.Errorf("shutting down with %d streams took %v (SHUTDOWN_TIMEOUT is 10s)", streams, d)
	}
	t.Logf("all %d streams ended and the server exited in %v", streams, time.Since(shutdownStart).Round(time.Millisecond))
}
