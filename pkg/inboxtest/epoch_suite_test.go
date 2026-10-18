// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// badRenewer is a Store whose RenewEpochs is wrong in the way mode says.
type badRenewer struct {
	*sqlite.Store
	mode  string
	calls int
}

func (b *badRenewer) RenewEpochs(ctx context.Context) error {
	b.calls++
	switch b.mode {
	case "does nothing":
		return nil
	case "fails the second time":
		if b.calls > 1 {
			return context.DeadlineExceeded
		}
	case "ignores a cancelled context":
		return b.Store.RenewEpochs(context.WithoutCancel(ctx))
	case "only unscoped":
		if _, scoped := inbox.TenantFrom(ctx); scoped {
			return nil
		}
	case "renews once":
		if b.calls > 1 {
			return nil
		}
	}
	return b.Store.RenewEpochs(ctx)
}

func TestTheEpochRenewerChecks(t *testing.T) {
	checks := map[string]func(testing.TB, inbox.Store, inbox.EpochRenewer){
		"every":     checkRenewChangesEveryEpoch,
		"tenants":   checkRenewReachesEveryTenant,
		"untouched": checkRenewChangesNothingElse,
		"twice":     checkRenewTwice,
		"cancelled": checkRenewCancelled,
	}
	for _, c := range []struct {
		mode  string
		check string
		want  string // empty: the check passes
	}{
		{"", "every", ""}, {"", "tenants", ""}, {"", "untouched", ""}, {"", "twice", ""}, {"", "cancelled", ""},
		{"does nothing", "every", "a cursor from before would still be accepted"},
		{"does nothing", "untouched", "the epoch did not change"},
		{"fails the second time", "twice", "a second time"},
		{"renews once", "twice", "each call must give a new one"},
		{"ignores a cancelled context", "cancelled", "wrapping context.Canceled"},
		{"only unscoped", "tenants", "renews every tenant whatever the scope"},
	} {
		t.Run(c.mode+"/"+c.check, func(t *testing.T) {
			s := &badRenewer{Store: openSQLite(t), mode: c.mode}
			rec := &recordingTB{TB: t}
			checks[c.check](rec, s, s)
			got := strings.Join(rec.errs, "\n")
			switch {
			case c.want == "" && got != "":
				t.Errorf("unexpected failure: %s", got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Errorf("failures %q do not mention %q", rec.errs, c.want)
			}
		})
	}
}
