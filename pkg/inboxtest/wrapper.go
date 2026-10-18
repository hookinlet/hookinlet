// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// optionalStoreInterfaces are the optional capabilities of an inbox.Store: the
// interfaces the server finds on its Store by type assertion, each with what the
// server does when the Store lacks it. This is the one list of them.
// TestEveryInboxInterfaceIsClassified fails when pkg/inbox declares an interface
// that is in neither this list nor the list of those that are not optional
// capabilities of a Store, and TestEveryStoreTypeAssertionIsListed when the code
// asserts a Store to an interface that is not here.
var optionalStoreInterfaces = []struct {
	typ     reflect.Type
	without string
}{
	{reflect.TypeFor[inbox.Backupper](), "GET /v1/admin/backup answers 501 not_implemented"},
	{reflect.TypeFor[inbox.StatusReporter](), "GET /v1/admin/status reports schema_version and database_bytes as 0 and leaves out free_bytes"},
	{reflect.TypeFor[inbox.EpochRenewer](), "an operator's or embedder's restore procedure cannot renew the epochs through the wrapper, so readers holding cursors from before a restore by other means are not told to start again (the server itself does not call it)"},
	{reflect.TypeFor[inbox.UnverifiedCounter](), "unverified messages are not capped: Options.MaxUnverified and any inbox.UnverifiedLimiter have no effect, and the server only logs a warning when it starts"},
}

func implements(v any, typ reflect.Type) bool { return reflect.TypeOf(v).Implements(typ) }

// AssertStoreWrapperForwards fails t if wrapper, a Store that wraps inner, does not
// implement every optional Store interface (inbox.Backupper, inbox.StatusReporter,
// inbox.UnverifiedCounter, inbox.EpochRenewer, and any added later) that inner implements. The server
// finds those by type assertion on the Store it is given, so a wrapper that embeds
// the inbox.Store interface hides them, and the server silently goes without:
// the failure says what each missing one turns off.
//
// It checks only that the methods exist, not what they do. A wrapper that embeds
// the concrete store passes, because the methods are promoted, but the promoted
// methods skip whatever the wrapper adds; forward each one explicitly instead (see
// "Wrapping a Store" in docs/extending.md). Call it from a test of the wrapper,
// with the store it wraps in production.
func AssertStoreWrapperForwards(t testing.TB, wrapper, inner inbox.Store) {
	t.Helper()
	if wrapper == nil || inner == nil {
		t.Fatalf("AssertStoreWrapperForwards: wrapper (%T) and inner (%T) must both be non-nil", wrapper, inner)
		return
	}
	var missing []string
	for _, o := range optionalStoreInterfaces {
		if implements(inner, o.typ) && !implements(wrapper, o.typ) {
			missing = append(missing, fmt.Sprintf("%s: without it, %s", o.typ, o.without))
		}
	}
	if len(missing) > 0 {
		t.Errorf("the Store wrapper %T does not implement %d optional interface(s) that the store it wraps (%T) does:\n  %s\n"+
			"Forward each one explicitly, applying the wrapper's own logic where it should (docs/extending.md, \"Wrapping a Store\").",
			wrapper, len(missing), inner, strings.Join(missing, "\n  "))
	}
}
