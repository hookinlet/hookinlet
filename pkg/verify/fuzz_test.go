// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // the oracle mirrors the verifier's sha1 option
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hookinlet/hookinlet/pkg/verify"
)

// The fuzz targets here run their seeds in a normal `go test`. To fuzz one:
//
//	go test -run='^$' -fuzz=FuzzStripe -fuzztime=90s ./pkg/verify

var fuzzNow = time.Unix(1_800_000_000, 0).UTC()

func fuzzRequest(headers ...string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/in/x", nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Add(headers[i], headers[i+1])
	}
	return r
}

func hmacHex(h func() hash.Hash, key []byte, parts ...string) string {
	m := hmac.New(h, key)
	for _, p := range parts {
		m.Write([]byte(p))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// fuzzVerify calls Verify and reports whether it accepted. An error that is
// neither a Failure nor a config error breaks the Verifier contract.
func fuzzVerify(t *testing.T, v verify.Verifier, r *http.Request, body, secret []byte) (verify.Result, bool) {
	t.Helper()
	res, err := v.Verify(r, body, secret, fuzzNow)
	if err == nil {
		return res, true
	}
	if _, isFailure := verify.IsFailure(err); !isFailure && !errors.Is(err, verify.ErrConfig) {
		t.Fatalf("Verify returned an error that is neither a failure nor a config error: %v", err)
	}
	return res, false
}

func mustNew(t *testing.T, name, config string) verify.Verifier {
	t.Helper()
	v, err := verify.Default().New(name, json.RawMessage(config))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// freshTimestamp reports whether ts is a canonical Unix timestamp inside the
// default tolerance of fuzzNow.
func freshTimestamp(ts string) bool {
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != ts || n < 0 || n >= 253402300800 {
		return false
	}
	return absDur(fuzzNow.Sub(time.Unix(n, 0))) <= verify.DefaultTolerance
}

// headerSafe reports whether s can be a header value that survives as given.
func headerSafe(s string) bool { return !strings.ContainsAny(s, "\r\n") }

// FuzzGitHub: arbitrary header text must not panic and is accepted only if it
// is the signature of the body; the true signature is always accepted.
func FuzzGitHub(f *testing.F) {
	f.Add([]byte("secret"), []byte(`{"a":1}`), "sha256=00", false)
	f.Add([]byte("s"), []byte(""), "sha256=", true)
	f.Add([]byte("s"), []byte("body"), "SHA256=abcd", true)
	f.Add([]byte("s"), []byte("body"), "sha256=\xff\x00", false)
	f.Fuzz(func(t *testing.T, secret, body []byte, header string, sign bool) {
		if len(secret) == 0 {
			return
		}
		v := mustNew(t, "github", `{}`)
		good := "sha256=" + hmacHex(sha256.New, secret, string(body))
		if sign {
			header = good
		}
		res, ok := fuzzVerify(t, v, fuzzRequest("X-Hub-Signature-256", header), body, secret)
		if sign && !ok {
			t.Fatal("a valid signature was refused")
		}
		if ok && !strings.EqualFold(header, good) {
			t.Fatalf("accepted %q for a body signed as %q", header, good)
		}
		if ok && (res.Unauthenticated || res.IDSigned) {
			t.Fatalf("GitHub's result claims too much: %+v", res)
		}
	})
}

// FuzzStripe covers the Stripe-Signature parser and the timestamp rules.
func FuzzStripe(f *testing.F) {
	f.Add([]byte("whsec_x"), []byte(`{"id":"evt_1","type":"a"}`), "t=1800000000,v1=00", "1800000000", "", false)
	f.Add([]byte("k"), []byte(""), "t=0,v1=,v0=ab", "0", "", true)
	f.Add([]byte("k"), []byte("b"), "t=99999999999999999999,v1=aa", "1", "v0=1,", true)
	f.Add([]byte("k"), []byte("b"), ",,,t=1,t=2,v1=zz", "2", " ", true)
	f.Fuzz(func(t *testing.T, secret, body []byte, header, ts, noise string, sign bool) {
		if len(secret) == 0 {
			return
		}
		v := mustNew(t, "stripe", `{}`)
		good := hmacHex(sha256.New, secret, ts, ".", string(body))
		if sign {
			header = noise + ",t=" + ts + ",v1=" + good
		}
		res, ok := fuzzVerify(t, v, fuzzRequest("Stripe-Signature", header), body, secret)
		// A header with exactly one t= and a fresh canonical timestamp must pass.
		if sign && freshTimestamp(ts) && !strings.Contains(noise, "t=") && !strings.Contains(noise, "v1") &&
			headerSafe(noise) && strings.Count(noise, ",") == 0 && !ok {
			t.Fatalf("a valid, fresh signature was refused: %q", header)
		}
		if ok {
			if !strings.Contains(header, "v1="+good) && !strings.Contains(header, "v1="+strings.ToUpper(good)) {
				t.Fatalf("accepted %q without the signature of the body", header)
			}
			if !strings.Contains(header, "t="+ts) || !freshTimestamp(ts) {
				t.Fatalf("accepted %q with the timestamp %q", header, ts)
			}
			if res.DeliveryID != "" && !res.IDSigned {
				t.Fatal("a delivery id from the signed body must be marked signed")
			}
		}
	})
}

// FuzzSlack: signed "v0:<ts>:<body>", and a url_verification handshake that
// must never echo more than the bound.
func FuzzSlack(f *testing.F) {
	f.Add([]byte("s"), []byte(`{"type":"url_verification","challenge":"abc"}`), "v0=00", "1800000000", true)
	f.Add([]byte("s"), []byte(`{"type":"event_callback","event_id":"E1","event":{"type":"x"}}`), "", "", true)
	f.Add([]byte("s"), []byte(`not json`), "v0=", "-1", false)
	f.Fuzz(func(t *testing.T, secret, body []byte, sig, ts string, sign bool) {
		if len(secret) == 0 {
			return
		}
		v := mustNew(t, "slack", `{}`)
		good := "v0=" + hmacHex(sha256.New, secret, "v0:", ts, ":", string(body))
		if sign {
			sig = good
		}
		res, ok := fuzzVerify(t, v, fuzzRequest("X-Slack-Request-Timestamp", ts, "X-Slack-Signature", sig), body, secret)
		if sign && freshTimestamp(ts) && !ok {
			t.Fatal("a valid, fresh signature was refused")
		}
		if ok {
			if !strings.EqualFold(sig, good) {
				t.Fatal("accepted a signature that is not the body's")
			}
			if !freshTimestamp(ts) {
				t.Fatalf("accepted the timestamp %q", ts)
			}
			if len(res.Challenge) > 256 {
				t.Fatalf("echoes a %d byte challenge", len(res.Challenge))
			}
		}
	})
}

// FuzzStandardWebhooks: id, timestamp, and a list of signatures.
func FuzzStandardWebhooks(f *testing.F) {
	f.Add([]byte("secret-bytes"), []byte(`{"type":"t"}`), "msg_1", "1800000000", "v1,AAAA v1,BBBB", true)
	f.Add([]byte("k"), []byte(""), "a.b", "0", "v1,=", false)
	f.Add([]byte("k"), []byte("x"), "", "1", "v2,abc  v1,", true)
	f.Fuzz(func(t *testing.T, key, body []byte, id, ts, sigs string, sign bool) {
		if len(key) == 0 {
			return
		}
		secret := []byte("whsec_" + base64.StdEncoding.EncodeToString(key))
		v := mustNew(t, "standardwebhooks", `{}`)
		m := hmac.New(sha256.New, key)
		m.Write([]byte(id + "." + ts + "." + string(body)))
		good := base64.StdEncoding.EncodeToString(m.Sum(nil))
		if sign {
			sigs = "v1," + good
		}
		res, ok := fuzzVerify(t, v, fuzzRequest("webhook-id", id, "webhook-timestamp", ts, "webhook-signature", sigs), body, secret)
		if sign && freshTimestamp(ts) && id != "" && len(id) <= 256 && !strings.Contains(id, ".") && headerSafe(id) && !ok {
			t.Fatalf("a valid, fresh signature was refused (id %q)", id)
		}
		if ok {
			if !strings.Contains(sigs, good) {
				t.Fatalf("accepted %q without the body's signature", sigs)
			}
			if strings.Contains(id, ".") || id == "" {
				t.Fatalf("accepted the id %q, which could be read as a different split", id)
			}
			if res.DeliveryID != "" && res.DeliveryID != id {
				t.Fatalf("delivery id %q is not the signed id %q", res.DeliveryID, id)
			}
		}
	})
}

// FuzzToken: only the exact secret is accepted.
func FuzzToken(f *testing.F) {
	f.Add([]byte("s3cret"), "s3cret")
	f.Add([]byte("s3cret"), "s3cret ")
	f.Add([]byte("a"), "")
	f.Fuzz(func(t *testing.T, secret []byte, got string) {
		if len(secret) == 0 {
			return
		}
		v := mustNew(t, "token", `{}`)
		_, ok := fuzzVerify(t, v, fuzzRequest("X-Inbox-Token", got), nil, secret)
		if ok && got != string(secret) {
			t.Fatalf("accepted %q for the secret %q", got, secret)
		}
		if _, ok := fuzzVerify(t, v, fuzzRequest("X-Inbox-Token", string(secret)), nil, secret); !ok {
			t.Fatal("the exact secret was refused")
		}
	})
}

// FuzzGenericHMACConfig feeds arbitrary configs to the factory. Whatever it
// builds must run, and a template it accepts must sign what the template says.
func FuzzGenericHMACConfig(f *testing.F) {
	f.Add(`{"header":"X-Sig"}`, "{body}", "sha256", "hex", "sha256=", true)
	f.Add(`{"header":"X-Sig","timestamp_header":"X-Ts"}`, "{timestamp}.{body}", "sha1", "base64", "", true)
	f.Add(`{"header":"X-Sig","timestamp_header":"X-Ts"}`, "v0:{timestamp}:{body}", "sha512", "base64url", "v1=", true)
	f.Add(`{"header":"X-Sig"}`, "{{body}}", "sha384", "hex", "", true)
	f.Add(`{`, "", "", "", "", false)
	f.Fuzz(func(t *testing.T, rawConfig, tmpl, alg, enc, prefix string, structured bool) {
		cfg := rawConfig
		if structured {
			if !utf8.ValidString(tmpl + prefix + alg + enc) {
				return // JSON cannot carry the bytes, so the config would differ from the oracle's
			}
			b, _ := json.Marshal(map[string]any{
				"header": "X-Sig", "timestamp_header": "X-Ts", "payload_template": tmpl,
				"algorithm": alg, "encoding": enc, "prefix": prefix,
			})
			cfg = string(b)
		}
		v, err := verify.Default().New("hmac", json.RawMessage(cfg))
		if err != nil {
			if !errors.Is(err, verify.ErrConfig) {
				t.Fatalf("config error does not wrap ErrConfig: %v", err)
			}
			return
		}
		if !structured {
			_, _ = v.Verify(fuzzRequest("X-Sig", "00"), []byte("b"), []byte("k"), fuzzNow)
			return
		}
		// Sign the way the template says, independently of the implementation.
		// The body holds the text "{timestamp}", which must stay literal.
		secret, body, tsStr := []byte("key"), []byte(`{"a":"{timestamp}"}`), strconv.FormatInt(fuzzNow.Unix(), 10)
		algs := map[string]func() hash.Hash{"": sha256.New, "sha1": sha1.New, "sha256": sha256.New, "sha384": sha512.New384, "sha512": sha512.New}
		if tmpl == "" {
			tmpl = "{timestamp}.{body}" // the default when a timestamp header is set
		}
		m := hmac.New(algs[alg], secret)
		m.Write([]byte(expandTemplate(tmpl, tsStr, string(body))))
		sum := m.Sum(nil)
		var encoded string
		switch enc {
		case "", "hex":
			encoded = hex.EncodeToString(sum)
		case "base64":
			encoded = base64.StdEncoding.EncodeToString(sum)
		case "base64url":
			encoded = base64.URLEncoding.EncodeToString(sum)
		}
		hdr := prefix + encoded
		if !headerSafe(hdr) {
			return
		}
		r := fuzzRequest("X-Sig", hdr, "X-Ts", tsStr)
		if _, ok := fuzzVerify(t, v, r, body, secret); !ok {
			t.Fatalf("a config the factory accepted refuses its own signature (template %q)", tmpl)
		}
		if _, ok := fuzzVerify(t, v, r, append([]byte("x"), body...), secret); ok {
			t.Fatalf("a changed body was accepted (template %q)", tmpl)
		}
	})
}

// expandTemplate expands {timestamp} and {body} in one pass.
func expandTemplate(tmpl, ts, body string) string {
	var b strings.Builder
	for len(tmpl) > 0 {
		switch {
		case strings.HasPrefix(tmpl, "{timestamp}"):
			b.WriteString(ts)
			tmpl = tmpl[len("{timestamp}"):]
		case strings.HasPrefix(tmpl, "{body}"):
			b.WriteString(body)
			tmpl = tmpl[len("{body}"):]
		default:
			b.WriteByte(tmpl[0])
			tmpl = tmpl[1:]
		}
	}
	return b.String()
}

// FuzzGenericHMACSignature drives one fixed configuration with arbitrary header
// values: only the true signature in its one canonical spelling passes.
func FuzzGenericHMACSignature(f *testing.F) {
	const cfg = `{"header":"X-Sig","algorithm":"sha256","encoding":"base64","prefix":"sig=","timestamp_header":"X-Ts","payload_template":"{timestamp}.{body}","delivery_id_header":"X-Id"}`
	f.Add([]byte("key"), []byte("body"), "sig=AAAA", "1800000000", "id-1")
	f.Add([]byte("key"), []byte(""), "sig=", "01800000000", "")
	f.Add([]byte("key"), []byte("b"), "sig=AA==", "-5", "\xff")
	f.Fuzz(func(t *testing.T, secret, body []byte, sigHdr, ts, id string) {
		if len(secret) == 0 {
			return
		}
		v := mustNew(t, "hmac", cfg)
		m := hmac.New(sha256.New, secret)
		m.Write([]byte(ts + "." + string(body)))
		good := "sig=" + base64.StdEncoding.EncodeToString(m.Sum(nil))
		res, ok := fuzzVerify(t, v, fuzzRequest("X-Sig", sigHdr, "X-Ts", ts, "X-Id", id), body, secret)
		if ok {
			if sigHdr != good {
				t.Fatalf("accepted the signature header %q, not the canonical %q", sigHdr, good)
			}
			if len(res.DeliveryID) > 256 || res.IDSigned {
				t.Fatalf("bad delivery id result %+v", res)
			}
		}
		if freshTimestamp(ts) {
			if _, ok := fuzzVerify(t, v, fuzzRequest("X-Sig", good, "X-Ts", ts), body, secret); !ok {
				t.Fatal("a valid signature was refused")
			}
		}
	})
}
