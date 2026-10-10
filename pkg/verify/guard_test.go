// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every comparison involving a secret, token, or signature must be
// constant-time (guardrail 22). The packages that do such comparisons route
// them through hmac.Equal or crypto/subtle; this test fails if a source file
// reaches for an early-exit comparison instead. It is a tripwire for review,
// not a proof: the real guarantee is that the code paths use hmac.Equal.
func TestNoEarlyExitComparisonsOnSecrets(t *testing.T) {
	forbidden := []string{
		"bytes.Equal(", "bytes.Compare(", "bytes.HasPrefix(", "bytes.Contains(",
		"strings.Compare(", "strings.EqualFold(", "reflect.DeepEqual(",
	}
	for _, dir := range []string{".", filepath.Join("..", "auth", "token")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f) //nolint:gosec // the package's own source files
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(src), "\n") {
				code, _, _ := strings.Cut(line, "//")
				for _, bad := range forbidden {
					if strings.Contains(code, bad) {
						t.Errorf("%s:%d uses %s; compare secrets with hmac.Equal or subtle.ConstantTimeCompare", f, i+1, bad)
					}
				}
			}
		}
	}
}

func TestComparisonsUseConstantTimePrimitives(t *testing.T) {
	// The primitives must actually be in use where signatures are compared.
	src, err := os.ReadFile("helpers.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "hmac.Equal(") {
		t.Error("anyEqual no longer uses hmac.Equal")
	}
	src, err = os.ReadFile("simple.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare(") {
		t.Error("the token verifier no longer uses subtle.ConstantTimeCompare")
	}
}

// ---- fuzzing: header and template parsers must never panic, and whatever they
// accept must satisfy the invariants the verifiers rely on ----

func FuzzParseStripeHeader(f *testing.F) {
	for _, seed := range []string{
		"", "t=1700000000,v1=" + strings.Repeat("a", 64), "t=,v1=", "v1=", "t=1,t=2,v1=00",
		strings.Repeat(",", 100), "t=99999999999999999999,v1=zz", "\x00=\xff", "t=1700000000, v1=" + strings.Repeat("0", 64),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		ts, sigs, err := parseStripeHeader(header)
		if err != nil {
			return
		}
		if _, ok := parseUnixSeconds(ts); !ok {
			t.Fatalf("accepted an invalid timestamp %q", ts)
		}
		if len(sigs) == 0 {
			t.Fatal("accepted a header without any usable signature")
		}
		for _, s := range sigs {
			if len(s) != 32 {
				t.Fatalf("signature of %d bytes accepted", len(s))
			}
		}
	})
}

func FuzzParseStandardWebhooksSignatures(f *testing.F) {
	for _, seed := range []string{
		"", "v1,", "v1,AAAA", "v1a,AAAA v1,", " ", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=",
		strings.Repeat("v1,x ", 200), "\x00,\xff",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		for _, s := range parseStandardWebhooksSignatures(header) {
			if len(s) != 32 {
				t.Fatalf("signature of %d bytes accepted", len(s))
			}
		}
	})
}

func FuzzParseTemplate(f *testing.F) {
	for _, seed := range []string{
		"{body}", "{timestamp}.{body}", "", "{", "}", "{body}{body}", "v0:{timestamp}:{body}", "{Body}", "{{body}}", "a{body}b{timestamp}c",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, tmpl string) {
		segs, err := parseTemplate(tmpl)
		if err != nil {
			return
		}
		bodies := 0
		var rebuilt strings.Builder
		for _, s := range segs {
			switch s.kind {
			case segBody:
				bodies++
				rebuilt.WriteString("{body}")
			case segTimestamp:
				rebuilt.WriteString("{timestamp}")
			default:
				if strings.ContainsAny(s.lit, "{}") {
					t.Fatalf("a literal segment contains a brace: %q", s.lit)
				}
				rebuilt.WriteString(s.lit)
			}
		}
		if bodies != 1 {
			t.Fatalf("accepted a template with %d bodies", bodies)
		}
		if rebuilt.String() != tmpl {
			t.Fatalf("template does not round-trip: %q -> %q", tmpl, rebuilt.String())
		}
	})
}

func FuzzParseUnixSeconds(f *testing.F) {
	for _, seed := range []string{"", "0", "1700000000", "-1", "+1", "1.5", " 1", "99999999999999", "٣"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tm, ok := parseUnixSeconds(s)
		if !ok {
			return
		}
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				t.Fatalf("accepted a non-digit in %q", s)
			}
		}
		if tm.Year() < 1970 || tm.Year() > 9999 {
			t.Fatalf("year %d out of range for %q", tm.Year(), s)
		}
	})
}
