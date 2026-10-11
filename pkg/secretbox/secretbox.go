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
	"errors"
	"fmt"
)

// KeySize is the required key length in bytes.
const KeySize = 32

// formatVersion prefixes every ciphertext and is authenticated as additional
// data, so the format can change without ambiguity.
const formatVersion byte = 1

// ErrOpen is returned by Open for any failure. It deliberately does not say
// whether the ciphertext was malformed, tampered with, or sealed under another
// key.
var ErrOpen = errors.New("secretbox: cannot open ciphertext")

// Box implements inbox.SecretBox.
type Box struct {
	aead cipher.AEAD
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
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext. The result is version || nonce || ciphertext+tag.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, formatVersion)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, []byte{formatVersion}), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(ciphertext []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) < 1+ns+b.aead.Overhead() || ciphertext[0] != formatVersion {
		return nil, ErrOpen
	}
	nonce, sealed := ciphertext[1:1+ns], ciphertext[1+ns:]
	pt, err := b.aead.Open(nil, nonce, sealed, []byte{formatVersion})
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}
