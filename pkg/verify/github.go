// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// github verifies GitHub webhooks: `X-Hub-Signature-256: sha256=<hex>`, the
// HMAC-SHA256 of the raw body keyed with the webhook secret.
//
// GitHub signs only the body. X-GitHub-Delivery and X-GitHub-Event are not
// covered by the signature, so they are metadata, not proof: someone holding
// one captured, validly signed body can replay it under a new delivery id.
// GitHub provides no timestamp that would let a receiver close that gap.
type github struct{}

func newGitHub(config json.RawMessage) (Verifier, error) {
	var c struct{}
	if err := decodeConfig(config, &c); err != nil {
		return nil, err
	}
	return github{}, nil
}

func (github) Verify(r *http.Request, body, secret []byte, _ time.Time) (Result, error) {
	if err := requireSecret(secret); err != nil {
		return Result{}, err
	}
	header, err := singleHeader(r, "X-Hub-Signature-256")
	if err != nil {
		return Result{}, err
	}
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return Result{}, fail("malformed signature header")
	}
	sig, err := hex.DecodeString(hexSig)
	if err != nil || len(sig) != sha256.Size {
		return Result{}, fail("malformed signature header")
	}
	if !anyEqual(mac(sha256.New, secret, body), [][]byte{sig}) {
		return Result{}, fail("signature mismatch")
	}
	return Result{
		DeliveryID: cleanID(r.Header.Get("X-GitHub-Delivery"), maxIDLen),
		EventType:  cleanID(r.Header.Get("X-GitHub-Event"), maxIDLen),
	}, nil
}
