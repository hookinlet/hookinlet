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

// badTracker is a Store whose reader methods are wrong in the way mode says.
type badTracker struct {
	*sqlite.Store
	mode string
}

func (b *badTracker) AckReader(ctx context.Context, id inbox.ID, name, epoch string, seq int64, maxReaders int) (inbox.ReaderPosition, error) {
	switch b.mode {
	case "ignores the cap":
		maxReaders = 0
	case "ignores the epoch":
		if in, err := b.GetInbox(ctx, id); err == nil {
			epoch = in.Epoch
		}
	case "allows the future":
		seq = min(seq, 1)
	}
	return b.Store.AckReader(ctx, id, name, epoch, seq, maxReaders)
}

func (b *badTracker) ForgetReader(ctx context.Context, id inbox.ID, name string) error {
	if b.mode == "forgets quietly" {
		_ = b.Store.ForgetReader(ctx, id, name)
		return nil
	}
	return b.Store.ForgetReader(ctx, id, name)
}

func TestTheReaderTrackerChecks(t *testing.T) {
	checks := map[string]func(testing.TB, inbox.Store, inbox.ReaderTracker, *Clock){
		"cap": checkReaderCap, "epoch": checkReaderWrongEpoch, "bounds": checkReaderSeqBounds, "forget": checkReaderForget,
	}
	for _, c := range []struct{ mode, check, want string }{
		{"", "cap", ""}, {"", "epoch", ""}, {"", "bounds", ""}, {"", "forget", ""},
		{"ignores the cap", "cap", "a third reader with a cap of 2"},
		{"ignores the epoch", "epoch", "want an error wrapping ErrEpochChanged"},
		{"allows the future", "bounds", "the newest is 3"},
		{"forgets quietly", "forget", "forgetting a reader twice"},
	} {
		t.Run(c.mode+"/"+c.check, func(t *testing.T) {
			s := &badTracker{Store: openSQLite(t), mode: c.mode}
			rec := &recordingTB{TB: t}
			checks[c.check](rec, s, s, NewClock(baseTime))
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
