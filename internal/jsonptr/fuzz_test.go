// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package jsonptr

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// The fuzz target here runs its seeds in a normal `go test`. To fuzz it:
//
//	go test -run='^$' -fuzz=FuzzStringAgainstReference -fuzztime=90s ./internal/jsonptr

// reference resolves pointer in doc the slow, obvious way: decode everything,
// then walk. It returns ok=false for an invalid document or a pointer that
// does not lead to a string.
func reference(doc []byte, pointer string) (string, bool) {
	// Only the first value is read; what follows it is ignored (see String).
	var v any
	if json.NewDecoder(bytes.NewReader(doc)).Decode(&v) != nil {
		return "", false
	}
	if pointer != "" {
		if pointer[0] != '/' {
			return "", false
		}
		for _, raw := range strings.Split(pointer[1:], "/") {
			var tok strings.Builder
			for i := 0; i < len(raw); i++ {
				if raw[i] != '~' {
					tok.WriteByte(raw[i])
					continue
				}
				if i+1 >= len(raw) || (raw[i+1] != '0' && raw[i+1] != '1') {
					return "", false
				}
				if raw[i+1] == '0' {
					tok.WriteByte('~')
				} else {
					tok.WriteByte('/')
				}
				i++
			}
			switch c := v.(type) {
			case map[string]any:
				next, ok := c[tok.String()]
				if !ok {
					return "", false
				}
				v = next
			case []any:
				s := tok.String()
				n, err := strconv.Atoi(s)
				if err != nil || strconv.Itoa(n) != s || n < 0 || n >= len(c) {
					return "", false
				}
				v = c[n]
			default:
				return "", false
			}
		}
	}
	s, ok := v.(string)
	return s, ok
}

// FuzzStringAgainstReference compares String with a plain decode-and-walk on
// documents small enough that String's work budget cannot run out.
func FuzzStringAgainstReference(f *testing.F) {
	f.Add([]byte(`{"type":"a.created"}`), "/type")
	f.Add([]byte(`{"a":[{"b":"x"},{"b":"y"}]}`), "/a/1/b")
	f.Add([]byte(`{"a/b":{"~":"t"}}`), "/a~1b/~0")
	f.Add([]byte(`{"a":"x","a":"y"}`), "/a")
	f.Add([]byte(`{"a":"x","a":{"b":1}}`), "/a")
	f.Add([]byte(`"top"`), "")
	f.Add([]byte(`{"type":"x"} trailing`), "/type")
	f.Add([]byte(`{"type":"x"}{"type":"y"}`), "/type")
	f.Add([]byte(`["a","b"]`), "/01")
	f.Add([]byte(`{"a":"\ud800"}`), "/a")
	f.Add([]byte("{\"a\":\"\xff\"}"), "/a")
	f.Add([]byte(strings.Repeat("[", 5000)), "/0/0")
	f.Fuzz(func(t *testing.T, doc []byte, pointer string) {
		got, ok := String(doc, pointer)
		if !ok {
			if got != "" {
				t.Fatalf("a failed lookup returned %q", got)
			}
		}
		if len(doc) > 4000 {
			return // String may legitimately stop at its work budget
		}
		want, wantOK := reference(doc, pointer)
		if ok != wantOK || got != want {
			t.Fatalf("String = (%q, %v), reference = (%q, %v)", got, ok, want, wantOK)
		}
	})
}
