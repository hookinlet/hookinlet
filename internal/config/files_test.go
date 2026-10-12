// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileOf(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecretsCanComeFromFiles(t *testing.T) {
	env := map[string]string{
		"HOOKINLET_ADMIN_TOKEN_FILE": fileOf(t, "from-a-file\n"),
		"HOOKINLET_MASTER_KEY_FILE":  fileOf(t, "key-without-newline"),
		"HOOKINLET_LISTEN_ADDR":      ":1",
	}
	getenv, err := WithSecretFiles(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got := getenv("HOOKINLET_ADMIN_TOKEN"); got != "from-a-file" {
		t.Errorf("the token = %q, want the file's content without its newline", got)
	}
	if got := getenv("HOOKINLET_MASTER_KEY"); got != "key-without-newline" {
		t.Errorf("the key = %q", got)
	}
	if got := getenv("HOOKINLET_LISTEN_ADDR"); got != ":1" {
		t.Errorf("an ordinary variable changed: %q", got)
	}
	if got := getenv("HOOKINLET_OLD_MASTER_KEY"); got != "" {
		t.Errorf("a variable nobody set: %q", got)
	}
	// Only one trailing newline goes, and a CRLF counts as one.
	crlf, _ := WithSecretFiles(func(k string) string {
		if k == "HOOKINLET_VERIFIER_SECRET_FILE" {
			return fileOf(t, "a\r\n\n")
		}
		return ""
	})
	if got := crlf("HOOKINLET_VERIFIER_SECRET"); got != "a\r\n" {
		t.Errorf("trailing newlines: %q", got)
	}
}

func TestSecretFilesFailLoudlyAndNeverEchoTheSecret(t *testing.T) {
	both := map[string]string{"HOOKINLET_ADMIN_TOKEN": "x", "HOOKINLET_ADMIN_TOKEN_FILE": fileOf(t, "y")}
	for name, env := range map[string]map[string]string{
		"both given":   both,
		"no such file": {"HOOKINLET_MASTER_KEY_FILE": filepath.Join(t.TempDir(), "absent")},
		"empty file":   {"HOOKINLET_MASTER_KEY_FILE": fileOf(t, "\n")},
		"too large":    {"HOOKINLET_MASTER_KEY_FILE": fileOf(t, strings.Repeat("a-secret-marker", 8000))},
		"a directory":  {"HOOKINLET_MASTER_KEY_FILE": t.TempDir()},
	} {
		_, err := WithSecretFiles(func(k string) string { return env[k] })
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "a-secret-marker") {
			t.Errorf("%s: the error echoes the file: %v", name, err)
		}
	}
}
