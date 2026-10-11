// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin

import (
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// CheckSettings reports whether p would be accepted by Create, without a Store,
// a master key, or a secret: the same field rules (inbox.Inbox.Validate) and,
// for a built-in verifier, the same check of its configuration. It lets a
// caller that is about to make several changes refuse a bad request before it
// makes any.
//
// A verifier it does not know is not an error here: an embedder may have
// registered it in the server that will do the work, and that server decides.
// p.VerifierSecret is ignored.
func CheckSettings(p CreateParams) error {
	if p.Verifier == "" {
		p.Verifier = "none"
	}
	if p.Name == "" {
		p.Name = p.Slug
	}
	if p.IngestKey == "" {
		p.IngestKey = "placeholder-key" // the key is made when the inbox is, and checked then
	}
	in := &inbox.Inbox{
		Slug: p.Slug, IngestKey: p.IngestKey, Name: p.Name, Verifier: p.Verifier, VerifierConfig: p.VerifierConfig,
		OnVerifyFail: p.OnVerifyFail, AgentTokenHash: inbox.SecretBytes{1},
		MaxBodyBytes: p.MaxBodyBytes, RetentionDays: p.RetentionDays, RetentionMaxCount: p.RetentionMaxCount,
		EventTypePointer: p.EventTypePointer,
	}
	in.ApplyDefaults()
	if err := in.Validate(); err != nil {
		return err
	}
	if inbox.KnownVerifier(p.Verifier) {
		return new(Service).checkVerifier(p.Verifier, in.VerifierConfig)
	}
	return nil
}
