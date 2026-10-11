// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const messageCols = `seq, received_at, method, path_suffix, query, headers, content_type, size, verified,
	verify_error, delivery_id, event_type, source_ip`

// bodyExpr returns the SQL expression that selects a message body and the
// argument it needs, if any. With a limit, bodies above it are replaced by NULL
// inside SQLite so they are never read into this process.
func bodyExpr(limit *int64) (expr string, args []any) {
	if limit == nil {
		return "body", nil
	}
	return "CASE WHEN size > ? THEN NULL ELSE body END", []any{*limit}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scanMessage reads a row selected as messageCols followed by the body.
func scanMessage(row rowScanner, inboxID inbox.ID, bodyLimit *int64) (*inbox.Message, error) {
	var (
		m                                       = inbox.Message{InboxID: inboxID}
		receivedAt, headers                     string
		body                                    []byte
		verified                                int
		verifyErr, deliveryID, eventType, srcIP sql.NullString
	)
	if err := row.Scan(&m.Seq, &receivedAt, &m.Method, &m.PathSuffix, &m.Query, &headers, &m.ContentType, &m.Size,
		&verified, &verifyErr, &deliveryID, &eventType, &srcIP, &body); err != nil {
		return nil, err
	}
	var err error
	if m.ReceivedAt, err = parseTime(receivedAt); err != nil {
		return nil, err
	}
	m.Headers = http.Header{}
	if err := json.Unmarshal([]byte(headers), &m.Headers); err != nil {
		return nil, fmt.Errorf("sqlite: corrupt headers for seq %d: %w", m.Seq, err)
	}
	m.Verified = verified != 0
	m.VerifyError, m.DeliveryID, m.EventType, m.SourceIP = verifyErr.String, deliveryID.String, eventType.String, srcIP.String

	switch withheld := bodyLimit != nil && m.Size > *bodyLimit; {
	case withheld:
		m.Body = nil
	case body == nil:
		m.Body = []byte{} // an empty body is empty, not nil
	default:
		m.Body = body
	}
	return &m, nil
}

// Append implements inbox.Store.
func (s *Store) Append(ctx context.Context, inboxID inbox.ID, m *inbox.NewMessage) (*inbox.Message, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := m.Validate(); err != nil {
		return nil, false, err
	}
	headers := inbox.RedactHeaders(m.Headers)
	headerJSON, err := json.Marshal(headers)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: encode headers: %w", err)
	}
	body := cloneBytes(m.Body)
	now := inbox.NormalizeTime(s.now())
	dedupeKey := m.DedupeKey
	if dedupeKey == "" {
		dedupeKey = m.DeliveryID
	}

	var (
		stored  *inbox.Message
		created bool
	)
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		// Only a verified delivery can be deduplicated, and only against another
		// verified one. The partial unique index enforces the same rule.
		if m.Verified && dedupeKey != "" {
			existing, err := scanMessage(tx.QueryRowContext(ctx,
				`SELECT `+messageCols+`, body FROM messages
				 WHERE inbox_id = ? AND dedupe_key = ? AND dedupe_key IS NOT NULL AND verified = 1`,
				int64(inboxID), dedupeKey), inboxID, nil)
			switch {
			case err == nil:
				stored, created = existing, false
				return nil
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("sqlite: look up duplicate: %w", err)
			}
		}

		// Take the next sequence number. The inbox row doubles as the lock that
		// serializes appends, and RETURNING hands back the value this exact
		// transaction claimed.
		var seq int64
		if err := tx.QueryRowContext(ctx,
			`UPDATE inboxes SET last_seq = last_seq + 1 WHERE id = ? RETURNING last_seq`, int64(inboxID)).Scan(&seq); err != nil {
			return notFound(fmt.Errorf("sqlite: assign seq: %w", err), "inbox")
		}

		// ReceivedAt never decreases with seq, even if the clock stepped back.
		receivedAt := now
		var prev string
		switch err := tx.QueryRowContext(ctx,
			`SELECT received_at FROM messages WHERE inbox_id = ? ORDER BY seq DESC LIMIT 1`, int64(inboxID)).Scan(&prev); {
		case err == nil:
			if p, perr := parseTime(prev); perr != nil {
				return perr
			} else if p.After(receivedAt) {
				receivedAt = p
			}
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: read previous message: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (
				inbox_id, seq, received_at, method, path_suffix, query, headers, content_type, body, size,
				verified, verify_error, delivery_id, event_type, source_ip, dedupe_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			int64(inboxID), seq, formatTime(receivedAt), m.Method, m.PathSuffix, m.Query, string(headerJSON),
			m.ContentType, body, len(body), boolInt(m.Verified), nullString(m.VerifyError),
			nullString(m.DeliveryID), nullString(m.EventType), nullString(m.SourceIP), nullString(dedupeKey)); err != nil {
			return fmt.Errorf("sqlite: insert message: %w", err)
		}

		stored, created = &inbox.Message{
			InboxID: inboxID, Seq: seq, ReceivedAt: receivedAt, Method: m.Method, PathSuffix: m.PathSuffix,
			Query: m.Query, Headers: headers, ContentType: m.ContentType, Body: cloneBytes(body), Size: int64(len(body)),
			Verified: m.Verified, VerifyError: m.VerifyError, DeliveryID: m.DeliveryID, EventType: m.EventType,
			SourceIP: m.SourceIP,
		}, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// Get implements inbox.Store.
func (s *Store) Get(ctx context.Context, inboxID inbox.ID, seq int64) (*inbox.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := scanMessage(s.rdb.QueryRowContext(ctx,
		`SELECT `+messageCols+`, body FROM messages WHERE inbox_id = ? AND seq = ?`, int64(inboxID), seq), inboxID, nil)
	if err != nil {
		return nil, s.ctxErr(ctx, notFound(err, "message"))
	}
	return m, nil
}

// LatestSeq implements inbox.Store.
func (s *Store) LatestSeq(ctx context.Context, inboxID inbox.ID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var seq int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT last_seq FROM inboxes WHERE id = ?`, int64(inboxID)).Scan(&seq); err != nil {
		return 0, s.ctxErr(ctx, notFound(err, "inbox"))
	}
	return seq, nil
}

// Stats implements inbox.Store.
//
// MessageCount is exact without counting rows. Messages are only ever removed
// as a prefix of an inbox (retention) or all together (deleting the inbox), and
// a duplicate delivery never consumes a seq, so the stored seqs are always the
// contiguous range oldest..latest. That makes the count a subtraction, which
// stays instant at millions of messages where COUNT(*) took over 100 ms. The
// conformance suite checks this against a real walk of the inbox.
func (s *Store) Stats(ctx context.Context, inboxID inbox.ID) (*inbox.InboxStats, error) {
	var st inbox.InboxStats
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM inboxes WHERE id = ?`, int64(inboxID)).Scan(&st.LatestSeq); err != nil {
			return notFound(err, "inbox")
		}
		var oldest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(seq) FROM messages WHERE inbox_id = ?`, int64(inboxID)).Scan(&oldest); err != nil {
			return fmt.Errorf("sqlite: find oldest message: %w", err)
		}
		if oldest.Valid {
			st.OldestSeq = oldest.Int64
			st.MessageCount = st.LatestSeq - st.OldestSeq + 1
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// Query implements inbox.Store.
func (s *Store) Query(ctx context.Context, inboxID inbox.ID, q inbox.Query) (*inbox.Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}

	bodySQL, args := bodyExpr(q.BodyLimit)
	var where []string
	where = append(where, "inbox_id = ?")
	args = append(args, int64(inboxID))
	order := "DESC"
	switch {
	case q.After != nil:
		where, args, order = append(where, "seq > ?"), append(args, *q.After), "ASC"
	case q.Before != nil:
		where, args = append(where, "seq < ?"), append(args, *q.Before)
	}
	if len(q.Types) > 0 {
		where = append(where, "event_type IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Types)), ",")+")")
		for _, t := range q.Types {
			args = append(args, t)
		}
	}
	if !q.Since.IsZero() {
		where, args = append(where, "received_at >= ?"), append(args, formatTime(ceilMicro(q.Since)))
	}
	if !q.Until.IsZero() {
		where, args = append(where, "received_at < ?"), append(args, formatTime(ceilMicro(q.Until)))
	}
	if q.Verified != nil {
		where, args = append(where, "verified = ?"), append(args, boolInt(*q.Verified))
	}
	if q.DeliveryID != "" {
		// "IS NOT NULL" is what lets SQLite use the partial delivery-id index; it
		// cannot prove a bound parameter is non-null by itself.
		where, args = append(where, "delivery_id IS NOT NULL", "delivery_id = ?"), append(args, q.DeliveryID)
	}
	args = append(args, q.EffectiveLimit())
	// Only constant fragments and "?" placeholders are concatenated; every value
	// is bound as an argument.
	stmt := `SELECT ` + messageCols + `, ` + bodySQL + ` FROM messages WHERE ` + strings.Join(where, " AND ") + //nolint:gosec // see above
		` ORDER BY seq ` + order + ` LIMIT ?`

	page := &inbox.Page{Messages: []*inbox.Message{}}
	// One read transaction gives LatestSeq and the messages the same snapshot.
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM inboxes WHERE id = ?`, int64(inboxID)).Scan(&page.LatestSeq); err != nil {
			return notFound(err, "inbox")
		}
		rows, err := tx.QueryContext(ctx, stmt, args...)
		if err != nil {
			return fmt.Errorf("sqlite: query messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMessage(rows, inboxID, q.BodyLimit)
			if err != nil {
				return fmt.Errorf("sqlite: query messages: %w", err)
			}
			page.Messages = append(page.Messages, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// queryDeliveryIDPlanSQL is the shape of a delivery-id query, used by the plan
// test to check that an index is chosen.
const queryDeliveryIDPlanSQL = `SELECT ` + messageCols + `, body FROM messages
	WHERE inbox_id = ? AND delivery_id IS NOT NULL AND delivery_id = ? ORDER BY seq DESC LIMIT ?`
