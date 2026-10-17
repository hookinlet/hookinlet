// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin

import (
	"bytes"
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
	// Legacy is how many of them are in the original sealed layout, which binds them
	// to nothing: they open for any inbox until Rekey writes them again.
	Legacy int
	// WrongKey lists the inboxes whose secret was sealed under a different master
	// key, each by its slug, followed by its tenant for an inbox not in the default
	// tenant ("alerts (tenant acme)"). NoKey is true when there are secrets but no key at all.
	WrongKey []string
	NoKey    bool
	// Damaged lists the inboxes whose secret does not open under this key though it
	// does not name another: it was changed, was sealed for another inbox, or is in
	// the original layout, which names no key, so a wrong key looks like damage.
	// Inboxes are named as in WrongKey.
	Damaged []string
	// Tenants breaks the secrets checked down by tenant (Inbox.Tenant), in the order
	// each tenant was first met. A slug names an inbox only within its tenant.
	Tenants []TenantSecrets
}

// TenantSecrets is what CheckSecrets found for one tenant's inboxes.
type TenantSecrets struct {
	// Tenant is the tenant key, empty for the default tenant.
	Tenant string
	// Checked is how many of the tenant's secrets were opened, and Unreadable the
	// slugs of those that did not open (with the wrong key, damaged, or no key).
	Checked    int
	Unreadable []string
}

// OK reports whether every secret can be opened.
func (r SecretReport) OK() bool { return !r.NoKey && len(r.WrongKey) == 0 && len(r.Damaged) == 0 }

// Unreadable counts the secrets that cannot be opened.
func (r SecretReport) Unreadable() int { return len(r.WrongKey) + len(r.Damaged) }

// Systemic reports whether the master key itself looks wrong, not one inbox: there are
// secrets and none of them opens (or there is no key at all). One secret that does not
// open among others that do is that inbox's problem alone.
func (r SecretReport) Systemic() bool {
	return r.NoKey || (r.Checked > 0 && r.Unreadable() == r.Checked)
}

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
		parts = append(parts, fmt.Sprintf("%d inbox(es) have a secret that does not open (%s): it is damaged, or was sealed by a release before 0.2.0, which bound secrets to the inbox's slug and not its UID (set it again)", len(r.Damaged), sample(r.Damaged)))
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
	return s.checkSecrets(ctx, SampleBounds{})
}

// SampleBounds bounds what SampleSecrets looks at. Zero means no bound.
type SampleBounds struct {
	// Secrets is how many secrets it opens at most, and PerTenant how many of one
	// tenant's it opens before it opens more of any one (it applies only when Secrets
	// is set).
	Secrets, PerTenant int
	// Inboxes is how many inboxes it looks at at most. Once it holds Secrets secrets
	// (opened, or passed over for their tenant's share), it looks at no more than
	// AfterFull further inboxes for another tenant's.
	Inboxes, AfterFull int
}

// SampleSecrets is CheckSecrets for a question asked often: it opens at most
// b.Secrets secrets and looks at no more than b.Inboxes inboxes (the oldest first),
// so its cost does not grow with the size of the store. A master key that opens
// nothing shows in any sample of secrets; one inbox whose secret is damaged may not,
// and CheckSecrets is what finds it.
//
// The sample is spread over the tenants: it first takes at most b.PerTenant secrets
// of each tenant, the oldest of each, in the order the tenants' first inboxes come;
// if that leaves room, it fills the rest with the oldest secrets it passed over. So a
// tenant whose secrets do not open (its key is gone from a key service, say) cannot
// fill the sample by itself while another tenant has secrets among the inboxes
// looked at. Once it has enough secrets to fill the sample it looks a little further
// (b.AfterFull inboxes) for other tenants, and no further, so that a store of one
// tenant is not read to the Inboxes bound on every check.
func (s *Service) SampleSecrets(ctx context.Context, b SampleBounds) (SecretReport, error) {
	return s.checkSecrets(ctx, b)
}

// checkSecrets is CheckSecrets with bounds.
func (s *Service) checkSecrets(ctx context.Context, b SampleBounds) (SecretReport, error) {
	var r SecretReport
	perTenant := b.PerTenant
	if b.Secrets <= 0 {
		perTenant = 0 // everything is opened, so there is no share to keep
	}
	tenants := map[string]int{} // tenant to its index in r.Tenants
	full := func() bool { return b.Secrets > 0 && r.Checked >= b.Secrets }
	var spare []*inbox.Inbox // passed over for its tenant's share, oldest first; at most b.Secrets
	heldAt := -1             // how many inboxes had been looked at when enough secrets were held
	var after inbox.ID
	scanned := 0
	left := func() int { // how many more inboxes may be looked at, or -1 for no bound
		n := -1
		if b.Inboxes > 0 {
			n = b.Inboxes - scanned
		}
		if heldAt >= 0 && b.AfterFull > 0 && (n < 0 || heldAt+b.AfterFull-scanned < n) {
			n = heldAt + b.AfterFull - scanned
		}
		return n
	}
scan:
	for {
		// A page never takes more than the sample may look at, and while the sample is
		// not yet held no more than AfterFull: the page in which it comes to be held then
		// ends within AfterFull inboxes of that point.
		limit := inbox.MaxListLimit
		if heldAt < 0 && b.AfterFull > 0 && b.AfterFull < limit {
			limit = b.AfterFull
		}
		if n := left(); n >= 0 && n < limit {
			limit = n
		}
		if limit <= 0 {
			break
		}
		page, err := s.Store.ListInboxes(ctx, inbox.ListParams{AfterID: after, Limit: limit})
		if err != nil {
			return r, err
		}
		for _, in := range page {
			if full() {
				break scan
			}
			scanned++
			after = in.ID
			if len(in.VerifierSecret) == 0 {
				continue
			}
			if i, ok := tenants[in.Tenant]; ok && perTenant > 0 && r.Tenants[i].Checked >= perTenant {
				if len(spare) < b.Secrets {
					spare = append(spare, in)
				}
			} else if err := s.openSecret(ctx, &r, tenants, in); err != nil {
				return r, err
			}
			if heldAt < 0 && b.Secrets > 0 && r.Checked+len(spare) >= b.Secrets {
				heldAt = scanned
			}
		}
		if len(page) < limit || full() || left() == 0 {
			break
		}
	}
	for _, in := range spare {
		if full() {
			break
		}
		if err := s.openSecret(ctx, &r, tenants, in); err != nil {
			return r, err
		}
	}
	return r, nil
}

// inboxName names an inbox in a report: its slug, which is unique only within its
// tenant, and the tenant unless it is the default one.
func inboxName(in *inbox.Inbox) string {
	if in.Tenant == "" {
		return in.Slug
	}
	return fmt.Sprintf("%s (tenant %s)", in.Slug, in.Tenant)
}

// openSecret opens one inbox's verifier secret and counts what it found in r. It
// returns an error only when ctx ended while the secret was being opened: the check
// did not find out whether it opens, and does not count it as unreadable. The box is
// given ctx scoped to the inbox's tenant; the Store calls of the check are not.
func (s *Service) openSecret(ctx context.Context, r *SecretReport, tenants map[string]int, in *inbox.Inbox) error {
	i, ok := tenants[in.Tenant]
	if !ok {
		i = len(r.Tenants)
		tenants[in.Tenant] = i
		r.Tenants = append(r.Tenants, TenantSecrets{Tenant: in.Tenant})
	}
	t := &r.Tenants[i]
	if s.Box == nil {
		r.Checked++
		t.Checked++
		r.NoKey = true
		t.Unreadable = append(t.Unreadable, in.Slug)
		return nil
	}
	pt, err := inbox.OpenWith(inbox.WithTenant(ctx, in.Tenant), s.Box, in.VerifierSecret, inbox.VerifierSecretContext(in.UID))
	clear(pt)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("checking the secret of inbox %q: %w", in.Slug, err)
	}
	r.Checked++
	t.Checked++
	if secretbox.Legacy(in.VerifierSecret) {
		r.Legacy++
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, secretbox.ErrWrongKey):
		r.WrongKey = append(r.WrongKey, inboxName(in))
	default:
		r.Damaged = append(r.Damaged, inboxName(in))
	}
	t.Unreadable = append(t.Unreadable, in.Slug)
	return nil
}

// RekeyResult is what Rekey did.
type RekeyResult struct {
	// Resealed is how many secrets were written again, Unchanged how many already
	// were in the current layout under the current key.
	Resealed, Unchanged int
	// Skipped is how many were left alone because the inbox was changed (its secret
	// set again, its verifier changed, or it was deleted and made again) after Rekey
	// planned them. Run it again to see to those.
	Skipped int
}

// Rekey seals every stored verifier secret again under s.Box, opening each with
// old, or with s.Box when it is already sealed under that. Run it after changing
// the master key, to move the secrets to the new one; or with old the same as
// s.Box, to bring secrets written by an earlier release up to the current layout.
//
// Every secret is opened with one of the two keys before any is written, so a
// wrong "old" key changes nothing. After that the secrets are written one at a time,
// and one that was changed since it was read is left alone (Skipped). A run that
// fails or is interrupted part way can be run again: what was done is not done twice.
func (s *Service) Rekey(ctx context.Context, old inbox.SecretBox) (RekeyResult, error) {
	var res RekeyResult
	if s.Box == nil || old == nil {
		return res, ErrNoSecretBox
	}
	type work struct {
		id       inbox.ID // not the slug: another tenant's inbox can have the same one
		slug     string
		original []byte // what was read, which the write must still find
		sealed   []byte
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
			ctxb := inbox.VerifierSecretContext(in.UID)
			bctx := inbox.WithTenant(ctx, in.Tenant) // for the box only: the Store calls stay unscoped
			if pt, err := inbox.OpenWith(bctx, s.Box, in.VerifierSecret, ctxb); err == nil {
				clear(pt)
				if !secretbox.Legacy(in.VerifierSecret) {
					res.Unchanged++
					continue
				}
			}
			pt, err := inbox.OpenWith(bctx, old, in.VerifierSecret, ctxb)
			if err != nil {
				if pt2, err2 := inbox.OpenWith(bctx, s.Box, in.VerifierSecret, ctxb); err2 == nil { // a legacy value under the current key
					pt, err = pt2, nil
				}
			}
			if err != nil && ctx.Err() != nil {
				return res, fmt.Errorf("opening the secret of inbox %q: %w; nothing was changed", in.Slug, err)
			}
			if err != nil {
				return res, fmt.Errorf("the secret of inbox %q opens with neither the old nor the new master key; nothing was changed", in.Slug)
			}
			sealed, err := inbox.SealWith(bctx, s.Box, pt, ctxb)
			clear(pt)
			if err != nil {
				return res, err
			}
			todo = append(todo, work{in.ID, in.Slug, bytes.Clone(in.VerifierSecret), sealed})
		}
		if len(page) < inbox.MaxListLimit {
			break
		}
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	for _, w := range todo {
		for attempt := 0; ; attempt++ {
			in, err := s.Store.GetInbox(ctx, w.id)
			if errors.Is(err, inbox.ErrNotFound) {
				break // deleted since
			}
			if err != nil {
				return res, err
			}
			if !bytes.Equal(in.VerifierSecret, w.original) {
				res.Skipped++ // changed since it was read: not ours to overwrite
				break
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
