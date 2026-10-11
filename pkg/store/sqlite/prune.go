// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

type retentionPolicy struct {
	inboxID  inbox.ID
	days     int
	maxCount int64
	lastSeq  int64
}

// Prune implements inbox.Store.
//
// ReceivedAt never decreases with seq inside an inbox, so both retention rules
// select a prefix of the inbox's messages. Each inbox therefore reduces to a
// single bound, "delete every message with seq <= bound", which is deleted in
// batches of pruneBatchRows so appends can interleave with a large prune.
func (s *Store) Prune(ctx context.Context, now time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// A delete that was interrupted is finished first.
	unfinished, err := s.unfinishedDeletes(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range unfinished {
		// One delete that keeps failing must not stop retention for every inbox.
		if err := s.finishDelete(ctx, id); err != nil && !errors.Is(err, inbox.ErrNotFound) {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			s.log.LogAttrs(ctx, slog.LevelError, "could not finish deleting an inbox; it will be tried again", slog.Int64("inbox_id", int64(id)), slog.String("err", err.Error()))
		}
	}
	policies, err := s.retentionPolicies(ctx)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, p := range policies {
		bound, err := s.pruneBound(ctx, p, now)
		if err != nil {
			return total, err
		}
		n, err := s.deleteUpTo(ctx, p.inboxID, bound)
		total += n // report what was actually deleted, even on a partial failure
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// deleteUpTo deletes every message of the inbox with seq <= bound, a batch at a
// time, so that appends can take their turn between batches. It returns how many
// it deleted, which on an error is how many it got through.
func (s *Store) deleteUpTo(ctx context.Context, id inbox.ID, bound int64) (int64, error) {
	var total int64
	for bound > 0 {
		var n int64
		err := s.writeTx(ctx, func(tx *sql.Tx) error {
			var err error
			n, err = deleteBatch(ctx, tx, id, bound)
			return err
		})
		if err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
		total += n
	}
	return total, nil
}

// deleteBatch deletes the oldest messages of the inbox that are at or below
// bound: at most pruneBatchRows of them, and fewer when their bodies add up to
// pruneBatchBytes first (but always at least one). Appends wait for the writer,
// so how long a batch takes is how long a delivery can be made to wait, and that
// depends on bytes as much as on rows.
func deleteBatch(ctx context.Context, tx *sql.Tx, id inbox.ID, bound int64) (int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, size FROM messages WHERE inbox_id = ? AND seq <= ? ORDER BY seq LIMIT ?`,
		int64(id), bound, pruneBatchRows)
	if err != nil {
		return 0, fmt.Errorf("sqlite: select messages to delete: %w", err)
	}
	var ids []any
	var bytes int64
	for rows.Next() {
		var rowID, size int64
		if err := rows.Scan(&rowID, &size); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("sqlite: select messages to delete: %w", err)
		}
		if len(ids) > 0 && bytes+size > pruneBatchBytes {
			break
		}
		ids = append(ids, rowID)
		bytes += size
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("sqlite: select messages to delete: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlite: select messages to delete: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// Only placeholders are concatenated; every value is a bound argument.
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, ids...) //nolint:gosec // placeholders only
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete messages: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE inboxes SET message_count = message_count - ? WHERE id = ?`, n, int64(id)); err != nil {
		return 0, fmt.Errorf("sqlite: update message count: %w", err)
	}
	return n, nil
}

func (s *Store) retentionPolicies(ctx context.Context) ([]retentionPolicy, error) {
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT id, retention_days, retention_max_count, last_seq FROM inboxes
		 WHERE retention_days > 0 OR retention_max_count > 0 ORDER BY id`)
	if err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: read retention policies: %w", err))
	}
	defer rows.Close()
	var out []retentionPolicy
	for rows.Next() {
		var p retentionPolicy
		if err := rows.Scan(&p.inboxID, &p.days, &p.maxCount, &p.lastSeq); err != nil {
			return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: read retention policies: %w", err))
		}
		out = append(out, p)
	}
	return out, s.ctxErr(ctx, rows.Err())
}

// pruneBound returns the highest seq that should be deleted from the inbox, or
// 0 if nothing should be.
func (s *Store) pruneBound(ctx context.Context, p retentionPolicy, now time.Time) (int64, error) {
	var bound int64

	if p.days > 0 {
		// Stored values are validated, but a database written by something else
		// might hold more, and a duration that overflows would put the cutoff in
		// the future and delete everything.
		days := min(p.days, inbox.MaxRetentionDays)
		cutoff := ceilMicro(now.Add(-time.Duration(days) * 24 * time.Hour))
		var newestExpired sql.NullInt64
		err := s.rdb.QueryRowContext(ctx,
			`SELECT MAX(seq) FROM messages WHERE inbox_id = ? AND received_at < ?`,
			int64(p.inboxID), formatTime(cutoff)).Scan(&newestExpired)
		if err != nil {
			return 0, s.ctxErr(ctx, fmt.Errorf("sqlite: find expired messages: %w", err))
		}
		bound = newestExpired.Int64
	}

	if p.maxCount > 0 {
		// Stored seqs are contiguous (only prefixes are ever deleted), so the
		// newest maxCount messages are exactly those above lastSeq - maxCount.
		if b := p.lastSeq - p.maxCount; b > bound {
			bound = b
		}
	}
	return bound, nil
}
