// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/verify"
)

// A template that puts {body} next to {timestamp} lets an attacker move
// characters between the two without changing the signed bytes. With a
// lenient timestamp parser (leading zeros allowed) this forged a body: a
// capture of body "amount=100" signed at T could be resent as "amount=1" with
// the header "00T", and the signed bytes "amount=100"+"T" were identical.
func TestGenericHMACRejectsAmbiguousTemplates(t *testing.T) {
	reg := verify.Default()
	cfg := func(tmpl string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"header": "X-Sig", "timestamp_header": "X-Ts", "payload_template": tmpl})
		return b
	}
	for _, bad := range []string{
		"{body}{timestamp}", "{timestamp}{body}", "{timestamp}123{body}", "{body}000{timestamp}",
		"{body}{timestamp}{timestamp}", "a{body}{timestamp}b",
	} {
		if _, err := reg.New("hmac", cfg(bad)); !errors.Is(err, verify.ErrConfig) {
			t.Errorf("template %q was accepted; want a configuration error", bad)
		}
	}
	for _, good := range []string{
		"{timestamp}.{body}", "{body}.{timestamp}", "{timestamp}:{body}", "v1={timestamp}&{body}", "{timestamp}x{body}", "{timestamp}\n{body}",
	} {
		if _, err := reg.New("hmac", cfg(good)); err != nil {
			t.Errorf("template %q rejected: %v", good, err)
		}
	}
}

// A timestamp header that the signed content does not include would be checked
// against the clock but never authenticated, silently turning replay protection
// into theatre: an old signed body plus a fresh timestamp header would pass.
func TestGenericHMACTimestampMustBeSigned(t *testing.T) {
	reg := verify.Default()
	for name, cfg := range map[string]string{
		"explicit template without a timestamp": `{"header":"X-Sig","timestamp_header":"X-Ts","payload_template":"{body}"}`,
		"literal text only around the body":     `{"header":"X-Sig","timestamp_header":"X-Ts","payload_template":"v0:{body}"}`,
	} {
		if _, err := reg.New("hmac", json.RawMessage(cfg)); !errors.Is(err, verify.ErrConfig) {
			t.Errorf("%s: accepted, want a configuration error", name)
		}
	}
}

func TestStandardWebhooksRejectsIDsThatCouldBlurTheSignedMessage(t *testing.T) {
	// The signed text is "<id>.<timestamp>.<body>", so a "." inside the id lets
	// the same bytes be read as a different id/timestamp/body split.
	now := unix(1614265330 + 3)
	runCases(t, "standardwebhooks", []tc{
		{"id containing a dot", "", swSecret, h("webhook-id", "msg.1614265330", "webhook-timestamp", swTS, "webhook-signature", swSig), swBody, now, failsWith("malformed webhook-id")},
	})
}

func TestSignaturesHaveExactlyOneEncoding(t *testing.T) {
	// One byte string must not be writable as several different signatures.
	now := unix(1614265330 + 3)
	nonCanonical := strings.TrimSuffix(swSig, "E=") + "F=" // same bytes, different trailing bits
	runCases(t, "standardwebhooks", []tc{
		{"canonical", "", swSecret, swHeaders(swSig), swBody, now, ok(swID, "")},
		{"non-canonical trailing bits", "", swSecret, swHeaders(nonCanonical), swBody, now, failsWith("malformed signature")},
		{"extra padding", "", swSecret, swHeaders(swSig + "="), swBody, now, failsWith("malformed signature")},
		{"missing padding", "", swSecret, swHeaders(strings.TrimSuffix(swSig, "=")), swBody, now, failsWith("malformed signature")},
	})

	const k3sig = "vWA86sbF9S923BI9e//rysHry2/mxrXoKLqcuy1fKm0="
	cfg := `{"header":"X-Sig","encoding":"base64","timestamp_header":"X-Ts"}`
	at := unix(1700000005)
	runCases(t, "hmac", []tc{
		{"generic base64, canonical", cfg, "k3", h("X-Sig", k3sig, "X-Ts", "1700000000"), "payload-3", at, ok("", "")},
		{"generic base64, unpadded is still the same encoding", cfg, "k3", h("X-Sig", strings.TrimSuffix(k3sig, "="), "X-Ts", "1700000000"), "payload-3", at, ok("", "")},
		{"generic base64, non-canonical trailing bits", cfg, "k3", h("X-Sig", strings.TrimSuffix(k3sig, "0=")+"1=", "X-Ts", "1700000000"), "payload-3", at, failsWith("malformed signature")},
		{"generic base64, too much padding", cfg, "k3", h("X-Sig", k3sig+"==", "X-Ts", "1700000000"), "payload-3", at, failsWith("malformed signature")},
	})
}

func TestSlackChallengeIsBounded(t *testing.T) {
	// The challenge is echoed to the sender, so an oversized one is ignored
	// (the delivery is then treated as an ordinary message) instead of reflected.
	long := strings.Repeat("c", 300)
	body := `{"type":"url_verification","challenge":"` + long + `"}`
	sig := slackSign("slack-secret", "1700000000", body)
	runCases(t, "slack", []tc{
		{"an oversized challenge is not returned", "", "slack-secret", h("X-Slack-Request-Timestamp", "1700000000", "X-Slack-Signature", sig), body, unix(1700000005), ok("", "url_verification")},
	})
}
