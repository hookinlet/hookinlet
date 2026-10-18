// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/textproto"
	"time"
)

// ---- none ----

type none struct{}

func newNone(config json.RawMessage) (Verifier, error) {
	var c struct{}
	if err := decodeConfig(config, &c); err != nil {
		return nil, err
	}
	return none{}, nil
}

// Verify always succeeds and says that it checked nothing. Use it only for
// inboxes that deliberately accept anything: what it accepts is stored, but never
// as verified.
func (none) Verify(*http.Request, []byte, []byte, time.Time) (Result, error) {
	return Result{Unauthenticated: true}, nil
}

// ---- token ----

const defaultTokenHeader = "X-Inbox-Token" //nolint:gosec // the name of a header, not a credential

// token checks that a request header equals the inbox's shared secret.
type token struct{ header string }

func newToken(config json.RawMessage) (Verifier, error) {
	var c struct {
		Header string `json:"header"`
	}
	if err := decodeConfig(config, &c); err != nil {
		return nil, err
	}
	if c.Header == "" {
		c.Header = defaultTokenHeader
	}
	if !validHeaderName(c.Header) {
		return nil, configErr("header %q is not a valid HTTP header name", c.Header)
	}
	return token{header: textproto.CanonicalMIMEHeaderKey(c.Header)}, nil
}

func (t token) Verify(r *http.Request, _ []byte, secret []byte, _ time.Time) (Result, error) {
	if err := requireSecret(secret); err != nil {
		return Result{}, err
	}
	got, err := singleHeader(r, t.header)
	if err != nil {
		return Result{}, err
	}
	// Comparing hashes keeps the comparison constant-time and also hides the
	// length of the secret, which ConstantTimeCompare would otherwise leak.
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256(secret)
	if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		return Result{}, fail(FailureMismatch, "token mismatch")
	}
	return Result{}, nil
}

// SecretHeaders implements HeaderRedactor: the token header carries the secret
// itself and must never be stored.
func (t token) SecretHeaders() []string { return []string{t.header} }

// validHeaderName reports whether s is a valid HTTP header field name (an RFC
// 7230 token).
func validHeaderName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '!', c == '#', c == '$', c == '%', c == '&', c == '\'', c == '*', c == '+',
			c == '-', c == '.', c == '^', c == '_', c == '`', c == '|', c == '~':
		default:
			return false
		}
	}
	return true
}
