// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"errors"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// suitePrincipal is a Principal that can read any inbox.
type suitePrincipal struct{}

func (suitePrincipal) CanReadInbox(*inbox.Inbox) bool { return true }
func (suitePrincipal) CanAdmin() bool                 { return false }

// AdmitterSuite runs the inbox.Admitter conformance tests.
//
// newAdmitter returns a new Admitter with nothing in use and no limit reached, so
// that a first access of any kind is admitted. The suite can only check what holds
// for every Admitter; that it refuses at its own limits is for the embedder's tests.
func AdmitterSuite(t *testing.T, newAdmitter func(t *testing.T) inbox.Admitter) {
	t.Helper()

	in := &inbox.Inbox{ID: 1, Slug: "suite-inbox", Name: "suite"}
	kinds := []inbox.AccessKind{inbox.AccessRead, inbox.AccessStream, inbox.AccessWait}

	// release is allowed to be nil, and must be safe to call when it is not.
	run := func(t *testing.T, a inbox.Admitter, kind inbox.AccessKind, in *inbox.Inbox) {
		t.Helper()
		release, err := a.Admit(t.Context(), suitePrincipal{}, in, kind)
		if err != nil {
			t.Fatalf("a first %s access was refused: %v", kind, err)
		}
		if release != nil {
			release()
		}
	}

	for _, kind := range kinds {
		t.Run("AdmitsAFirstAccess/"+string(kind), func(t *testing.T) {
			run(t, newAdmitter(t), kind, in)
		})
	}

	t.Run("AdmitsAReadThatIsNotToOneInbox", func(t *testing.T) {
		run(t, newAdmitter(t), inbox.AccessRead, nil)
	})

	t.Run("ReleasingGivesBackWhatWasTaken", func(t *testing.T) {
		a := newAdmitter(t)
		for _, kind := range kinds {
			// If a release did not give back what its access took, repeating
			// the cycle would eventually be refused.
			for i := range 200 {
				release, err := a.Admit(t.Context(), suitePrincipal{}, in, kind)
				if err != nil {
					t.Fatalf("%s access %d after releasing the earlier ones was refused: %v", kind, i, err)
				}
				if release != nil {
					release()
				}
			}
		}
	})

	t.Run("RefusalsAreTheDocumentedErrors", func(t *testing.T) {
		a := newAdmitter(t)
		// Take accesses until one is refused, if the Admitter has a limit low
		// enough to reach. Whatever it answers must be one of the errors the
		// server turns into a status.
		var held []func()
		defer func() {
			for _, r := range held {
				r()
			}
		}()
		for range 5000 {
			release, err := a.Admit(t.Context(), suitePrincipal{}, in, inbox.AccessStream)
			if err == nil {
				if release != nil {
					held = append(held, release)
				}
				continue
			}
			if !errors.Is(err, inbox.ErrRateLimited) && !errors.Is(err, inbox.ErrQuotaExceeded) && !errors.Is(err, inbox.ErrForbidden) {
				t.Errorf("refused with %v, which wraps none of ErrRateLimited, ErrQuotaExceeded or ErrForbidden", err)
			}
			if release != nil {
				t.Error("returned a release function with an error")
			}
			return
		}
	})

	t.Run("ACancelledContextDoesNotPanic", func(t *testing.T) {
		// A cancelled context may be refused, or admitted; either way it must not panic.
		a := newAdmitter(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		release, err := a.Admit(ctx, suitePrincipal{}, in, inbox.AccessRead)
		if err == nil && release != nil {
			release()
		}
	})
}
