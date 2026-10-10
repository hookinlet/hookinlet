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

	// PageBodyBudget caps the total body bytes in one page. A page ends early
	// when the next message would exceed it (but always holds at least one
	// message), and the next link continues from there.
	PageBodyBudget = 16 << 20
)

// Page is one page of messages ready to be written out.
type Page struct {
	Messages  []Message `json:"messages"`
	Next      *string   `json:"next"`
	LatestSeq int64     `json:"latest_seq"`
}

// FetchPage runs the request against the Store and assembles the response.
//
// Two bounds keep a hostile or just unlucky inbox from costing too much. Bodies
// over InlineBodyBytes are not loaded with the query, and the page stops once
// PageBodyBudget bytes of bodies are in it. Both only shorten the page, so
// following the next link still visits every message exactly once.
func FetchPage(ctx context.Context, st inbox.Store, in *inbox.Inbox, req PageRequest) (*Page, error) {
	q := req.Query
	limit := int64(InlineBodyBytes)
	if req.Summary {
		limit = 0
	}
	q.BodyLimit = &limit
	res, err := st.Query(ctx, in.ID, q)
	if err != nil {
		return nil, err
	}

	kept := make([]*inbox.Message, 0, len(res.Messages))
	var used int64
	for _, m := range res.Messages {
		if !req.Summary {
			if len(kept) > 0 && used+m.Size > PageBodyBudget {
				break // the next link picks up here
			}
			used += m.Size
			if m.Body == nil {
				full, err := st.Get(ctx, in.ID, m.Seq)
				if errors.Is(err, inbox.ErrNotFound) {
					continue // pruned since the query ran
				}
				if err != nil {
					return nil, fmt.Errorf("wire: load body of seq %d: %w", m.Seq, err)
				}
				m = full
			}
		}
		kept = append(kept, m)
	}

	page := &Page{Messages: make([]Message, 0, len(kept)), LatestSeq: res.LatestSeq}
	for _, m := range kept {
		page.Messages = append(page.Messages, FromStored(m, in.Slug, in.Verifier, !req.Summary))
	}
	if next := req.NextLink(in.Slug, kept); next != "" {
		page.Next = &next
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
}

// FetchInfo returns the metadata of an inbox.
func FetchInfo(ctx context.Context, st inbox.Store, in *inbox.Inbox) (*InboxInfo, error) {
	stats, err := st.Stats(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &InboxInfo{
		Slug: in.Slug, Name: in.Name, Verifier: in.Verifier,
		MessageCount: stats.MessageCount, OldestSeq: stats.OldestSeq, LatestSeq: stats.LatestSeq,
	}, nil
}
