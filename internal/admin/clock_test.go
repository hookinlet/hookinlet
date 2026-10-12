// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// pruneRecorder remembers the time retention was applied at.
type pruneRecorder struct {
	inbox.Store
	at time.Time
}

func (p *pruneRecorder) Prune(_ context.Context, now time.Time) (int64, error) {
	p.at = now
	return 0, nil
}

// A wall clock that jumps forward between two runs does not delete what is not yet due:
// the run uses the time that really passed. The next run accepts the clock as it reads.
func TestRetentionIgnoresAClockThatStepsForward(t *testing.T) {
	wall := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var mono time.Duration
	rec := &pruneRecorder{}
	svc := &admin.Service{Store: rec, Now: func() time.Time { return wall }, Monotonic: func() time.Duration { return mono }}
	ctx := t.Context()
	run := func() time.Time {
		t.Helper()
		if _, err := svc.SweepRetention(ctx); err != nil {
			t.Fatal(err)
		}
		return rec.at
	}

	if got := run(); !got.Equal(wall) {
		t.Fatalf("the first run: %v, want the clock", got)
	}
	// An hour passes, as both clocks agree.
	wall, mono = wall.Add(time.Hour), mono+time.Hour
	if got := run(); !got.Equal(wall) {
		t.Fatalf("an ordinary run: %v, want %v", got, wall)
	}
	// The wall clock leaps a year ahead while an hour really passes.
	before := wall
	wall, mono = wall.Add(365*24*time.Hour), mono+time.Hour
	want := before.Add(time.Hour)
	if got := run(); !got.Equal(want) {
		t.Errorf("after a forward step: %v, want %v (the time that really passed)", got, want)
	}
	// It stays there: the next run takes the clock as it reads.
	wall, mono = wall.Add(time.Hour), mono+time.Hour
	if got := run(); !got.Equal(wall) {
		t.Errorf("the run after: %v, want %v", got, wall)
	}
	// A step back is not this guard's business (an earlier time deletes less).
	wall, mono = wall.Add(-48*time.Hour), mono+time.Hour
	if got := run(); !got.Equal(wall) {
		t.Errorf("after a step back: %v, want %v", got, wall)
	}
}
