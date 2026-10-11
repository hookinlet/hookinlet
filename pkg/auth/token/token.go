// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package token generates and checks bearer tokens. Tokens are random, shown
// once, and stored only as a SHA-256 hash.
//
// Stability: this package is pre-1.0. Its API can still change, and every change is
// listed in CHANGELOG.md; after v1.0.0, semantic versioning applies.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// Prefix marks tokens so that secret scanners can recognize them.
const Prefix = "hki_"

// randomBytes is the entropy in a generated token: 256 bits.
const randomBytes = 32

// Generate returns a new random token and its hash. Show the token to its
// owner once and store only the hash.
func Generate() (token string, hash []byte, err error) {
	var b [randomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, fmt.Errorf("token: %w", err)
	}
	token = Prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return token, Hash(token), nil
}

// Hash returns the SHA-256 hash of token. Because tokens carry 256 bits of
// entropy, a fast unsalted hash is sufficient.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal reports whether token hashes to want, in constant time.
func Equal(token string, want []byte) bool {
	return subtle.ConstantTimeCompare(Hash(token), want) == 1
}
