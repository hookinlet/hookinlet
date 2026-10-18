// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// An inbox whose verifier names its signature header says which header it is, in
// the REST metadata and in MCP list_inboxes; every other inbox leaves the field
// out. Names only: the value of the header is not in either.
func TestInboxMetadataNamesTheSignatureHeader(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	const sigValue = "sig-value-that-must-not-appear"
	ae.newInbox("signed", "hmac", "s", func(in *inbox.Inbox) {
		in.VerifierConfig = json.RawMessage(`{"header":"x-acme-signature","timestamp_header":"X-Acme-Ts"}`)
	})
	ae.newInbox("github-box", "github", "s")
	ae.newInbox("open", "none", "")
	ae.post("/in/signed", "{}", "X-Acme-Signature", sigValue)

	want := map[string][]string{"signed": {"X-Acme-Signature"}, "github-box": nil, "open": nil, "agent-a": nil}

	for slug, names := range want {
		rec := ae.get("/v1/inboxes/"+slug, testAdminToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", slug, rec.Code, rec.Body)
		}
		raw := rec.Body.String()
		if strings.Contains(raw, sigValue) {
			t.Errorf("%s: the metadata carries a header value: %s", slug, raw)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		sh, present := got["signature_headers"]
		if names == nil {
			if present {
				t.Errorf("%s: signature_headers = %s, want it absent", slug, sh)
			}
			continue
		}
		var list []string
		if err := json.Unmarshal(sh, &list); err != nil || !slices.Equal(list, names) {
			t.Errorf("%s: signature_headers = %s, want %v", slug, sh, names)
		}
	}

	cs := connectMCP(t, base, testAdminToken)
	out := callTool(t, cs, "list_inboxes", nil)
	if out.err != "" {
		t.Fatal(out.err)
	}
	if strings.Contains(string(out.raw), sigValue) {
		t.Errorf("list_inboxes carries a header value: %s", out.raw)
	}
	var l struct {
		Inboxes []struct {
			Slug             string   `json:"slug"`
			SignatureHeaders []string `json:"signature_headers"`
		} `json:"inboxes"`
	}
	if err := json.Unmarshal(out.raw, &l); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, in := range l.Inboxes {
		names, ok := want[in.Slug]
		if !ok {
			continue
		}
		seen++
		if !slices.Equal(in.SignatureHeaders, names) {
			t.Errorf("list_inboxes %s: signature_headers = %v, want %v", in.Slug, in.SignatureHeaders, names)
		}
	}
	if seen != len(want) {
		t.Errorf("list_inboxes showed %d of %d inboxes: %s", seen, len(want), out.raw)
	}
	if strings.Contains(string(out.raw), "signature_headers\":null") {
		t.Errorf("an absent list is written as null: %s", out.raw)
	}
}

// An inbox whose verifier cannot be built (unknown name, bad config) still
// answers; it just names no header.
func TestInboxMetadataWithAnUnbuildableVerifier(t *testing.T) {
	ae, _, _ := mcpEnv(t)
	ae.newInbox("odd", "hmac", "s", func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`{"header":""}`) })
	rec := ae.get("/v1/inboxes/odd", testAdminToken)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "signature_headers") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// classified is a custom verifier that fails with the kind it is given.
type classified struct{ kind verify.FailureKind }

func (c classified) Verify(*http.Request, []byte, []byte, time.Time) (verify.Result, error) {
	return verify.Result{}, &verify.Failure{Reason: "nope", Kind: c.kind}
}

// The observer is told which kind of failure it was, for each kind, and only for a
// delivery that failed verification.
func TestObservedVerificationFailureKinds(t *testing.T) {
	cfg := `{"header":"X-Sig","timestamp_header":"X-Ts"}`
	cases := []struct {
		name    string
		headers []string
		want    inbox.VerifyFailure
	}{
		{"no signature", []string{"X-Ts", signedTS}, inbox.VerifyFailureMissing},
		{"unreadable signature", []string{"X-Sig", "zz", "X-Ts", signedTS}, inbox.VerifyFailureMalformed},
		{"wrong signature", []string{"X-Sig", hexMAC("other", signedTS+".{}"), "X-Ts", signedTS}, inbox.VerifyFailureMismatch},
		{"old timestamp", []string{"X-Sig", hexMAC("s", "1600000000.{}"), "X-Ts", "1600000000"}, inbox.VerifyFailureStale},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, obs := observed(t)
			e.newInbox("box", "hmac", "s", func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(cfg) })
			if rec := e.post("/in/box", "{}", c.headers...); rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			ev := one(t, obs)
			if ev.Outcome != inbox.IngestVerificationFailed || ev.VerifyFailure != c.want {
				t.Errorf("event = %q / %q, want %q / %q", ev.Outcome, ev.VerifyFailure, inbox.IngestVerificationFailed, c.want)
			}
			// The kind is a word of the fixed set and nothing the sender sent.
			if strings.Contains(string(ev.VerifyFailure), "X-") || strings.Contains(string(ev.VerifyFailure), "sig") {
				t.Errorf("the kind names a header: %q", ev.VerifyFailure)
			}
		})
	}
}

// Every other outcome leaves the field empty, including a delivery that failed
// verification but was stored because the inbox says so.
func TestVerifyFailureIsEmptyForOtherOutcomes(t *testing.T) {
	e, obs := observed(t)
	e.newInbox("good", "github", "s")
	e.newInbox("kept", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	e.post("/in/good", "{}", "X-Hub-Signature-256", githubSig("s", "{}"))
	e.post("/in/kept", "{}", "X-Hub-Signature-256", githubSig("wrong", "{}"))
	e.post("/in/good", "{}", "X-Hub-Signature-256", githubSig("s", "{}")) // not a duplicate: no delivery id
	evs := obs.seen()
	if len(evs) != 3 {
		t.Fatalf("%d events: %+v", len(evs), evs)
	}
	for _, ev := range evs {
		if ev.Outcome == inbox.IngestVerificationFailed || ev.VerifyFailure != "" {
			t.Errorf("event = %q / %q, want a stored delivery with no failure kind", ev.Outcome, ev.VerifyFailure)
		}
	}
}

// A custom verifier that names no kind gives an empty one, and a kind outside the
// set never reaches the observer.
func TestCustomVerifierKinds(t *testing.T) {
	for _, c := range []struct {
		name string
		kind verify.FailureKind
		want inbox.VerifyFailure
	}{
		{"none given", "", ""},
		{"made up", "Authorization header was 'Bearer abc'", ""},
		{"in the set", verify.FailureStale, inbox.VerifyFailureStale},
	} {
		t.Run(c.name, func(t *testing.T) {
			reg := verify.Default()
			if err := reg.Register("custom", func(json.RawMessage) (verify.Verifier, error) { return classified{c.kind}, nil }); err != nil {
				t.Fatal(err)
			}
			e, obs := observed(t, func(o *Options) { o.Verifiers = reg })
			e.newInbox("box", "custom", "")
			if rec := e.post("/in/box", "{}"); rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d", rec.Code)
			}
			if ev := one(t, obs); ev.VerifyFailure != c.want {
				t.Errorf("VerifyFailure = %q, want %q", ev.VerifyFailure, c.want)
			}
		})
	}
}
