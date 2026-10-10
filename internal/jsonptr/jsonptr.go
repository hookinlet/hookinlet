// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package jsonptr reads a string out of a JSON document with an RFC 6901 JSON
// pointer. It is used to find the event type in the body of a generic webhook.
package jsonptr

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// maxDepth bounds how deep a pointer may go, so a hostile pointer or document
// cannot cause unbounded work.
const maxDepth = 32

// String returns the string the pointer addresses in doc. It reports false if
// doc is not valid JSON, the pointer is malformed or points at nothing, or the
// value it points at is not a string.
//
// doc is untrusted. Nothing here executes or interprets its content.
func String(doc []byte, pointer string) (string, bool) {
	tokens, ok := parse(pointer)
	if !ok || len(tokens) > maxDepth {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	var v any
	if dec.Decode(&v) != nil {
		return "", false
	}
	for _, tok := range tokens {
		switch node := v.(type) {
		case map[string]any:
			next, found := node[tok]
			if !found {
				return "", false
			}
			v = next
		case []any:
			i, ok := arrayIndex(tok, len(node))
			if !ok {
				return "", false
			}
			v = node[i]
		default:
			return "", false
		}
	}
	s, isString := v.(string)
	return s, isString
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

// arrayIndex parses an array index token: decimal digits, no leading zeros
// (except "0" itself), and in range. "-" (the position past the end) never
// addresses an existing element.
func arrayIndex(tok string, length int) (int, bool) {
	if tok == "" || (len(tok) > 1 && tok[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n >= length {
		return 0, false
	}
	return n, true
}
