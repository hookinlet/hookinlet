// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package secretbox_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

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
	ct, err := a.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ct); !errors.Is(err, secretbox.ErrOpen) {
		t.Fatalf("want ErrOpen, got %v", err)
	}
}

func TestOpenRejectsUnknownVersion(t *testing.T) {
	box, _ := secretbox.New(key(1))
	ct, _ := box.Seal([]byte("secret"))
	ct[0] = 99
	if _, err := box.Open(ct); !errors.Is(err, secretbox.ErrOpen) {
		t.Fatalf("want ErrOpen, got %v", err)
	}
}

func TestErrorDoesNotRevealPlaintext(t *testing.T) {
	box, _ := secretbox.New(key(1))
	ct, _ := box.Seal([]byte("plaintext-marker"))
	ct[len(ct)-1] ^= 1
	_, err := box.Open(ct)
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("plaintext-marker")) {
		t.Fatalf("unexpected error %v", err)
	}
}
