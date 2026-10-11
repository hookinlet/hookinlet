// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package inboxtest provides conformance suites that every implementation of
// the interfaces in pkg/inbox must pass. A behavior that is not in a suite is
// not part of the contract.
//
// Stability: this package is pre-1.0. Its API can still change, and every change is
// listed in CHANGELOG.md; after v1.0.0, semantic versioning applies.
package inboxtest

import (
	"bytes"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// SecretBoxSuite runs the inbox.SecretBox conformance tests. newBox must
// return a box under a fixed key each time it is called, so the suite can
// check that separate boxes interoperate.
func SecretBoxSuite(t *testing.T, newBox func(t *testing.T) inbox.SecretBox) {
	t.Helper()

	t.Run("RoundTrip", func(t *testing.T) {
		box := newBox(t)
		for name, pt := range map[string][]byte{
			"empty":    {},
			"nil":      nil,
			"short":    []byte("s3cr3t"),
			"binary":   {0, 1, 2, 0xff, 0xfe, 0},
			"large":    bytes.Repeat([]byte("0123456789abcdef"), 4096),
			"all-byte": allBytes(),
		} {
			ct, err := box.Seal(pt)
			if err != nil {
				t.Fatalf("%s: Seal: %v", name, err)
			}
			got, err := box.Open(ct)
			if err != nil {
				t.Fatalf("%s: Open: %v", name, err)
			}
			if !bytes.Equal(got, pt) {
				t.Errorf("%s: round trip mismatch", name)
			}
		}
	})

	t.Run("CiphertextHidesPlaintext", func(t *testing.T) {
		box := newBox(t)
		pt := []byte("a-very-recognizable-plaintext-marker")
		ct, err := box.Seal(pt)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ct, pt) {
			t.Error("ciphertext contains the plaintext")
		}
	})

	t.Run("SealIsRandomized", func(t *testing.T) {
		box := newBox(t)
		a, _ := box.Seal([]byte("same"))
		b, _ := box.Seal([]byte("same"))
		if bytes.Equal(a, b) {
			t.Error("sealing the same plaintext twice gave identical ciphertexts")
		}
	})

	t.Run("InteroperatesAcrossInstances", func(t *testing.T) {
		ct, err := newBox(t).Seal([]byte("shared"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := newBox(t).Open(ct)
		if err != nil || string(got) != "shared" {
			t.Errorf("second instance could not open: %q, %v", got, err)
		}
	})

	t.Run("TamperedCiphertextFails", func(t *testing.T) {
		box := newBox(t)
		ct, err := box.Seal([]byte("integrity matters"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range ct {
			bad := bytes.Clone(ct)
			bad[i] ^= 0x01
			if _, err := box.Open(bad); err == nil {
				t.Fatalf("flipping a bit at offset %d was not detected", i)
			}
		}
	})

	t.Run("MalformedCiphertextFails", func(t *testing.T) {
		box := newBox(t)
		ct, _ := box.Seal([]byte("x"))
		for name, bad := range map[string][]byte{
			"nil":       nil,
			"empty":     {},
			"one byte":  {1},
			"truncated": ct[:len(ct)-1],
			"short":     ct[:len(ct)/2],
			"extended":  append(bytes.Clone(ct), 0),
		} {
			if _, err := box.Open(bad); err == nil {
				t.Errorf("%s: Open succeeded", name)
			}
		}
	})

	t.Run("DoesNotAliasInput", func(t *testing.T) {
		box := newBox(t)
		pt := []byte("original")
		ct, _ := box.Seal(pt)
		pt[0] = 'X'
		got, err := box.Open(ct)
		if err != nil || string(got) != "original" {
			t.Errorf("Seal retained the caller's slice: %q, %v", got, err)
		}
	})
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
