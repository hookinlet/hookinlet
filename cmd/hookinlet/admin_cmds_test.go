// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/server"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

const cliAdminToken = "cli-admin-token-0123456789-abcdefghij" //nolint:gosec // a fake value; the tests prove it is never printed

// liveServer runs a real server over a data directory and returns its address
// and the directory.
func liveServer(t *testing.T) (url, dir string) {
	t.Helper()
	dir = t.TempDir()
	store, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	box, err := secretbox.New([]byte(strings.Repeat("k", secretbox.KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{
		Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: store, SecretBox: box,
		Authenticator: token.NewAuthenticator(store, cliAdminToken), Notifier: local.New(), RetentionInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, dir
}

func tokenIn(t *testing.T, out string) string {
	t.Helper()
	m := tokenRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no agent token in:\n%s", out)
	}
	return m[1]
}

// ---- direct mode ----

func TestDirectModeLifecycle(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir, "HOOKINLET_MASTER_KEY": masterKey()}

	if code, _, _ := runCmd(t, env, "migrate"); code != 0 { // a database must exist before it is listed
		t.Fatal("migrate failed")
	}
	if code, out, _ := runCmd(t, env, "inbox", "list"); code != 0 || !strings.Contains(out, "no inboxes") {
		t.Errorf("empty list: %d %q", code, out)
	}
	for _, slug := range []string{"alpha-inbox", "beta-inbox"} {
		if code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", slug, "--verifier", "none"); code != 0 {
			t.Fatalf("create %s: %d %s", slug, code, errOut)
		}
	}
	code, out, _ := runCmd(t, env, "inbox", "list")
	if code != 0 || !strings.Contains(out, "alpha-inbox") || !strings.Contains(out, "beta-inbox") || !strings.Contains(out, "SLUG") {
		t.Errorf("list: %d\n%s", code, out)
	}

	// show, with the flag after the slug and before it.
	for _, args := range [][]string{{"inbox", "show", "alpha-inbox"}, {"inbox", "show", "--json", "alpha-inbox"}, {"inbox", "show", "alpha-inbox", "--json"}} {
		code, out, errOut := runCmd(t, env, args...)
		if code != 0 || !strings.Contains(out, "alpha-inbox") {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	var shown admin.View
	_, out, _ = runCmd(t, env, "inbox", "show", "--json", "alpha-inbox")
	if err := json.Unmarshal([]byte(out), &shown); err != nil || shown.Slug != "alpha-inbox" || shown.MessageCount == nil {
		t.Errorf("show --json: %v %s", err, out)
	}

	// update changes only what is given.
	if code, out, errOut := runCmd(t, env, "inbox", "update", "alpha-inbox", "--name", "Alpha", "--retention-days", "14", "--disabled"); code != 0 {
		t.Fatalf("update: %d %s %s", code, out, errOut)
	}
	_, out, _ = runCmd(t, env, "inbox", "show", "--json", "alpha-inbox")
	_ = json.Unmarshal([]byte(out), &shown)
	if shown.Name != "Alpha" || shown.RetentionDays != 14 || !shown.Disabled || shown.Verifier != "none" {
		t.Errorf("after update: %+v", shown)
	}
	runCmd(t, env, "inbox", "update", "alpha-inbox", "--disabled=false")
	_, out, _ = runCmd(t, env, "inbox", "show", "--json", "alpha-inbox")
	_ = json.Unmarshal([]byte(out), &shown)
	if shown.Disabled {
		t.Error("--disabled=false did not re-enable the inbox")
	}
	if code, _, errOut := runCmd(t, env, "inbox", "update", "alpha-inbox"); code != exitUsage || !strings.Contains(errOut, "nothing to change") {
		t.Errorf("update with nothing: %d %q", code, errOut)
	}

	// set-secret, from the environment and from stdin, and tokens.
	if code, _, errOut := runCmd(t, env, "inbox", "update", "alpha-inbox", "--verifier", "github"); code != 0 {
		t.Fatal(errOut)
	}
	envSecret := map[string]string{"HOOKINLET_DATA_DIR": dir, "HOOKINLET_MASTER_KEY": masterKey(), "HOOKINLET_VERIFIER_SECRET": "secret-one"}
	if code, out, errOut := runCmd(t, envSecret, "inbox", "set-secret", "alpha-inbox"); code != 0 || strings.Contains(out+errOut, "secret-one") {
		t.Errorf("set-secret: %d %q %q", code, out, errOut)
	}
	stdin = strings.NewReader("secret-two\n")
	t.Cleanup(func() { stdin = os.Stdin })
	if code, _, errOut := runCmd(t, env, "inbox", "set-secret", "alpha-inbox", "--secret-stdin"); code != 0 {
		t.Errorf("set-secret from stdin: %d %q", code, errOut)
	}
	if code, _, errOut := runCmd(t, env, "inbox", "set-secret", "alpha-inbox"); code != exitUsage || !strings.Contains(errOut, "no secret given") {
		t.Errorf("set-secret with none: %d %q", code, errOut)
	}
	noKey := map[string]string{"HOOKINLET_DATA_DIR": dir, "HOOKINLET_VERIFIER_SECRET": "x"}
	if code, _, errOut := runCmd(t, noKey, "inbox", "set-secret", "alpha-inbox"); code != exitUsage || !strings.Contains(errOut, "--master-key") {
		t.Errorf("set-secret without a master key: %d %q", code, errOut)
	}
	_, out, _ = runCmd(t, env, "inbox", "show", "alpha-inbox")
	if !strings.Contains(out, "secret set") || strings.Contains(out, "secret-two") {
		t.Errorf("show after set-secret:\n%s", out)
	}

	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	code, out, errOut := runCmd(t, env, "token", "create", "--name", "second", "--inbox", "beta-inbox")
	if code != 0 {
		t.Fatalf("token create: %d %s", code, errOut)
	}
	newTok := tokenRE.FindStringSubmatch(out)
	if newTok == nil {
		t.Fatalf("no token in:\n%s", out)
	}
	if rec, err := s.GetTokenByHash(t.Context(), token.Hash(newTok[1])); err != nil || rec.Name != "second" || len(rec.InboxIDs) != 1 {
		t.Errorf("the new token is not the stored one: %+v, %v", rec, err)
	}
	if code, out, _ := runCmd(t, env, "token", "list", "--inbox", "beta-inbox"); code != 0 || !strings.Contains(out, "second") || !strings.Contains(out, "default") || strings.Contains(out, newTok[1]) {
		t.Errorf("token list: %d\n%s", code, out)
	}
	for _, args := range [][]string{
		{"token", "create", "--name", "x"},
		{"token", "create", "--name", "x", "--all-inboxes", "--inbox", "beta-inbox"},
		{"token", "create", "--inbox", "beta-inbox"},
		{"token", "revoke"},
		{"token", "revoke", "abc"},
		{"token", "nonsense"},
	} {
		if code, _, _ := runCmd(t, env, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}
	if code, _, _ := runCmd(t, env, "token", "create", "--name", "x", "--inbox", "no-such-inbox"); code == 0 {
		t.Error("a token for an unknown inbox was created")
	}
	rec, _ := s.GetTokenByHash(t.Context(), token.Hash(newTok[1]))
	if code, _, errOut := runCmd(t, env, "token", "revoke", strconv.FormatInt(int64(rec.ID), 10)); code != 0 {
		t.Fatalf("token revoke: %d %s", code, errOut)
	}
	if _, err := s.GetTokenByHash(t.Context(), token.Hash(newTok[1])); err == nil {
		t.Error("a revoked token still resolves")
	}
	if code, _, _ := runCmd(t, env, "token", "revoke", strconv.FormatInt(int64(rec.ID), 10)); code == 0 {
		t.Error("revoking twice succeeded")
	}

	// delete needs --yes.
	if code, _, errOut := runCmd(t, env, "inbox", "delete", "beta-inbox"); code != exitUsage || !strings.Contains(errOut, "--yes") {
		t.Errorf("delete without --yes: %d %q", code, errOut)
	}
	if _, err := s.GetInboxBySlug(t.Context(), "beta-inbox"); err != nil {
		t.Error("an unconfirmed delete deleted the inbox")
	}
	if code, _, errOut := runCmd(t, env, "inbox", "delete", "beta-inbox", "--yes"); code != 0 {
		t.Errorf("delete: %d %s", code, errOut)
	}
	if _, err := s.GetInboxBySlug(t.Context(), "beta-inbox"); err == nil {
		t.Error("the inbox is still there")
	}
	if code, _, errOut := runCmd(t, env, "inbox", "show", "beta-inbox"); code != exitRuntime || !strings.Contains(errOut, "no such inbox") {
		t.Errorf("show a deleted inbox: %d %q", code, errOut)
	}
}

func TestDirectModeRetentionAndBackup(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir}
	if code, _, e := runCmd(t, env, "inbox", "create", "--slug", "kept-small", "--verifier", "none", "--retention-max-count", "2"); code != 0 {
		t.Fatal(e)
	}
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := s.GetInboxBySlug(t.Context(), "kept-small")
	for range 5 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	if code, out, e := runCmd(t, env, "retention", "run"); code != 0 || !strings.Contains(out, "deleted 3 messages") {
		t.Errorf("retention run: %d %q %q", code, out, e)
	}
	if code, out, _ := runCmd(t, env, "retention", "run", "--json"); code != 0 || !strings.Contains(out, `"deleted": 0`) {
		t.Errorf("a second run: %d %q", code, out)
	}
	if code, _, _ := runCmd(t, env, "retention"); code != exitUsage {
		t.Errorf("retention with no command: %d", code)
	}

	file := filepath.Join(t.TempDir(), "copy.db")
	if code, out, e := runCmd(t, env, "backup", "--out", file); code != 0 || !strings.Contains(out, "wrote ") {
		t.Fatalf("backup: %d %q %q", code, out, e)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("backup file: %v %v", info, err)
	}
	if code, _, e := runCmd(t, env, "backup", "--out", file); code == 0 || !strings.Contains(e, "already exists") {
		t.Errorf("a second backup over the first: %d %q", code, e)
	}
	if code, _, e := runCmd(t, env, "backup", "--out", file, "--force"); code != 0 {
		t.Errorf("--force: %d %q", code, e)
	}
	if code, _, _ := runCmd(t, env, "backup"); code != exitUsage {
		t.Errorf("backup without --out: %d", code)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(file), ".hookinlet-backup-*"))
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	// The copy is a working database holding the kept messages.
	restored := t.TempDir()
	data, _ := os.ReadFile(file)                                                                  //nolint:gosec // a path inside a temp directory this test created
	if err := os.WriteFile(filepath.Join(restored, sqlite.DBFileName), data, 0o600); err != nil { //nolint:gosec // same
		t.Fatal(err)
	}
	r, err := sqlite.Open(t.Context(), restored)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	rin, err := r.GetInboxBySlug(t.Context(), "kept-small")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Stats(t.Context(), rin.ID); st.MessageCount != 2 || st.LatestSeq != 5 {
		t.Errorf("restored stats: %+v", st)
	}
}

// ---- through a running server ----

func TestRemoteModeLifecycle(t *testing.T) {
	url, dir := liveServer(t)
	env := map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": cliAdminToken}

	code, out, errOut := runCmd(t, env, "inbox", "create", "--slug", "remote-one", "--verifier", "token", "--name", "Remote")
	if code != 0 {
		t.Fatalf("create: %d %s", code, errOut)
	}
	agentTok := tokenIn(t, out)
	if !strings.Contains(out, "ingest url:   "+url+"/in/") || !strings.Contains(out, "warning:") {
		t.Errorf("create output:\n%s", out)
	}
	if strings.Contains(out+errOut, cliAdminToken) {
		t.Error("the admin token was printed")
	}

	// What the server holds is what the command said.
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	in, err := s.GetInboxBySlug(t.Context(), "remote-one")
	if err != nil {
		t.Fatalf("stored inbox: %+v %v", in, err)
	}
	if rec, err := s.GetTokenByHash(t.Context(), token.Hash(agentTok)); err != nil || len(rec.InboxIDs) != 1 || rec.InboxIDs[0] != in.ID {
		t.Fatalf("stored token: %+v %v", rec, err)
	}

	if code, out, _ := runCmd(t, env, "inbox", "list"); code != 0 || !strings.Contains(out, "remote-one") {
		t.Errorf("list: %d %q", code, out)
	}
	if code, out, _ := runCmd(t, env, "inbox", "show", "remote-one"); code != 0 || !strings.Contains(out, "ingest url") || !strings.Contains(out, "no secret set") {
		t.Errorf("show: %d\n%s", code, out)
	}
	if code, _, e := runCmd(t, env, "inbox", "update", "remote-one", "--retention-days", "7", "--name", "Renamed"); code != 0 {
		t.Errorf("update: %d %s", code, e)
	}
	envSecret := map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": cliAdminToken, "HOOKINLET_VERIFIER_SECRET": "remote-secret"}
	if code, _, e := runCmd(t, envSecret, "inbox", "set-secret", "remote-one"); code != 0 || strings.Contains(e, "remote-secret") {
		t.Errorf("set-secret: %d %q", code, e)
	}
	got, _ := s.GetInboxBySlug(t.Context(), "remote-one")
	if got.Name != "Renamed" || got.RetentionDays != 7 || len(got.VerifierSecret) == 0 {
		t.Errorf("after update and set-secret: %+v", got)
	}

	// A second token works beside the first; revoking the first leaves it.
	code, out, _ = runCmd(t, env, "token", "create", "--name", "next", "--inbox", "remote-one", "--json")
	var created admin.TokenCreated
	if code != 0 || json.Unmarshal([]byte(out), &created) != nil || created.Token == "" || created.Token == agentTok {
		t.Fatalf("token create --json: %d %s", code, out)
	}
	if code, out, _ := runCmd(t, env, "token", "list", "--json"); code != 0 || !strings.Contains(out, `"next"`) || strings.Contains(out, created.Token) {
		t.Errorf("token list --json: %d %s", code, out)
	}
	old, err := s.GetTokenByHash(t.Context(), token.Hash(agentTok))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCmd(t, env, "token", "revoke", strconv.FormatInt(int64(old.ID), 10)); code != 0 {
		t.Fatalf("token revoke: %d %s", code, e)
	}
	if _, err := s.GetTokenByHash(t.Context(), token.Hash(agentTok)); err == nil {
		t.Error("the revoked token still resolves")
	}
	if _, err := s.GetTokenByHash(t.Context(), token.Hash(created.Token)); err != nil {
		t.Errorf("the other token stopped resolving: %v", err)
	}

	if code, out, _ := runCmd(t, env, "retention", "run"); code != 0 || !strings.Contains(out, "deleted 0 messages") {
		t.Errorf("retention run: %d %q", code, out)
	}
	file := filepath.Join(t.TempDir(), "remote.db")
	if code, out, e := runCmd(t, env, "backup", "--out", file); code != 0 {
		t.Errorf("backup: %d %q %q", code, out, e)
	}
	if data, err := os.ReadFile(file); err != nil || !strings.HasPrefix(string(data), "SQLite format 3\x00") { //nolint:gosec // a temp path
		t.Errorf("backup file: %v", err)
	}

	if code, _, e := runCmd(t, env, "inbox", "delete", "remote-one", "--yes"); code != 0 {
		t.Errorf("delete: %d %s", code, e)
	}
	if code, _, e := runCmd(t, env, "inbox", "show", "remote-one"); code != exitRuntime || !strings.Contains(e, "not found") {
		t.Errorf("show deleted: %d %q", code, e)
	}
}

func TestRemoteModeListFollowsEveryPage(t *testing.T) {
	url, _ := liveServer(t)
	env := map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": cliAdminToken}
	// More inboxes than a page holds would be slow to make; the page size is the
	// store's default of 100, so make 105 and check the command sees them all.
	for i := range 105 {
		slug := "page-test-" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i)
		if code, _, e := runCmd(t, env, "inbox", "create", "--slug", slug, "--verifier", "none"); code != 0 {
			t.Fatalf("create %s: %s", slug, e)
		}
	}
	code, out, _ := runCmd(t, env, "inbox", "list", "--json")
	var got struct{ Inboxes []admin.View }
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got.Inboxes) != 105 {
		t.Errorf("list returned %d inboxes (code %d)", len(got.Inboxes), code)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestRemoteModeErrorsAreClearAndNeverShowTheToken(t *testing.T) {
	url, _ := liveServer(t)

	cases := []struct {
		name string
		env  map[string]string
		args []string
		code int
		want string
	}{
		{"no token", map[string]string{"HOOKINLET_URL": url}, []string{"inbox", "list"}, exitUsage, "HOOKINLET_ADMIN_TOKEN"},
		{"wrong token", map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": "wrong-token-value"}, []string{"inbox", "list"}, exitRuntime, "authentication required (HTTP 401)"},
		{"an agent token", map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": "hki_notreal"}, []string{"inbox", "list"}, exitRuntime, "HTTP 401"},
		{"plain http to another machine", map[string]string{"HOOKINLET_URL": "http://hooks.example.com", "HOOKINLET_ADMIN_TOKEN": cliAdminToken}, []string{"inbox", "list"}, exitUsage, "plain HTTP"},
		{"not a url", map[string]string{"HOOKINLET_URL": "hooks.example.com", "HOOKINLET_ADMIN_TOKEN": cliAdminToken}, []string{"inbox", "list"}, exitUsage, "--url must be"},
		{"credentials in the url", map[string]string{"HOOKINLET_URL": "https://admin:pw@hooks.example.com", //nolint:gosec // a fake value, to prove it is refused
			"HOOKINLET_ADMIN_TOKEN": cliAdminToken}, []string{"inbox", "list"}, exitUsage, "--url must be"},
		{"url and data dir", map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": cliAdminToken}, []string{"inbox", "list", "--data-dir", t.TempDir()}, exitUsage, "not both"},
		{"nothing listening", map[string]string{"HOOKINLET_URL": "http://127.0.0.1:1", "HOOKINLET_ADMIN_TOKEN": cliAdminToken}, []string{"inbox", "list"}, exitRuntime, "cannot reach the server"},
		{"invalid input is a 400", map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": cliAdminToken}, []string{"inbox", "create", "--slug", "No", "--verifier", "none"}, exitUsage, "slug"},
	}
	for _, c := range cases {
		code, out, errOut := runCmd(t, c.env, c.args...)
		if code != c.code || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: code %d (want %d), stderr %q (want %q)", c.name, code, c.code, errOut, c.want)
		}
		for _, secret := range []string{cliAdminToken, "wrong-token-value", "hki_notreal", "pw@"} {
			if strings.Contains(out+errOut, secret) {
				t.Errorf("%s: the output shows %q", c.name, secret)
			}
		}
	}
	// A duplicate names the slug.
	env := map[string]string{"HOOKINLET_URL": url, "HOOKINLET_ADMIN_TOKEN": cliAdminToken}
	runCmd(t, env, "inbox", "create", "--slug", "twice-inbox", "--verifier", "none")
	if code, _, e := runCmd(t, env, "inbox", "create", "--slug", "twice-inbox", "--verifier", "none"); code != exitRuntime || !strings.Contains(e, `"twice-inbox" already exists`) {
		t.Errorf("duplicate: %d %q", code, e)
	}
	// http to loopback is fine, and the flag allows the rest.
	if _, err := newRemoteAdmin("http://localhost:8080", "t", false); err != nil {
		t.Errorf("loopback over http: %v", err)
	}
	if _, err := newRemoteAdmin("http://10.1.2.3:8080", "t", true); err != nil {
		t.Errorf("--insecure-http: %v", err)
	}
}

func TestACreateFlagFromTheEnvironmentIsNotRequiredForRemote(t *testing.T) {
	// The token and URL may come from flags too.
	url, _ := liveServer(t)
	if code, _, e := runCmd(t, map[string]string{}, "inbox", "list", "--url", url, "--token", cliAdminToken); code != 0 {
		t.Errorf("flags: %d %q", code, e)
	}
}

// Nothing the CLI prints or writes may contain a secret it was given.
func TestTheCLIOutputNeverContainsTheVerifierSecret(t *testing.T) {
	dir := t.TempDir()
	const secret = "plaintext-marker-5551234"
	env := map[string]string{"HOOKINLET_DATA_DIR": dir, "HOOKINLET_MASTER_KEY": masterKey(), "HOOKINLET_VERIFIER_SECRET": secret}
	var all strings.Builder
	for _, args := range [][]string{
		{"inbox", "create", "--slug", "no-leak", "--verifier", "github"},
		{"inbox", "set-secret", "no-leak"},
		{"inbox", "show", "no-leak"}, {"inbox", "show", "no-leak", "--json"}, {"inbox", "list"}, {"inbox", "list", "--json"},
		{"inbox", "update", "no-leak", "--name", "x"}, {"token", "create", "--name", "n", "--inbox", "no-leak"}, {"token", "list"}, {"token", "list", "--json"},
	} {
		_, out, e := runCmd(t, env, args...)
		all.WriteString(out + e)
	}
	if strings.Contains(all.String(), secret) {
		t.Error("a command printed the verifier secret")
	}
	if regexp.MustCompile(`[0-9a-f]{64}`).MatchString(all.String()) {
		t.Error("a command printed something that looks like a token hash")
	}
	if raw := readDB(t, dir); strings.Contains(string(raw), secret) {
		t.Error("the secret is in the database in the clear")
	}
}

// The admin token must go only where --url says. A server that answers with a
// redirect does not get to send it somewhere else.
func TestARedirectIsNotFollowedWithTheAdminToken(t *testing.T) {
	var gotAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect) //nolint:gosec // a test server redirecting to another test server
	}))
	defer redirector.Close()

	code, _, errOut := runCmd(t, map[string]string{"HOOKINLET_URL": redirector.URL, "HOOKINLET_ADMIN_TOKEN": cliAdminToken}, "inbox", "list")
	if code != exitRuntime || !strings.Contains(errOut, "redirected") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
	if v := gotAuth.Load(); v != nil {
		t.Errorf("the redirect target received a request (Authorization %q)", v)
	}
}

func TestDirectModeRefusesAMissingDatabaseExceptForCreate(t *testing.T) {
	typo := filepath.Join(t.TempDir(), "typo")
	env := map[string]string{"HOOKINLET_DATA_DIR": typo}
	for _, args := range [][]string{
		{"inbox", "list"}, {"inbox", "show", "x-inbox"}, {"inbox", "update", "x-inbox", "--name", "n"}, {"inbox", "delete", "x-inbox", "--yes"},
		{"token", "list"}, {"token", "create", "--name", "n", "--all-inboxes"}, {"token", "revoke", "1"}, {"retention", "run"}, {"backup", "--out", filepath.Join(t.TempDir(), "b.db")},
	} {
		code, out, errOut := runCmd(t, env, args...)
		if code != exitRuntime || !strings.Contains(errOut, "there is no database in") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	if _, err := os.Stat(typo); err == nil {
		t.Error("a read-only command created the data directory")
	}
	// Creating is how a database comes to exist.
	if code, _, e := runCmd(t, env, "inbox", "create", "--slug", "first-inbox", "--verifier", "none"); code != 0 {
		t.Fatalf("create: %d %s", code, e)
	}
	if code, _, e := runCmd(t, env, "inbox", "list"); code != 0 {
		t.Errorf("list after create: %d %s", code, e)
	}
}

func TestAnUpdateConflictIsExplainedForWhatItIs(t *testing.T) {
	var buf strings.Builder
	if code := failUpdate(&buf, &apiError{Status: 409, Code: "conflict", Message: "an inbox with that slug already exists, or it was changed at the same time"}); code != exitRuntime ||
		!strings.Contains(buf.String(), "changed by someone else") || strings.Contains(buf.String(), "named") {
		t.Errorf("%d %q", code, buf.String())
	}
	buf.Reset()
	if code := failUpdate(&buf, &apiError{Status: 400, Code: "invalid_parameter", Message: "retention_days must be between 0 and 36500"}); code != exitUsage {
		t.Errorf("a 400 is a usage error: %d", code)
	}
}

func TestBackupRefusesBeforeDownloadingAndNeverClobbers(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir}
	runCmd(t, env, "inbox", "create", "--slug", "one-inbox", "--verifier", "none")
	out := filepath.Join(t.TempDir(), "b.db")
	if err := os.WriteFile(out, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The check comes first: no server is needed to be told.
	code, _, e := runCmd(t, map[string]string{"HOOKINLET_URL": "http://127.0.0.1:1", "HOOKINLET_ADMIN_TOKEN": cliAdminToken}, "backup", "--out", out)
	if code != exitRuntime || !strings.Contains(e, "already exists") || strings.Contains(e, "cannot reach") {
		t.Errorf("code %d, stderr %q", code, e)
	}
	// And a file that appears after that check is still not replaced.
	tmp := filepath.Join(filepath.Dir(out), "new.tmp")
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := placeBackup(tmp, out, false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("placeBackup over an existing file: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "precious" { //nolint:gosec // a temp path
		t.Errorf("the existing file was replaced: %q", b)
	}
	if err := placeBackup(tmp, out, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "new" { //nolint:gosec // a temp path
		t.Errorf("--force did not replace: %q", b)
	}
}

func TestRenewEpochs(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir}
	if code, _, e := runCmd(t, env, "inbox", "create", "--slug", "epoch-one", "--verifier", "none"); code != 0 {
		t.Fatal(e)
	}
	read := func() string {
		s, err := sqlite.Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		in, err := s.GetInboxBySlug(t.Context(), "epoch-one")
		if err != nil {
			t.Fatal(err)
		}
		return in.Epoch
	}
	before := read()
	if code, out, e := runCmd(t, env, "renew-epochs"); code != 0 || !strings.Contains(out, "new epoch") {
		t.Fatalf("renew-epochs: %d %q %q", code, out, e)
	}
	if after := read(); after == "" || after == before {
		t.Errorf("epoch %q after renewing, was %q", after, before)
	}
	if code, _, e := runCmd(t, map[string]string{"HOOKINLET_DATA_DIR": filepath.Join(dir, "typo")}, "renew-epochs"); code != exitRuntime || !strings.Contains(e, "no database") {
		t.Errorf("a mistyped directory: %d %q", code, e)
	}
}
