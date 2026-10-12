// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package secretbox is the default inbox.SecretBox: AES-256-GCM with a random
// nonce per message.
//
// Stability: this package is pre-1.0. Its API can still change, and every change is
// listed in CHANGELOG.md; after v1.0.0, semantic versioning applies.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeySize is the required key length in bytes.
const KeySize = 32

const (
	// formatV1 is the original layout: version || nonce || ciphertext+tag, with only
	// the version authenticated. Boxes still open it, so a database written by an
	// earlier release keeps working, and Legacy reports it so that it can be sealed
	// again in the current layout.
	formatV1 byte = 1
	// formatV2 is version || key id || nonce || ciphertext+tag. The version, the key
	// id and the caller's context are authenticated. The key id says which key
	// sealed the value, so a wrong key is recognized as that and not as damage; the
	// context binds the value to what it belongs to (an inbox), so that a sealed
	// value copied to another row does not open there.
	formatV2 byte = 2

	keyIDSize = 4
)

// ErrOpen is returned by Open for any failure. It deliberately does not say
// whether the ciphertext was malformed or tampered with.
var ErrOpen = errors.New("secretbox: cannot open ciphertext")

// ErrWrongKey is returned by Open, in place of a plain ErrOpen, for a value that
// was sealed under a different key. It matches ErrOpen too. Only values in the
// current layout can say so.
var ErrWrongKey = fmt.Errorf("%w: sealed under a different master key", ErrOpen)

// Box implements inbox.SecretBox.
type Box struct {
	aead cipher.AEAD
	id   [keyIDSize]byte
}

// New returns a Box for key, which must be exactly KeySize bytes. The key is
// not retained beyond the cipher it initializes.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	b := &Box{aead: aead}
	// The id is a one-way fingerprint of the key, so it can sit beside the
	// ciphertext without giving the key away.
	sum := sha256.Sum256(append([]byte("hookinlet secretbox key id\x00"), key...))
	copy(b.id[:], sum[:keyIDSize])
	return b, nil
}

// KeyID returns the id of the key, as it is written into every value this Box
// seals. It is safe to log.
func (b *Box) KeyID() string { return hex.EncodeToString(b.id[:]) }

// Seal encrypts plaintext and binds it to context, which Open must be given again
// (the inbox's slug, for a verifier secret). The result is version || key id ||
// nonce || ciphertext+tag.
func (b *Box) Seal(plaintext, context []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	out := make([]byte, 0, 1+keyIDSize+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, formatV2)
	out = append(out, b.id[:]...)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, b.aad(formatV2, b.id[:], context)), nil
}

// aad is what Seal authenticates besides the plaintext. Its pieces have fixed
// sizes except the last, so no two different (version, key id, context) triples
// give the same bytes.
func (b *Box) aad(version byte, id, context []byte) []byte {
	out := make([]byte, 0, 1+len(id)+len(context))
	out = append(out, version)
	out = append(out, id...)
	return append(out, context...)
}

// Open decrypts a value produced by Seal, given the same context. A value sealed
// by an earlier release (the original layout) opens whatever the context is; see
// Legacy.
func (b *Box) Open(ciphertext, context []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) == 0 {
		return nil, ErrOpen
	}
	switch ciphertext[0] {
	case formatV2:
		if len(ciphertext) < 1+keyIDSize+ns+b.aead.Overhead() {
			return nil, ErrOpen
		}
		id := ciphertext[1 : 1+keyIDSize]
		if subtle.ConstantTimeCompare(id, b.id[:]) != 1 {
			return nil, ErrWrongKey
		}
		nonce, sealed := ciphertext[1+keyIDSize:1+keyIDSize+ns], ciphertext[1+keyIDSize+ns:]
		pt, err := b.aead.Open(nil, nonce, sealed, b.aad(formatV2, id, context))
		if err != nil {
			return nil, ErrOpen
		}
		return pt, nil
	case formatV1:
		if len(ciphertext) < 1+ns+b.aead.Overhead() {
			return nil, ErrOpen
		}
		nonce, sealed := ciphertext[1:1+ns], ciphertext[1+ns:]
		pt, err := b.aead.Open(nil, nonce, sealed, []byte{formatV1})
		if err != nil {
			return nil, ErrOpen
		}
		return pt, nil
	}
	return nil, ErrOpen
}

// Legacy reports whether ciphertext is in the original layout, which names no key
// and is bound to nothing. Such a value should be opened and sealed again.
func Legacy(ciphertext []byte) bool { return len(ciphertext) > 0 && ciphertext[0] == formatV1 }
