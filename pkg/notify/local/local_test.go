// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package local_test

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// goleak fails the run if any test leaves a goroutine behind.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestConformance(t *testing.T) {
	inboxtest.NotifierSuite(t, func(t *testing.T) inbox.Notifier { return local.New() })
}

func TestSubscribersAreCounted(t *testing.T) {
	n := local.New()
	_, c1 := n.Subscribe(t.Context(), 1)
	_, c2 := n.Subscribe(t.Context(), 1)
	_, c3 := n.Subscribe(t.Context(), 2)
	if got := n.Subscribers(); got != 3 {
		t.Errorf("Subscribers = %d, want 3", got)
	}
	c1()
	c1()
	c2()
	c3()
	if got := n.Subscribers(); got != 0 {
		t.Errorf("Subscribers = %d after cancelling everything, want 0", got)
	}
}

// The notifier keeps no state for an inbox nobody is listening to, so a server
// that has seen a million inboxes does not hold a million map entries.
func TestNothingIsKeptForInboxesNobodyListensTo(t *testing.T) {
	n := local.New()
	for i := range 10_000 {
		_, cancel := n.Subscribe(t.Context(), inbox.ID(i))
		cancel()
	}
	if got := n.Subscribers(); got != 0 {
		t.Errorf("%d subscriptions remain", got)
	}
}
