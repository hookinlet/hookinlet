// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// interfering changes the inbox behind the caller's back before each of the
// first N updates, as an edit from another process would: the caller's copy is
// then stale and the version check refuses it.
type interfering struct {
	inbox.Store
	remaining atomic.Int64
	updates   atomic.Int64
}

func (s *interfering) UpdateInbox(ctx context.Context, in *inbox.Inbox) error {
	s.updates.Add(1)
	if s.remaining.Add(-1) >= 0 {
		other, err := s.GetInbox(ctx, in.ID)
		if err != nil {
			return err
		}
		other.Name = other.Name + "+"
		if err := s.Store.UpdateInbox(ctx, other); err != nil {
			return err
		}
	}
	return s.Store.UpdateInbox(ctx, in)
}

func newService(t *testing.T, interferences int64) (*admin.Service, *interfering, *sqlite.Store) {
	t.Helper()
	real, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = real.Close() })
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	st := &interfering{Store: real}
	st.remaining.Store(interferences)
	svc := &admin.Service{Store: st, Box: box}
	if _, err := svc.Create(t.Context(), admin.CreateParams{Slug: "retry-inbox", Verifier: "token"}); err != nil {
		t.Fatal(err)
	}
	st.updates.Store(0)
	return svc, st, real
}

// An edit from another process lands between the read and the write: the change
// is applied again on top of it, and neither is lost.
func TestEditsAreRetriedWhenAnotherProcessGotThereFirst(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := map[string]func(svc *admin.Service) error{
		"update": func(svc *admin.Service) error {
			_, err := svc.Update(t.Context(), "retry-inbox", admin.Patch{Name: str("mine")})
			return err
		},
		"set secret": func(svc *admin.Service) error {
			_, err := svc.SetSecret(t.Context(), "retry-inbox", "a-secret")
			return err
		},
		"rotate token": func(svc *admin.Service) error {
			_, err := svc.RotateToken(t.Context(), "retry-inbox")
			return err
		},
	}
	for name, do := range cases {
		svc, st, real := newService(t, 2)
		before, _ := real.GetInboxBySlug(t.Context(), "retry-inbox")
		if err := do(svc); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := st.updates.Load(); got != 3 {
			t.Errorf("%s: %d update attempts, want 3 (two stale, one good)", name, got)
		}
		after, _ := real.GetInboxBySlug(t.Context(), "retry-inbox")
		if after.Name == before.Name && name != "update" && len(after.VerifierSecret) == len(before.VerifierSecret) && string(after.AgentTokenHash) == string(before.AgentTokenHash) {
			t.Errorf("%s: nothing changed", name)
		}
		// The other process's edits are still there.
		if name != "update" && after.Name != "retry-inbox++" {
			t.Errorf("%s: the interfering edits were lost: name %q", name, after.Name)
		}
		if name == "update" && after.Name != "mine" {
			t.Errorf("update: name %q", after.Name)
		}
	}
}

func TestEditsGiveUpWhenTheInboxKeepsChanging(t *testing.T) {
	str := func(s string) *string { return &s }
	svc, st, _ := newService(t, 1_000)
	_, err := svc.Update(t.Context(), "retry-inbox", admin.Patch{Name: str("never")})
	if !errors.Is(err, inbox.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}
	if got := st.updates.Load(); got < 2 || got > 20 {
		t.Errorf("%d attempts before giving up", got)
	}
}

func TestRotatedTokensAreNeverReusedOrReturnedTwice(t *testing.T) {
	svc, _, real := newService(t, 0)
	seen := map[string]bool{}
	for range 5 {
		c, err := svc.RotateToken(t.Context(), "retry-inbox")
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.AgentToken] {
			t.Fatal("a token was issued twice")
		}
		seen[c.AgentToken] = true
		in, _ := real.GetInboxBySlug(t.Context(), "retry-inbox")
		if !token.Equal(c.AgentToken, in.AgentTokenHash) {
			t.Error("the issued token is not the stored one")
		}
	}
}
