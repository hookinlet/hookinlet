// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// synchronousFull is the value SQLite reports for PRAGMA synchronous=FULL.
const synchronousFull = 2

// A 200 from ingest means the message is on disk, so the connection that commits
// must sync the WAL on every commit. With synchronous=NORMAL a commit survives a
// crash of the process but the last few can be lost on power loss.
func TestWriterSyncsEveryCommit(t *testing.T) {
	s, _ := internalStore(t)

	var level int
	err := s.writeTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&level)
	})
	if err != nil {
		t.Fatal(err)
	}
	if level != synchronousFull {
		t.Errorf("writer PRAGMA synchronous = %d, want %d (FULL)", level, synchronousFull)
	}

	if err := s.rdb.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level != synchronousFull {
		t.Errorf("reader PRAGMA synchronous = %d, want %d (FULL)", level, synchronousFull)
	}
}

// BenchmarkAppendSequential is one sender appending 1 KiB messages one after
// another, which is the cost of a durable commit with nothing else going on:
//
//	go test -run '^$' -bench BenchmarkAppendSequential -benchtime 3000x ./pkg/store/sqlite/
func BenchmarkAppendSequential(b *testing.B) {
	s, err := Open(b.Context(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	in := &inbox.Inbox{Slug: "bench", Name: "b", Verifier: "none", AgentTokenHash: []byte("hash-bench-0123456")}
	if err := s.CreateInbox(b.Context(), in); err != nil {
		b.Fatal(err)
	}
	body := bytes.Repeat([]byte("x"), 1024)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := s.Append(b.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: body, Verified: true}); err != nil {
			b.Fatal(err)
		}
	}
}
