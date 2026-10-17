// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// recordingTB records what a helper reports instead of failing the test.
type recordingTB struct {
	testing.TB
	errs []string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recordingTB) Fatalf(format string, args ...any) { r.Errorf(format, args...) }

// embedsInterface is the mistake: embedding the interface promotes only its 19
// methods, so every optional capability of the store inside is hidden.
type embedsInterface struct{ inbox.Store }

// embedsConcrete keeps every capability, through promotion.
type embedsConcrete struct{ *sqlite.Store }

// forwardsAll forwards each optional capability explicitly.
type forwardsAll struct{ inbox.Store }

func (f forwardsAll) Backup(ctx context.Context) (io.ReadCloser, int64, error) {
	return f.Store.(inbox.Backupper).Backup(ctx)
}

func (f forwardsAll) Status(ctx context.Context) (inbox.StoreStatus, error) {
	return f.Store.(inbox.StatusReporter).Status(ctx)
}

func (f forwardsAll) CountUnverified(ctx context.Context, id inbox.ID, limit int64) (int64, error) {
	return f.Store.(inbox.UnverifiedCounter).CountUnverified(ctx, id, limit)
}

func openSQLite(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAssertStoreWrapperForwards(t *testing.T) {
	inner := openSQLite(t)

	t.Run("a wrapper that embeds inbox.Store fails, naming every missing interface", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		AssertStoreWrapperForwards(rec, embedsInterface{inner}, inner)
		if len(rec.errs) != 1 {
			t.Fatalf("reported %d failures, want 1: %q", len(rec.errs), rec.errs)
		}
		named := 0
		for _, o := range optionalStoreInterfaces {
			if implements(inner, o.typ) {
				named++
				if !strings.Contains(rec.errs[0], o.typ.String()) {
					t.Errorf("the failure does not name %s:\n%s", o.typ, rec.errs[0])
				}
			}
		}
		if named == 0 {
			t.Fatal("the SQLite store implements none of the optional interfaces; the test proves nothing")
		}
		if !strings.Contains(rec.errs[0], "embedsInterface") {
			t.Errorf("the failure does not name the wrapper:\n%s", rec.errs[0])
		}
	})

	t.Run("a wrapper that embeds the concrete store passes", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		AssertStoreWrapperForwards(rec, embedsConcrete{inner}, inner)
		if len(rec.errs) != 0 {
			t.Errorf("unexpected failure: %q", rec.errs)
		}
	})

	t.Run("a wrapper that forwards each interface passes", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		AssertStoreWrapperForwards(rec, forwardsAll{inner}, inner)
		if len(rec.errs) != 0 {
			t.Errorf("unexpected failure: %q", rec.errs)
		}
	})

	t.Run("nothing is required that the inner store lacks", func(t *testing.T) {
		bare := embedsInterface{inner} // implements none of them
		rec := &recordingTB{TB: t}
		AssertStoreWrapperForwards(rec, embedsInterface{bare}, bare)
		if len(rec.errs) != 0 {
			t.Errorf("unexpected failure: %q", rec.errs)
		}
	})

	t.Run("nil stores are reported", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		AssertStoreWrapperForwards(rec, nil, inner)
		AssertStoreWrapperForwards(rec, inner, nil)
		if len(rec.errs) != 2 {
			t.Errorf("reported %q, want two failures", rec.errs)
		}
	})
}
