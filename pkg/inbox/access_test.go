// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func TestAccessFromIsFalseOutsideARequest(t *testing.T) {
	if a, ok := inbox.AccessFrom(context.Background()); ok || a != (inbox.AccessInfo{}) {
		t.Errorf("AccessFrom(Background) = %+v, %v; want nothing", a, ok)
	}
	want := inbox.AccessInfo{Operation: "listMessages", RequestID: "abc", ClientIP: "203.0.113.7"}
	got, ok := inbox.AccessFrom(inbox.WithAccess(context.Background(), want))
	if !ok || got != want {
		t.Errorf("AccessFrom(WithAccess(%+v)) = %+v, %v", want, got, ok)
	}
}

func TestAccessInfoClientIPMayBeEmpty(t *testing.T) {
	// An empty address is carried as it is, so an embedder can tell "unknown" from a value.
	got, ok := inbox.AccessFrom(inbox.WithAccess(context.Background(), inbox.AccessInfo{Operation: "ingest"}))
	if !ok || got.ClientIP != "" {
		t.Errorf("AccessFrom = %+v, %v; want an empty ClientIP", got, ok)
	}
}

func TestRefuseAfterKeepsTheRefusalAndCarriesTheDelay(t *testing.T) {
	for _, sentinel := range []error{inbox.ErrRateLimited, inbox.ErrQuotaExceeded, inbox.ErrForbidden} {
		err := fmt.Errorf("plan limit: %w", inbox.RefuseAfter(sentinel, 90*time.Second))
		if !errors.Is(err, sentinel) {
			t.Errorf("%v: errors.Is(%v) is false", err, sentinel)
		}
		var ra inbox.RetryAfterer
		if !errors.As(err, &ra) || ra.RetryAfter() != 90*time.Second {
			t.Errorf("%v: no RetryAfterer of 90s found", err)
		}
		if err.Error() != "plan limit: "+sentinel.Error() {
			t.Errorf("Error() = %q: RefuseAfter must not change the text", err.Error())
		}
	}
	if inbox.RefuseAfter(nil, time.Second) != nil {
		t.Error("RefuseAfter(nil) is not nil")
	}
}
