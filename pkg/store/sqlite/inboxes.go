// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const inboxCols = `id, slug, tenant, uid, ingest_key, name, version, verifier, verifier_config, verifier_secret, on_verify_fail,
	max_body_bytes, retention_days, retention_max_count, event_type_pointer,
	last_seq, disabled, created_at, updated_at, epoch`

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanInbox(row rowScanner) (*inbox.Inbox, error) {
	var (
		in                   inbox.Inbox
		config               string
		secret               []byte
		disabled             int
		createdAt, updatedAt string
	)
	if err := row.Scan(&in.ID, &in.Slug, &in.Tenant, &in.UID, &in.IngestKey, &in.Name, &in.Version, &in.Verifier, &config, &secret, &in.OnVerifyFail,
		&in.MaxBodyBytes, &in.RetentionDays, &in.RetentionMaxCount, &in.EventTypePointer,
		&in.LastSeq, &disabled, &createdAt, &updatedAt, &in.Epoch); err != nil {
		return nil, err
	}
	in.VerifierConfig = json.RawMessage(config)
	in.VerifierSecret = secret
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

// scopeSQL is what limits a statement on the inboxes table to the tenant the context
// is scoped to, with the value for its placeholder, or nothing if it is not scoped.
func scopeSQL(ctx context.Context) (clause string, args []any) {
	if t, scoped := inbox.TenantFrom(ctx); scoped {
		return ` AND tenant = ?`, []any{t}
	}
	return "", nil
}

// scoped returns args followed by the arguments of scopeSQL.
func scoped(ctx context.Context, args ...any) (clause string, all []any) {
	clause, extra := scopeSQL(ctx)
	return clause, append(args, extra...)
}

// messageScope limits a statement on the messages table to the inboxes of the
// tenant the context is scoped to, if it is.
func messageScope(ctx context.Context) (clause string, args []any) {
	if t, scopedTo := inbox.TenantFrom(ctx); scopedTo {
		return ` AND inbox_id IN (SELECT id FROM inboxes WHERE tenant = ?)`, []any{t}
	}
	return "", nil
}

func (s *Store) getInboxWhere(ctx context.Context, where string, arg any, what string) (*inbox.Inbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, args := scoped(ctx, arg)
	in, err := scanInbox(s.rdb.QueryRowContext(ctx, `SELECT `+inboxCols+` FROM inboxes WHERE deleting = 0 AND `+where+scope, args...))
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A slug names an inbox within one tenant: the one the context is scoped to, or
	// the default tenant when it is not.
	tenant, _ := inbox.TenantFrom(ctx)
	in, err := scanInbox(s.rdb.QueryRowContext(ctx,
		`SELECT `+inboxCols+` FROM inboxes WHERE deleting = 0 AND tenant = ? AND slug = ?`, tenant, slug))
	if err != nil {
		return nil, s.ctxErr(ctx, notFound(err, "inbox"))
	}
	return in, nil
}

// ResolveIngestKey implements inbox.Store.
func (s *Store) ResolveIngestKey(ctx context.Context, key string) (*inbox.Inbox, error) {
	return s.getInboxWhere(ctx, `ingest_key = ?`, key, "ingest key")
}

// ListInboxes implements inbox.Store.
func (s *Store) ListInboxes(ctx context.Context, p inbox.ListParams) ([]*inbox.Inbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, args := scoped(ctx, int64(p.AfterID))
	args = append(args, p.EffectiveLimit())
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT `+inboxCols+` FROM inboxes WHERE deleting = 0 AND id > ?`+scope+` ORDER BY id LIMIT ?`, args...) //nolint:gosec // scope is a constant fragment with a placeholder
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
	if in == nil {
		return inbox.Invalidf("inbox is required")
	}
	cp := *in
	cp.ApplyDefaults()
	if err := cp.Validate(); err != nil {
		return err
	}
	if t, scopedTo := inbox.TenantFrom(ctx); scopedTo {
		if cp.Tenant != "" && cp.Tenant != t {
			return inbox.Invalidf("the inbox is for tenant %q, but the context is scoped to another", cp.Tenant)
		}
		cp.Tenant = t
	}
	now := inbox.NormalizeTime(s.now())
	epoch, err := newEpoch()
	if err != nil {
		return err
	}
	if cp.UID == "" {
		if cp.UID, err = inbox.NewUID(); err != nil {
			return fmt.Errorf("sqlite: generate uid: %w", err)
		}
	}

	var id int64
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		// Writes are serialized, so checking first and then inserting cannot
		// race with another writer.
		var one int
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE tenant = ? AND slug = ?`, cp.Tenant, cp.Slug).Scan(&one); {
		case err == nil:
			var deleting int
			if tx.QueryRowContext(ctx, `SELECT deleting FROM inboxes WHERE tenant = ? AND slug = ?`, cp.Tenant, cp.Slug).Scan(&deleting) == nil && deleting == 1 {
				return fmt.Errorf("%w: slug %q belongs to an inbox that is still being deleted; the retention sweep finishes that (or run: hookinlet retention run)", inbox.ErrConflict, cp.Slug)
			}
			return fmt.Errorf("%w: slug %q is already taken", inbox.ErrConflict, cp.Slug)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check slug: %w", err)
		}
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE ingest_key = ?`, cp.IngestKey).Scan(&one); {
		case err == nil:
			return fmt.Errorf("%w: ingest key is already in use", inbox.ErrConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check ingest key: %w", err)
		}
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE uid = ?`, cp.UID).Scan(&one); {
		case err == nil:
			return fmt.Errorf("%w: uid is already in use", inbox.ErrConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check uid: %w", err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO inboxes (
				slug, slug_unused, tenant, uid, ingest_key, name, version, verifier, verifier_config, verifier_secret, on_verify_fail, agent_token_hash,
				max_body_bytes, retention_days, retention_max_count, event_type_pointer, last_seq, disabled,
				created_at, updated_at, epoch)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, randomblob(32), ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			cp.Slug, cp.UID, cp.Tenant, cp.UID, cp.IngestKey, cp.Name, cp.Verifier, string(cp.VerifierConfig), secretArg(cp.VerifierSecret), cp.OnVerifyFail,
			cp.MaxBodyBytes, cp.RetentionDays, cp.RetentionMaxCount, cp.EventTypePointer,
			boolInt(cp.Disabled), formatTime(now), formatTime(now), epoch)
		if err != nil {
			return fmt.Errorf("sqlite: insert inbox: %w", err)
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return err
	}

	cp.ID, cp.Version, cp.LastSeq, cp.CreatedAt, cp.UpdatedAt, cp.Epoch = inbox.ID(id), 1, 0, now, now, epoch
	*in = cp
	return nil
}

// UpdateInbox implements inbox.Store.
func (s *Store) UpdateInbox(ctx context.Context, in *inbox.Inbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil {
		return inbox.Invalidf("inbox is required")
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
		scope, scopeArgs := scoped(ctx, int64(cp.ID))
		switch err := tx.QueryRowContext(ctx, `SELECT version FROM inboxes WHERE id = ? AND deleting = 0`+scope, scopeArgs...).Scan(&stored); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: inbox", inbox.ErrNotFound)
		case err != nil:
			return fmt.Errorf("sqlite: read version: %w", err)
		}
		if stored != cp.Version {
			return fmt.Errorf("%w: inbox was modified concurrently (have version %d, stored is %d)", inbox.ErrConflict, cp.Version, stored)
		}
		var otherKey int64
		switch err := tx.QueryRowContext(ctx, `SELECT id FROM inboxes WHERE ingest_key = ? AND id <> ?`,
			cp.IngestKey, int64(cp.ID)).Scan(&otherKey); {
		case err == nil:
			return fmt.Errorf("%w: ingest key is already in use", inbox.ErrConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: check ingest key: %w", err)
		}

		// last_seq, slug, tenant, uid, and created_at are deliberately not in this list.
		if _, err := tx.ExecContext(ctx, `UPDATE inboxes SET
				ingest_key = ?, name = ?, version = version + 1, verifier = ?, verifier_config = ?, verifier_secret = ?,
				on_verify_fail = ?, max_body_bytes = ?, retention_days = ?,
				retention_max_count = ?, event_type_pointer = ?, disabled = ?, updated_at = ?
			WHERE id = ?`,
			cp.IngestKey, cp.Name, cp.Verifier, string(cp.VerifierConfig), secretArg(cp.VerifierSecret), cp.OnVerifyFail,
			cp.MaxBodyBytes, cp.RetentionDays, cp.RetentionMaxCount, cp.EventTypePointer,
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
// seconds, and every delivery to every inbox waits. So the inbox is first marked
// as being deleted (after which no reader, writer, or token can see it, and its
// tokens are revoked), then its messages are deleted a batch at a time like
// retention does, and only then the row. If the process stops part way, the
// retention sweep (Prune) finishes the job, as does calling DeleteInbox again.
func (s *Store) DeleteInbox(ctx context.Context, id inbox.ID) error {
	scope, scopeArgs := scoped(ctx, int64(id))
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE inboxes SET disabled = 1, deleting = 1, version = version + 1, updated_at = ? WHERE id = ?`+scope, //nolint:gosec // scope is a constant fragment with a placeholder
			append([]any{formatTime(s.now())}, scopeArgs...)...)
		if err != nil {
			return fmt.Errorf("sqlite: close inbox for deletion: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: inbox", inbox.ErrNotFound)
		}
		// Tokens stop reading it, and a token that read only this inbox goes.
		if _, err := tx.ExecContext(ctx, `DELETE FROM token_inboxes WHERE inbox_id = ?`, int64(id)); err != nil {
			return fmt.Errorf("sqlite: revoke tokens of inbox: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE all_inboxes = 0 AND id NOT IN (SELECT token_id FROM token_inboxes)`); err != nil {
			return fmt.Errorf("sqlite: remove tokens left with no inbox: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// A sweep may have finished it while this one was working: the inbox is gone,
	// which is what was asked for.
	if err := s.finishDelete(ctx, id); err != nil && !errors.Is(err, inbox.ErrNotFound) {
		return err
	}
	return nil
}

// finishDelete removes the messages of an inbox marked as being deleted, then
// the inbox. It is also what completes a delete that was interrupted.
func (s *Store) finishDelete(ctx context.Context, id inbox.ID) error {
	if _, err := s.deleteUpTo(ctx, id, math.MaxInt64); err != nil {
		return err
	}
	// Whatever arrived while the batches ran goes with the row.
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM inboxes WHERE id = ? AND deleting = 1`, int64(id))
		if err != nil {
			return fmt.Errorf("sqlite: delete inbox: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: inbox", inbox.ErrNotFound)
		}
		return nil
	})
}

// unfinishedDeletes lists the inboxes whose delete was started and not completed.
func (s *Store) unfinishedDeletes(ctx context.Context) ([]inbox.ID, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id FROM inboxes WHERE deleting = 1 ORDER BY id`)
	if err != nil {
		return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: find unfinished deletes: %w", err))
	}
	defer rows.Close()
	var ids []inbox.ID
	for rows.Next() {
		var id inbox.ID
		if err := rows.Scan(&id); err != nil {
			return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: find unfinished deletes: %w", err))
		}
		ids = append(ids, id)
	}
	return ids, s.ctxErr(ctx, rows.Err())
}

var _ inbox.EpochRenewer = (*Store)(nil)

// RenewEpochs implements inbox.EpochRenewer: it gives every inbox a new epoch. Do
// it after restoring a database from a copy of the data directory, or after
// restoring one snapshot more than once: the sequence numbers the restored server
// hands out may not be the ones its predecessor handed out, and a new epoch is how
// a client holding an old cursor finds out. (A snapshot taken by Backup already
// has new epochs.) It adds one to each inbox's Version.
func (s *Store) RenewEpochs(ctx context.Context) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		// A reader's position is a sequence number of the epoch it was set in.
		if _, err := tx.ExecContext(ctx, `DELETE FROM readers`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE inboxes SET epoch = lower(hex(randomblob(16))), version = version + 1`)
		return err
	})
}

// newEpoch returns a fresh random epoch: 128 bits, hex.
func newEpoch() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sqlite: generate epoch: %w", err)
	}
	return hex.EncodeToString(b), nil
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
