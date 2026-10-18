// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/verify"
)

func sha256Hex(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// Each failure path of each built-in verifier reports the kind that describes it.
func TestFailureKinds(t *testing.T) {
	const ts = 1700000000
	now := unix(ts + 5)
	late := unix(ts + 100000)
	body := `{"a":1}`
	tsStr := fmt.Sprint(ts)

	hmacCfg := `{"header":"X-Sig","timestamp_header":"X-Ts"}`
	hmacSig := sha256Hex("k", tsStr+"."+body)
	slackSig := "v0=" + sha256Hex("k", "v0:"+tsStr+":"+body)
	stripeSig := sha256Hex("k", tsStr+"."+body)

	cases := []struct {
		name     string
		verifier string
		config   string
		headers  hdr
		now      time.Time
		want     verify.FailureKind
	}{
		{"hmac: no signature header", "hmac", hmacCfg, h("X-Ts", tsStr), now, verify.FailureMissing},
		{"hmac: no timestamp header", "hmac", hmacCfg, h("X-Sig", hmacSig), now, verify.FailureMissing},
		{"hmac: signature not hex", "hmac", hmacCfg, h("X-Sig", "zz", "X-Ts", tsStr), now, verify.FailureMalformed},
		{"hmac: timestamp not a number", "hmac", hmacCfg, h("X-Sig", hmacSig, "X-Ts", "soon"), now, verify.FailureMalformed},
		{"hmac: header sent twice", "hmac", hmacCfg, h("X-Sig", hmacSig, "X-Sig", hmacSig, "X-Ts", tsStr), now, verify.FailureMalformed},
		{"hmac: wrong signature", "hmac", hmacCfg, h("X-Sig", sha256Hex("other", tsStr+"."+body), "X-Ts", tsStr), now, verify.FailureMismatch},
		{"hmac: right signature, old timestamp", "hmac", hmacCfg, h("X-Sig", hmacSig, "X-Ts", tsStr), late, verify.FailureStale},
		{"hmac: wrong signature and old timestamp is a mismatch", "hmac", hmacCfg, h("X-Sig", sha256Hex("other", tsStr+"."+body), "X-Ts", tsStr), late, verify.FailureMismatch},

		{"github: no header", "github", "", h(), now, verify.FailureMissing},
		{"github: no prefix", "github", "", h("X-Hub-Signature-256", sha256Hex("k", body)), now, verify.FailureMalformed},
		{"github: wrong signature", "github", "", h("X-Hub-Signature-256", "sha256="+sha256Hex("other", body)), now, verify.FailureMismatch},

		{"slack: no timestamp", "slack", "", h("X-Slack-Signature", slackSig), now, verify.FailureMissing},
		{"slack: bad timestamp", "slack", "", h("X-Slack-Signature", slackSig, "X-Slack-Request-Timestamp", "x"), now, verify.FailureMalformed},
		{"slack: bad signature", "slack", "", h("X-Slack-Signature", "v0=zz", "X-Slack-Request-Timestamp", tsStr), now, verify.FailureMalformed},
		{"slack: wrong signature", "slack", "", h("X-Slack-Signature", "v0="+sha256Hex("other", "v0:"+tsStr+":"+body), "X-Slack-Request-Timestamp", tsStr), now, verify.FailureMismatch},
		{"slack: old timestamp", "slack", "", h("X-Slack-Signature", slackSig, "X-Slack-Request-Timestamp", tsStr), late, verify.FailureStale},

		{"stripe: no header", "stripe", "", h(), now, verify.FailureMissing},
		{"stripe: no t=", "stripe", "", h("Stripe-Signature", "v1="+stripeSig), now, verify.FailureMalformed},
		{"stripe: wrong signature", "stripe", "", h("Stripe-Signature", "t="+tsStr+",v1="+sha256Hex("other", tsStr+"."+body)), now, verify.FailureMismatch},
		{"stripe: old timestamp", "stripe", "", h("Stripe-Signature", "t="+tsStr+",v1="+stripeSig), late, verify.FailureStale},

		{"token: no header", "token", "", h(), now, verify.FailureMissing},
		{"token: header sent twice", "token", "", h("X-Inbox-Token", "k", "X-Inbox-Token", "k"), now, verify.FailureMalformed},
		{"token: wrong token", "token", "", h("X-Inbox-Token", "nope"), now, verify.FailureMismatch},

		{"standardwebhooks: no id", "standardwebhooks", "", h("Webhook-Timestamp", tsStr, "Webhook-Signature", "v1,AAAA"), now, verify.FailureMissing},
		{"standardwebhooks: bad timestamp", "standardwebhooks", "", h("Webhook-Id", "m1", "Webhook-Timestamp", "x", "Webhook-Signature", "v1,AAAA"), now, verify.FailureMalformed},
	}
	reg := verify.Default()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The whsec_ secret is only needed by standardwebhooks, which fails
			// on the headers before it uses one.
			secret := "k"
			if c.verifier == "standardwebhooks" {
				secret = "whsec_" + "dGVzdC1zZWNyZXQ="
			}
			v, err := reg.New(c.verifier, []byte(c.config))
			if err != nil {
				t.Fatal(err)
			}
			_, err = v.Verify(request(t, c.headers), []byte(body), []byte(secret), c.now)
			if _, ok := verify.IsFailure(err); !ok {
				t.Fatalf("error = %v, want a verification failure", err)
			}
			if got := verify.FailureKindOf(err); got != c.want {
				t.Errorf("kind = %q, want %q (%v)", got, c.want, err)
			}
		})
	}
}

// Only a vouched-for vocabulary exists, and a failure that names no kind, or an
// error that is not a failure, has none.
func TestFailureKindOf(t *testing.T) {
	if got := verify.FailureKindOf(&verify.Failure{Reason: "x"}); got != "" {
		t.Errorf("an unclassified failure has kind %q", got)
	}
	if got := verify.FailureKindOf(fmt.Errorf("wrapped: %w", &verify.Failure{Reason: "x", Kind: verify.FailureStale})); got != verify.FailureStale {
		t.Errorf("a wrapped failure has kind %q", got)
	}
	if got := verify.FailureKindOf(errors.New("plain")); got != "" {
		t.Errorf("a plain error has kind %q", got)
	}
	if got := verify.FailureKindOf(nil); got != "" {
		t.Errorf("nil has kind %q", got)
	}
}

// Only the generic hmac verifier names a signature header, because only its
// header is per inbox; the names are canonical and are not values.
func TestSignatureHeaders(t *testing.T) {
	reg := verify.Default()
	for _, c := range []struct {
		verifier, config string
		want             []string
	}{
		{"hmac", `{"header":"x-my-sig"}`, []string{"X-My-Sig"}},
		{"hmac", `{"header":"X-Sig","timestamp_header":"X-Ts","delivery_id_header":"X-Id"}`, []string{"X-Sig"}},
		{"stripe", "", nil},
		{"github", "", nil},
		{"slack", "", nil},
		{"standardwebhooks", "", nil},
		{"token", "", nil},
		{"none", "", nil},
	} {
		v, err := reg.New(c.verifier, []byte(c.config))
		if err != nil {
			t.Fatal(err)
		}
		sh, ok := v.(verify.SignatureHeaderer)
		if (c.want != nil) != ok {
			t.Errorf("%s: implements SignatureHeaderer = %v, want %v", c.verifier, ok, c.want != nil)
			continue
		}
		if ok {
			got := sh.SignatureHeaders()
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("%s: SignatureHeaders = %v, want %v", c.verifier, got, c.want)
			}
		}
	}
}
