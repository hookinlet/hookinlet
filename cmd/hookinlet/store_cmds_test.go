// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

var tokenRE = regexp.MustCompile(`agent token:\s+(hki_\S+)`)

func writeFile(path string) error { return os.WriteFile(path, []byte("x"), 0o600) }

// readDB returns the raw bytes of the database file in dir.
func readDB(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, sqlite.DBFileName)) //nolint:gosec // a path inside a temp directory this test created
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func masterKey() string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) }

func TestMigrate(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir}

	files, err := os.ReadDir("../../pkg/store/sqlite/migrations")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for i := range files {
		all = append(all, strconv.Itoa(i+1))
	}
	applied := "[" + strings.Join(all, " ") + "]"
	code, out, errOut := runCmd(t, env, "migrate")
	if code != 0 || !strings.Contains(out, "applied migrations "+applied) {
		t.Fatalf("first migrate: code %d, out %q, err %q", code, out, errOut)
	}
	code, out, _ = runCmd(t, env, "migrate")
	if code != 0 || !strings.Contains(out, "up to date at version "+strconv.Itoa(len(files))) {
		t.Errorf("second migrate: code %d, out %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, sqlite.DBFileName)); err != nil {
		t.Errorf("database not created: %v", err)
	}

	// The flag wins over the environment.
	other := t.TempDir()
	if code, _, _ := runCmd(t, env, "migrate", "--data-dir", other); code != 0 {
		t.Fatal("migrate with --data-dir failed")
	}
	if _, err := os.Stat(filepath.Join(other, sqlite.DBFileName)); err != nil {
		t.Errorf("--data-dir was ignored: %v", err)
	}
}

func TestMigrateErrors(t *testing.T) {
	if code, _, errOut := runCmd(t, map[string]string{"HOOKINLET_DATA_DIR": t.TempDir(), "HOOKINLET_MASTER_KEY": "not base64!"}, "migrate"); code != exitUsage || !strings.Contains(errOut, "--master-key") {
		t.Errorf("bad master key: code %d, stderr %q", code, errOut)
	}
	if code, _, _ := runCmd(t, nil, "migrate", "extra"); code != exitUsage {
		t.Errorf("stray argument: code %d", code)
	}
	if code, _, _ := runCmd(t, nil, "migrate", "--nope"); code != exitUsage {
		t.Errorf("bad flag: code %d", code)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCmd(t, map[string]string{"HOOKINLET_DATA_DIR": filepath.Join(file, "sub")}, "migrate"); code != exitRuntime || errOut == "" {
		t.Errorf("unusable data dir: code %d, stderr %q", code, errOut)
	}
}

func TestInboxCreate(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir}

	code, out, errOut := runCmd(t, env, "inbox", "create", "--slug", "stripe-prod", "--ingest-key", "stripe-prod", "--name", "Stripe (prod)",
		"--verifier", "stripe", "--on-verify-fail", "store", "--retention-days", "30", "--retention-max-count", "5000",
		"--max-body-bytes", "65536", "--event-type-pointer", "/type", "--verifier-config", `{"tolerance_seconds":300}`)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	m := tokenRE.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "/in/stripe-prod") || !strings.Contains(out, "shown only once") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	agentToken := m[1]

	// The token is not on disk; only its hash is, and the hash matches.
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	in, err := s.GetInboxBySlug(t.Context(), "stripe-prod")
	if err != nil {
		t.Fatal(err)
	}
	if in.Name != "Stripe (prod)" || in.Verifier != "stripe" || in.OnVerifyFail != "store" || in.RetentionDays != 30 ||
		in.RetentionMaxCount != 5000 || in.MaxBodyBytes != 65536 || in.EventTypePointer != "/type" {
		t.Errorf("inbox fields not applied: %#v", *in)
	}
	if rec, err := s.GetTokenByHash(t.Context(), token.Hash(agentToken)); err != nil || rec.AllInboxes || len(rec.InboxIDs) != 1 || rec.InboxIDs[0] != in.ID {
		t.Errorf("the printed token does not match a stored hash for this inbox: %+v, %v", rec, err)
	}
	raw := readDB(t, dir)
	if bytes.Contains(raw, []byte(agentToken)) {
		t.Error("the agent token itself was written to the database")
	}

	// Defaults: name falls back to the slug, and tokens differ per inbox.
	code, out2, _ := runCmd(t, env, "inbox", "create", "--slug", "second-inbox", "--verifier", "none")
	if code != 0 {
		t.Fatalf("second create: code %d", code)
	}
	if m2 := tokenRE.FindStringSubmatch(out2); m2 == nil || m2[1] == agentToken {
		t.Errorf("second inbox token: %v", m2)
	}
	second, err := s.GetInboxBySlug(t.Context(), "second-inbox")
	if err != nil || second.Name != "second-inbox" || second.Verifier != "none" {
		t.Errorf("defaults: %+v, %v", second, err)
	}
}

func TestInboxCreateSealsTheVerifierSecret(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{ //nolint:gosec // fake sentinel values that exist to prove secrets never leak
		"HOOKINLET_DATA_DIR":        dir,
		"HOOKINLET_MASTER_KEY":      masterKey(),
		"HOOKINLET_VERIFIER_SECRET": "whsec_plaintext_marker",
	}
	if code, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "with-secret", "--verifier", "github"); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}

	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.GetInboxBySlug(t.Context(), "with-secret")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(in.VerifierSecret, []byte("whsec_plaintext_marker")) {
		t.Error("the verifier secret is stored in plaintext")
	}
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	if pt, err := box.Open(in.VerifierSecret, inbox.VerifierSecretContext(in.Slug)); err != nil || string(pt) != "whsec_plaintext_marker" {
		t.Errorf("sealed secret does not open with the master key: %q, %v", pt, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := readDB(t, dir)
	if bytes.Contains(raw, []byte("whsec_plaintext_marker")) {
		t.Error("the verifier secret appears in the database file")
	}
}

func TestInboxCreateErrors(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir}
	if code, _, _ := runCmd(t, env, "inbox", "create", "--slug", "taken-slug", "--verifier", "none"); code != 0 {
		t.Fatal("setup failed")
	}

	tests := []struct {
		name     string
		env      map[string]string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"missing slug", env, []string{"inbox", "create"}, exitUsage, "--slug is required"},
		{"invalid slug", env, []string{"inbox", "create", "--slug", "Bad Slug", "--verifier", "none"}, exitUsage, "slug must be"},
		{"duplicate slug", env, []string{"inbox", "create", "--slug", "taken-slug", "--verifier", "none"}, exitRuntime, `"taken-slug" already exists`},
		{"unknown verifier", env, []string{"inbox", "create", "--slug", "abc", "--verifier", "pigeon"}, exitUsage, "unknown verifier"},
		{"bad verifier config", env, []string{"inbox", "create", "--slug", "abc", "--verifier", "none", "--verifier-config", "{nope"}, exitUsage, "not valid JSON"},
		{"bad on-verify-fail", env, []string{"inbox", "create", "--slug", "abc", "--verifier", "none", "--on-verify-fail", "maybe"}, exitUsage, "on_verify_fail"},
		{"negative retention", env, []string{"inbox", "create", "--slug", "abc", "--verifier", "none", "--retention-days", "-1"}, exitUsage, "retention_days"},
		{"secret without master key", map[string]string{"HOOKINLET_DATA_DIR": dir, "HOOKINLET_VERIFIER_SECRET": "s"},
			[]string{"inbox", "create", "--slug", "abc", "--verifier", "none"}, exitUsage, "master-key"},
		{"unknown flag", env, []string{"inbox", "create", "--slug", "abc", "--verifier", "none", "--bogus"}, exitUsage, "bogus"},
		{"stray argument", env, []string{"inbox", "create", "--slug", "abc", "--verifier", "none", "extra"}, exitUsage, "unexpected argument"},
		{"unknown inbox command", env, []string{"inbox", "frobnicate"}, exitUsage, "unknown inbox command"},
		{"no inbox command", env, []string{"inbox"}, exitUsage, "Usage"},
	}
	for _, tt := range tests {
		code, out, errOut := runCmd(t, tt.env, tt.args...)
		if code != tt.wantCode || !strings.Contains(errOut, tt.wantErr) {
			t.Errorf("%s: code %d (want %d), stderr %q (want substring %q)", tt.name, code, tt.wantCode, errOut, tt.wantErr)
		}
		if tokenRE.MatchString(out) {
			t.Errorf("%s: a token was printed for a failed create", tt.name)
		}
	}

	// A failed create leaves no inbox behind.
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.GetInboxBySlug(t.Context(), "abc"); err == nil {
		t.Error("a failed create left an inbox behind")
	}
}

func TestStoreCommandsDoNotLeakSecretsInErrors(t *testing.T) {
	env := map[string]string{ //nolint:gosec // fake sentinel values that exist to prove secrets never leak
		"HOOKINLET_DATA_DIR":        t.TempDir(),
		"HOOKINLET_MASTER_KEY":      "definitely-not-valid-base64-sentinel",
		"HOOKINLET_VERIFIER_SECRET": "verifier-secret-sentinel",
	}
	_, _, errOut := runCmd(t, env, "inbox", "create", "--slug", "leakcheck", "--verifier", "none")
	for _, s := range []string{"definitely-not-valid-base64-sentinel", "verifier-secret-sentinel"} {
		if strings.Contains(errOut, s) {
			t.Errorf("stderr leaks %q: %s", s, errOut)
		}
	}
}
