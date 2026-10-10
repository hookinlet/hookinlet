// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
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
		for bound > 0 {
			var n int64
			err := s.writeTx(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id IN (
						SELECT id FROM messages WHERE inbox_id = ? AND seq <= ? ORDER BY seq LIMIT ?)`,
					int64(p.inboxID), bound, pruneBatchRows)
				if err != nil {
					return fmt.Errorf("sqlite: prune: %w", err)
				}
				n, _ = res.RowsAffected()
				return nil
			})
			total += n // report what was actually deleted, even on a partial failure
			if err != nil {
				return total, err
			}
			if n < pruneBatchRows {
				break
			}
		}
	}
	return total, nil
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
		cutoff := ceilMicro(now.Add(-time.Duration(p.days) * 24 * time.Hour))
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
