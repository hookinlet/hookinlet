// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"slices"
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

func TestSampleSecretsStopsAtItsBounds(t *testing.T) {
	svc, _ := secretService(t) // alpha, bravo, charlie have secrets, then plain has none
	for _, c := range []struct {
		secrets, inboxes int
		want             int
	}{{2, 0, 2}, {0, 1, 1}, {0, 2, 2}, {100, 100, 3}, {0, 0, 3}} {
		r, err := svc.SampleSecrets(t.Context(), c.secrets, 1, c.inboxes)
		if err != nil || r.Checked != c.want || !r.OK() {
			t.Errorf("SampleSecrets(%d, %d) checked %d (%v, %v), want %d", c.secrets, c.inboxes, r.Checked, r.OK(), err, c.want)
		}
	}
	// A wrong key shows in a sample just as it does in the whole.
	wrong := &admin.Service{Store: svc.Store, Box: boxOf(t, 2)}
	if r, _ := wrong.SampleSecrets(t.Context(), 1, 1, 0); !r.Systemic() {
		t.Errorf("a sample under the wrong key: %+v", r)
	}
}

// tenantStore is a fresh store with inboxes made in the order given, each with a
// secret sealed under key 1 and in the tenant named for it. It returns the sealing
// contexts of each tenant's secrets too.
func tenantStore(t *testing.T, tenants []string) (*sqlite.Store, map[string][]string) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box := boxOf(t, 1)
	contexts := map[string][]string{}
	for i, tenant := range tenants {
		uid, err := inbox.NewUID()
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := box.Seal([]byte("s"), inbox.VerifierSecretContext(uid))
		if err != nil {
			t.Fatal(err)
		}
		contexts[tenant] = append(contexts[tenant], string(inbox.VerifierSecretContext(uid)))
		slug := fmt.Sprintf("hook-%02d", i)
		if err := st.CreateInbox(t.Context(), &inbox.Inbox{Slug: slug, UID: uid, IngestKey: slug, Name: slug, Verifier: "token", Tenant: tenant, VerifierSecret: sealed}); err != nil {
			t.Fatal(err)
		}
	}
	return st, contexts
}

func checkedBy(r admin.SecretReport) map[string]int {
	m := map[string]int{}
	for _, t := range r.Tenants {
		m[t.Tenant] = t.Checked
	}
	return m
}

// The sample takes a few of each tenant's secrets before more of any one, so the
// tenant with the oldest inboxes does not fill it; what is left is filled with the
// oldest secrets passed over; and it still looks at no more inboxes than it may.
func TestSampleSecretsSpreadsOverTenants(t *testing.T) {
	var tenants []string
	for range 12 {
		tenants = append(tenants, "acme")
	}
	tenants = append(tenants, "", "", "globex")
	st, contexts := tenantStore(t, tenants)
	svc := &admin.Service{Store: st, Box: boxOf(t, 1)}
	for _, c := range []struct {
		secrets, perTenant, inboxes int
		want                        map[string]int
	}{
		{6, 2, 0, map[string]int{"acme": 3, "": 2, "globex": 1}}, // two each, then the oldest passed over
		{10, 2, 0, map[string]int{"acme": 7, "": 2, "globex": 1}},
		{5, 5, 0, map[string]int{"acme": 5}},                      // acme's share fills it before the others are reached
		{20, 3, 13, map[string]int{"acme": 12, "": 1}},            // globex is past the scan bound
		{3, 1, 0, map[string]int{"acme": 1, "": 1, "globex": 1}},  // one each
		{0, 1, 0, map[string]int{"acme": 12, "": 2, "globex": 1}}, // no bound: everything
	} {
		want := c.want
		r, err := svc.SampleSecrets(t.Context(), c.secrets, c.perTenant, c.inboxes)
		if got := checkedBy(r); err != nil || !r.OK() || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("SampleSecrets(%d, %d, %d) checked %v (%v, %v), want %v", c.secrets, c.perTenant, c.inboxes, got, r.OK(), err, want)
		}
	}

	// A tenant whose secrets do not open is named, with how many, and the rest open.
	r, _ := (&admin.Service{Store: svc.Store, Box: tenantBox{boxOf(t, 1), contexts["acme"]}}).SampleSecrets(t.Context(), 25, 5, 5000)
	if r.Systemic() || r.Checked != 15 {
		t.Fatalf("with acme's key gone: %+v", r)
	}
	for _, ts := range r.Tenants {
		if want := map[string]int{"acme": 12}[ts.Tenant]; len(ts.Unreadable) != want {
			t.Errorf("tenant %q: %d unreadable, want %d", ts.Tenant, len(ts.Unreadable), want)
		}
	}
}

// tenantBox fails Open for the secrets sealed with the contexts in down: one
// tenant's, as when a key service has lost that tenant's key.
type tenantBox struct {
	inbox.SecretBox
	down []string
}

func (b tenantBox) Open(ct, context []byte) ([]byte, error) {
	if slices.Contains(b.down, string(context)) {
		return nil, errors.New("key service: no key for this tenant")
	}
	return b.SecretBox.Open(ct, context)
}
