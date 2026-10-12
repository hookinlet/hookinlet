// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/verify"
)

// An inbox is reached at a random key, not at its slug, and the key can be
// changed.
func TestIngestKeysAreRandomAndRotate(t *testing.T) {
	a := newAdminEnv(t)
	c := unjson[createdJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "stripe", "verifier": "none"}, 201))
	key := c.Inbox.IngestKey
	if len(key) < 20 || key == "stripe" || c.Inbox.IngestPath != "/in/"+key {
		t.Fatalf("ingest key %q, path %q", key, c.Inbox.IngestPath)
	}
	if st, _ := do2(t, "POST", a.base+"/in/stripe", nil, `{}`); st != http.StatusNotFound {
		t.Errorf("the slug is a way in: %d", st)
	}
	if st, _ := do2(t, "POST", a.base+"/in/"+key, nil, `{}`); st != http.StatusOK {
		t.Errorf("the key is not a way in: %d", st)
	}

	// Two inboxes never share a key, and an admin may choose a readable one.
	other := unjson[createdJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "github", "verifier": "none"}, 201))
	if other.Inbox.IngestKey == key {
		t.Error("two inboxes share a key")
	}
	chosen := unjson[createdJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "readable", "ingest_key": "my-readable-key", "verifier": "none"}, 201))
	if chosen.Inbox.IngestKey != "my-readable-key" {
		t.Errorf("chosen key = %q", chosen.Inbox.IngestKey)
	}
	if st, _ := a.admin("POST", "/v1/admin/inboxes", map[string]any{"slug": "taken", "ingest_key": "my-readable-key", "verifier": "none"}); st != http.StatusConflict {
		t.Errorf("a key in use: %d", st)
	}
	if st, _ := a.admin("POST", "/v1/admin/inboxes", map[string]any{"slug": "bad-key", "ingest_key": "no/slash", "verifier": "none"}); st != http.StatusBadRequest {
		t.Errorf("a bad key: %d", st)
	}

	// Rotating moves the inbox: the old URL stops, the new one works, messages stay.
	moved := unjson[viewJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes/stripe/rotate-ingest-key", nil, 200))
	if moved.IngestKey == key || len(moved.IngestKey) < 20 {
		t.Fatalf("rotated key = %q (was %q)", moved.IngestKey, key)
	}
	if st, _ := do2(t, "POST", a.base+"/in/"+key, nil, `{}`); st != http.StatusNotFound {
		t.Errorf("the old key still works: %d", st)
	}
	if st, _ := do2(t, "POST", a.base+"/in/"+moved.IngestKey, nil, `{}`); st != http.StatusOK {
		t.Errorf("the new key does not work: %d", st)
	}
	shown := unjson[viewJSON](t, a.mustAdmin("GET", "/v1/admin/inboxes/stripe", nil, 200))
	if shown.LastSeq != 2 || shown.IngestKey != moved.IngestKey {
		t.Errorf("after rotation: %+v", shown)
	}
	if st, _ := a.admin("POST", "/v1/admin/inboxes/no-such-inbox/rotate-ingest-key", nil); st != http.StatusNotFound {
		t.Errorf("rotating a missing inbox: %d", st)
	}
}

// A verifier must be named. "none" is a choice, and what it accepts is stored but
// not verified.
func TestAVerifierMustBeNamedAndNoneVerifiesNothing(t *testing.T) {
	a := newAdminEnv(t)
	st, body := a.admin("POST", "/v1/admin/inboxes", map[string]any{"slug": "undecided"})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "verifier") {
		t.Errorf("a create with no verifier: %d %s", st, body)
	}
	if st, _ := a.admin("POST", "/v1/admin/inboxes", map[string]any{"slug": "undecided", "verifier": ""}); st != http.StatusBadRequest {
		t.Errorf("an empty verifier: %d", st)
	}

	c := unjson[createdJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "open-door", "verifier": "none", "event_type_pointer": "/type"}, 201))
	hdr := http.Header{"X-Hub-Signature-256": {"sha256=" + strings.Repeat("0", 64)}, "X-GitHub-Delivery": {"d-1"}, "X-Verified": {"true"}}
	for range 2 {
		if st, body := do2(t, "POST", a.base+c.Inbox.IngestPath, hdr, `{"type":"claimed.type"}`); st != http.StatusOK {
			t.Fatalf("a delivery to a none inbox: %d %s", st, body)
		}
	}
	tok := http.Header{"Authorization": {"Bearer " + c.AgentToken}}
	_, page := do2(t, "GET", a.base+"/v1/inboxes/open-door/messages?after=0", tok, "")
	p := unjson[apiPage](t, []byte(page))
	if len(p.Messages) != 2 {
		t.Fatalf("%d messages, want 2: nothing deduplicates a delivery that nothing checked", len(p.Messages))
	}
	for _, m := range p.Messages {
		if m.Verified {
			t.Errorf("a delivery to a none inbox is verified: %+v", m)
		}
		if m.EventType != nil || m.DeliveryID != nil {
			t.Errorf("a none inbox took claims from the sender: type %v id %v", m.EventType, m.DeliveryID)
		}
	}
	_, page = do2(t, "GET", a.base+"/v1/inboxes/open-door/messages?after=0&verified=true", tok, "")
	if p := unjson[apiPage](t, []byte(page)); len(p.Messages) != 0 {
		t.Errorf("verified=true returned %d messages from a none inbox", len(p.Messages))
	}
}

// A secret is checked when it is set, and does not outlive the verifier it was for.
func TestSecretsAreCheckedWhenSetAndDoNotSurviveAVerifierChange(t *testing.T) {
	a := newAdminEnv(t)
	if st, body := a.admin("POST", "/v1/admin/inboxes", map[string]any{"slug": "sw-bad", "verifier": "standardwebhooks", "verifier_secret": "not-a-whsec"}); st != http.StatusBadRequest || strings.Contains(string(body), "not-a-whsec") { //nolint:gosec // a value made up for a test
		t.Errorf("a Standard Webhooks inbox with an unusable secret: %d %s", st, body)
	}
	sw := unjson[createdJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "sw-ok", "verifier": "standardwebhooks"}, 201))
	if len(sw.Warnings) != 1 {
		t.Errorf("no warning for a verifier with no secret: %v", sw.Warnings)
	}
	if st, _ := a.admin("PUT", "/v1/admin/inboxes/sw-ok/secret", map[string]any{"secret": "plain-secret"}); st != http.StatusBadRequest {
		t.Errorf("an unusable secret was accepted: %d", st)
	}
	good := unjson[viewJSON](t, a.mustAdmin("PUT", "/v1/admin/inboxes/sw-ok/secret", map[string]any{"secret": "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"}, 200)) //nolint:gosec // a value made up for a test
	if !good.HasVerifierSecret || len(good.Warnings) != 0 {
		t.Errorf("after a good secret: %+v", good)
	}

	// A GitHub signing key must not become a shared token.
	g := unjson[createdJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "gh-then-token", "verifier": "github", "verifier_secret": "s3cret"}, 201))
	changed := unjson[viewJSON](t, a.mustAdmin("PATCH", "/v1/admin/inboxes/gh-then-token", map[string]any{"verifier": "token"}, 200))
	if changed.HasVerifierSecret {
		t.Error("the old secret survived a change of verifier")
	}
	if len(changed.Warnings) < 2 || !strings.Contains(strings.Join(changed.Warnings, " "), "secret was removed") {
		t.Errorf("warnings = %v, want one saying the secret was removed and one saying a secret is needed", changed.Warnings)
	}
	if st, _ := do2(t, "POST", a.base+g.Inbox.IngestPath, http.Header{"X-Inbox-Token": {"s3cret"}}, `{}`); st == http.StatusOK {
		t.Error("the old signing key was accepted as a token")
	}
	// Changing something else keeps the secret.
	kept := unjson[viewJSON](t, a.mustAdmin("PATCH", "/v1/admin/inboxes/sw-ok", map[string]any{"name": "renamed"}, 200))
	if !kept.HasVerifierSecret {
		t.Error("an unrelated change removed the secret")
	}
}

// A verifier that says it checked nothing cannot also vouch for an id or a type.
func TestAnUnauthenticatedResultKeepsNoClaims(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		reg := verify.Default()
		if err := reg.Register("shrugs", func(json.RawMessage) (verify.Verifier, error) { return shrugs{}, nil }); err != nil {
			t.Fatal(err)
		}
		o.Verifiers = reg
	})
	in := e.newInbox("shrug-box", "shrugs", "")
	e.post("/in/shrug-box", `{"x":1}`)
	m := e.messages(in)
	if len(m) != 1 || m[0].Verified || m[0].DeliveryID != "" || m[0].EventType != "" {
		t.Errorf("stored %+v", m)
	}
}

type shrugs struct{}

func (shrugs) Verify(*http.Request, []byte, []byte, time.Time) (verify.Result, error) {
	return verify.Result{Unauthenticated: true, DeliveryID: "claimed-id", EventType: "claimed.type"}, nil
}
