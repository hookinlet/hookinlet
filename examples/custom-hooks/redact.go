// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// redacted is what a redacted value is replaced with.
const redacted = "[redacted]"

// Redactor stores a delivery with the values of some JSON keys replaced, so that a
// card number or an e-mail address is never kept. It is an inbox.BodyScreener: the
// server finds that with a type assertion on the Hooks it is given and shows it the
// body of every delivery that is about to be stored, after the signature has been
// checked.
//
// It embeds the OutcomeCounter by its concrete type, so BeforeIngest, AfterIngest and
// ObserveIngest are still found by the server. A wrapper that embeds the inbox.Hooks
// interface instead would hide ScreenBody and every other optional interface, and
// nothing would be redacted: wrap with the concrete type, or forward each method.
//
// The stored message is marked modified, and its signature no longer matches what is
// stored. A body that is not JSON, or has none of the keys, is stored as received.
type Redactor struct {
	*OutcomeCounter
	keys map[string]bool
}

// NewRedactor redacts the values of keys, at any depth, in the JSON bodies it is shown.
func NewRedactor(next *OutcomeCounter, keys ...string) *Redactor {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return &Redactor{OutcomeCounter: next, keys: set}
}

// ScreenBody implements inbox.BodyScreener. It is called on every delivery's request,
// so it does nothing slow, and it never modifies the body it is given: it returns a new
// one.
//
// A redaction that cannot be stored is refused and not skipped: letting the original
// through would store what the embedder meant to keep out. Refusing has a cost, since
// a sender may retry or give up on an endpoint that fails, so it is only for what
// must never be stored.
func (r *Redactor) ScreenBody(_ context.Context, req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	dec := json.NewDecoder(bytes.NewReader(req.Body))
	dec.UseNumber() // keep numbers as they were written
	var doc any
	if err := dec.Decode(&doc); err != nil || dec.More() {
		return inbox.BodyDecision{}, nil // not a single JSON value: stored as received
	}
	if !r.scrub(doc) {
		return inbox.BodyDecision{}, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return inbox.BodyDecision{}, fmt.Errorf("%w: cannot redact", inbox.ErrForbidden)
	}
	if int64(len(out)) > req.Inbox.MaxBodyBytes {
		return inbox.BodyDecision{}, fmt.Errorf("%w: redacted body too large", inbox.ErrForbidden)
	}
	return inbox.BodyDecision{Replace: true, Body: out}, nil
}

// scrub replaces the values of the keys in v, at any depth, and says whether it did.
func (r *Redactor) scrub(v any) bool {
	changed := false
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if r.keys[k] {
				v[k] = redacted
				changed = true
			} else if r.scrub(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range v {
			if r.scrub(child) {
				changed = true
			}
		}
	}
	return changed
}

var _ inbox.BodyScreener = (*Redactor)(nil)
