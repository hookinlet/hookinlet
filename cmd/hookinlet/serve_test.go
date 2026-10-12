// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/config"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// The whole path, with nothing mocked: create an inbox with the CLI, start the
// server on a real socket, send webhooks over HTTP, shut down gracefully, and
// read the messages back from the database on disk.
func TestServeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{ //nolint:gosec // fake values for a test
		"HOOKINLET_DATA_DIR":        dir,
		"HOOKINLET_ADMIN_TOKEN":     "admin-token-for-the-serve-end-to-end-test",
		"HOOKINLET_MASTER_KEY":      masterKey(),
		"HOOKINLET_VERIFIER_SECRET": "e2e-shared-secret",
		"HOOKINLET_LISTEN_ADDR":     "127.0.0.1:0",
	}
	if code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "e2e-box", "--ingest-key", "e2e-box", "--verifier", "token"); code != 0 {
		t.Fatalf("inbox create: code %d, stderr %q", code, errOut)
	}

	var logs bytes.Buffer
	cfg, err := config.Load(nil, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	srv, closeStore, err := buildServer(t.Context(), cfg, newLogger(&logs, cfg))
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	send := func(path, tokenValue, body string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if tokenValue != "" {
			req.Header.Set("X-Inbox-Token", tokenValue)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	if code, body := send("/in/e2e-box", "e2e-shared-secret", `{"hello":"world"}`); code != http.StatusOK || strings.TrimSpace(body) != `{"seq":1}` {
		t.Fatalf("first delivery: %d %q", code, body)
	}
	if code, body := send("/in/e2e-box/sub/path?q=1", "e2e-shared-secret", "second"); code != http.StatusOK || strings.TrimSpace(body) != `{"seq":2}` {
		t.Fatalf("second delivery: %d %q", code, body)
	}
	if code, _ := send("/in/e2e-box", "wrong-secret", "x"); code != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d", code)
	}
	if code, _ := send("/in/e2e-box", "", "x"); code != http.StatusUnauthorized {
		t.Errorf("no secret: %d", code)
	}
	if code, _ := send("/in/nobody", "e2e-shared-secret", "x"); code != http.StatusNotFound {
		t.Errorf("unknown inbox: %d", code)
	}
	resp, err := http.Get(base + "/readyz") //nolint:noctx // test against a local listener
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz: %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not shut down")
	}
	closeStore()

	// Everything acknowledged is on disk after the server has stopped.
	store, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	in, err := store.GetInboxBySlug(t.Context(), "e2e-box")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.Get(t.Context(), in.ID, 2)
	if err != nil || string(m.Body) != "second" || m.PathSuffix != "sub/path" || m.Query != "q=1" || !m.Verified {
		t.Errorf("message 2 = %+v, %v", m, err)
	}
	if m.Headers.Get("X-Inbox-Token") != "" {
		t.Error("the shared token was stored")
	}
	if m1, err := store.Get(t.Context(), in.ID, 1); err != nil || string(m1.Body) != `{"hello":"world"}` {
		t.Errorf("message 1 = %v, %v", m1, err)
	}

	for _, secret := range []string{"e2e-shared-secret", "admin-token-for-the-serve-end-to-end-test", "wrong-secret", `{"hello":"world"}`} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the logs contain %q", secret)
		}
	}
}

func TestServeFailsReadablyWhenTheDataDirectoryIsUnusable(t *testing.T) {
	file := t.TempDir() + "/a-file"
	if err := writeFile(file); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"HOOKINLET_DATA_DIR":    file + "/sub",
		"HOOKINLET_ADMIN_TOKEN": "a-valid-admin-token-of-sufficient-length-0123",
		"HOOKINLET_MASTER_KEY":  masterKey(),
	}
	code, _, errOut := runCmd(t, env, "serve")
	if code != exitRuntime || !strings.Contains(errOut, "server setup failed") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

// A master key that is not the one the secrets were sealed under stops the server
// before it listens, says what to do, and `rekey` is what does it.
func TestServeRefusesTheWrongMasterKeyAndRekeyFixesIt(t *testing.T) {
	dir := t.TempDir()
	oldKey := masterKey()
	newKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	env := func(key string, more ...string) map[string]string { //nolint:gosec // fake values for a test
		m := map[string]string{
			"HOOKINLET_DATA_DIR":        dir,
			"HOOKINLET_ADMIN_TOKEN":     "admin-token-for-the-rekey-test-0123456789",
			"HOOKINLET_MASTER_KEY":      key,
			"HOOKINLET_VERIFIER_SECRET": "a-shared-secret",
			"HOOKINLET_LISTEN_ADDR":     "127.0.0.1:0",
		}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}
	if code, _, e := runCmd(t, env(oldKey), "inbox", "create", "--slug", "keyed", "--verifier", "token"); code != 0 {
		t.Fatal(e)
	}

	var errOut bytes.Buffer
	code := serve(nil, func(k string) string { return env(newKey)[k] }, &errOut)
	if code != exitRuntime || !strings.Contains(errOut.String(), "hookinlet rekey") || !strings.Contains(errOut.String(), "keyed") {
		t.Fatalf("serve with the wrong key: code %d\n%s", code, errOut.String())
	}
	if strings.Contains(errOut.String(), newKey) || strings.Contains(errOut.String(), oldKey) {
		t.Error("a key was printed")
	}

	// A wrong old key changes nothing and says so.
	wrongOld := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32))
	if code, _, e := runCmd(t, env(newKey, "HOOKINLET_OLD_MASTER_KEY", wrongOld), "rekey"); code != exitRuntime || !strings.Contains(e, "nothing was changed") {
		t.Errorf("rekey with a wrong old key: %d %q", code, e)
	}
	if code, out, e := runCmd(t, env(newKey, "HOOKINLET_OLD_MASTER_KEY", oldKey), "rekey"); code != 0 || !strings.Contains(out, "sealed 1 secret") {
		t.Fatalf("rekey: %d %q %q", code, out, e)
	}
	cfg, err := config.Load(nil, func(k string) string { return env(newKey)[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	srv, closeStore, err := buildServer(t.Context(), cfg, newLogger(io.Discard, cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	if err := srv.CheckSecrets(t.Context()); err != nil {
		t.Errorf("after rekeying: %v", err)
	}
	// And a bad old key is a usage error, not a crash.
	if code, _, _ := runCmd(t, env(newKey, "HOOKINLET_OLD_MASTER_KEY", "not base64!"), "rekey"); code != exitUsage {
		t.Errorf("a malformed old key: %d", code)
	}
}

// A second server on a data directory that a server is using stops at once, and the
// commands that need the server stopped refuse to run beside one.
func TestASecondServerAndTheMaintenanceCommandsRefuseARunningDirectory(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{ //nolint:gosec // fake values for a test
		"HOOKINLET_DATA_DIR":    dir,
		"HOOKINLET_ADMIN_TOKEN": "admin-token-for-the-lock-test-0123456789",
		"HOOKINLET_MASTER_KEY":  masterKey(),
		"HOOKINLET_LISTEN_ADDR": "127.0.0.1:0",
	}
	cfg, err := config.Load(nil, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, closeStore, err := buildServer(t.Context(), cfg, newLogger(io.Discard, cfg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = buildServer(t.Context(), cfg, newLogger(io.Discard, cfg))
	var locked *sqlite.LockedError
	if !errors.As(err, &locked) {
		t.Errorf("a second server: %v", err)
	}
	for _, args := range [][]string{{"rekey"}, {"renew-epochs"}} {
		if code, _, e := runCmd(t, env, args...); code != exitRuntime || !strings.Contains(e, "holds the lock") {
			t.Errorf("%v beside a server: %d %q", args, code, e)
		}
	}
	// The ordinary commands work in the directory of a running server, as they did.
	if code, _, e := runCmd(t, env, "inbox", "list"); code != 0 {
		t.Errorf("inbox list beside a server: %d %q", code, e)
	}
	closeStore()
	if code, _, e := runCmd(t, env, "renew-epochs"); code != 0 {
		t.Errorf("renew-epochs after the server stopped: %d %q", code, e)
	}
}
