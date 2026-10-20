// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// registeringStore is a Store that refuses to create an inbox for a tenant that has
// not been registered, as an embedder's does when each customer is set up first.
type registeringStore struct {
	inbox.Store
	mu         sync.Mutex
	registered map[string]bool
}

func (s *registeringStore) register(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[tenant] = true
}

func (s *registeringStore) CreateInbox(ctx context.Context, in *inbox.Inbox) error {
	tenant := in.Tenant
	if scoped, _ := inbox.TenantFrom(ctx); tenant == "" {
		tenant = scoped
	}
	s.mu.Lock()
	ok := tenant == "" || s.registered[tenant]
	s.mu.Unlock()
	if !ok && len(tenant) <= inbox.MaxTenantLen { // an over-long key is for the inner Store to refuse
		return fmt.Errorf("tenant %q is not registered", tenant)
	}
	return s.Store.CreateInbox(ctx, in)
}

func newRegisteringStore(t *testing.T, now func() time.Time) *registeringStore {
	t.Helper()
	inner, err := sqlite.Open(t.Context(), t.TempDir(), sqlite.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	return &registeringStore{Store: inner, registered: map[string]bool{}}
}

// The suite prepares each tenant it uses, once per Store, before it uses it, so a Store
// that has to be told of its tenants passes.
func TestStoreSuitePreparesTenants(t *testing.T) {
	var mu sync.Mutex
	calls := map[*registeringStore][]string{}
	StoreSuite(t, func(t *testing.T, now func() time.Time) inbox.Store {
		return newRegisteringStore(t, now)
	}, StoreOptions{PrepareTenant: func(t *testing.T, s inbox.Store, tenant string) {
		rs := s.(*registeringStore)
		mu.Lock()
		calls[rs] = append(calls[rs], tenant)
		mu.Unlock()
		rs.register(tenant)
	}})

	if len(calls) == 0 {
		t.Fatal("PrepareTenant was never called")
	}
	for _, tenants := range calls {
		seen := map[string]bool{}
		for _, tenant := range tenants {
			if seen[tenant] {
				t.Errorf("tenant %q prepared twice on one Store: %v", tenant, tenants)
			}
			seen[tenant] = true
			if !slices.Contains([]string{"org-a", "org-b", "org-c"}, tenant) {
				t.Errorf("PrepareTenant called for %q; want only org-a, org-b and org-c", tenant)
			}
		}
	}
}

// Without the hook the same Store fails the tenant cases, which is what makes the hook
// necessary and the test above meaningful.
func TestRegisteringStoreRefusesAnUnpreparedTenant(t *testing.T) {
	s := newRegisteringStore(t, time.Now)
	t.Cleanup(func() { _ = s.Close() })
	in := mkInbox("probe", func(in *inbox.Inbox) { in.Tenant = "org-a" })
	if err := s.CreateInbox(t.Context(), in); err == nil {
		t.Fatal("an inbox was made for a tenant that is not registered")
	}
	s.register("org-a")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatalf("after registering: %v", err)
	}
}

// A Store that needs no registration passes with no options at all, and with a
// StoreOptions that has no hook.
func TestStoreSuiteWithoutPrepareTenant(t *testing.T) {
	newPlain := func(t *testing.T, now func() time.Time) inbox.Store {
		t.Helper()
		s, err := sqlite.Open(t.Context(), t.TempDir(), sqlite.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("no options", func(t *testing.T) { StoreSuite(t, newPlain) })
	t.Run("empty options", func(t *testing.T) { StoreSuite(t, newPlain, StoreOptions{}) })
}
