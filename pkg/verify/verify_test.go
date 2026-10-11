// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/verify"
)

// ---- test plumbing ----

// hdr is a request's headers. A header with several values lists them all.
type hdr map[string][]string

func h(kv ...string) hdr {
	out := hdr{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = append(out[kv[i]], kv[i+1])
	}
	return out
}

func request(t *testing.T, headers hdr) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/in/x", nil)
	for k, vs := range headers {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return r
}

// want describes the expected outcome of a Verify call.
type want struct {
	ok         bool   // verification passes
	failure    string // else: a *Failure whose reason contains this
	config     bool   // else: an error wrapping ErrConfig
	deliveryID string
	eventType  string
	challenge  string
}

type tc struct {
	name    string
	config  string // verifier_config JSON; "" means {}
	secret  string
	headers hdr
	body    string
	now     time.Time
	want    want
}

func runCases(t *testing.T, verifierName string, cases []tc) {
	t.Helper()
	reg := verify.Default()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := json.RawMessage(c.config)
			v, err := reg.New(verifierName, cfg)
			if err != nil {
				if c.want.config && errors.Is(err, verify.ErrConfig) {
					return // rejected when built
				}
				t.Fatalf("New: %v", err)
			}
			res, err := v.Verify(request(t, c.headers), []byte(c.body), []byte(c.secret), c.now)
			switch {
			case c.want.ok:
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if res.DeliveryID != c.want.deliveryID || res.EventType != c.want.eventType || string(res.Challenge) != c.want.challenge {
					t.Errorf("result = %+v, want id %q type %q challenge %q", res, c.want.deliveryID, c.want.eventType, c.want.challenge)
				}
				// Only the schemes whose signature covers the id vouch for it.
				signedID := (verifierName == "stripe" || verifierName == "slack" || verifierName == "standardwebhooks") && c.want.deliveryID != ""
				if res.IDSigned != signedID {
					t.Errorf("%s: IDSigned = %v, want %v", verifierName, res.IDSigned, signedID)
				}
				if res.Unauthenticated != (verifierName == "none") {
					t.Errorf("%s: Unauthenticated = %v", verifierName, res.Unauthenticated)
				}
			case c.want.config:
				if !errors.Is(err, verify.ErrConfig) {
					t.Fatalf("error = %v, want ErrConfig", err)
				}
				if _, isFailure := verify.IsFailure(err); isFailure {
					t.Error("a configuration problem must not look like a failed signature")
				}
			default:
				reason, isFailure := verify.IsFailure(err)
				if !isFailure {
					t.Fatalf("error = %v, want a verification failure containing %q", err, c.want.failure)
				}
				if !strings.Contains(reason, c.want.failure) {
					t.Errorf("reason = %q, want it to contain %q", reason, c.want.failure)
				}
				if res.DeliveryID != "" || res.EventType != "" || res.Challenge != nil {
					t.Errorf("a failed verification returned data: %+v", res)
				}
			}
		})
	}
}

func unix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func ok(id, typ string) want       { return want{ok: true, deliveryID: id, eventType: typ} }
func failsWith(reason string) want { return want{failure: reason} }

var configProblem = want{config: true}

// ---- stripe ----

const (
	stripeSecret = "whsec_test_secret" //nolint:gosec // a made-up test value, not a credential
	stripeTS     = 1700000000
	stripeBody   = `{"id":"evt_1NXWPnCo6bFb1KQto6C8OWvE","object":"event","type":"invoice.paid"}`
	// Computed with Python's hmac module, not with this package: Stripe
	// publishes no numeric example, so this is an independent implementation.
	stripeSig        = "c9ac03fdbb0dd036d73f7e9cf0efd78fc260597468ec9622e2c1074888b9fba9"
	stripeRotatedSig = "5a988b4a3fd4b2d820b0c46d18e93f180530e78a4eece1116e1cedace71787b1" // secret "whsec_rotated"
	stripePlainSig   = "bccb5876c56313011e99b773ad9a135ff076f2e4051904ca8046e0b7bc41f3e2" // body "plain body, not json"
)

func TestStripe(t *testing.T) {
	good := h("Stripe-Signature", "t=1700000000,v1="+stripeSig)
	now := unix(stripeTS + 10)
	runCases(t, "stripe", []tc{
		{"valid", "", stripeSecret, good, stripeBody, now, ok("evt_1NXWPnCo6bFb1KQto6C8OWvE", "invoice.paid")},
		{"valid, body is not JSON", "", stripeSecret, h("Stripe-Signature", "t=1700000000,v1="+stripePlainSig), "plain body, not json", now, ok("", "")},
		{"valid with unknown schemes and spaces", "", stripeSecret,
			h("Stripe-Signature", "t=1700000000, v0=deadbeef, v1="+stripeSig+", x=y"), stripeBody, now, ok("evt_1NXWPnCo6bFb1KQto6C8OWvE", "invoice.paid")},
		{"second of several v1 signatures matches (secret rotation)", "", "whsec_rotated",
			h("Stripe-Signature", "t=1700000000,v1="+stripeSig+",v1="+stripeRotatedSig), stripeBody, now, ok("evt_1NXWPnCo6bFb1KQto6C8OWvE", "invoice.paid")},
		{"first of several v1 signatures matches", "", stripeSecret,
			h("Stripe-Signature", "t=1700000000,v1="+stripeSig+",v1="+stripeRotatedSig), stripeBody, now, ok("evt_1NXWPnCo6bFb1KQto6C8OWvE", "invoice.paid")},

		{"wrong secret", "", "whsec_wrong", good, stripeBody, now, failsWith("signature mismatch")},
		{"tampered body", "", stripeSecret, good, strings.Replace(stripeBody, "invoice.paid", "invoice.void", 1), now, failsWith("signature mismatch")},
		{"tampered timestamp", "", stripeSecret, h("Stripe-Signature", "t=1700000001,v1="+stripeSig), stripeBody, now, failsWith("signature mismatch")},
		{"signature from a different body", "", stripeSecret, h("Stripe-Signature", "t=1700000000,v1="+stripePlainSig), stripeBody, now, failsWith("signature mismatch")},

		{"stale", "", stripeSecret, good, stripeBody, unix(stripeTS + 600), failsWith("timestamp outside tolerance")},
		{"from the future", "", stripeSecret, good, stripeBody, unix(stripeTS - 600), failsWith("timestamp outside tolerance")},
		{"exactly at tolerance", "", stripeSecret, good, stripeBody, unix(stripeTS + 300), ok("evt_1NXWPnCo6bFb1KQto6C8OWvE", "invoice.paid")},
		{"one second past tolerance", "", stripeSecret, good, stripeBody, unix(stripeTS + 301), failsWith("timestamp outside tolerance")},
		{"custom tolerance allows an older request", `{"tolerance_seconds":900}`, stripeSecret, good, stripeBody, unix(stripeTS + 600), ok("evt_1NXWPnCo6bFb1KQto6C8OWvE", "invoice.paid")},
		{"custom tolerance can be stricter", `{"tolerance_seconds":5}`, stripeSecret, good, stripeBody, now, failsWith("timestamp outside tolerance")},

		{"missing header", "", stripeSecret, hdr{}, stripeBody, now, failsWith("missing Stripe-Signature")},
		{"header sent twice", "", stripeSecret, hdr{"Stripe-Signature": {good["Stripe-Signature"][0], good["Stripe-Signature"][0]}}, stripeBody, now, failsWith("multiple Stripe-Signature")},
		{"garbage header", "", stripeSecret, h("Stripe-Signature", "not a signature"), stripeBody, now, failsWith("malformed")},
		{"empty header", "", stripeSecret, h("Stripe-Signature", ""), stripeBody, now, failsWith("malformed")},
		{"timestamp not numeric", "", stripeSecret, h("Stripe-Signature", "t=abc,v1="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"timestamp negative", "", stripeSecret, h("Stripe-Signature", "t=-5,v1="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"timestamp with a plus", "", stripeSecret, h("Stripe-Signature", "t=+1700000000,v1="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"timestamp with a fraction", "", stripeSecret, h("Stripe-Signature", "t=1700000000.5,v1="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"timestamp too large", "", stripeSecret, h("Stripe-Signature", "t=99999999999999,v1="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"timestamp missing", "", stripeSecret, h("Stripe-Signature", "v1="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"timestamp given twice", "", stripeSecret, h("Stripe-Signature", "t=1700000000,t=1700000001,v1="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"no v1 signature", "", stripeSecret, h("Stripe-Signature", "t=1700000000,v0="+stripeSig), stripeBody, now, failsWith("malformed")},
		{"v1 not hex", "", stripeSecret, h("Stripe-Signature", "t=1700000000,v1=zzzz"), stripeBody, now, failsWith("malformed")},
		{"v1 wrong length", "", stripeSecret, h("Stripe-Signature", "t=1700000000,v1="+stripeSig[:40]), stripeBody, now, failsWith("malformed")},
		{"absurdly many parts", "", stripeSecret, h("Stripe-Signature", "t=1700000000,v1="+stripeSig+strings.Repeat(",x=y", 100)), stripeBody, now, failsWith("malformed")},

		{"no secret configured", "", "", good, stripeBody, now, configProblem},
		{"unknown config key", `{"tolerence_seconds":5}`, stripeSecret, good, stripeBody, now, configProblem}, //nolint:misspell // a deliberate typo: an unrecognized key
		{"negative tolerance", `{"tolerance_seconds":-1}`, stripeSecret, good, stripeBody, now, configProblem},
		{"huge tolerance", `{"tolerance_seconds":999999999}`, stripeSecret, good, stripeBody, now, configProblem},
		{"config is not an object", `[1]`, stripeSecret, good, stripeBody, now, configProblem},
	})

}

// signedFields covers behavior that needs a body whose signature is not in the
// table above, so these tests sign with the standard library themselves.
func TestStripeTakesIDAndTypeFromTheSignedBody(t *testing.T) {
	sign := func(body string) hdr {
		m := hmac.New(sha256.New, []byte(stripeSecret))
		m.Write([]byte("1700000000." + body))
		return h("Stripe-Signature", "t=1700000000,v1="+hex.EncodeToString(m.Sum(nil)))
	}
	now := unix(stripeTS + 10)
	long := strings.Repeat("e", 300)
	bodies := []struct {
		name, body string
		want       want
	}{
		{"overlong id is dropped", `{"id":"` + long + `","type":"x"}`, ok("", "x")},
		{"overlong type is dropped", `{"id":"evt_1","type":"` + long + `"}`, ok("evt_1", "")},
		{"non-string id is ignored", `{"id":12345,"type":"x"}`, ok("", "x")},
		{"null id is ignored", `{"id":null,"type":"x"}`, ok("", "x")},
		{"nested id is not top level", `{"data":{"id":"evt_nested"},"type":"x"}`, ok("", "x")},
		{"a JSON array body", `[{"id":"evt_1"}]`, ok("", "")},
		{"leading whitespace", " \n{\"id\":\"evt_ws\"}", ok("evt_ws", "")},
		{"invalid JSON", `{"id":"evt_1"`, ok("", "")},
	}
	for _, b := range bodies {
		t.Run(b.name, func(t *testing.T) {
			runCases(t, "stripe", []tc{{b.name, "", stripeSecret, sign(b.body), b.body, now, b.want}})
		})
	}
}

func TestStandardWebhooksTakesTypeFromTheSignedBody(t *testing.T) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(swSecret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"type":"invoice.paid","data":{}}`
	m := hmac.New(sha256.New, key)
	m.Write([]byte(swID + "." + swTS + "." + body))
	sig := "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
	runCases(t, "standardwebhooks", []tc{
		{"event type from the body", "", swSecret, swHeaders(sig), body, unix(1614265330 + 3), ok(swID, "invoice.paid")},
	})
}

// ---- github ----

const (
	// From GitHub's documentation, "Validating webhook deliveries".
	githubSecret = "It's a Secret to Everybody"
	githubBody   = "Hello, World!"
	githubSig    = "757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
)

func TestGitHub(t *testing.T) {
	now := unix(1700000000) // GitHub signatures carry no timestamp, so the clock is irrelevant
	good := h("X-Hub-Signature-256", "sha256="+githubSig, "X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958", "X-GitHub-Event", "push")
	runCases(t, "github", []tc{
		{"documented example vector", "", githubSecret, good, githubBody, now, ok("72d3162e-cc78-11e3-81ab-4c9367dc0958", "push")},
		{"without delivery and event headers", "", githubSecret, h("X-Hub-Signature-256", "sha256="+githubSig), githubBody, now, ok("", "")},
		{"uppercase hex is the same signature", "", githubSecret, h("X-Hub-Signature-256", "sha256="+strings.ToUpper(githubSig)), githubBody, now, ok("", "")},
		{"an old signature is still valid, as GitHub sends no timestamp", "", githubSecret, good, githubBody, unix(1), ok("72d3162e-cc78-11e3-81ab-4c9367dc0958", "push")},
		{"overlong delivery id is dropped", "", githubSecret, h("X-Hub-Signature-256", "sha256="+githubSig, "X-GitHub-Delivery", strings.Repeat("d", 300)), githubBody, now, ok("", "")},

		{"wrong secret", "", "another secret", good, githubBody, now, failsWith("signature mismatch")},
		{"tampered body", "", githubSecret, good, "Hello, World?", now, failsWith("signature mismatch")},
		{"empty body against the signature of another body", "", githubSecret, good, "", now, failsWith("signature mismatch")},
		{"trailing newline changes the signature", "", githubSecret, good, githubBody + "\n", now, failsWith("signature mismatch")},

		{"missing header", "", githubSecret, hdr{}, githubBody, now, failsWith("missing X-Hub-Signature-256")},
		{"header sent twice", "", githubSecret, hdr{"X-Hub-Signature-256": {"sha256=" + githubSig, "sha256=" + githubSig}}, githubBody, now, failsWith("multiple")},
		{"no scheme prefix", "", githubSecret, h("X-Hub-Signature-256", githubSig), githubBody, now, failsWith("malformed")},
		{"legacy sha1 scheme", "", githubSecret, h("X-Hub-Signature-256", "sha1="+githubSig[:40]), githubBody, now, failsWith("malformed")},
		{"prefix in the wrong case", "", githubSecret, h("X-Hub-Signature-256", "SHA256="+githubSig), githubBody, now, failsWith("malformed")},
		{"not hex", "", githubSecret, h("X-Hub-Signature-256", "sha256="+strings.Repeat("z", 64)), githubBody, now, failsWith("malformed")},
		{"truncated signature", "", githubSecret, h("X-Hub-Signature-256", "sha256="+githubSig[:32]), githubBody, now, failsWith("malformed")},
		{"empty signature", "", githubSecret, h("X-Hub-Signature-256", "sha256="), githubBody, now, failsWith("malformed")},

		{"no secret configured", "", "", good, githubBody, now, configProblem},
		{"takes no config", `{"x":1}`, githubSecret, good, githubBody, now, configProblem},
	})
}

// ---- slack ----

const (
	// From Slack's documentation, "Verifying requests from Slack".
	slackDocSecret = "8f742231b10e8888abcd99yyyzzz85a5"
	slackDocTS     = "1531420618"
	slackDocSig    = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
	slackDocBody   = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V" +
		"&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=" +
		"&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN" +
		"&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"

	slackSecret    = "slack-secret"
	slackChallenge = `{"token":"Jhj5dZrVaK7ZwHHjRyZWjbDl","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","type":"url_verification"}`
	slackChalSig   = "v0=8db4f003dcafcd5c5babbccc185d36dd46491fd12b0fe7d784acd3d179f53951"
	slackEvent     = `{"type":"event_callback","event_id":"Ev0PV52K21","event":{"type":"message","text":"hi"}}`
	slackEventSig  = "v0=66eba03060722fb653974ef16f0144b36918e2226f08a8d6566ba53a0957e8b4"
)

func TestSlack(t *testing.T) {
	docHeaders := h("X-Slack-Request-Timestamp", slackDocTS, "X-Slack-Signature", slackDocSig)
	docNow := unix(1531420618 + 5)
	at1700 := unix(1700000000 + 5)
	runCases(t, "slack", []tc{
		{"documented example vector (a slash command)", "", slackDocSecret, docHeaders, slackDocBody, docNow, ok("", "")},
		{"event_callback gives the inner event type and the event id", "", slackSecret,
			h("X-Slack-Request-Timestamp", "1700000000", "X-Slack-Signature", slackEventSig), slackEvent, at1700, ok("Ev0PV52K21", "message")},
		{"url_verification returns the challenge", "", slackSecret,
			h("X-Slack-Request-Timestamp", "1700000000", "X-Slack-Signature", slackChalSig), slackChallenge, at1700,
			want{ok: true, challenge: "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"}},

		{"wrong secret", "", "wrong", docHeaders, slackDocBody, docNow, failsWith("signature mismatch")},
		{"tampered body", "", slackDocSecret, docHeaders, slackDocBody + "&x=1", docNow, failsWith("signature mismatch")},
		{"tampered timestamp", "", slackDocSecret, h("X-Slack-Request-Timestamp", "1531420619", "X-Slack-Signature", slackDocSig), slackDocBody, unix(1531420619), failsWith("signature mismatch")},
		{"a forged url_verification gets no challenge", "", "wrong",
			h("X-Slack-Request-Timestamp", "1700000000", "X-Slack-Signature", slackChalSig), slackChallenge, at1700, failsWith("signature mismatch")},

		{"stale", "", slackDocSecret, docHeaders, slackDocBody, unix(1531420618 + 600), failsWith("timestamp outside tolerance")},
		{"from the future", "", slackDocSecret, docHeaders, slackDocBody, unix(1531420618 - 600), failsWith("timestamp outside tolerance")},
		{"within a custom tolerance", `{"tolerance_seconds":900}`, slackDocSecret, docHeaders, slackDocBody, unix(1531420618 + 600), ok("", "")},

		{"missing signature header", "", slackDocSecret, h("X-Slack-Request-Timestamp", slackDocTS), slackDocBody, docNow, failsWith("missing X-Slack-Signature")},
		{"missing timestamp header", "", slackDocSecret, h("X-Slack-Signature", slackDocSig), slackDocBody, docNow, failsWith("missing X-Slack-Request-Timestamp")},
		{"timestamp not numeric", "", slackDocSecret, h("X-Slack-Request-Timestamp", "12abc", "X-Slack-Signature", slackDocSig), slackDocBody, docNow, failsWith("malformed timestamp")},
		{"timestamp negative", "", slackDocSecret, h("X-Slack-Request-Timestamp", "-1", "X-Slack-Signature", slackDocSig), slackDocBody, docNow, failsWith("malformed timestamp")},
		{"timestamp empty", "", slackDocSecret, h("X-Slack-Request-Timestamp", "", "X-Slack-Signature", slackDocSig), slackDocBody, docNow, failsWith("malformed timestamp")},
		{"no v0 prefix", "", slackDocSecret, h("X-Slack-Request-Timestamp", slackDocTS, "X-Slack-Signature", slackDocSig[3:]), slackDocBody, docNow, failsWith("malformed signature")},
		{"signature not hex", "", slackDocSecret, h("X-Slack-Request-Timestamp", slackDocTS, "X-Slack-Signature", "v0=nothex"), slackDocBody, docNow, failsWith("malformed signature")},
		{"signature header twice", "", slackDocSecret, hdr{"X-Slack-Request-Timestamp": {slackDocTS}, "X-Slack-Signature": {slackDocSig, slackDocSig}}, slackDocBody, docNow, failsWith("multiple")},

		{"no secret configured", "", "", docHeaders, slackDocBody, docNow, configProblem},
		{"unknown config key", `{"nope":1}`, slackDocSecret, docHeaders, slackDocBody, docNow, configProblem},
	})
}

// ---- standard webhooks ----

const (
	// From the Standard Webhooks / Svix documentation.
	swSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" //nolint:gosec // the published example secret from the Standard Webhooks docs
	swID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	swTS     = "1614265330"
	swBody   = `{"test": 2432232314}`
	swSig    = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func swHeaders(sig string) hdr {
	return h("webhook-id", swID, "webhook-timestamp", swTS, "webhook-signature", sig)
}

func TestStandardWebhooks(t *testing.T) {
	now := unix(1614265330 + 3)
	runCases(t, "standardwebhooks", []tc{
		{"documented example vector", "", swSecret, swHeaders(swSig), swBody, now, ok(swID, "")},
		{"event type comes from the body", "", swSecret, swHeaders(swSig), swBody, now, ok(swID, "")},
		{"several signatures, the valid one last", "", swSecret, swHeaders("v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= " + swSig), swBody, now, ok(swID, "")},
		{"several signatures, the valid one first", "", swSecret, swHeaders(swSig + " v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="), swBody, now, ok(swID, "")},
		{"other signature versions are ignored", "", swSecret, swHeaders("v1a,AAAA " + swSig + " v2,BBBB"), swBody, now, ok(swID, "")},

		{"wrong secret", "", "whsec_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", swHeaders(swSig), swBody, now, failsWith("signature mismatch")},
		{"tampered body", "", swSecret, swHeaders(swSig), `{"test": 2432232315}`, now, failsWith("signature mismatch")},
		{"tampered id", "", swSecret, h("webhook-id", "msg_other", "webhook-timestamp", swTS, "webhook-signature", swSig), swBody, now, failsWith("signature mismatch")},
		{"tampered timestamp", "", swSecret, h("webhook-id", swID, "webhook-timestamp", "1614265331", "webhook-signature", swSig), swBody, unix(1614265331), failsWith("signature mismatch")},
		{"only a non-v1 signature", "", swSecret, swHeaders("v1a," + strings.TrimPrefix(swSig, "v1,")), swBody, now, failsWith("malformed signature")},

		{"stale", "", swSecret, swHeaders(swSig), swBody, unix(1614265330 + 600), failsWith("timestamp outside tolerance")},
		{"from the future", "", swSecret, swHeaders(swSig), swBody, unix(1614265330 - 600), failsWith("timestamp outside tolerance")},
		{"within a custom tolerance", `{"tolerance_seconds":900}`, swSecret, swHeaders(swSig), swBody, unix(1614265330 + 600), ok(swID, "")},

		{"missing id", "", swSecret, h("webhook-timestamp", swTS, "webhook-signature", swSig), swBody, now, failsWith("missing webhook-id")},
		{"missing timestamp", "", swSecret, h("webhook-id", swID, "webhook-signature", swSig), swBody, now, failsWith("missing webhook-timestamp")},
		{"missing signature", "", swSecret, h("webhook-id", swID, "webhook-timestamp", swTS), swBody, now, failsWith("missing webhook-signature")},
		{"empty id", "", swSecret, h("webhook-id", "", "webhook-timestamp", swTS, "webhook-signature", swSig), swBody, now, failsWith("malformed webhook-id")},
		{"overlong id", "", swSecret, h("webhook-id", strings.Repeat("i", 300), "webhook-timestamp", swTS, "webhook-signature", swSig), swBody, now, failsWith("malformed webhook-id")},
		{"timestamp not numeric", "", swSecret, h("webhook-id", swID, "webhook-timestamp", "soon", "webhook-signature", swSig), swBody, now, failsWith("malformed timestamp")},
		{"signature without a version", "", swSecret, swHeaders(strings.TrimPrefix(swSig, "v1,")), swBody, now, failsWith("malformed signature")},
		{"signature not base64", "", swSecret, swHeaders("v1,!!!!"), swBody, now, failsWith("malformed signature")},
		{"signature of the wrong length", "", swSecret, swHeaders("v1,AAAA"), swBody, now, failsWith("malformed signature")},
		{"signature header twice", "", swSecret, hdr{"webhook-id": {swID}, "webhook-timestamp": {swTS}, "webhook-signature": {swSig, swSig}}, swBody, now, failsWith("multiple")},
		{"id header twice", "", swSecret, hdr{"webhook-id": {swID, swID}, "webhook-timestamp": {swTS}, "webhook-signature": {swSig}}, swBody, now, failsWith("multiple")},

		{"no secret configured", "", "", swHeaders(swSig), swBody, now, configProblem},
		{"secret without the whsec_ prefix", "", "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", swHeaders(swSig), swBody, now, configProblem},
		{"secret that is not base64", "", "whsec_!!!not base64!!!", swHeaders(swSig), swBody, now, configProblem},
		{"secret that decodes to nothing", "", "whsec_", swHeaders(swSig), swBody, now, configProblem},
	})
}

// ---- generic hmac ----

func TestGenericHMAC(t *testing.T) {
	now := unix(1700000000 + 5)
	runCases(t, "hmac", []tc{
		{"sha1, hex, with a prefix", `{"header":"X-Sig","algorithm":"sha1","prefix":"sha1="}`, "k1",
			h("X-Sig", "sha1=03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc"), "payload-1", now, ok("", "")},
		{"sha512, base64url without padding", `{"header":"X-Sig","algorithm":"sha512","encoding":"base64url"}`, "k2",
			h("X-Sig", "DVF-bgIfDokah2MYDUANZ4Sp95JbyWyTfLVcMQg6g2RNU2zbK7vPrQgiwpDmnv55M7uptI1z7Rm9nlZgxjnCTg"), "payload-2", now, ok("", "")},
		{"sha512, base64url with padding is accepted", `{"header":"X-Sig","algorithm":"sha512","encoding":"base64url"}`, "k2",
			h("X-Sig", "DVF-bgIfDokah2MYDUANZ4Sp95JbyWyTfLVcMQg6g2RNU2zbK7vPrQgiwpDmnv55M7uptI1z7Rm9nlZgxjnCTg=="), "payload-2", now, ok("", "")},
		{"sha384, hex", `{"header":"X-Sig","algorithm":"sha384"}`, "k6",
			h("X-Sig", "1b7ce8542e581031ed18d4bd93600dd9be23ce3fe1fd1818c83185a18f5ea08c9de52465a83643d7553520b2e5e99b47"), "payload-6", now, ok("", "")},
		{"timestamp header with the default template", `{"header":"X-Sig","encoding":"base64","timestamp_header":"X-Ts"}`, "k3",
			h("X-Sig", "vWA86sbF9S923BI9e//rysHry2/mxrXoKLqcuy1fKm0=", "X-Ts", "1700000000"), "payload-3", now, ok("", "")},
		{"custom payload template", `{"header":"X-Sig","timestamp_header":"X-Ts","payload_template":"v0:{timestamp}:{body}"}`, "k4",
			h("X-Sig", "f46dacd5a69fb9dbdd45e675f63d8e2192f3027e0ab147946705cd3d7914cd2c", "X-Ts", "1700000000"), "payload-4", now, ok("", "")},
		{"placeholder text inside the body is not expanded", `{"header":"X-Sig","timestamp_header":"X-Ts"}`, "k5",
			h("X-Sig", "531f2fbe9de4720cb973242f3c77aca38f1008a9728f1eccf5e86b3f6a0bb9a4", "X-Ts", "1700000000"), "{timestamp} literal {body}", now, ok("", "")},
		{"uppercase hex", `{"header":"X-Sig","algorithm":"sha1","prefix":"sha1="}`, "k1",
			h("X-Sig", "sha1="+strings.ToUpper("03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc")), "payload-1", now, ok("", "")},
		{"delivery id from a header", `{"header":"X-Sig","algorithm":"sha1","prefix":"sha1=","delivery_id_header":"X-Request-Id"}`, "k1",
			h("X-Sig", "sha1=03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc", "X-Request-Id", "req-42"), "payload-1", now, ok("req-42", "")},
		{"the header name is case-insensitive", `{"header":"x-sig","algorithm":"sha1","prefix":"sha1="}`, "k1",
			h("X-SIG", "sha1=03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc"), "payload-1", now, ok("", "")},

		{"wrong secret", `{"header":"X-Sig","algorithm":"sha1","prefix":"sha1="}`, "other",
			h("X-Sig", "sha1=03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc"), "payload-1", now, failsWith("signature mismatch")},
		{"tampered body", `{"header":"X-Sig","algorithm":"sha1","prefix":"sha1="}`, "k1",
			h("X-Sig", "sha1=03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc"), "payload-X", now, failsWith("signature mismatch")},
		{"the wrong algorithm for the signature", `{"header":"X-Sig","algorithm":"sha256","prefix":"sha1="}`, "k1",
			h("X-Sig", "sha1=03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc"), "payload-1", now, failsWith("signature mismatch")},
		{"tampered timestamp", `{"header":"X-Sig","encoding":"base64","timestamp_header":"X-Ts"}`, "k3",
			h("X-Sig", "vWA86sbF9S923BI9e//rysHry2/mxrXoKLqcuy1fKm0=", "X-Ts", "1700000001"), "payload-3", unix(1700000001), failsWith("signature mismatch")},
		{"stale", `{"header":"X-Sig","encoding":"base64","timestamp_header":"X-Ts"}`, "k3",
			h("X-Sig", "vWA86sbF9S923BI9e//rysHry2/mxrXoKLqcuy1fKm0=", "X-Ts", "1700000000"), "payload-3", unix(1700000600), failsWith("timestamp outside tolerance")},
		{"from the future", `{"header":"X-Sig","encoding":"base64","timestamp_header":"X-Ts"}`, "k3",
			h("X-Sig", "vWA86sbF9S923BI9e//rysHry2/mxrXoKLqcuy1fKm0=", "X-Ts", "1700000000"), "payload-3", unix(1699999400), failsWith("timestamp outside tolerance")},
		{"timestamp header missing", `{"header":"X-Sig","encoding":"base64","timestamp_header":"X-Ts"}`, "k3",
			h("X-Sig", "vWA86sbF9S923BI9e//rysHry2/mxrXoKLqcuy1fKm0="), "payload-3", now, failsWith("missing X-Ts")},
		{"timestamp malformed", `{"header":"X-Sig","encoding":"base64","timestamp_header":"X-Ts"}`, "k3",
			h("X-Sig", "vWA86sbF9S923BI9e//rysHry2/mxrXoKLqcuy1fKm0=", "X-Ts", "yesterday"), "payload-3", now, failsWith("malformed timestamp")},
		{"signature header missing", `{"header":"X-Sig"}`, "k1", hdr{}, "payload-1", now, failsWith("missing X-Sig")},
		{"signature header twice", `{"header":"X-Sig"}`, "k1", hdr{"X-Sig": {"aa", "bb"}}, "payload-1", now, failsWith("multiple")},
		{"missing prefix", `{"header":"X-Sig","algorithm":"sha1","prefix":"sha1="}`, "k1",
			h("X-Sig", "03b92a35d598f7ee75f4d1ebe0347a6f34ea58bc"), "payload-1", now, failsWith("malformed")},
		{"not hex", `{"header":"X-Sig"}`, "k1", h("X-Sig", "xyz"), "payload-1", now, failsWith("malformed")},
		{"not base64", `{"header":"X-Sig","encoding":"base64"}`, "k1", h("X-Sig", "***"), "payload-1", now, failsWith("malformed")},
		{"empty signature", `{"header":"X-Sig"}`, "k1", h("X-Sig", ""), "payload-1", now, failsWith("malformed")},
		{"prefix only", `{"header":"X-Sig","prefix":"sha256="}`, "k1", h("X-Sig", "sha256="), "payload-1", now, failsWith("malformed")},

		{"no secret configured", `{"header":"X-Sig"}`, "", h("X-Sig", "aa"), "payload-1", now, configProblem},
		{"header is required", `{}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"header is not a valid name", `{"header":"X Sig"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"unknown algorithm", `{"header":"X-Sig","algorithm":"md5"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"unknown encoding", `{"header":"X-Sig","encoding":"rot13"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"template without a body", `{"header":"X-Sig","payload_template":"static"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"template with two bodies", `{"header":"X-Sig","payload_template":"{body}{body}"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"template with an unknown placeholder", `{"header":"X-Sig","payload_template":"{body}{method}"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"template with a stray brace", `{"header":"X-Sig","payload_template":"}{body}"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"template uses a timestamp that has no header", `{"header":"X-Sig","payload_template":"{timestamp}.{body}"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"tolerance without a timestamp header", `{"header":"X-Sig","tolerance_seconds":60}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"timestamp header is not a valid name", `{"header":"X-Sig","timestamp_header":"bad name"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"delivery id header is not a valid name", `{"header":"X-Sig","delivery_id_header":"bad name"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem},
		{"unknown config key", `{"header":"X-Sig","algorithim":"sha1"}`, "k1", h("X-Sig", "aa"), "p", now, configProblem}, //nolint:misspell // a deliberate typo: an unrecognized key
	})
}

// ---- token ----

func TestToken(t *testing.T) {
	now := unix(1700000000)
	runCases(t, "token", []tc{
		{"default header", "", "s3cret", h("X-Inbox-Token", "s3cret"), "b", now, ok("", "")},
		{"custom header, any case", `{"header":"x-my-token"}`, "s3cret", h("X-My-Token", "s3cret"), "b", now, ok("", "")},
		{"secret containing unusual characters", "", "p@ss w0rd/+=é", h("X-Inbox-Token", "p@ss w0rd/+=é"), "b", now, ok("", "")},

		{"wrong token", "", "s3cret", h("X-Inbox-Token", "s3creT"), "b", now, failsWith("token mismatch")},
		{"token is a prefix of the secret", "", "s3cret", h("X-Inbox-Token", "s3cre"), "b", now, failsWith("token mismatch")},
		{"secret is a prefix of the token", "", "s3cret", h("X-Inbox-Token", "s3crets"), "b", now, failsWith("token mismatch")},
		{"empty token", "", "s3cret", h("X-Inbox-Token", ""), "b", now, failsWith("token mismatch")},
		{"token with surrounding space", "", "s3cret", h("X-Inbox-Token", " s3cret "), "b", now, failsWith("token mismatch")},
		{"missing header", "", "s3cret", hdr{}, "b", now, failsWith("missing X-Inbox-Token")},
		{"header sent twice", "", "s3cret", hdr{"X-Inbox-Token": {"s3cret", "s3cret"}}, "b", now, failsWith("multiple")},
		{"the token in another header does not count", `{"header":"X-A"}`, "s3cret", h("X-B", "s3cret"), "b", now, failsWith("missing X-A")},

		{"no secret configured", "", "", h("X-Inbox-Token", ""), "b", now, configProblem},
		{"invalid header name", `{"header":"bad name"}`, "s3cret", h("X-Inbox-Token", "s3cret"), "b", now, configProblem},
		{"unknown config key", `{"hdr":"X-A"}`, "s3cret", h("X-Inbox-Token", "s3cret"), "b", now, configProblem},
	})

	t.Run("the token header is reported for redaction", func(t *testing.T) {
		v, err := verify.Default().New("token", json.RawMessage(`{"header":"x-my-token"}`))
		if err != nil {
			t.Fatal(err)
		}
		red, ok := v.(verify.HeaderRedactor)
		if !ok {
			t.Fatal("the token verifier must implement HeaderRedactor")
		}
		if got := red.SecretHeaders(); len(got) != 1 || got[0] != "X-My-Token" {
			t.Errorf("SecretHeaders = %v, want [X-My-Token]", got)
		}
	})
}

// ---- none ----

func TestNone(t *testing.T) {
	runCases(t, "none", []tc{
		{"accepts anything", "", "", hdr{}, "anything at all", unix(1), ok("", "")},
		{"ignores a secret", "", "unused", hdr{}, "", unix(1), ok("", "")},
		{"takes no config", `{"x":1}`, "", hdr{}, "", unix(1), configProblem},
	})
}

// ---- registry ----

func TestRegistry(t *testing.T) {
	reg := verify.Default()
	wantNames := []string{"github", "hmac", "none", "slack", "standardwebhooks", "stripe", "token"}
	if got := reg.Names(); strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Errorf("Names = %v, want %v", got, wantNames)
	}
	for _, n := range wantNames {
		if !reg.Has(n) {
			t.Errorf("Has(%q) = false", n)
		}
	}
	if reg.Has("carrier-pigeon") {
		t.Error("Has reports an unregistered name")
	}

	_, err := reg.New("carrier-pigeon", nil)
	if !errors.Is(err, verify.ErrUnknown) || !errors.Is(err, verify.ErrConfig) {
		t.Errorf("unknown verifier: %v, want ErrUnknown wrapping ErrConfig", err)
	}
	if _, err := reg.New("none", nil); err != nil {
		t.Errorf("nil config should mean {}: %v", err)
	}
	if _, err := reg.New("none", json.RawMessage(`{}`)); err != nil {
		t.Errorf("{} config: %v", err)
	}
}

func TestRegistryExtensibility(t *testing.T) {
	custom := func(json.RawMessage) (verify.Verifier, error) {
		return nil, errors.New("boom: not a config error type")
	}
	reg := verify.NewRegistry()
	if len(reg.Names()) != 0 {
		t.Error("NewRegistry should be empty")
	}
	if err := reg.Register("acme-v2", custom); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("acme-v2", custom); err == nil {
		t.Error("registering a name twice succeeded")
	}
	for _, bad := range []string{"", "Upper", "has space", "under_score", strings.Repeat("a", 33)} {
		if err := reg.Register(bad, custom); err == nil {
			t.Errorf("registered invalid name %q", bad)
		}
	}
	if err := reg.Register("nil-factory", nil); err == nil {
		t.Error("registered a nil factory")
	}

	// A factory's plain error is reported as a configuration error.
	if _, err := reg.New("acme-v2", nil); !errors.Is(err, verify.ErrConfig) {
		t.Errorf("a failing factory should surface as ErrConfig, got %v", err)
	}

	// Registries are independent.
	a, b := verify.Default(), verify.Default()
	if err := a.Register("extra", custom); err != nil {
		t.Fatal(err)
	}
	if b.Has("extra") {
		t.Error("registering on one Default() registry affected another")
	}
}

func TestFailureNeverCarriesRequestData(t *testing.T) {
	const marker = "SECRET-MARKER-9f3a"
	v, _ := verify.Default().New("token", nil)
	_, err := v.Verify(request(t, h("X-Inbox-Token", marker)), []byte(marker), []byte("the-real-secret"), unix(1))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "the-real-secret") {
		t.Errorf("error text leaks request data or the secret: %v", err)
	}
}

// slackSign signs a Slack request with the standard library, independent of
// the verifier under test.
func slackSign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + ts + ":" + body))
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

// A secret that cannot work is found when it is set, not at the first delivery.
func TestStandardWebhooksSecretIsCheckedWhenSet(t *testing.T) {
	v, err := verify.Default().New("standardwebhooks", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	checker, ok := v.(verify.SecretChecker)
	if !ok {
		t.Fatal("the standardwebhooks verifier does not check its secret")
	}
	for secret, good := range map[string]bool{
		"whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw": true,
		"not-a-whsec":                            false,
		"whsec_":                                 false,
		"whsec_!!!not base64!!!":                 false,
		"":                                       false,
	} {
		err := checker.CheckSecret([]byte(secret))
		if (err == nil) != good {
			t.Errorf("CheckSecret(%q) = %v, want ok=%v", secret, err, good)
		}
		if err != nil && (!errors.Is(err, verify.ErrConfig) || (len(secret) > 8 && strings.Contains(err.Error(), secret))) {
			t.Errorf("CheckSecret(%q): the error %v is not a config error, or repeats the secret", secret, err)
		}
	}
}
