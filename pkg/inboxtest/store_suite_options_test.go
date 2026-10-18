// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// fullable is a Store that can be switched to refuse appends in the way mode says.
type fullable struct {
	inbox.Store
	full atomic.Bool
	mode string
}

func (f *fullable) Append(ctx context.Context, id inbox.ID, m *inbox.NewMessage) (*inbox.Message, bool, error) {
	if !f.full.Load() {
		return f.Store.Append(ctx, id, m)
	}
	switch f.mode {
	case "stores anyway":
		return f.Store.Append(ctx, id, m)
	case "wrong error":
		return nil, false, errors.New("write failed: no space left on device")
	case "uses a sequence number":
		// the refusal comes after the message was stored
		if _, _, err := f.Store.Append(ctx, id, &inbox.NewMessage{Method: "POST", Verified: true}); err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w: no room", inbox.ErrStorageFull)
	}
	return nil, false, fmt.Errorf("%w: no room", inbox.ErrStorageFull)
}

func TestCheckStorageFull(t *testing.T) {
	for _, c := range []struct {
		mode string
		undo bool
		want string // a part of the failure message; empty when the Store is right
	}{
		{"refuses", true, ""},
		{"refuses", false, ""},
		{"stores anyway", true, "stored a message"},
		{"wrong error", true, "wraps inbox.ErrStorageFull"},
		{"uses a sequence number", true, "used up a sequence number"},
	} {
		t.Run(fmt.Sprintf("%s, undo %v", c.mode, c.undo), func(t *testing.T) {
			s := &fullable{Store: openSQLite(t), mode: c.mode}
			rec := &recordingTB{TB: t}
			checkStorageFull(rec, s, func() func() {
				s.full.Store(true)
				if !c.undo {
					return nil
				}
				return func() { s.full.Store(false) }
			})
			got := strings.Join(rec.errs, "\n")
			if c.want == "" && got != "" {
				t.Errorf("unexpected failure: %s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("failures %q do not mention %q", rec.errs, c.want)
			}
		})
	}
}
