// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// DefaultTokenName is the name of the token Create issues with a new inbox.
const DefaultTokenName = "default"

// TokenView is a token as an admin sees it. It never carries the token itself
// or its hash.
type TokenView struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	AllInboxes bool     `json:"all_inboxes"`
	Inboxes    []string `json:"inboxes"`
	CreatedAt  string   `json:"created_at"`
}

// TokenParams describes a new token: a name, and either a list of inbox slugs
// it may read or all of them.
type TokenParams struct {
	Name       string   `json:"name"`
	Inboxes    []string `json:"inboxes,omitempty"`
	AllInboxes bool     `json:"all_inboxes,omitempty"`
}

// TokenCreated is the result of CreateToken. Token is the only copy there will
// ever be.
type TokenCreated struct {
	Record TokenView `json:"record"`
	Token  string    `json:"token"`
}

type issued struct {
	Record *inbox.Token
	Token  string
}

// issue generates a token and stores it.
func (s *Service) issue(ctx context.Context, name string, ids []inbox.ID, all bool) (*issued, error) {
	tok, hash, err := token.Generate()
	if err != nil {
		return nil, fmt.Errorf("admin: generate token: %w", err)
	}
	rec := &inbox.Token{Name: name, Hash: hash, AllInboxes: all, InboxIDs: ids}
	if err := s.Store.CreateToken(ctx, rec); err != nil {
		return nil, err
	}
	return &issued{Record: rec, Token: tok}, nil
}

// CreateToken issues a token for the named inboxes, or for all of them. The
// token is returned here and never again.
func (s *Service) CreateToken(ctx context.Context, p TokenParams) (*TokenCreated, error) {
	if p.AllInboxes == (len(p.Inboxes) > 0) {
		return nil, inbox.Invalidf("name the inboxes the token may read, or set all_inboxes, but not both and not neither")
	}
	if len(p.Inboxes) > inbox.MaxTokenInboxes {
		return nil, inbox.Invalidf("a token may be scoped to at most %d inboxes; use all_inboxes for more", inbox.MaxTokenInboxes)
	}
	var ids []inbox.ID
	for _, slug := range p.Inboxes {
		in, err := s.Store.GetInboxBySlug(ctx, slug)
		if err != nil {
			return nil, err
		}
		ids = append(ids, in.ID)
	}
	if err := (&inbox.Token{Name: p.Name, Hash: []byte{1}, AllInboxes: p.AllInboxes, InboxIDs: ids}).Validate(); err != nil {
		return nil, err
	}
	t, err := s.issue(ctx, p.Name, ids, p.AllInboxes)
	if err != nil {
		return nil, err
	}
	v, err := s.tokenViews(ctx, []*inbox.Token{t.Record})
	if err != nil {
		return nil, err
	}
	if len(v) == 0 { // its only inboxes were deleted while it was being issued
		return nil, fmt.Errorf("the inbox was deleted: %w", inbox.ErrNotFound)
	}
	return &TokenCreated{Record: v[0], Token: t.Token}, nil
}

// ListTokens returns tokens in ID order after the given one. A slug limits the
// list to the tokens that can read that inbox, those for all inboxes included.
// next is the ID to pass as after for the following page, or 0 on the last.
func (s *Service) ListTokens(ctx context.Context, after inbox.TokenID, limit int, slug string) (views []TokenView, next inbox.TokenID, err error) {
	p := inbox.TokenListParams{AfterID: after, Limit: limit}
	if slug != "" {
		in, err := s.Store.GetInboxBySlug(ctx, slug)
		if err != nil {
			return nil, 0, err
		}
		p.InboxID = in.ID
	}
	tokens, err := s.Store.ListTokens(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	if len(tokens) > 0 && len(tokens) >= p.EffectiveLimit() {
		next = tokens[len(tokens)-1].ID
	}
	views, err = s.tokenViews(ctx, tokens)
	return views, next, err
}

// RevokeToken deletes a token. It stops working at once.
func (s *Service) RevokeToken(ctx context.Context, id inbox.TokenID) error {
	return s.Store.DeleteToken(ctx, id)
}

// tokenViews names each token's inboxes by slug. The tokens were read a moment
// ago, so an inbox may have been deleted since: it is left out, and a token that
// read only inboxes that are gone is left out with them (deleting the inbox revokes it),
// rather than the whole listing failing with "not found" for something the caller
// did not ask about.
func (s *Service) tokenViews(ctx context.Context, tokens []*inbox.Token) ([]TokenView, error) {
	slugs := map[inbox.ID]string{}
	gone := map[inbox.ID]bool{}
	out := make([]TokenView, 0, len(tokens))
	for _, t := range tokens {
		v := TokenView{
			ID: int64(t.ID), Name: t.Name, AllInboxes: t.AllInboxes, Inboxes: []string{},
			CreatedAt: t.CreatedAt.UTC().Format(TimeLayout),
		}
		for _, id := range t.InboxIDs {
			if gone[id] {
				continue
			}
			slug, ok := slugs[id]
			if !ok {
				in, err := s.Store.GetInbox(ctx, id)
				if errors.Is(err, inbox.ErrNotFound) {
					gone[id] = true
					continue
				}
				if err != nil {
					return nil, err
				}
				slug = in.Slug
				slugs[id] = slug
			}
			v.Inboxes = append(v.Inboxes, slug)
		}
		if len(t.InboxIDs) > 0 && len(v.Inboxes) == 0 {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}
