// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

var _ inbox.ReaderTracker = (*Store)(nil)

// AckReader implements inbox.ReaderTracker. The cap is counted and the row
// written inside one write transaction, and every write goes through the single
// writer connection, so concurrent calls cannot exceed it.
func (s *Store) AckReader(ctx context.Context, inboxID inbox.ID, name, epoch string, seq int64, maxReaders int) (inbox.ReaderPosition, error) {
	if err := ctx.Err(); err != nil {
		return inbox.ReaderPosition{}, err
	}
	if err := inbox.ValidateReaderName(name); err != nil {
		return inbox.ReaderPosition{}, err
	}
	now := inbox.NormalizeTime(s.now())
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var current string
		var latest int64
		scope, scopeArgs := scoped(ctx, int64(inboxID))
		if err := tx.QueryRowContext(ctx, `SELECT epoch, last_seq FROM inboxes WHERE id = ? AND deleting = 0`+scope, scopeArgs...).Scan(&current, &latest); err != nil {
			return notFound(err, "inbox")
		}
		switch {
		case epoch != current:
			return fmt.Errorf("%w: the position is for another epoch of the inbox", inbox.ErrEpochChanged)
		case seq < 0 || seq > latest:
			return inbox.Invalidf("seq must be from 0 to the inbox's newest sequence number (%d)", latest)
		}
		// A row from another epoch is never meant to exist (a new epoch deletes them),
		// but if one does it must neither be counted nor outlive this call.
		if _, err := tx.ExecContext(ctx, `DELETE FROM readers WHERE inbox_id = ? AND epoch <> ?`, int64(inboxID), current); err != nil {
			return fmt.Errorf("sqlite: drop stale readers: %w", err)
		}
		if maxReaders > 0 {
			var exists int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM readers WHERE inbox_id = ? AND name = ?`, int64(inboxID), name).Scan(&exists)
			if err == sql.ErrNoRows {
				var n int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM readers WHERE inbox_id = ?`, int64(inboxID)).Scan(&n); err != nil {
					return fmt.Errorf("sqlite: count readers: %w", err)
				}
				if n >= maxReaders {
					return fmt.Errorf("%w: the inbox already has %d named readers", inbox.ErrConflict, n)
				}
			} else if err != nil {
				return fmt.Errorf("sqlite: look up reader: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO readers (inbox_id, name, seq, epoch, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (inbox_id, name) DO UPDATE SET seq = excluded.seq, epoch = excluded.epoch, updated_at = excluded.updated_at`,
			int64(inboxID), name, seq, current, formatTime(now)); err != nil {
			return fmt.Errorf("sqlite: set reader: %w", err)
		}
		return nil
	})
	if err != nil {
		return inbox.ReaderPosition{}, err
	}
	return inbox.ReaderPosition{Name: name, Seq: seq, Epoch: epoch, UpdatedAt: now}, nil
}

// Readers implements inbox.ReaderTracker.
func (s *Store) Readers(ctx context.Context, inboxID inbox.ID) ([]inbox.ReaderPosition, error) {
	var out []inbox.ReaderPosition
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		var live int
		scope, scopeArgs := scoped(ctx, int64(inboxID))
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE id = ? AND deleting = 0`+scope, scopeArgs...).Scan(&live); err != nil {
			return notFound(err, "inbox")
		}
		rows, err := tx.QueryContext(ctx, `SELECT r.name, r.seq, r.epoch, r.updated_at FROM readers r
			JOIN inboxes i ON i.id = r.inbox_id AND i.epoch = r.epoch
			WHERE r.inbox_id = ? ORDER BY r.name`, int64(inboxID))
		if err != nil {
			return fmt.Errorf("sqlite: list readers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p inbox.ReaderPosition
			var at string
			if err := rows.Scan(&p.Name, &p.Seq, &p.Epoch, &at); err != nil {
				return fmt.Errorf("sqlite: list readers: %w", err)
			}
			if p.UpdatedAt, err = parseTime(at); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ForgetReader implements inbox.ReaderTracker.
func (s *Store) ForgetReader(ctx context.Context, inboxID inbox.ID, name string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var live int
		scope, scopeArgs := scoped(ctx, int64(inboxID))
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE id = ? AND deleting = 0`+scope, scopeArgs...).Scan(&live); err != nil {
			return notFound(err, "inbox")
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM readers WHERE inbox_id = ? AND name = ?`, int64(inboxID), name)
		if err != nil {
			return fmt.Errorf("sqlite: forget reader: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: reader", inbox.ErrNotFound)
		}
		return nil
	})
}
