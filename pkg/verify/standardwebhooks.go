// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const standardWebhooksSecretPrefix = "whsec_"

// standardwebhooks verifies the Standard Webhooks scheme (also used by Svix):
// headers webhook-id, webhook-timestamp, and webhook-signature
// ("v1,<base64> v1,<base64> ..."), where each signature is HMAC-SHA256 over
// "<id>.<timestamp>.<body>" keyed with the base64-decoded secret. The secret is
// written "whsec_<base64>". The id and timestamp are signed, so the id is a
// trustworthy deduplication key.
type standardWebhooks struct{ tolerance time.Duration }

func newStandardWebhooks(config json.RawMessage) (Verifier, error) {
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
	return standardWebhooks{tolerance: tol}, nil
}

func (s standardWebhooks) Verify(r *http.Request, body, secret []byte, now time.Time) (Result, error) {
	if err := requireSecret(secret); err != nil {
		return Result{}, err
	}
	key, err := decodeStandardWebhooksSecret(secret)
	if err != nil {
		return Result{}, err
	}
	defer clear(key)
	id, err := singleHeader(r, "webhook-id")
	if err != nil {
		return Result{}, err
	}
	// The signed text is "<id>.<timestamp>.<body>"; a "." inside the id would let
	// the same bytes be read as a different split.
	if id == "" || len(id) > maxIDLen || strings.Contains(id, ".") {
		return Result{}, fail("malformed webhook-id header")
	}
	tsStr, err := singleHeader(r, "webhook-timestamp")
	if err != nil {
		return Result{}, err
	}
	ts, ok := parseUnixSeconds(tsStr)
	if !ok {
		return Result{}, fail("malformed timestamp header")
	}
	header, err := singleHeader(r, "webhook-signature")
	if err != nil {
		return Result{}, err
	}
	sigs := parseStandardWebhooksSignatures(header)
	if len(sigs) == 0 {
		return Result{}, fail("malformed signature header")
	}

	want := mac(sha256.New, key, []byte(id), []byte("."), []byte(tsStr), []byte("."), body)
	if !anyEqual(want, sigs) {
		return Result{}, fail("signature mismatch")
	}
	if !withinTolerance(ts, now, s.tolerance) {
		return Result{}, fail("timestamp outside tolerance")
	}

	f := topLevelStrings(body, "type")
	return Result{DeliveryID: cleanID(id, maxIDLen), EventType: cleanID(f["type"], maxIDLen)}, nil
}

// decodeStandardWebhooksSecret turns "whsec_<base64>" into the signing key. A
// secret in any other shape is a configuration problem, not a bad delivery.
func decodeStandardWebhooksSecret(secret []byte) ([]byte, error) {
	// bytes.CutPrefix, not a string conversion: a Go string cannot be wiped.
	enc, ok := bytes.CutPrefix(secret, []byte(standardWebhooksSecretPrefix))
	if !ok {
		return nil, configErr("the secret must look like whsec_<base64>")
	}
	key := make([]byte, base64.StdEncoding.DecodedLen(len(enc)))
	n, err := base64.StdEncoding.Decode(key, enc)
	key = key[:n]
	if err != nil || len(key) == 0 {
		clear(key)
		return nil, configErr("the secret must look like whsec_<base64>")
	}
	return key, nil
}

// parseStandardWebhooksSignatures returns the decodable v1 signatures in a
// space-delimited "v1,<base64>" list. Other versions, which the spec reserves
// for asymmetric schemes, are ignored.
func parseStandardWebhooksSignatures(h string) [][]byte {
	var sigs [][]byte
	fields := strings.Fields(h)
	if len(fields) > maxSignatureParts {
		return nil
	}
	for _, f := range fields {
		version, enc, ok := strings.Cut(f, ",")
		if !ok || version != "v1" {
			continue
		}
		if sig, err := decodeBase64(base64.StdEncoding, enc); err == nil && len(sig) == sha256.Size && len(enc)%4 == 0 {
			sigs = append(sigs, sig)
		}
	}
	return sigs
}
