// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package benchseed bulk-loads a realistic number of messages into a data
// directory so benchmarks can measure queries at scale. It writes rows with SQL
// directly, bypassing Store.Append, because a million single-row transactions
// would take minutes. It is for benchmarks and tests only.
package benchseed

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the database/sql driver

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// EventTypes is how many distinct event types the seeded messages rotate through.
const EventTypes = 20

// Start is the received_at of seq 1; each later message arrives one second after
// the previous one.
var Start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Seed creates an inbox named slug in the data directory dir and fills it with
// rows messages. Message i (1-based):
//   - arrives i seconds after Start
//   - has event type "type-<i mod EventTypes>"
//   - is verified unless i is a multiple of 100, and has delivery id "evt_<i>"
//     when verified
//   - carries a JSON body of about 300 bytes
//
// It returns the inbox. The store must not be open on dir while Seed runs.
func Seed(ctx context.Context, dir, slug string, rows int) (*inbox.Inbox, error) {
	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	in := &inbox.Inbox{
		Slug: slug, IngestKey: slug, Name: slug, Verifier: "none",
		MaxBodyBytes: inbox.DefaultMaxBodyBytes,
	}
	if err := store.CreateInbox(ctx, in); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.Close(); err != nil { // checkpoints the WAL so the file is plain
		return nil, err
	}

	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(OFF)") // the seed can simply be redone if it is lost
	abs, err := filepath.Abs(filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO messages (
		inbox_id, seq, received_at, method, path_suffix, query, headers, content_type, body, size,
		verified, verify_error, delivery_id, event_type, source_ip)
		VALUES (?, ?, ?, 'POST', '', '', '{"Content-Type":["application/json"],"User-Agent":["bench/1.0"]}',
		        'application/json', ?, ?, ?, NULL, ?, ?, '203.0.113.7')`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	pad := strings.Repeat("x", 220)
	for i := 1; i <= rows; i++ {
		body := fmt.Sprintf(`{"id":"evt_%d","type":"type-%d","pad":"%s"}`, i, i%EventTypes, pad)
		verified := i%100 != 0
		var delivery any
		if verified {
			delivery = fmt.Sprintf("evt_%d", i)
		}
		if _, err := stmt.ExecContext(ctx, int64(in.ID), i, Start.Add(time.Duration(i)*time.Second).Format("2006-01-02T15:04:05.000000Z"),
			[]byte(body), len(body), boolInt(verified), delivery, fmt.Sprintf("type-%d", i%EventTypes)); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("seed row %d: %w", i, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inboxes SET last_seq = ?, message_count = ? WHERE id = ?`, rows, rows, int64(in.ID)); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	in.LastSeq = int64(rows)
	return in, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
