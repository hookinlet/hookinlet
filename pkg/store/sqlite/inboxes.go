// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const inboxCols = `id, slug, name, version, verifier, verifier_config, verifier_secret, on_verify_fail,
	agent_token_hash, max_body_bytes, retention_days, retention_max_count, event_type_pointer,
	last_seq, disabled, created_at, updated_at`

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanInbox(row rowScanner) (*inbox.Inbox, error) {
	var (
		in                   inbox.Inbox
		config               string
		secret               []byte
		hash                 []byte
		disabled             int
		createdAt, updatedAt string
	)
	if err := row.Scan(&in.ID, &in.Slug, &in.Name, &in.Version, &in.Verifier, &config, &secret, &in.OnVerifyFail,
		&hash, &in.MaxBodyBytes, &in.RetentionDays, &in.RetentionMaxCount, &in.EventTypePointer,
		&in.LastSeq, &disabled, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	in.VerifierConfig = json.RawMessage(config)
	in.VerifierSecret = secret
	in.AgentTokenHash = hash
	in.Disabled = disabled != 0
	var err error
	if in.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if in.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &in, nil
}

// notFound maps "no rows" to inbox.ErrNotFound and passes other errors through.
func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", inbox.ErrNotFound, what)
	}
	return err
}

func (s *Store) getInboxWhere(ctx context.Context, where string, arg any, what string) (*inbox.Inbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	in, err := scanInbox(s.rdb.QueryRowContext(ctx, `SELECT `+inboxCols+` FROM inboxes WHERE `+where, arg))
	if err != nil {
		return nil, s.ctxErr(ctx, notFound(err, what))
	}
	return in, nil
}

// GetInbox implements inbox.Store.
func (s *Store) GetInbox(ctx context.Context, id inbox.ID) (*inbox.Inbox, error) {
	return s.getInboxWhere(ctx, `id = ?`, int64(id), "inbox")
}

// GetInboxBySlug implements inbox.Store.
func (s *Store) GetInboxBySlug(ctx context.Context, slug string) (*inbox.Inbox, error) {
	return s.getInboxWhere(ctx, `slug = ?`, slug, "inbox")
}

// ResolveIngestKey implements inbox.Store. In the open-source store the ingest
// key is the slug.
func (s *Store) ResolveIngestKey(ctx context.Context, key string) (*inbox.Inbox, error) {
	return s.getInboxWhere(ctx, `slug = ?`, key, "ingest key")
}

// GetInboxByTokenHash implements inbox.Store.
func (s *Store) GetInboxByTokenHash(ctx context.Context, hash []byte) (*inbox.Inbox, error) {
	if len(hash) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: token", inbox.ErrNotFound)
	}
	return s.getInboxWhere(ctx, `agent_token_hash = ?`, hash, "token")
}

// ListInboxes implements inbox.Store.
func (s *Store) ListInboxes(ctx context.Context, p inbox.ListParams) ([]*inbox.Inbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT `+inboxCols+` FROM inboxes WHERE id > ? ORDER BY id LIMIT ?`, int64(p.AfterID), p.EffectiveLimit())
	if err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list inboxes: %w", err))
	}
	defer rows.Close()
	var out []*inbox.Inbox
	for rows.Next() {
		in, err := scanInbox(rows)
		if err != nil {
			return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list inboxes: %w", err))
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: list inboxes: %w", err))
	}
	return out, nil
}

func secretArg(b inbox.SecretBytes) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// CreateInbox implements inbox.Store.
func (s *Store) CreateInbox(ctx context.Context, in *inbox.Inbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cp := *in
	cp.ApplyDefaults()
	if err := cp.Validate(); err != nil {
		return err
	}
	now := inbox.NormalizeTime(s.now())

	var id int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// Writes are serialized, so checking first and then inserting cannot
		// race with another writer.
		var one int
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE slug = ?`, cp.Slug).Scan(&one); {
		case err == nil:
			return fmt.Errorf("%w: slug %q is already taken", inbox.ErrConflict, cp.Slug)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check slug: %w", err)
		}
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE agent_token_hash = ?`, []byte(cp.AgentTokenHash)).Scan(&one); {
		case err == nil:
			return fmt.Errorf("%w: agent token hash is already in use", inbox.ErrConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check token hash: %w", err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO inboxes (
				slug, name, version, verifier, verifier_config, verifier_secret, on_verify_fail, agent_token_hash,
				max_body_bytes, retention_days, retention_max_count, event_type_pointer, last_seq, disabled,
				created_at, updated_at)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
			cp.Slug, cp.Name, cp.Verifier, string(cp.VerifierConfig), secretArg(cp.VerifierSecret), cp.OnVerifyFail,
			[]byte(cp.AgentTokenHash), cp.MaxBodyBytes, cp.RetentionDays, cp.RetentionMaxCount, cp.EventTypePointer,
			boolInt(cp.Disabled), formatTime(now), formatTime(now))
		if err != nil {
			return fmt.Errorf("sqlite: insert inbox: %w", err)
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return err
	}

	cp.ID, cp.Version, cp.LastSeq, cp.CreatedAt, cp.UpdatedAt = inbox.ID(id), 1, 0, now, now
	*in = cp
	return nil
}

// UpdateInbox implements inbox.Store.
func (s *Store) UpdateInbox(ctx context.Context, in *inbox.Inbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cp := *in
	cp.ApplyDefaults()
	if err := cp.Validate(); err != nil {
		return err
	}
	now := inbox.NormalizeTime(s.now())

	var refreshed *inbox.Inbox
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var stored int64
		switch err := tx.QueryRowContext(ctx, `SELECT version FROM inboxes WHERE id = ?`, int64(cp.ID)).Scan(&stored); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: inbox", inbox.ErrNotFound)
		case err != nil:
			return fmt.Errorf("sqlite: read version: %w", err)
		}
		if stored != cp.Version {
			return fmt.Errorf("%w: inbox was modified concurrently (have version %d, stored is %d)", inbox.ErrConflict, cp.Version, stored)
		}
		var other int64
		switch err := tx.QueryRowContext(ctx, `SELECT id FROM inboxes WHERE agent_token_hash = ? AND id <> ?`,
			[]byte(cp.AgentTokenHash), int64(cp.ID)).Scan(&other); {
		case err == nil:
			return fmt.Errorf("%w: agent token hash is already in use", inbox.ErrConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check token hash: %w", err)
		}

		// last_seq, slug, and created_at are deliberately not in this list.
		if _, err := tx.ExecContext(ctx, `UPDATE inboxes SET
				name = ?, version = version + 1, verifier = ?, verifier_config = ?, verifier_secret = ?,
				on_verify_fail = ?, agent_token_hash = ?, max_body_bytes = ?, retention_days = ?,
				retention_max_count = ?, event_type_pointer = ?, disabled = ?, updated_at = ?
			WHERE id = ?`,
			cp.Name, cp.Verifier, string(cp.VerifierConfig), secretArg(cp.VerifierSecret), cp.OnVerifyFail,
			[]byte(cp.AgentTokenHash), cp.MaxBodyBytes, cp.RetentionDays, cp.RetentionMaxCount, cp.EventTypePointer,
			boolInt(cp.Disabled), formatTime(now), int64(cp.ID)); err != nil {
			return fmt.Errorf("sqlite: update inbox: %w", err)
		}
		var err error
		refreshed, err = scanInbox(tx.QueryRowContext(ctx, `SELECT `+inboxCols+` FROM inboxes WHERE id = ?`, int64(cp.ID)))
		return err
	})
	if err != nil {
		return err
	}
	*in = *refreshed
	return nil
}

// DeleteInbox implements inbox.Store.
//
// Deleting the inbox row would take its messages with it in one transaction
// (ON DELETE CASCADE), which for a large inbox holds the single writer for
// seconds, and every delivery to every inbox waits. So the inbox is first
// closed to ingest and to agents (disabled, with its token hash replaced by
// random bytes nobody holds), then its messages are deleted a batch at a time
// like retention does, and only then the row. If the process stops part way, the
// inbox is left disabled and partly emptied, and deleting it again finishes the
// job.
func (s *Store) DeleteInbox(ctx context.Context, id inbox.ID) error {
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE inboxes SET disabled = 1, agent_token_hash = randomblob(32), version = version + 1, updated_at = ? WHERE id = ?`,
			formatTime(s.now()), int64(id))
		if err != nil {
			return fmt.Errorf("sqlite: close inbox for deletion: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: inbox", inbox.ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := s.deleteUpTo(ctx, id, math.MaxInt64); err != nil {
		return err
	}
	// Whatever arrived while the batches ran goes with the row.
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM inboxes WHERE id = ?`, int64(id))
		if err != nil {
			return fmt.Errorf("sqlite: delete inbox: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: inbox", inbox.ErrNotFound)
		}
		return nil
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cloneBytes copies b, returning a non-nil empty slice for empty input.
func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return []byte{}
	}
	return bytes.Clone(b)
}
