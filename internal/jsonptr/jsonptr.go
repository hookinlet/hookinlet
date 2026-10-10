// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package jsonptr reads a string out of a JSON document with an RFC 6901 JSON
// pointer. It is used to find the event type in the body of a generic webhook.
package jsonptr

import (
	"bytes"
	"encoding/json"
	"strings"
)

const (
	// maxDepth bounds how deep a pointer may go.
	maxDepth = 32
	// maxValues bounds how many JSON values a lookup will step through, which
	// bounds its CPU time whatever the document looks like. A value the lookup
	// has already found is still returned if the budget runs out afterwards.
	maxValues = 10_000
)

// String returns the string the pointer addresses in doc. It reports false if
// doc is not valid JSON, the pointer is malformed or points at nothing, or the
// value it points at is not a string.
//
// doc is untrusted and may be large. The document is read as a stream and is
// never decoded into generic values, so memory stays proportional to the
// document and a hostile one (millions of tiny values, huge nesting) cannot
// multiply it. If the same key appears twice the last one wins, as in
// encoding/json.
func String(doc []byte, pointer string) (string, bool) {
	tokens, ok := parse(pointer)
	if !ok || len(tokens) > maxDepth {
		return "", false
	}
	w := &walker{dec: json.NewDecoder(bytes.NewReader(doc)), budget: maxValues}
	s, found := w.lookup(tokens)
	if w.invalid {
		return "", false
	}
	return s, found
}

type walker struct {
	dec       *json.Decoder
	budget    int
	done      bool // stop reading: the document is invalid or the budget is spent
	invalid   bool
	exhausted bool
}

// spend uses one unit of the budget and reports whether work may continue.
func (w *walker) spend() bool {
	if w.done {
		return false
	}
	if w.budget--; w.budget < 0 {
		w.done, w.exhausted = true, true
		return false
	}
	return true
}

func (w *walker) fail() {
	w.done, w.invalid = true, true
}

// discard accepts any JSON value without keeping it.
type discard struct{}

func (*discard) UnmarshalJSON([]byte) error { return nil }

// target captures a value only if it is a string.
type target struct {
	s        string
	isString bool
}

func (t *target) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		t.isString = json.Unmarshal(b, &t.s) == nil
	}
	return nil
}

// skip consumes one value.
func (w *walker) skip() {
	if !w.spend() {
		return
	}
	var d discard
	if w.dec.Decode(&d) != nil {
		w.fail()
	}
}

// lookup consumes exactly one value from the stream and returns the string at
// the remaining path inside it, if any.
func (w *walker) lookup(path []string) (string, bool) {
	if !w.spend() {
		return "", false
	}
	if len(path) == 0 {
		var t target
		if w.dec.Decode(&t) != nil {
			w.fail()
			return "", false
		}
		return t.s, t.isString
	}

	tok, err := w.dec.Token()
	if err != nil {
		w.fail()
		return "", false
	}
	delim, isContainer := tok.(json.Delim)
	if !isContainer {
		return "", false // a scalar has nothing inside it
	}

	var (
		res   string
		found bool
	)
	switch delim {
	case '{':
		for w.dec.More() && !w.done {
			kt, err := w.dec.Token()
			if err != nil {
				w.fail()
				break
			}
			if key, _ := kt.(string); key == path[0] {
				res, found = w.lookup(path[1:]) // a later duplicate replaces an earlier one
			} else {
				w.skip()
			}
		}
	case '[':
		want, indexed := arrayIndex(path[0])
		for i := 0; w.dec.More() && !w.done; i++ {
			if indexed && i == want {
				res, found = w.lookup(path[1:]) //nolint:gosec // len(path) > 0 was established at the top of lookup
			} else {
				w.skip()
			}
		}
	}
	if w.done {
		return res, found // stopped early; the caller sees invalid or exhausted
	}
	if _, err := w.dec.Token(); err != nil { // the closing delimiter
		w.fail()
		return "", false
	}
	return res, found
}

// parse splits a pointer into unescaped reference tokens. The empty pointer
// (the whole document) yields no tokens.
func parse(p string) ([]string, bool) {
	if p == "" {
		return nil, true
	}
	if p[0] != '/' {
		return nil, false
	}
	var tokens []string
	for _, raw := range strings.Split(p[1:], "/") {
		tok, ok := unescape(raw)
		if !ok {
			return nil, false
		}
		tokens = append(tokens, tok)
	}
	return tokens, true
}

// unescape applies RFC 6901's "~1" -> "/" then "~0" -> "~". Any other use of
// "~" is malformed.
func unescape(s string) (string, bool) {
	if !strings.Contains(s, "~") {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '~' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			return "", false
		}
		i++
		switch s[i] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", false
		}
	}
	return b.String(), true
}

// arrayIndex parses an array index token: decimal digits with no leading zeros
// (except "0" itself). "-" (the position past the end) never addresses an
// existing element.
func arrayIndex(tok string) (int, bool) {
	if tok == "" || len(tok) > 9 || (len(tok) > 1 && tok[0] == '0') {
		return 0, false
	}
	n := 0
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, false
		}
		n = n*10 + int(tok[i]-'0')
	}
	return n, true
}
