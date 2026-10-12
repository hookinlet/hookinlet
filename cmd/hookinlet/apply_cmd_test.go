// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/applyfile"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/server"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

const (
	fakeHookSecret   = "fake-hook-secret-VALUE-1111"
	fakeFileSecret   = "fake-file-secret-VALUE-2222"
	fakeUpdateSecret = "fake-rotated-secret-VALUE-3333" //nolint:gosec // a fake value; the tests prove it is never printed
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// applyEnv is a real in-process server whose log is kept, and which counts the
// requests that change anything, so tests can say "nothing was sent".
type applyEnv struct {
	url    string
	logs   *syncBuf
	writes *atomic.Int64 // POST, PUT, PATCH, DELETE to /admin
	t      *testing.T
	dir    string
}

func newApplyEnv(t *testing.T) *applyEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	box, err := secretbox.New([]byte(strings.Repeat("k", secretbox.KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	srv, err := server.New(server.Options{
		Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Store: store, SecretBox: box, Authenticator: token.NewAuthenticator(store, cliAdminToken),
		Notifier: local.New(), RetentionInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/admin") && r.Method != http.MethodGet {
			writes.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return &applyEnv{url: ts.URL, logs: logs, writes: &writes, t: t, dir: dir}
}

func (e *applyEnv) env(extra map[string]string) map[string]string {
	m := map[string]string{"HOOKINLET_URL": e.url, "HOOKINLET_ADMIN_TOKEN": cliAdminToken}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (e *applyEnv) views() map[string]admin.View {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.url+"/v1/admin/inboxes", nil)
	req.Header.Set("Authorization", "Bearer "+cliAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct{ Inboxes []admin.View }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatal(err)
	}
	m := map[string]admin.View{}
	for _, v := range out.Inboxes {
		m[v.Slug] = v
	}
	return m
}

// deliver posts to an inbox with the token verifier's header.
func (e *applyEnv) deliver(slug, tokenHeader string) int {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.url+"/in/"+e.views()[slug].IngestKey, strings.NewReader(`{"n":1}`))
	req.Header.Set("X-Inbox-Token", tokenHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readTokens(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path inside a temp directory this test created
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not JSON: %v", path, err)
	}
	return m
}

const twoInboxes = `
inboxes:
  - slug: hook-one
    name: Hook one
    verifier: token
    secret_env: HOOK_SECRET
    on_verify_fail: reject
    retention_days: 30
    event_type_pointer: /type
  - slug: hook-two
    verifier: hmac
    verifier_config: { header: X-Sig, algorithm: sha256, encoding: hex }
    secret_file: SECRETFILE
    max_body_bytes: 2048
`

// mustNotLeak fails if any of the values appears in any of the texts.
func mustNotLeak(t *testing.T, what []string, values ...string) {
	t.Helper()
	for _, v := range values {
		if v == "" {
			t.Fatal("empty needle")
		}
		for _, w := range what {
			if strings.Contains(w, v) {
				t.Errorf("a secret or token leaked: %q found in:\n%s", v, w)
			}
		}
	}
}

func TestApplyCreatesThenIsIdempotentThenUpdates(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	secretFile := writeTestFile(t, dir, "hmac-secret", fakeFileSecret+"\n")
	f := writeTestFile(t, dir, "inboxes.yaml", strings.Replace(twoInboxes, "SECRETFILE", secretFile, 1))
	tokens := filepath.Join(dir, "tokens.json")
	env := e.env(map[string]string{"HOOK_SECRET": fakeHookSecret})
	var allOut []string

	// 1. Create.
	code, out, errOut := runCmd(t, env, "apply", "-f", f, "--tokens-out", tokens)
	allOut = append(allOut, out, errOut)
	if code != 0 {
		t.Fatalf("apply: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"created", "hook-one", "hook-two", "secret set", "2 created, 0 updated, 0 unchanged, 2 secrets set"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	st, err := os.Stat(tokens)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("tokens file: %v mode %v", err, st)
	}
	toks := readTokens(t, tokens)
	if len(toks) != 2 || !strings.HasPrefix(toks["hook-one"], "hki_") || !strings.HasPrefix(toks["hook-two"], "hki_") || toks["hook-one"] == toks["hook-two"] {
		t.Fatalf("tokens: %v", toks)
	}
	// The tokens work, and the secrets were stored as given (the file's without its newline).
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+"/v1/inboxes/hook-one", nil)
	req.Header.Set("Authorization", "Bearer "+toks["hook-one"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("agent token does not work: %v %v", err, resp)
	}
	_ = resp.Body.Close()
	if got := e.deliver("hook-one", fakeHookSecret); got != 200 {
		t.Errorf("delivery with the right secret: %d", got)
	}
	if got := e.deliver("hook-one", "wrong"); got != 401 {
		t.Errorf("delivery with the wrong secret: %d (negative test)", got)
	}
	v := e.views()["hook-one"]
	if v.Name != "Hook one" || v.RetentionDays != 30 || v.EventTypePointer != "/type" || !v.HasVerifierSecret {
		t.Errorf("hook-one: %+v", v)
	}
	if two := e.views()["hook-two"]; two.Name != "hook-two" || two.MaxBodyBytes != 2048 || two.Verifier != "hmac" || !two.HasVerifierSecret {
		t.Errorf("hook-two: %+v (name defaults to the slug)", two)
	}

	// 2. Run it again: nothing changes but the secrets, which are set again.
	writesBefore := e.writes.Load()
	again := filepath.Join(dir, "again.json")
	code, out, errOut = runCmd(t, env, "apply", "-f", f, "--tokens-out", again)
	allOut = append(allOut, out, errOut)
	if code != 0 {
		t.Fatalf("second apply: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "0 created, 0 updated, 2 unchanged, 2 secrets set") || strings.Contains(out, "created  ") {
		t.Errorf("second run is not idempotent:\n%s", out)
	}
	// Two PUTs for the secrets and nothing else.
	if got := e.writes.Load() - writesBefore; got != 2 {
		t.Errorf("the second run made %d changing requests, want 2 (the secrets only)", got)
	}
	if _, err := os.Stat(again); !os.IsNotExist(err) {
		t.Errorf("a tokens file was made although nothing was created: %v", err)
	}

	// 3. Change one declared field, and one thing the file does not mention.
	if code, _, errOut := runCmd(t, env, "inbox", "update", "hook-two", "--name", "Renamed by hand"); code != 0 {
		t.Fatal(errOut)
	}
	f2 := writeTestFile(t, dir, "inboxes2.yaml", strings.Replace(strings.Replace(twoInboxes, "SECRETFILE", secretFile, 1), "retention_days: 30", "retention_days: 7", 1))
	code, out, errOut = runCmd(t, env, "apply", "-f", f2)
	allOut = append(allOut, out, errOut)
	if code != 0 {
		t.Fatalf("third apply: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "updated           hook-one: retention_days") || !strings.Contains(out, "1 updated, 1 unchanged") {
		t.Errorf("update not reported as one field:\n%s", out)
	}
	vs := e.views()
	if vs["hook-one"].RetentionDays != 7 || vs["hook-one"].Name != "Hook one" {
		t.Errorf("hook-one after update: %+v", vs["hook-one"])
	}
	if vs["hook-two"].Name != "Renamed by hand" {
		t.Errorf("a field the file does not declare was changed: %+v", vs["hook-two"])
	}

	// 4. A new secret in the environment replaces the old one.
	env["HOOK_SECRET"] = fakeUpdateSecret
	if code, _, errOut := runCmd(t, env, "apply", "-f", f); code != 0 {
		t.Fatal(errOut)
	}
	if got := e.deliver("hook-one", fakeUpdateSecret); got != 200 {
		t.Errorf("the replaced secret does not work: %d", got)
	}
	if got := e.deliver("hook-one", fakeHookSecret); got != 401 {
		t.Errorf("the old secret still works: %d", got)
	}

	// Nothing the user saw, and nothing the server logged, has a secret or a token.
	allOut = append(allOut, e.logs.String())
	mustNotLeak(t, allOut, fakeHookSecret, fakeFileSecret, fakeUpdateSecret, toks["hook-one"], toks["hook-two"])
	mustNotLeak(t, allOut, strings.TrimPrefix(toks["hook-one"], "hki_"))
}

func TestApplyWithoutTokensOutRefusesToCreate(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: new-one\n    verifier: none\n  - slug: new-two\n    verifier: none\n")
	for _, args := range [][]string{{"apply", "-f", f}, {"apply", "-f", f, "--dry-run"}} {
		code, out, errOut := runCmd(t, e.env(nil), args...)
		if code != exitUsage || !strings.Contains(errOut, "--tokens-out") {
			t.Errorf("%v: code %d out %q err %q", args, code, out, errOut)
		}
		mustNotLeak(t, []string{out, errOut}, "hki_")
	}
	if len(e.views()) != 0 || e.writes.Load() != 0 {
		t.Errorf("something was changed: %v, %d writes", e.views(), e.writes.Load())
	}
	// With nothing to create, the flag is not needed.
	if code, _, errOut := runCmd(t, e.env(nil), "apply", "-f", f, "--tokens-out", filepath.Join(dir, "t.json")); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := runCmd(t, e.env(nil), "apply", "-f", f); code != 0 {
		t.Errorf("an existing inbox needs no --tokens-out: %s", errOut)
	}
}

func TestApplyTokensOutIsCheckedBeforeAnyChange(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: new-one\n    verifier: none\n")
	existing := writeTestFile(t, dir, "taken.json", "precious")
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"an existing file":    existing,
		"a symlink":           link,
		"a missing directory": filepath.Join(dir, "no-such-dir", "t.json"),
	} {
		for _, dry := range []bool{false, true} {
			args := []string{"apply", "-f", f, "--tokens-out", path}
			if dry {
				args = append(args, "--dry-run")
			}
			code, _, errOut := runCmd(t, e.env(nil), args...)
			if code == 0 || !strings.Contains(errOut, "--tokens-out") || !strings.Contains(errOut, "nothing was changed") {
				t.Errorf("%s (dry run %v): code %d err %q", name, dry, code, errOut)
			}
		}
	}
	b, _ := os.ReadFile(existing) //nolint:gosec // a temp file of this test
	if string(b) != "precious" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
	if len(e.views()) != 0 || e.writes.Load() != 0 {
		t.Errorf("an inbox was created although the tokens had nowhere to go: %v", e.views())
	}
}

func TestApplyDryRunChangesNothing(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: dry-one\n    verifier: token\n    secret_env: HOOK_SECRET\n    retention_days: 5\n")
	env := e.env(map[string]string{"HOOK_SECRET": fakeHookSecret})
	tokens := filepath.Join(dir, "t.json")

	code, out, errOut := runCmd(t, env, "apply", "-f", f, "--dry-run", "--tokens-out", tokens)
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, want := range []string{"would create", "dry-one", "would set secret", "dry run: nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	if len(e.views()) != 0 || e.writes.Load() != 0 {
		t.Errorf("a dry run changed the server")
	}
	if _, err := os.Stat(tokens); !os.IsNotExist(err) {
		t.Errorf("a dry run made the tokens file: %v", err)
	}
	mustNotLeak(t, []string{out, errOut, e.logs.String()}, fakeHookSecret)

	// An existing inbox that differs: planned, not applied.
	if code, _, errOut := runCmd(t, env, "apply", "-f", f, "--tokens-out", tokens); code != 0 {
		t.Fatal(errOut)
	}
	changed := writeTestFile(t, dir, "c.yaml", "inboxes:\n  - slug: dry-one\n    verifier: token\n    secret_env: HOOK_SECRET\n    retention_days: 9\n")
	writes := e.writes.Load()
	code, out, _ = runCmd(t, env, "apply", "-f", changed, "--dry-run")
	if code != 0 || !strings.Contains(out, "would update") || !strings.Contains(out, "retention_days") {
		t.Errorf("%d %s", code, out)
	}
	if e.writes.Load() != writes || e.views()["dry-one"].RetentionDays != 5 {
		t.Error("a dry run changed an existing inbox")
	}
	// A dry run still demands the secret be there.
	code, _, errOut = runCmd(t, e.env(nil), "apply", "-f", changed, "--dry-run")
	if code != exitUsage || !strings.Contains(errOut, "not set or is empty") || strings.Contains(errOut, "HOOK_SECRET") {
		t.Errorf("dry run with a missing secret: %d %q", code, errOut)
	}
}

func TestApplyChecksTheWholeFileBeforeTouchingTheServer(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	tokens := filepath.Join(dir, "t.json")
	for name, tc := range map[string]struct {
		yaml, wantErr string
		env           map[string]string
	}{
		"unknown key in the second inbox":  {"inboxes:\n  - slug: good-one\n    verifier: none\n  - slug: bad-one\n    verifier: none\n    retenton_days: 3\n", "line 6", nil},
		"literal secret":                   {"inboxes:\n  - slug: good-one\n  - slug: bad-one\n    verifier: token\n    secret: abc\n", "secret_env", nil},
		"missing environment variable":     {"inboxes:\n  - slug: good-one\n  - slug: bad-one\n    verifier: token\n    secret_env: NOT_SET_ANYWHERE\n", "line 5", nil},
		"missing secret file":              {"inboxes:\n  - slug: good-one\n  - slug: bad-one\n    verifier: token\n    secret_file: " + filepath.Join(dir, "absent") + "\n", "line 5", nil},
		"configuration without a verifier": {"inboxes:\n  - slug: good-one\n    verifier: none\n  - slug: bad-one\n    verifier: none\n    verifier_config: {tolerance_seconds: 300}\n", "bad-one", nil},
		"a blank value":                    {"inboxes:\n  - slug: good-one\n    verifier: none\n  - slug: bad-one\n    verifier: none\n    retention_days:\n", "leave the key out", nil},
		"an empty on_verify_fail":          {"inboxes:\n  - slug: good-one\n    verifier: none\n  - slug: bad-one\n    verifier: none\n    on_verify_fail: ''\n", "on_verify_fail must be one of", nil},
		"bad verifier configuration":       {"inboxes:\n  - slug: good-one\n  - slug: bad-one\n    verifier: stripe\n    verifier_config: {tolerance_seconds: soon}\n", "tolerance", nil},
		"bad slug":                         {"inboxes:\n  - slug: good-one\n    verifier: none\n  - slug: Bad_One\n    verifier: none\n", "slug must be", nil},
	} {
		f := writeTestFile(t, dir, "bad.yaml", tc.yaml)
		code, out, errOut := runCmd(t, e.env(tc.env), "apply", "-f", f, "--tokens-out", tokens)
		if code != exitUsage || !strings.Contains(errOut, tc.wantErr) {
			t.Errorf("%s: code %d out %q err %q", name, code, out, errOut)
		}
		if _, err := os.Stat(tokens); !os.IsNotExist(err) {
			t.Errorf("%s: the tokens file was created", name)
		}
	}
	if len(e.views()) != 0 || e.writes.Load() != 0 {
		t.Errorf("a bad file changed the server: %v", e.views())
	}
	if code, _, errOut := runCmd(t, e.env(nil), "apply"); code != exitUsage || !strings.Contains(errOut, "-f") {
		t.Errorf("no -f: %d %q", code, errOut)
	}
	if code, _, _ := runCmd(t, e.env(nil), "apply", "-f", filepath.Join(dir, "missing.yaml")); code != exitUsage {
		t.Errorf("missing file: %d", code)
	}
}

func TestApplyNeverDeletesAndListsWhatItDoesNotManage(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	env := e.env(nil)
	if code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "legacy-inbox", "--verifier", "none"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "other-legacy", "--verifier", "none"); code != 0 {
		t.Fatal(errOut)
	}
	f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: managed-one\n    verifier: none\n")
	code, out, errOut := runCmd(t, env, "apply", "-f", f, "--tokens-out", filepath.Join(dir, "t.json"))
	if code != 0 {
		t.Fatal(errOut)
	}
	if !strings.Contains(out, "not managed") || !strings.Contains(out, "legacy-inbox, other-legacy") || !strings.Contains(out, "2 not managed") {
		t.Errorf("not-managed inboxes are not listed:\n%s", out)
	}
	if len(e.views()) != 3 {
		t.Errorf("an inbox was deleted: %v", e.views())
	}
	// There is no way to ask apply to prune.
	if code, _, _ := runCmd(t, env, "apply", "-f", f, "--prune"); code != exitUsage {
		t.Errorf("--prune is accepted: %d", code)
	}
}

func TestApplyStopsAtAServerRefusalAndKeepsTheTokensAlreadyIssued(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	// A verifier this binary does not know passes the file check (a server
	// may have registered it) and is refused by the server.
	f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: first-one\n    verifier: none\n  - slug: second-one\n    verifier: custom-thing\n  - slug: third-one\n    verifier: none\n")
	tokens := filepath.Join(dir, "t.json")
	code, out, errOut := runCmd(t, e.env(nil), "apply", "-f", f, "--tokens-out", tokens)
	if code != exitRuntime || !strings.Contains(errOut, "second-one") || !strings.Contains(errOut, "unknown verifier") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	vs := e.views()
	if _, ok := vs["first-one"]; !ok || len(vs) != 1 {
		t.Errorf("state after the failure: %v", vs)
	}
	if toks := readTokens(t, tokens); len(toks) != 1 || !strings.HasPrefix(toks["first-one"], "hki_") {
		t.Errorf("the token of the inbox that was created is not in the file: %v", toks)
	}
	if !strings.Contains(out, "first-one") {
		t.Errorf("the summary omits what was done:\n%s", out)
	}
	mustNotLeak(t, []string{out, errOut}, readTokens(t, tokens)["first-one"])
}

func TestApplyFromStandardInputAndJSON(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	stdin = strings.NewReader("inboxes:\n  - slug: piped-one\n    verifier: none\n    retention_days: 2\n")
	t.Cleanup(func() { stdin = os.Stdin })
	tokens := filepath.Join(dir, "t.json")
	code, out, errOut := runCmd(t, e.env(nil), "apply", "-f", "-", "--json", "--tokens-out", tokens)
	if code != 0 {
		t.Fatal(errOut)
	}
	var rep struct {
		DryRun     bool                `json:"dry_run"`
		Created    []string            `json:"created"`
		Updated    map[string][]string `json:"updated"`
		Unchanged  []string            `json:"unchanged"`
		SecretSet  []string            `json:"secret_set"`
		NotManaged []string            `json:"not_managed"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Created) != 1 || rep.Created[0] != "piped-one" || rep.Unchanged == nil || rep.NotManaged == nil {
		t.Fatalf("--json output: %v\n%s", err, out)
	}
	mustNotLeak(t, []string{out}, readTokens(t, tokens)["piped-one"])
}

func TestApplyDirectMode(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	env := map[string]string{"HOOKINLET_DATA_DIR": data, "HOOKINLET_MASTER_KEY": masterKey(), "HOOK_SECRET": fakeHookSecret}
	f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: direct-one\n    verifier: token\n    secret_env: HOOK_SECRET\n")
	tokens := filepath.Join(dir, "t.json")

	// A dry run before any database exists plans against nothing and makes nothing.
	code, out, errOut := runCmd(t, env, "apply", "-f", f, "--dry-run", "--tokens-out", tokens)
	if code != 0 || !strings.Contains(out, "would create") {
		t.Fatalf("dry run: %d %s %s", code, out, errOut)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("a dry run made the data directory: %v", err)
	}
	// Secrets need the master key, and that is found out before anything is made.
	noKey := map[string]string{"HOOKINLET_DATA_DIR": data, "HOOK_SECRET": fakeHookSecret}
	if code, _, errOut := runCmd(t, noKey, "apply", "-f", f, "--tokens-out", tokens); code != exitUsage || !strings.Contains(errOut, "HOOKINLET_MASTER_KEY") {
		t.Errorf("no master key: %d %q", code, errOut)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("a refused run made the data directory: %v", err)
	}

	if code, out, errOut = runCmd(t, env, "apply", "-f", f, "--tokens-out", tokens); code != 0 {
		t.Fatalf("apply: %d %s %s", code, out, errOut)
	}
	code, out, _ = runCmd(t, env, "apply", "-f", f)
	if code != 0 || !strings.Contains(out, "0 created, 0 updated, 1 unchanged, 1 secrets set") {
		t.Errorf("rerun: %d\n%s", code, out)
	}
	code, out, _ = runCmd(t, env, "inbox", "show", "--json", "direct-one")
	if code != 0 || !strings.Contains(out, `"has_verifier_secret": true`) {
		t.Errorf("show: %d %s", code, out)
	}
	mustNotLeak(t, []string{out}, fakeHookSecret)
	// Both --url and --data-dir is still an error.
	both := map[string]string{"HOOKINLET_DATA_DIR": data, "HOOKINLET_URL": "http://localhost:1", "HOOKINLET_ADMIN_TOKEN": "x"}
	if code, _, _ := runCmd(t, both, "apply", "-f", f); code != exitUsage {
		t.Errorf("url and data-dir together: %d", code)
	}
}

// A secret pasted where the name of its variable or its file belongs is not
// repeated in anything apply says, even when it looks like a name.
func TestApplyNeverEchoesWhatWasWrittenAsASecretSource(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	const looksLikeAName = "whsec_9fK2abcDEF0123456789xyz"
	for _, key := range []string{"secret_env", "secret_file"} {
		f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: pasted-one\n    verifier: token\n    "+key+": "+looksLikeAName+"\n")
		for _, dry := range []bool{false, true} {
			args := []string{"apply", "-f", f, "--tokens-out", filepath.Join(dir, "t.json")}
			if dry {
				args = append(args, "--dry-run")
			}
			code, out, errOut := runCmd(t, e.env(nil), args...)
			if code == 0 {
				t.Errorf("%s dry=%v: accepted", key, dry)
			}
			mustNotLeak(t, []string{out, errOut, e.logs.String()}, looksLikeAName)
		}
	}
}

// A blank value is not a zero: it would switch retention off or re-enable an
// inbox that was disabled.
func TestApplyDoesNotTreatABlankValueAsASetting(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	tokens := filepath.Join(dir, "t.json")
	first := writeTestFile(t, dir, "a.yaml", "inboxes:\n  - slug: kept-one\n    verifier: none\n    retention_days: 30\n    disabled: true\n")
	if code, _, errOut := runCmd(t, e.env(nil), "apply", "-f", first, "--tokens-out", tokens); code != 0 {
		t.Fatal(errOut)
	}
	blank := writeTestFile(t, dir, "b.yaml", "inboxes:\n  - slug: kept-one\n    verifier: none\n    retention_days:\n    disabled:\n")
	if code, _, errOut := runCmd(t, e.env(nil), "apply", "-f", blank); code != exitUsage || !strings.Contains(errOut, "leave the key out") {
		t.Errorf("a blank value was accepted: %d %q", code, errOut)
	}
	if v := e.views()["kept-one"]; v.RetentionDays != 30 || !v.Disabled {
		t.Errorf("a blank value changed the inbox: %+v", v)
	}
}

// A file whose later inbox is bad creates nothing; and a run that fails before any
// token was issued leaves no empty tokens file in the way of the next try.
func TestApplyLeavesNoEmptyTokensFileAndNothingHalfDone(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	tokens := filepath.Join(dir, "t.json")
	f := writeTestFile(t, dir, "i.yaml", "inboxes:\n  - slug: first-ok\n    verifier: none\n  - slug: second-bad\n    verifier: none\n    verifier_config: {tolerance_seconds: 300}\n")
	if code, _, errOut := runCmd(t, e.env(nil), "apply", "-f", f, "--tokens-out", tokens); code != exitUsage {
		t.Fatalf("%d %s", code, errOut)
	}
	if len(e.views()) != 0 {
		t.Errorf("a bad file created inboxes: %v", e.views())
	}
	// The first create fails at the server: no token was issued, so no file is left.
	g := writeTestFile(t, dir, "j.yaml", "inboxes:\n  - slug: only-one\n    verifier: custom-thing\n")
	if code, _, _ := runCmd(t, e.env(nil), "apply", "-f", g, "--tokens-out", tokens); code != exitRuntime {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(tokens); !os.IsNotExist(err) {
		t.Errorf("an empty tokens file was left behind: %v", err)
	}
	// After a partial failure the file holds what was issued, and the message says
	// to give the next run a new file.
	h := writeTestFile(t, dir, "k.yaml", "inboxes:\n  - slug: made-one\n    verifier: none\n  - slug: refused-one\n    verifier: custom-thing\n")
	code, _, errOut := runCmd(t, e.env(nil), "apply", "-f", h, "--tokens-out", tokens)
	if code != exitRuntime || !strings.Contains(errOut, "new file") {
		t.Errorf("code %d err %q", code, errOut)
	}
	if toks := readTokens(t, tokens); len(toks) != 1 {
		t.Errorf("tokens = %v", toks)
	}
}

type failingSecretAPI struct{ adminAPI }

func (failingSecretAPI) Update(context.Context, string, admin.Patch) (admin.View, error) {
	return admin.View{}, nil
}
func (failingSecretAPI) SetSecret(context.Context, string, string) (admin.View, error) {
	return admin.View{}, errors.New("the store is unavailable")
}

// When the secret cannot be stored, what apply says names the inbox and the
// reason, and never the secret.
func TestApplyDoesNotLeakTheSecretWhenStoringItFails(t *testing.T) {
	const secret = "s3cr3t-value-that-must-not-appear"
	it := applyfile.Item{Spec: applyfile.Spec{Slug: "abc"}, Action: applyfile.Update, Fields: []string{"retention_days"}, SetSecret: true}
	var out, errOut bytes.Buffer
	rep := &applyReport{w: &out}
	err := applyOne(t.Context(), failingSecretAPI{}, it, secret, false, nil, rep, &errOut)
	if err == nil {
		t.Fatal("the failure was swallowed")
	}
	mustNotLeak(t, []string{err.Error(), out.String(), errOut.String()}, secret)
}

// What a file asks for is checked as a whole before anything is made: a verifier
// that is missing, a secret the verifier cannot use, and an ingest key that two
// inboxes would share each stop the run with nothing created.
func TestApplyRefusesTheWholeFileForWhatTheServerWouldRefuseLater(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	tokens := filepath.Join(dir, "t.json")
	env := e.env(map[string]string{"BAD_SW_SECRET": "not-a-whsec"}) //nolint:gosec // a value made up for a test
	for name, tc := range map[string]struct{ yaml, want string }{
		"a new inbox with no verifier":     {"inboxes:\n  - slug: first-ok\n    verifier: none\n  - slug: second-bad\n", "needs a verifier"},
		"a secret the verifier cannot use": {"inboxes:\n  - slug: first-ok\n    verifier: none\n  - slug: second-bad\n    verifier: standardwebhooks\n    secret_env: BAD_SW_SECRET\n", "cannot be used"},
		"one ingest key for two inboxes":   {"inboxes:\n  - slug: first-ok\n    verifier: none\n    ingest_key: shared-key\n  - slug: second-bad\n    verifier: none\n    ingest_key: shared-key\n", "ingest_key is already used"},
	} {
		f := writeTestFile(t, dir, "bad.yaml", tc.yaml)
		code, _, errOut := runCmd(t, env, "apply", "-f", f, "--tokens-out", tokens)
		if code != exitUsage || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code %d err %q, want %q", name, code, errOut, tc.want)
		}
		if len(e.views()) != 0 {
			t.Fatalf("%s: inboxes were created: %v", name, e.views())
		}
	}
	// A key that another inbox already has is found the same way, and the answer
	// of the server to a conflicting key does not blame the slug.
	if code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "taken-key-owner", "--verifier", "none", "--ingest-key", "owned-key"); code != 0 {
		t.Fatal(errOut)
	}
	f := writeTestFile(t, dir, "k.yaml", "inboxes:\n  - slug: newcomer\n    verifier: none\n    ingest_key: owned-key\n")
	if code, _, errOut := runCmd(t, env, "apply", "-f", f, "--tokens-out", tokens); code != exitUsage || !strings.Contains(errOut, `"taken-key-owner"`) {
		t.Errorf("a key that is taken: code %d err %q", code, errOut)
	}
	code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "newcomer", "--verifier", "none", "--ingest-key", "owned-key")
	if code == 0 || !strings.Contains(errOut, "ingest key is already in use") || strings.Contains(errOut, "named") {
		t.Errorf("the conflict blames the wrong thing: code %d err %q", code, errOut)
	}
}

// Changing an inbox's verifier removes its secret, and the commands say so.
func TestChangingAVerifierSaysThatTheSecretWasRemoved(t *testing.T) {
	e := newApplyEnv(t)
	dir := t.TempDir()
	env := e.env(map[string]string{"HOOKINLET_VERIFIER_SECRET": "a-shared-secret"})
	if code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "was-github", "--verifier", "github"); code != 0 {
		t.Fatal(errOut)
	}
	code, _, errOut := runCmd(t, e.env(nil), "inbox", "update", "was-github", "--verifier", "slack")
	if code != 0 || !strings.Contains(errOut, "secret was removed") {
		t.Errorf("inbox update: code %d, stderr %q", code, errOut)
	}
	// And apply, which has no secret to give it.
	if code, _, errOut := runCmd(t, env, "inbox", "update", "was-github", "--verifier", "github"); code != 0 {
		t.Fatal(errOut)
	}
	f := writeTestFile(t, dir, "v.yaml", "inboxes:\n  - slug: was-github\n    verifier: stripe\n")
	code, out, errOut := runCmd(t, e.env(nil), "apply", "-f", f)
	if code != 0 || !strings.Contains(out, "updated") {
		t.Fatalf("apply: %d %s %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "was-github") || !strings.Contains(errOut, "secret") {
		t.Errorf("apply did not warn about the missing secret: %q", errOut)
	}
}
