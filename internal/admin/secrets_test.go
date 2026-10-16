// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

func boxOf(t *testing.T, b byte) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{b}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// secretService is a Service over a fresh store with three inboxes that have a
// verifier secret, sealed under key 1, and one that has none.
func secretService(t *testing.T) (*admin.Service, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := &admin.Service{Store: st, Box: boxOf(t, 1)}
	for _, slug := range []string{"alpha", "bravo", "charlie"} {
		if _, err := svc.Create(t.Context(), admin.CreateParams{Slug: slug, Verifier: "token", VerifierSecret: "secret-of-" + slug}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Create(t.Context(), admin.CreateParams{Slug: "plain", Verifier: "none"}); err != nil {
		t.Fatal(err)
	}
	return svc, st
}

func TestCheckSecretsFindsAWrongKey(t *testing.T) {
	svc, st := secretService(t)
	r, err := svc.CheckSecrets(t.Context())
	if err != nil || !r.OK() || r.Checked != 3 {
		t.Fatalf("with the right key: %+v, %v", r, err)
	}

	wrong := &admin.Service{Store: st, Box: boxOf(t, 2)}
	r, err = wrong.CheckSecrets(t.Context())
	if err != nil || r.OK() || len(r.WrongKey) != 3 {
		t.Fatalf("with another key: %+v, %v", r, err)
	}
	if err := r.Err(); !errors.Is(err, admin.ErrUnreadableSecrets) {
		t.Errorf("Err() = %v", err)
	}

	none := &admin.Service{Store: st}
	if r, _ := none.CheckSecrets(t.Context()); !r.NoKey || r.OK() {
		t.Errorf("with no key at all: %+v", r)
	}
}

func TestASecretCopiedToAnotherInboxDoesNotOpen(t *testing.T) {
	svc, st := secretService(t)
	a, _ := st.GetInboxBySlug(t.Context(), "alpha")
	b, _ := st.GetInboxBySlug(t.Context(), "bravo")
	b.VerifierSecret = a.VerifierSecret // as someone with write access to the database might
	if err := st.UpdateInbox(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	r, err := svc.CheckSecrets(t.Context())
	if err != nil || len(r.Damaged) != 1 || r.Damaged[0] != "bravo" {
		t.Errorf("a secret moved to another inbox: %+v, %v", r, err)
	}
}

// The slug can be used again by a later inbox; the secret sealed for the first must
// not open on the second.
func TestASecretDoesNotOpenOnALaterInboxWithTheSameSlug(t *testing.T) {
	svc, st := secretService(t)
	old, _ := st.GetInboxBySlug(t.Context(), "alpha")
	if err := st.DeleteInbox(t.Context(), old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(t.Context(), admin.CreateParams{Slug: "alpha", Verifier: "token"}); err != nil {
		t.Fatal(err)
	}
	now, _ := st.GetInboxBySlug(t.Context(), "alpha")
	if now.UID == old.UID {
		t.Fatalf("the new inbox has the old one's uid %q", now.UID)
	}
	now.VerifierSecret = old.VerifierSecret
	if err := st.UpdateInbox(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	r, err := svc.CheckSecrets(t.Context())
	if err != nil || len(r.Damaged) != 1 || r.Damaged[0] != "alpha" {
		t.Errorf("a secret carried over to a later inbox with the same slug: %+v, %v", r, err)
	}
}

func TestSetSecretSealsToTheInboxUID(t *testing.T) {
	svc, st := secretService(t)
	if _, err := svc.SetSecret(t.Context(), "plain", "now-with-a-secret"); err != nil {
		t.Fatal(err)
	}
	in, _ := st.GetInboxBySlug(t.Context(), "plain")
	if pt, err := svc.Box.Open(in.VerifierSecret, inbox.VerifierSecretContext(in.UID)); err != nil || string(pt) != "now-with-a-secret" {
		t.Errorf("the secret does not open with the inbox's uid: %q, %v", pt, err)
	}
	if _, err := svc.Box.Open(in.VerifierSecret, inbox.VerifierSecretContext(in.Slug)); err == nil {
		t.Error("the secret still opens with the slug")
	}
}

func TestRekeyMovesEverySecretToTheNewKey(t *testing.T) {
	_, st := secretService(t)
	oldBox, newBox := boxOf(t, 1), boxOf(t, 2)
	svc := &admin.Service{Store: st, Box: newBox}

	res, err := svc.Rekey(t.Context(), oldBox)
	if err != nil || res.Resealed != 3 || res.Unchanged != 0 {
		t.Fatalf("rekey: %+v, %v", res, err)
	}
	if r, _ := svc.CheckSecrets(t.Context()); !r.OK() {
		t.Errorf("after rekeying, under the new key: %+v", r)
	}
	for _, slug := range []string{"alpha", "bravo", "charlie"} {
		in, _ := st.GetInboxBySlug(t.Context(), slug)
		if pt, err := newBox.Open(in.VerifierSecret, inbox.VerifierSecretContext(in.UID)); err != nil || string(pt) != "secret-of-"+slug {
			t.Errorf("%s: %q, %v", slug, pt, err)
		}
		if _, err := oldBox.Open(in.VerifierSecret, inbox.VerifierSecretContext(in.UID)); err == nil {
			t.Errorf("%s still opens with the old key", slug)
		}
	}
	// Run again: nothing is done twice, and the old key is no longer needed.
	res, err = svc.Rekey(t.Context(), oldBox)
	if err != nil || res.Resealed != 0 || res.Unchanged != 3 {
		t.Errorf("a second run: %+v, %v", res, err)
	}
}

func TestRekeyWithTheWrongOldKeyChangesNothing(t *testing.T) {
	_, st := secretService(t)
	before, _ := st.GetInboxBySlug(t.Context(), "charlie")
	svc := &admin.Service{Store: st, Box: boxOf(t, 2)}
	if _, err := svc.Rekey(t.Context(), boxOf(t, 3)); err == nil {
		t.Fatal("rekey with a key that opens nothing succeeded")
	}
	after, _ := st.GetInboxBySlug(t.Context(), "charlie")
	if !bytes.Equal(before.VerifierSecret, after.VerifierSecret) || before.Version != after.Version {
		t.Error("a failed rekey changed an inbox")
	}
	// And an inbox sealed under neither key stops it before any other is touched.
	bravo := mustSecret(t, st, "bravo")
	other, _ := st.GetInboxBySlug(t.Context(), "alpha")
	sealed, _ := boxOf(t, 4).Seal([]byte("x"), inbox.VerifierSecretContext(other.UID))
	other.VerifierSecret = sealed
	if err := st.UpdateInbox(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rekey(t.Context(), boxOf(t, 1)); err == nil {
		t.Fatal("rekey went ahead with an unreadable secret")
	}
	if !bytes.Equal(bravo, mustSecret(t, st, "bravo")) {
		t.Error("rekey changed another inbox before it found the unreadable one")
	}
	r, _ := (&admin.Service{Store: st, Box: boxOf(t, 1)}).CheckSecrets(t.Context())
	if len(r.WrongKey) != 1 || r.WrongKey[0] != "alpha" {
		t.Errorf("only alpha should be unreadable under the old key, and the rest untouched: %+v", r)
	}
}

func mustSecret(t *testing.T, st *sqlite.Store, slug string) []byte {
	t.Helper()
	in, err := st.GetInboxBySlug(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return in.VerifierSecret
}

// A secret written by the first release has no key id and is bound to nothing; rekey
// with the same key brings it up to date.
func TestRekeyUpgradesAFirstReleaseSecret(t *testing.T) {
	svc, st := secretService(t)
	key := bytes.Repeat([]byte{1}, secretbox.KeySize)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{5}, aead.NonceSize())
	legacy := aead.Seal(append([]byte{1}, nonce...), nonce, []byte("secret-of-alpha"), []byte{1})
	in, _ := st.GetInboxBySlug(t.Context(), "alpha")
	in.VerifierSecret = legacy
	if err := st.UpdateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if r, _ := svc.CheckSecrets(t.Context()); !r.OK() {
		t.Fatalf("a first-release secret should still open: %+v", r)
	}
	res, err := svc.Rekey(t.Context(), svc.Box)
	if err != nil || res.Resealed != 1 || res.Unchanged != 2 {
		t.Fatalf("rekey: %+v, %v", res, err)
	}
	if secretbox.Legacy(mustSecret(t, st, "alpha")) {
		t.Error("the secret is still in the first layout")
	}
	if _, err := svc.Box.Open(mustSecret(t, st, "alpha"), inbox.VerifierSecretContext(in.UID)); err != nil {
		t.Errorf("after the upgrade: %v", err)
	}
}

// editsOnFetch edits the inbox with the given ID the first time Rekey fetches it to
// write, as another process would.
type editsOnFetch struct {
	inbox.Store
	id   inbox.ID
	done bool
}

func (e *editsOnFetch) GetInbox(ctx context.Context, id inbox.ID) (*inbox.Inbox, error) {
	if id == e.id && !e.done {
		e.done = true
		in, err := e.Store.GetInbox(ctx, id)
		if err != nil {
			return nil, err
		}
		in.VerifierSecret = nil // an admin changed the verifier, which drops its secret
		in.Verifier = "none"
		if err := e.UpdateInbox(ctx, in); err != nil {
			return nil, err
		}
	}
	return e.Store.GetInbox(ctx, id)
}

func TestRekeyDoesNotOverwriteAnEditMadeWhileItRan(t *testing.T) {
	_, st := secretService(t)
	alpha, _ := st.GetInboxBySlug(t.Context(), "alpha")
	svc := &admin.Service{Store: &editsOnFetch{Store: st, id: alpha.ID}, Box: boxOf(t, 2)}
	res, err := svc.Rekey(t.Context(), boxOf(t, 1))
	if err != nil || res.Skipped != 1 || res.Resealed != 2 {
		t.Fatalf("rekey: %+v, %v", res, err)
	}
	in, _ := st.GetInboxBySlug(t.Context(), "alpha")
	if in.Verifier != "none" || len(in.VerifierSecret) != 0 {
		t.Errorf("the edit was overwritten: verifier %q, secret of %d bytes", in.Verifier, len(in.VerifierSecret))
	}
}
