// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package secretbox_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

var ctx = []byte("an-inbox")

func key(b byte) []byte { return bytes.Repeat([]byte{b}, secretbox.KeySize) }

func TestConformance(t *testing.T) {
	inboxtest.SecretBoxSuite(t, func(t *testing.T) inbox.SecretBox {
		box, err := secretbox.New(key(1))
		if err != nil {
			t.Fatal(err)
		}
		return box
	})
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := secretbox.New(make([]byte, n)); err == nil {
			t.Errorf("key of %d bytes accepted", n)
		}
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	a, _ := secretbox.New(key(1))
	b, _ := secretbox.New(key(2))
	ct, err := a.Seal([]byte("secret"), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ct, ctx); !errors.Is(err, secretbox.ErrWrongKey) {
		t.Fatalf("want ErrOpen, got %v", err)
	}
}

func TestOpenRejectsUnknownVersion(t *testing.T) {
	box, _ := secretbox.New(key(1))
	ct, _ := box.Seal([]byte("secret"), ctx)
	ct[0] = 99
	if _, err := box.Open(ct, ctx); !errors.Is(err, secretbox.ErrOpen) {
		t.Fatalf("want ErrOpen, got %v", err)
	}
}

func TestErrorDoesNotRevealPlaintext(t *testing.T) {
	box, _ := secretbox.New(key(1))
	ct, _ := box.Seal([]byte("plaintext-marker"), ctx)
	ct[len(ct)-1] ^= 1
	_, err := box.Open(ct, ctx)
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("plaintext-marker")) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestAWrongKeyIsToldApartFromDamage(t *testing.T) {
	a, _ := secretbox.New(key(1))
	b, _ := secretbox.New(key(2))
	ct, _ := a.Seal([]byte("secret"), ctx)
	if _, err := b.Open(ct, ctx); !errors.Is(err, secretbox.ErrWrongKey) || !errors.Is(err, secretbox.ErrOpen) {
		t.Errorf("another key: %v", err)
	}
	damaged := bytes.Clone(ct)
	damaged[len(damaged)-1] ^= 1
	if _, err := a.Open(damaged, ctx); errors.Is(err, secretbox.ErrWrongKey) || !errors.Is(err, secretbox.ErrOpen) {
		t.Errorf("damage under the right key: %v", err)
	}
	if a.KeyID() == b.KeyID() || len(a.KeyID()) != 8 {
		t.Errorf("key ids %q and %q", a.KeyID(), b.KeyID())
	}
	again, _ := secretbox.New(key(1))
	if again.KeyID() != a.KeyID() {
		t.Error("the same key has two ids")
	}
}

// The layout of the first release: version 1, nothing but the version authenticated.
func TestAValueFromTheFirstReleaseStillOpens(t *testing.T) {
	k := key(7)
	block, _ := aes.NewCipher(k)
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{9}, aead.NonceSize())
	legacy := append([]byte{1}, nonce...)
	legacy = aead.Seal(legacy, nonce, []byte("old secret"), []byte{1})

	box, _ := secretbox.New(k)
	got, err := box.Open(legacy, ctx)
	if err != nil || string(got) != "old secret" {
		t.Fatalf("opening a version 1 value: %q, %v", got, err)
	}
	if !secretbox.Legacy(legacy) {
		t.Error("a version 1 value is not reported as legacy")
	}
	fresh, _ := box.Seal(got, ctx)
	if secretbox.Legacy(fresh) {
		t.Error("a new value is in the old layout")
	}
	other, _ := secretbox.New(key(8))
	if _, err := other.Open(legacy, ctx); !errors.Is(err, secretbox.ErrOpen) {
		t.Errorf("a version 1 value under another key: %v", err)
	}
}
