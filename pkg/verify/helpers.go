// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"hash"
	"net/http"
	"time"
)

// singleHeader returns the only value of the named request header. A missing
// header or several values are failures: with several, a proxy and the origin
// could disagree about which one counts.
func singleHeader(r *http.Request, name string) (string, error) {
	switch vs := r.Header.Values(name); len(vs) {
	case 0:
		return "", fail(FailureMissing, "missing "+name+" header")
	case 1:
		return vs[0], nil
	default:
		return "", fail(FailureMalformed, "multiple "+name+" headers")
	}
}

// requireSecret returns a configuration error if no secret is set. A verifier
// that needs one must never run without it.
func requireSecret(secret []byte) error {
	if len(secret) == 0 {
		return configErr("this verifier needs a secret and the inbox has none")
	}
	return nil
}

// parseUnixSeconds parses a Unix timestamp written as plain decimal digits in
// canonical form. Signs, spaces, fractions, leading zeros, and anything past the
// year 9999 are rejected. Leading zeros matter: if "0123" and "123" were the
// same time, characters could slide between a timestamp and the adjacent part
// of a signed message without changing the signed bytes or the parsed time.
func parseUnixSeconds(s string) (time.Time, bool) {
	const maxSeconds = 253402300799 // 9999-12-31T23:59:59Z, 12 digits at most
	if s == "" || len(s) > 12 || (len(s) > 1 && s[0] == '0') {
		return time.Time{}, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return time.Time{}, false
		}
		n = n*10 + int64(s[i]-'0')
	}
	if n > maxSeconds {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}

// mac returns HMAC(newHash, key) over the concatenation of parts.
func mac(newHash func() hash.Hash, key []byte, parts ...[]byte) []byte {
	h := hmac.New(newHash, key)
	for _, p := range parts {
		h.Write(p) // hash.Hash.Write never returns an error
	}
	return h.Sum(nil)
}

// anyEqual reports whether want equals any candidate, comparing every one in
// constant time.
func anyEqual(want []byte, candidates [][]byte) bool {
	matched := false
	for _, c := range candidates {
		if hmac.Equal(c, want) {
			matched = true
		}
	}
	return matched
}

// topLevelStrings returns the string values of the named top-level keys of a
// JSON object body. Missing keys, non-string values, and bodies that are not a
// JSON object are simply absent from the result. The body is untrusted until
// the caller has verified its signature.
func topLevelStrings(body []byte, keys ...string) map[string]string {
	out := make(map[string]string, len(keys))
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil {
		return out
	}
	for _, k := range keys {
		var s string
		if raw, ok := obj[k]; ok && json.Unmarshal(raw, &s) == nil {
			out[k] = s
		}
	}
	return out
}

// decodeBase64 decodes base64 in its one canonical form. Padding is optional
// but, if present, must be exactly right, and unused trailing bits must be
// zero. Without this, one signature could be written many ways ("AB==",
// "AB=", "AC==" all decode to the same byte in a lenient decoder).
func decodeBase64(enc *base64.Encoding, s string) ([]byte, error) {
	if len(s)%4 == 0 {
		return enc.Strict().DecodeString(s)
	}
	return enc.WithPadding(base64.NoPadding).Strict().DecodeString(s)
}
