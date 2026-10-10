// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package token

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	tok, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, Prefix) {
		t.Errorf("missing prefix: %q", tok)
	}
	// 4-char prefix + 43 base64url characters for 32 bytes.
	if len(tok) != len(Prefix)+43 {
		t.Errorf("unexpected token length %d", len(tok))
	}
	if len(hash) != 32 || !bytes.Equal(hash, Hash(tok)) {
		t.Errorf("hash does not match token")
	}
	if bytes.Contains(hash, []byte(tok)) {
		t.Error("hash contains the token")
	}
}

func TestGenerateIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		tok, _, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestEqual(t *testing.T) {
	tok, hash, _ := Generate()
	other, _, _ := Generate()
	cases := []struct {
		name  string
		token string
		hash  []byte
		want  bool
	}{
		{"correct", tok, hash, true},
		{"wrong token", other, hash, false},
		{"empty token", "", hash, false},
		{"tampered token", tok + "x", hash, false},
		{"nil hash", tok, nil, false},
		{"short hash", tok, hash[:16], false},
		{"empty token and nil hash", "", nil, false},
	}
	for _, c := range cases {
		if got := Equal(c.token, c.hash); got != c.want {
			t.Errorf("%s: Equal = %v, want %v", c.name, got, c.want)
		}
	}
}
