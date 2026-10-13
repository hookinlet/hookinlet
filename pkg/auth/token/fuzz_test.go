// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package token

import (
	"net/http"
	"strings"
	"testing"
)

// The fuzz target here runs its seeds in a normal `go test`. To fuzz it:
//
//	go test -run='^$' -fuzz=FuzzAuthenticate -fuzztime=90s ./pkg/auth/token

// FuzzAuthenticate: with any Authorization headers, the admin is recognized
// only for exactly one "Bearer <admin token>" header, and nothing else is let
// in without a lookup (the store is nil, so a lookup would panic).
func FuzzAuthenticate(f *testing.F) {
	const admin = "admin-token-0123456789-abcdefghijklmnop"
	f.Add("Bearer "+admin, "")
	f.Add("bearer "+admin, "x")
	f.Add("BEARER "+admin+" ", "")
	f.Add("Bearer  "+admin, "")
	f.Add("Bearer "+admin, "Bearer "+admin)
	f.Add("Bearer\t"+admin, "")
	f.Add("Basic "+admin, "")
	f.Add("Bearer "+strings.Repeat("a", 600), "")
	f.Add("Bearer "+admin+"\x00", "")
	f.Add("BİEARER "+admin, "")
	f.Add("Bearer K"+admin, "")
	f.Fuzz(func(t *testing.T, h1, h2 string) {
		a := NewAuthenticator(nil, admin)
		r := &http.Request{Header: http.Header{}}
		var values []string
		for _, h := range []string{h1, h2} {
			if h != "" {
				r.Header.Add("Authorization", h)
				values = append(values, h)
			}
		}
		if strings.Contains(strings.Join(values, "\n"), Prefix) {
			return // might reach the store; the stub below covers tokens that exist
		}
		p, err := a.Authenticate(r)
		if err != nil {
			return
		}
		if _, isAdmin := p.(Admin); !isAdmin || len(values) != 1 {
			t.Fatalf("authenticated as %T with headers %q", p, values)
		}
		scheme, tok, ok := strings.Cut(values[0], " ")
		if !ok || !strings.EqualFold(scheme, "bearer") || tok != admin {
			t.Fatalf("admin let in by %q", values[0])
		}
	})
}
