// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// CreateToken implements inbox.Store.
func (s *Store) CreateToken(ctx context.Context, t *inbox.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t == nil {
		return inbox.Invalidf("token is required")
	}
	cp := *t
	if err := cp.Validate(); err != nil {
		return err
	}
	cp.InboxIDs = sortedIDs(cp.InboxIDs)
	now := inbox.NormalizeTime(s.now())

	var id int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var one int
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM tokens WHERE hash = ?`, []byte(cp.Hash)).Scan(&one); {
		case err == nil:
			return fmt.Errorf("%w: token hash is already in use", inbox.ErrConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check token hash: %w", err)
		}
		for _, inboxID := range cp.InboxIDs {
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE id = ? AND deleting = 0`, int64(inboxID)).Scan(&one); err != nil {
				return notFound(fmt.Errorf("sqlite: check inbox of token: %w", err), "inbox")
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO tokens (name, hash, all_inboxes, created_at) VALUES (?, ?, ?, ?)`,
			cp.Name, []byte(cp.Hash), boolInt(cp.AllInboxes), formatTime(now))
		if err != nil {
			return fmt.Errorf("sqlite: insert token: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		for _, inboxID := range cp.InboxIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO token_inboxes (token_id, inbox_id) VALUES (?, ?)`, id, int64(inboxID)); err != nil {
				return fmt.Errorf("sqlite: scope token: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	cp.ID, cp.CreatedAt = inbox.TokenID(id), now
	*t = cp
	return nil
}

func sortedIDs(ids []inbox.ID) []inbox.ID {
	out := append([]inbox.ID(nil), ids...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// GetTokenByHash implements inbox.Store. One statement reads the token and its
// inboxes, so the two agree.
func (s *Store) GetTokenByHash(ctx context.Context, hash []byte) (*inbox.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(hash) == 0 {
		return nil, fmt.Errorf("%w: token", inbox.ErrNotFound)
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT t.id, t.name, t.hash, t.all_inboxes, t.created_at, ti.inbox_id
		FROM tokens t LEFT JOIN token_inboxes ti ON ti.token_id = t.id
		WHERE t.hash = ? ORDER BY ti.inbox_id`, hash)
	if err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: look up token: %w", err))
	}
	defer rows.Close()
	var out *inbox.Token
	for rows.Next() {
		var (
			t         inbox.Token
			h         []byte
			all       int
			createdAt string
			inboxID   sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.Name, &h, &all, &createdAt, &inboxID); err != nil {
			return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: look up token: %w", err))
		}
		if out == nil {
			t.Hash, t.AllInboxes = h, all != 0
			if t.CreatedAt, err = parseTime(createdAt); err != nil {
				return nil, err
			}
			out = &t
		}
		if inboxID.Valid {
			out.InboxIDs = append(out.InboxIDs, inbox.ID(inboxID.Int64))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: look up token: %w", err))
	}
	if out == nil {
		return nil, fmt.Errorf("%w: token", inbox.ErrNotFound)
	}
	return out, nil
}

// ListTokens implements inbox.Store.
func (s *Store) ListTokens(ctx context.Context, p inbox.TokenListParams) ([]*inbox.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `SELECT id, name, hash, all_inboxes, created_at FROM tokens WHERE id > ?`
	args := []any{int64(p.AfterID)}
	if p.InboxID != 0 {
		query += ` AND (all_inboxes = 1 OR id IN (SELECT token_id FROM token_inboxes WHERE inbox_id = ?))`
		args = append(args, int64(p.InboxID))
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, p.EffectiveLimit())

	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list tokens: %w", err))
	}
	defer rows.Close()
	var out []*inbox.Token
	byID := map[inbox.TokenID]*inbox.Token{}
	for rows.Next() {
		var (
			t         inbox.Token
			h         []byte
			all       int
			createdAt string
		)
		if err := rows.Scan(&t.ID, &t.Name, &h, &all, &createdAt); err != nil {
			return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list tokens: %w", err))
		}
		t.Hash, t.AllInboxes = h, all != 0
		if t.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
		byID[t.ID] = &t
	}
	if err := rows.Err(); err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list tokens: %w", err))
	}
	if len(out) == 0 {
		return out, nil
	}
	// The inboxes of those tokens, in one query.
	marks := strings.TrimSuffix(strings.Repeat("?,", len(out)), ",")
	ids := make([]any, len(out))
	for i, t := range out {
		ids[i] = int64(t.ID)
	}
	//nolint:gosec // marks is only "?,?,...", one per token; the values are bound
	scopes, err := s.rdb.QueryContext(ctx, `SELECT token_id, inbox_id FROM token_inboxes WHERE token_id IN (`+marks+`) ORDER BY token_id, inbox_id`, ids...)
	if err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list token inboxes: %w", err))
	}
	defer scopes.Close()
	for scopes.Next() {
		var tid, iid int64
		if err := scopes.Scan(&tid, &iid); err != nil {
			return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list token inboxes: %w", err))
		}
		if t := byID[inbox.TokenID(tid)]; t != nil {
			t.InboxIDs = append(t.InboxIDs, inbox.ID(iid))
		}
	}
	if err := scopes.Err(); err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list token inboxes: %w", err))
	}
	return out, nil
}

// DeleteToken implements inbox.Store.
func (s *Store) DeleteToken(ctx context.Context, id inbox.TokenID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, int64(id))
		if err != nil {
			return fmt.Errorf("sqlite: delete token: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: token", inbox.ErrNotFound)
		}
		return nil
	})
}
