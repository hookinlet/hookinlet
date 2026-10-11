// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"context"
	"errors"
	"fmt"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const (
	// InlineBodyBytes is the largest body the Store loads as part of the page
	// query. Bigger ones are fetched one at a time, and only if the page still
	// has room, so a page of large messages cannot pull gigabytes into memory.
	InlineBodyBytes = 64 << 10

	// PageBudget caps the encoded size of one page: bodies, headers, paths, and
	// everything else sent for each message, after JSON escaping. A page ends
	// early when the next message would exceed it (but always holds at least one
	// message), and the next link continues from there. It applies to summaries
	// too, since the headers and query a sender chooses are stored and can be
	// large.
	PageBudget = 16 << 20
)

// Page is one page of messages ready to be written out.
type Page struct {
	Messages  []Message `json:"messages"`
	Next      *string   `json:"next"`
	LatestSeq int64     `json:"latest_seq"`
	Epoch     string    `json:"epoch"`
	// Gap is true when an ascending read started below the oldest message kept:
	// messages after the cursor were removed before they were read.
	Gap bool `json:"gap"`

	// Cursor is the seq to ask for messages after to continue an ascending read.
	// It is the last message returned, except that when Complete it may jump
	// ahead to LatestSeq: nothing matching lies in between, so there is no need
	// to scan those rows again. Not part of the response.
	Cursor int64 `json:"-"`
	// Complete reports, for an ascending read, that nothing further matches as of
	// LatestSeq: the page was not full and was not cut short. Not part of the
	// response.
	Complete bool `json:"-"`
}

// FetchPage runs the request against the Store and assembles the response.
//
// Two bounds keep a hostile or just unlucky inbox from costing too much. Bodies
// over InlineBodyBytes are not loaded with the query, and the page stops once it
// would exceed PageBudget encoded bytes. Both only shorten the page, so
// following the next link still visits every message exactly once.
func FetchPage(ctx context.Context, st inbox.Store, in *inbox.Inbox, req PageRequest) (*Page, error) {
	if err := CheckEpoch(in, req.Epoch); err != nil {
		return nil, err
	}
	q := req.Query
	limit := int64(InlineBodyBytes)
	if req.BodyCap > 0 {
		limit = req.BodyCap
	}
	if req.Summary {
		limit = 0
	}
	q.BodyLimit = &limit
	res, err := st.Query(ctx, in.ID, q)
	if err != nil {
		return nil, err
	}

	budget := req.Budget
	if budget <= 0 {
		budget = PageBudget
	}
	page := &Page{Messages: make([]Message, 0, len(res.Messages)), LatestSeq: res.LatestSeq, Epoch: in.Epoch}
	page.Gap = q.After != nil && Gap(*q.After, res.OldestSeq, res.LatestSeq)
	kept := make([]*inbox.Message, 0, len(res.Messages))
	var used int64
	cut := false
	examined := int64(0) // the last seq the query returned, whether or not it made the page
	if q.After != nil {
		examined = *q.After
	}
	for _, m := range res.Messages {
		examined = m.Seq
		truncated := false
		if !req.Summary && m.Body == nil && req.BodyCap > 0 && m.Size > 0 {
			// Over the cap: leave the body out and say so. The caller fetches
			// that one message whole if it wants it.
			truncated = true
		} else if !req.Summary && m.Body == nil {
			full, err := st.Get(ctx, in.ID, m.Seq)
			if errors.Is(err, inbox.ErrNotFound) {
				continue // pruned since the query ran
			}
			if err != nil {
				return nil, fmt.Errorf("wire: load body of seq %d: %w", m.Seq, err)
			}
			m = full
		}
		enc := FromStored(m, in.Slug, in.Verifier, !req.Summary)
		enc.Epoch = in.Epoch
		enc.BodyTruncated = truncated
		cost := enc.encodedSize()
		if len(page.Messages) > 0 && used+cost > budget {
			cut = true
			break // the next link picks up here
		}
		used += cost
		kept = append(kept, m)
		page.Messages = append(page.Messages, enc)
	}
	if next := req.NextLink(in.Slug, in.Epoch, kept); next != "" {
		page.Next = &next
	}

	// Where an ascending read should continue. If the store returned fewer rows
	// than asked for, it returned everything that matched up to LatestSeq (both
	// come from one snapshot), so the cursor can safely jump there.
	if q.Ascending() {
		switch {
		case !cut && len(res.Messages) < q.EffectiveLimit():
			page.Complete = true
			page.Cursor = max(examined, res.LatestSeq)
		case cut:
			// The messages after the last one kept were not delivered.
			page.Cursor = kept[len(kept)-1].Seq
		default:
			// Everything the query returned was either delivered or pruned since.
			page.Cursor = examined
		}
	}
	return page, nil
}

// InboxInfo is the JSON form of an inbox's public metadata. It never includes
// anything secret.
type InboxInfo struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Verifier     string `json:"verifier"`
	MessageCount int64  `json:"message_count"`
	OldestSeq    int64  `json:"oldest_seq"`
	LatestSeq    int64  `json:"latest_seq"`
	Epoch        string `json:"epoch"`
}

// FetchInfo returns the metadata of an inbox.
func FetchInfo(ctx context.Context, st inbox.Store, in *inbox.Inbox) (*InboxInfo, error) {
	stats, err := st.Stats(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &InboxInfo{
		Slug: in.Slug, Name: in.Name, Verifier: in.Verifier,
		MessageCount: stats.MessageCount, OldestSeq: stats.OldestSeq, LatestSeq: stats.LatestSeq, Epoch: in.Epoch,
	}, nil
}

// CheckEpoch refuses a cursor that came from another epoch of the inbox. An
// empty epoch means the caller did not say, and passes.
func CheckEpoch(in *inbox.Inbox, epoch string) error {
	if epoch != "" && epoch != in.Epoch {
		return inbox.ErrEpochChanged
	}
	return nil
}

// Gap reports whether a reader that has everything up to seq after has missed
// messages: some in (after, oldest) were stored once and are gone. oldest is the
// lowest seq still stored (0 if none is) and latest the newest ever assigned.
// A cursor of 0 is a reader with no history, which has missed nothing it knew of.
func Gap(after, oldest, latest int64) bool {
	if after <= 0 {
		return false
	}
	if oldest == 0 {
		return after < latest // everything after the cursor was removed
	}
	return after < oldest-1
}
