// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"errors"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
)

func screen(t *testing.T, r *Redactor, maxBody int64, body string) (inbox.BodyDecision, error) {
	t.Helper()
	return r.ScreenBody(t.Context(), &inbox.BodyScreenRequest{
		Inbox: &inbox.Inbox{Slug: "orders", MaxBodyBytes: maxBody}, Verified: true, Body: []byte(body),
	})
}

// The redactor is a BodyScreener, so it can run the suite a screener of your own would.
func TestRedactorPassesTheScreenerSuite(t *testing.T) {
	inboxtest.BodyScreenerSuite(t, func(*testing.T) inbox.BodyScreener {
		return NewRedactor(NewOutcomeCounter(inbox.NopHooks{}), "card", "a")
	})
}

func TestRedactorReplacesTheValuesOfItsKeysAtAnyDepth(t *testing.T) {
	r := NewRedactor(NewOutcomeCounter(inbox.NopHooks{}), "card", "email")
	dec, err := screen(t, r, 1<<20, `{"id":12345678901234567890,"card":"4111","items":[{"email":"a@example.com","n":1}]}`)
	if err != nil || !dec.Replace {
		t.Fatalf("%+v, %v", dec, err)
	}
	const want = `{"card":"[redacted]","id":12345678901234567890,"items":[{"email":"[redacted]","n":1}]}`
	if string(dec.Body) != want {
		t.Errorf("got %s\nwant %s", dec.Body, want)
	}
}

func TestRedactorKeepsWhatItHasNothingToRedactIn(t *testing.T) {
	r := NewRedactor(NewOutcomeCounter(inbox.NopHooks{}), "card")
	for _, body := range []string{`{"id":1}`, `plain text`, ``, `{"a":1} {"card":"x"}`, "\xff\xfe"} {
		if dec, err := screen(t, r, 1<<20, body); err != nil || dec.Replace {
			t.Errorf("%q: %+v, %v; want the body kept", body, dec, err)
		}
	}
}

// What cannot be stored once redacted is refused, never let through as it was.
func TestRedactorRefusesARedactionThatDoesNotFit(t *testing.T) {
	r := NewRedactor(NewOutcomeCounter(inbox.NopHooks{}), "k")
	dec, err := screen(t, r, 10, `{"k":1}`)
	if !errors.Is(err, inbox.ErrForbidden) || dec.Replace {
		t.Errorf("%+v, %v", dec, err)
	}
}

// It stays an observer and a hook as well: the server must still find all three.
func TestRedactorKeepsTheCountersInterfaces(t *testing.T) {
	var h inbox.Hooks = NewRedactor(NewOutcomeCounter(inbox.NopHooks{}), "k")
	if _, ok := h.(inbox.IngestObserver); !ok {
		t.Error("the redactor hides the counter's IngestObserver")
	}
	if _, ok := h.(inbox.BodyScreener); !ok {
		t.Error("the redactor is not a BodyScreener")
	}
}
