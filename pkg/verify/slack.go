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

// maxChallengeLen bounds the handshake challenge that is echoed back. Slack's
// are a few dozen characters.
const maxChallengeLen = 256

// slack verifies Slack requests: `X-Slack-Signature: v0=<hex>`, the
// HMAC-SHA256 of "v0:<X-Slack-Request-Timestamp>:<body>" keyed with the app's
// signing secret. Both the timestamp and the body are signed.
//
// After the signature checks out, a url_verification handshake is answered by
// returning its challenge, and the delivery is not stored.
type slack struct{ tolerance time.Duration }

func newSlack(config json.RawMessage) (Verifier, error) {
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
	return slack{tolerance: tol}, nil
}

func (s slack) Verify(r *http.Request, body, secret []byte, now time.Time) (Result, error) {
	if err := requireSecret(secret); err != nil {
		return Result{}, err
	}
	tsStr, err := singleHeader(r, "X-Slack-Request-Timestamp")
	if err != nil {
		return Result{}, err
	}
	ts, ok := parseUnixSeconds(tsStr)
	if !ok {
		return Result{}, fail("malformed timestamp header")
	}
	header, err := singleHeader(r, "X-Slack-Signature")
	if err != nil {
		return Result{}, err
	}
	hexSig, ok := strings.CutPrefix(header, "v0=")
	if !ok {
		return Result{}, fail("malformed signature header")
	}
	sig, err := hex.DecodeString(hexSig)
	if err != nil || len(sig) != sha256.Size {
		return Result{}, fail("malformed signature header")
	}

	want := mac(sha256.New, secret, []byte("v0:"), []byte(tsStr), []byte(":"), body)
	if !anyEqual(want, [][]byte{sig}) {
		return Result{}, fail("signature mismatch")
	}
	if !withinTolerance(ts, now, s.tolerance) {
		return Result{}, fail("timestamp outside tolerance")
	}

	res := Result{}
	var payload struct {
		Type      string          `json:"type"`
		Challenge string          `json:"challenge"`
		EventID   string          `json:"event_id"`
		Event     json.RawMessage `json:"event"`
	}
	// Slash commands and interactive payloads are form-encoded, not JSON; they
	// simply have no event type or id here.
	if json.Unmarshal(body, &payload) == nil {
		switch payload.Type {
		case "url_verification":
			// The challenge is echoed to the sender, so an oversized one is
			// ignored and the delivery is treated as an ordinary message.
			if len(payload.Challenge) <= maxChallengeLen {
				res.Challenge = []byte(payload.Challenge)
			} else {
				res.EventType = payload.Type
			}
		case "event_callback":
			var ev struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(payload.Event, &ev)
			res.EventType = cleanID(ev.Type, maxIDLen)
			res.DeliveryID = cleanID(payload.EventID, maxIDLen)
		default:
			res.EventType = cleanID(payload.Type, maxIDLen)
		}
	}
	return res, nil
}
