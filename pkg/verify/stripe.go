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

const (
	stripeHeader = "Stripe-Signature"
	// maxSignatureParts bounds how many comma-separated parts of a signature
	// header are examined.
	maxSignatureParts = 50
)

// stripe verifies Stripe webhooks: `Stripe-Signature: t=<unix>,v1=<hex>[,v1=...]`,
// where v1 is HMAC-SHA256 over "<t>.<body>" keyed with the endpoint's signing
// secret (used as the literal string, including its whsec_ prefix). Any one
// matching v1 is enough, which allows Stripe's secret rotation.
type stripe struct{ tolerance time.Duration }

func newStripe(config json.RawMessage) (Verifier, error) {
	var c struct {
		ToleranceSeconds int `json:"tolerance_seconds"`
	}
	if err := decodeConfig(config, &c); err != nil {
		return nil, err
	}
	tol, err := toleranceFrom(c.ToleranceSeconds)
	if err != nil {
		return nil, err
	}
	return stripe{tolerance: tol}, nil
}

func (s stripe) Verify(r *http.Request, body, secret []byte, now time.Time) (Result, error) {
	if err := requireSecret(secret); err != nil {
		return Result{}, err
	}
	header, err := singleHeader(r, stripeHeader)
	if err != nil {
		return Result{}, err
	}
	tsStr, sigs, err := parseStripeHeader(header)
	if err != nil {
		return Result{}, err
	}
	ts, _ := parseUnixSeconds(tsStr) // parseStripeHeader already checked it

	want := mac(sha256.New, secret, []byte(tsStr), []byte("."), body)
	if !anyEqual(want, sigs) {
		return Result{}, fail(FailureMismatch, "signature mismatch")
	}
	// The signature is checked first so a forged request learns nothing about
	// the tolerance window.
	if !withinTolerance(ts, now, s.tolerance) {
		return Result{}, fail(FailureStale, "timestamp outside tolerance")
	}

	f := topLevelStrings(body, "id", "type")
	id := cleanID(f["id"], maxIDLen)
	return Result{DeliveryID: id, IDSigned: id != "", EventType: cleanID(f["type"], maxIDLen)}, nil
}

// parseStripeHeader extracts the timestamp and the decodable v1 signatures.
// Unknown schemes (such as v0) and undecodable v1 values are ignored.
func parseStripeHeader(h string) (ts string, sigs [][]byte, err error) {
	parts := strings.Split(h, ",")
	if len(parts) > maxSignatureParts {
		return "", nil, fail(FailureMalformed, "malformed signature header")
	}
	for _, part := range parts {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			if ts != "" {
				return "", nil, fail(FailureMalformed, "malformed signature header")
			}
			ts = v
		case "v1":
			if sig, derr := hex.DecodeString(v); derr == nil && len(sig) == sha256.Size {
				sigs = append(sigs, sig)
			}
		}
	}
	if _, ok := parseUnixSeconds(ts); !ok || len(sigs) == 0 {
		return "", nil, fail(FailureMalformed, "malformed signature header")
	}
	return ts, sigs, nil
}
