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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// SecretBoxSuite runs the inbox.SecretBox conformance tests. newBox must
// return a box under a fixed key each time it is called, so the suite can
// check that separate boxes interoperate. When the box is also an
// inbox.ContextSecretBox, it checks that the context methods agree with Seal and
// Open; ContextSecretBoxSuite checks that they honour the context.
func SecretBoxSuite(t *testing.T, newBox func(t *testing.T) inbox.SecretBox, otherKey ...func(t *testing.T) inbox.SecretBox) {
	t.Helper()
	ctx1, ctx2 := []byte("inbox-one"), []byte("inbox-two")

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
			ct, err := box.Seal(pt, ctx1)
			if err != nil {
				t.Fatalf("%s: Seal: %v", name, err)
			}
			got, err := box.Open(ct, ctx1)
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
		ct, err := box.Seal(pt, ctx1)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ct, pt) {
			t.Error("ciphertext contains the plaintext")
		}
	})

	t.Run("SealIsRandomized", func(t *testing.T) {
		box := newBox(t)
		a, errA := box.Seal([]byte("same"), ctx1)
		b, errB := box.Seal([]byte("same"), ctx1)
		if errA != nil || errB != nil {
			t.Fatalf("Seal: %v, %v", errA, errB)
		}
		if bytes.Equal(a, b) {
			t.Error("sealing the same plaintext twice gave identical ciphertexts")
		}
	})

	if len(otherKey) > 0 {
		t.Run("AValueSealedUnderAnotherKeyDoesNotOpen", func(t *testing.T) {
			ct, err := newBox(t).Seal([]byte("secret"), ctx1)
			if err != nil {
				t.Fatal(err)
			}
			if pt, err := otherKey[0](t).Open(ct, ctx1); err == nil {
				t.Errorf("a box under a different key opened the value: %q", pt)
			}
		})
	}

	t.Run("InteroperatesAcrossInstances", func(t *testing.T) {
		ct, err := newBox(t).Seal([]byte("shared"), ctx1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := newBox(t).Open(ct, ctx1)
		if err != nil || string(got) != "shared" {
			t.Errorf("second instance could not open: %q, %v", got, err)
		}
	})

	t.Run("IsBoundToItsContext", func(t *testing.T) {
		box := newBox(t)
		ct, err := box.Seal([]byte("belongs to one"), ctx1)
		if err != nil {
			t.Fatal(err)
		}
		for name, ctx := range map[string][]byte{"another": ctx2, "none": nil, "a prefix": ctx1[:3], "an extension": append(bytes.Clone(ctx1), 'x')} {
			if got, err := box.Open(ct, ctx); err == nil {
				t.Errorf("a value sealed for one context opened with %s: %q", name, got)
			}
		}
		if got, err := box.Open(ct, ctx1); err != nil || string(got) != "belongs to one" {
			t.Errorf("the right context: %q, %v", got, err)
		}
	})

	t.Run("TamperedCiphertextFails", func(t *testing.T) {
		box := newBox(t)
		ct, err := box.Seal([]byte("integrity matters"), ctx1)
		if err != nil {
			t.Fatal(err)
		}
		for i := range ct {
			bad := bytes.Clone(ct)
			bad[i] ^= 0x01
			if _, err := box.Open(bad, ctx1); err == nil {
				t.Fatalf("flipping a bit at offset %d was not detected", i)
			}
		}
	})

	t.Run("MalformedCiphertextFails", func(t *testing.T) {
		box := newBox(t)
		ct, _ := box.Seal([]byte("x"), ctx1)
		for name, bad := range map[string][]byte{
			"nil":       nil,
			"empty":     {},
			"one byte":  {1},
			"truncated": ct[:len(ct)-1],
			"short":     ct[:len(ct)/2],
			"extended":  append(bytes.Clone(ct), 0),
		} {
			if _, err := box.Open(bad, ctx1); err == nil {
				t.Errorf("%s: Open succeeded", name)
			}
		}
	})

	if _, ok := newBox(t).(inbox.ContextSecretBox); ok {
		t.Run("ContextMethodsAgree", func(t *testing.T) {
			testContextMethodsAgree(t, newBox(t).(inbox.ContextSecretBox), ctx1, ctx2)
		})
	}

	t.Run("DoesNotAliasInput", func(t *testing.T) {
		box := newBox(t)
		pt := []byte("original")
		ct, _ := box.Seal(pt, ctx1)
		pt[0] = 'X'
		got, err := box.Open(ct, ctx1)
		if err != nil || string(got) != "original" {
			t.Errorf("Seal retained the caller's slice: %q, %v", got, err)
		}
	})
}

// ContextSecretBoxSuite runs the conformance tests of an inbox.ContextSecretBox
// that does I/O, such as one that asks a key service: besides agreeing with Seal
// and Open, its context methods must give up on a context that is already
// cancelled or past its deadline, with an error that wraps ctx.Err(). A box that
// does no I/O (pkg/secretbox) may ignore the context, and need only pass
// SecretBoxSuite. newBox is as for SecretBoxSuite.
func ContextSecretBoxSuite(t *testing.T, newBox func(t *testing.T) inbox.SecretBox) {
	t.Helper()
	ctx1, ctx2 := []byte("inbox-one"), []byte("inbox-two")
	box := func(t *testing.T) inbox.ContextSecretBox {
		t.Helper()
		b := newBox(t)
		cb, ok := b.(inbox.ContextSecretBox)
		if !ok {
			t.Fatalf("%T does not implement inbox.ContextSecretBox", b)
		}
		return cb
	}

	t.Run("ContextMethodsAgree", func(t *testing.T) {
		testContextMethodsAgree(t, box(t), ctx1, ctx2)
	})

	t.Run("GivesUpOnAnEndedContext", func(t *testing.T) {
		b := box(t)
		ct, err := b.Seal([]byte("secret"), ctx1)
		if err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		expired, cancel2 := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel2()
		for name, c := range map[string]struct {
			ctx  context.Context
			want error
		}{"cancelled": {cancelled, context.Canceled}, "past its deadline": {expired, context.DeadlineExceeded}} {
			if got, err := b.SealContext(c.ctx, []byte("secret"), ctx1); !errors.Is(err, c.want) {
				t.Errorf("SealContext with a context %s: %q, %v; want an error wrapping %v", name, got, err, c.want)
			}
			if got, err := b.OpenContext(c.ctx, ct, ctx1); !errors.Is(err, c.want) {
				t.Errorf("OpenContext with a context %s: %q, %v; want an error wrapping %v", name, got, err, c.want)
			}
		}
	})
}

// testContextMethodsAgree checks that SealContext and OpenContext do what Seal and
// Open do: a value sealed by either opens with either, and is bound the same way.
func testContextMethodsAgree(t *testing.T, box inbox.ContextSecretBox, ctx1, ctx2 []byte) {
	t.Helper()
	ctx := t.Context()
	pt := []byte("either way")
	viaCtx, err := box.SealContext(ctx, pt, ctx1)
	if err != nil {
		t.Fatalf("SealContext: %v", err)
	}
	viaPlain, err := box.Seal(pt, ctx1)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(viaCtx, pt) {
		t.Error("SealContext's ciphertext contains the plaintext")
	}
	if again, err := box.SealContext(ctx, pt, ctx1); err != nil || bytes.Equal(again, viaCtx) {
		t.Errorf("sealing the same plaintext twice with SealContext gave identical ciphertexts (%v)", err)
	}
	for name, ct := range map[string][]byte{"SealContext": viaCtx, "Seal": viaPlain} {
		if got, err := box.Open(ct, ctx1); err != nil || !bytes.Equal(got, pt) {
			t.Errorf("Open of a value from %s: %q, %v", name, got, err)
		}
		if got, err := box.OpenContext(ctx, ct, ctx1); err != nil || !bytes.Equal(got, pt) {
			t.Errorf("OpenContext of a value from %s: %q, %v", name, got, err)
		}
		if got, err := box.OpenContext(ctx, ct, ctx2); err == nil {
			t.Errorf("OpenContext opened a value from %s with another binding: %q", name, got)
		}
		bad := bytes.Clone(ct)
		bad[len(bad)-1] ^= 0x01
		if got, err := box.OpenContext(ctx, bad, ctx1); err == nil {
			t.Errorf("OpenContext opened a tampered value from %s: %q", name, got)
		}
	}
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
