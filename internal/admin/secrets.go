// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

// ErrUnreadableSecrets means a stored verifier secret cannot be opened with the
// master key the server was given. Deliveries to those inboxes would all fail, and
// it usually means the key is not the one the data was written under.
var ErrUnreadableSecrets = errors.New("stored verifier secrets cannot be opened")

// SecretReport is what CheckSecrets found.
type SecretReport struct {
	// Checked is how many inboxes have a verifier secret.
	Checked int
	// WrongKey lists the inboxes whose secret was sealed under a different master
	// key. NoKey is true when there are secrets but no key at all.
	WrongKey []string
	NoKey    bool
	// Damaged lists the inboxes whose secret does not open under this key though it
	// does not name another: it was changed, was sealed for another inbox, or is in
	// the original layout, which names no key, so a wrong key looks like damage.
	Damaged []string
}

// OK reports whether every secret can be opened.
func (r SecretReport) OK() bool { return !r.NoKey && len(r.WrongKey) == 0 && len(r.Damaged) == 0 }

// Err describes what is wrong, or is nil. It lists at most a few slugs.
func (r SecretReport) Err() error {
	if r.OK() {
		return nil
	}
	var parts []string
	if r.NoKey {
		parts = append(parts, fmt.Sprintf("%d inbox(es) have a verifier secret but no master key is configured", r.Checked))
	}
	if len(r.WrongKey) > 0 {
		parts = append(parts, fmt.Sprintf("%d inbox(es) have a secret sealed under a different master key (%s)", len(r.WrongKey), sample(r.WrongKey)))
	}
	if len(r.Damaged) > 0 {
		parts = append(parts, fmt.Sprintf("%d inbox(es) have a secret that does not open (%s): it is damaged, or was sealed by an older release under a different master key", len(r.Damaged), sample(r.Damaged)))
	}
	return fmt.Errorf("%w: %s", ErrUnreadableSecrets, strings.Join(parts, "; "))
}

func sample(slugs []string) string {
	if len(slugs) <= 5 {
		return strings.Join(slugs, ", ")
	}
	return strings.Join(slugs[:5], ", ") + ", ..."
}

// CheckSecrets opens every stored verifier secret, to find out before a delivery
// does whether the master key is the one they were sealed under.
func (s *Service) CheckSecrets(ctx context.Context) (SecretReport, error) {
	var r SecretReport
	var after inbox.ID
	for {
		page, err := s.Store.ListInboxes(ctx, inbox.ListParams{AfterID: after, Limit: inbox.MaxListLimit})
		if err != nil {
			return r, err
		}
		for _, in := range page {
			after = in.ID
			if len(in.VerifierSecret) == 0 {
				continue
			}
			r.Checked++
			if s.Box == nil {
				r.NoKey = true
				continue
			}
			pt, err := s.Box.Open(in.VerifierSecret, []byte(in.Slug))
			clear(pt)
			switch {
			case err == nil:
			case errors.Is(err, secretbox.ErrWrongKey):
				r.WrongKey = append(r.WrongKey, in.Slug)
			default:
				r.Damaged = append(r.Damaged, in.Slug)
			}
		}
		if len(page) < inbox.MaxListLimit {
			return r, nil
		}
	}
}

// RekeyResult is what Rekey did.
type RekeyResult struct {
	// Resealed is how many secrets were written again, Unchanged how many already
	// were in the current layout under the current key.
	Resealed, Unchanged int
}

// Rekey seals every stored verifier secret again under s.Box, opening each with
// old, or with s.Box when it is already sealed under that. Run it after changing
// the master key, to move the secrets to the new one; or with old the same as
// s.Box, to bring secrets written by an earlier release up to the current layout.
//
// Nothing is written unless every secret can be opened with one of the two, so a
// wrong "old" key changes nothing. A run that is interrupted can be run again: what
// was done is not done twice.
func (s *Service) Rekey(ctx context.Context, old inbox.SecretBox) (RekeyResult, error) {
	var res RekeyResult
	if s.Box == nil || old == nil {
		return res, ErrNoSecretBox
	}
	type work struct {
		slug   string
		sealed []byte
	}
	var todo []work
	var after inbox.ID
	for {
		page, err := s.Store.ListInboxes(ctx, inbox.ListParams{AfterID: after, Limit: inbox.MaxListLimit})
		if err != nil {
			return res, err
		}
		for _, in := range page {
			after = in.ID
			if len(in.VerifierSecret) == 0 {
				continue
			}
			ctxb := []byte(in.Slug)
			if pt, err := s.Box.Open(in.VerifierSecret, ctxb); err == nil {
				clear(pt)
				if !secretbox.Legacy(in.VerifierSecret) {
					res.Unchanged++
					continue
				}
			}
			pt, err := old.Open(in.VerifierSecret, ctxb)
			if err != nil {
				if pt2, err2 := s.Box.Open(in.VerifierSecret, ctxb); err2 == nil { // a legacy value under the current key
					pt, err = pt2, nil
				}
			}
			if err != nil {
				return res, fmt.Errorf("the secret of inbox %q opens with neither the old nor the new master key; nothing was changed", in.Slug)
			}
			sealed, err := s.Box.Seal(pt, ctxb)
			clear(pt)
			if err != nil {
				return res, err
			}
			todo = append(todo, work{in.Slug, sealed})
		}
		if len(page) < inbox.MaxListLimit {
			break
		}
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	for _, w := range todo {
		for attempt := 0; ; attempt++ {
			in, err := s.Store.GetInboxBySlug(ctx, w.slug)
			if errors.Is(err, inbox.ErrNotFound) {
				break // deleted since
			}
			if err != nil {
				return res, err
			}
			in.VerifierSecret = w.sealed
			err = s.Store.UpdateInbox(ctx, in)
			if errors.Is(err, inbox.ErrConflict) && attempt < maxPatchRetries {
				continue
			}
			if err != nil {
				return res, err
			}
			res.Resealed++
			break
		}
	}
	return res, nil
}
