// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package secretbox_test

import (
	"bytes"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

// The fuzz targets here run their seeds in a normal `go test`. To fuzz one:
//
//	go test -run='^$' -fuzz=FuzzOpen -fuzztime=90s ./pkg/secretbox

// FuzzOpen: a stored value is read back from disk, so Open must survive any
// bytes, and must only ever open something Seal made.
func FuzzOpen(f *testing.F) {
	box, err := secretbox.New(key(1))
	if err != nil {
		f.Fatal(err)
	}
	sealed, err := box.Seal([]byte("secret"), ctx) // fresh in each process; only used as a seed
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sealed, ctx)
	f.Add(sealed[:len(sealed)-1], ctx)
	f.Add([]byte{}, ctx)
	f.Add([]byte{1}, []byte(nil))
	f.Add([]byte{2}, []byte(nil))
	f.Add(append([]byte{1}, make([]byte, 64)...), ctx)
	f.Add(append([]byte{2}, make([]byte, 64)...), ctx)
	f.Fuzz(func(t *testing.T, ct, c []byte) {
		pt, err := box.Open(ct, c)
		if err != nil {
			return
		}
		// Only a value Seal made can open, and it holds the one plaintext sealed here;
		// the layout that names no context opens under any (see Legacy).
		if string(pt) != "secret" {
			t.Fatalf("opened %x as %q", ct, pt)
		}
		if !secretbox.Legacy(ct) && !bytes.Equal(c, ctx) {
			t.Fatalf("opened under the wrong context %q", c)
		}
	})
}

// FuzzSealOpen: whatever is sealed opens, only under its own context.
func FuzzSealOpen(f *testing.F) {
	box, err := secretbox.New(key(1))
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte("s"), []byte("a"), []byte("b"))
	f.Add([]byte{}, []byte{}, []byte("x"))
	f.Fuzz(func(t *testing.T, pt, c1, c2 []byte) {
		sealed, err := box.Seal(pt, c1)
		if err != nil {
			return
		}
		got, err := box.Open(sealed, c1)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("round trip: %q, %v", got, err)
		}
		if _, err := box.Open(sealed, c2); (err == nil) != bytes.Equal(c1, c2) {
			t.Fatalf("context %q opened a value sealed for %q: %v", c2, c1, err)
		}
	})
}
