// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package applyfile_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/applyfile"
)

func TestParseAcceptsTheFullFormat(t *testing.T) {
	specs, err := applyfile.Parse([]byte(`
inboxes:
  - slug: stripe
    name: Stripe (production)
    verifier: stripe
    secret_env: STRIPE_WEBHOOK_SECRET
    verifier_config: { tolerance_seconds: 300 }
    on_verify_fail: store
    max_body_bytes: 1048576
    retention_days: 30
    retention_max_count: 50000
    event_type_pointer: /type
    disabled: true
  - slug: plain-one
    secret_file: /run/secrets/x
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs", len(specs))
	}
	s := specs[0]
	if s.Slug != "stripe" || *s.Name != "Stripe (production)" || *s.Verifier != "stripe" || s.SecretEnv != "STRIPE_WEBHOOK_SECRET" ||
		string(*s.VerifierConfig) != `{"tolerance_seconds":300}` || *s.OnVerifyFail != "store" || *s.MaxBodyBytes != 1048576 ||
		*s.RetentionDays != 30 || *s.RetentionMaxCount != 50000 || *s.EventTypePointer != "/type" || !*s.Disabled {
		t.Errorf("parsed wrongly: %+v", s)
	}
	if specs[1].Name != nil || specs[1].Verifier != nil || specs[1].SecretFile != "/run/secrets/x" {
		t.Errorf("undeclared fields must stay nil: %+v", specs[1])
	}
}

func TestParseRefusesBadFiles(t *testing.T) {
	const literal = "whsec_VERYSECRETVALUE"
	for _, tc := range []struct {
		name string
		yaml string
		want []string // every one must appear in the error
	}{
		{"empty file", "", []string{"empty"}},
		{"not yaml", "inboxes: [", []string{"not valid YAML"}},
		{"unknown top-level key", "inboxes:\n  - slug: abc\nextra: 1\n", []string{"line 3", `"extra"`}},
		{"duplicate top-level key", "inboxes: []\ninboxes: []\n", []string{"line 2", "duplicate", "inboxes"}},
		{"no inboxes key", "foo: 1\n", []string{`"foo"`}},
		{"inboxes not a list", "inboxes: {a: 1}\n", []string{"line 1", "must be a list"}},
		{"unknown key", "inboxes:\n  - slug: abc\n    retention_day: 3\n", []string{"line 3", "retention_day", "unknown key"}},
		{"duplicate key", "inboxes:\n  - slug: abc\n    name: a\n    name: b\n", []string{"line 4", "duplicate key", "name", "line 3"}},
		{"literal secret", "inboxes:\n  - slug: abc\n    verifier: stripe\n    secret: " + literal + "\n", []string{"line 4", "secret", "secret_env", "secret_file"}},
		{"literal verifier_secret", "inboxes:\n  - slug: abc\n    verifier_secret: " + literal + "\n", []string{"line 3", "verifier_secret", "secret_env"}},
		{"both secret sources", "inboxes:\n  - slug: abc\n    secret_env: A\n    secret_file: /x\n", []string{"line 2", "mutually exclusive"}},
		{"secret_env holding a secret", "inboxes:\n  - slug: abc\n    secret_env: 'whsec_a/b+c='\n", []string{"line 3", "secret_env", "not the secret itself"}},
		{"missing slug", "inboxes:\n  - name: x\n", []string{"slug is required"}},
		{"bad slug", "inboxes:\n  - slug: Bad_Slug\n", []string{"line 2", "slug must be 3-64"}},
		{"short slug", "inboxes:\n  - slug: ab\n", []string{"slug must be 3-64"}},
		{"duplicate slug", "inboxes:\n  - slug: abc\n  - slug: abc\n", []string{"line 3", "declared twice", "line 2"}},
		{"bad verifier name", "inboxes:\n  - slug: abc\n    verifier: Stripe!\n", []string{"verifier must be"}},
		{"bad verifier config", "inboxes:\n  - slug: abc\n    verifier: stripe\n    verifier_config: {tolerance_seconds: abc}\n", []string{"line 2", "tolerance"}},
		{"bad on_verify_fail", "inboxes:\n  - slug: abc\n    on_verify_fail: maybe\n", []string{"on_verify_fail"}},
		{"wrong type", "inboxes:\n  - slug: abc\n    retention_days: lots\n", []string{"line 3", "retention_days", "an integer"}},
		{"negative retention", "inboxes:\n  - slug: abc\n    retention_days: -1\n", []string{"retention_days"}},
		{"body too big", "inboxes:\n  - slug: abc\n    max_body_bytes: 99999999999\n", []string{"max_body_bytes"}},
		{"zero body limit", "inboxes:\n  - slug: abc\n    max_body_bytes: 0\n", []string{"max_body_bytes", "leave it out"}},
		{"bad pointer", "inboxes:\n  - slug: abc\n    event_type_pointer: type\n", []string{"event_type_pointer"}},
		{"empty name", "inboxes:\n  - slug: abc\n    name: ''\n", []string{"name"}},
		{"duplicate key inside verifier_config", "inboxes:\n  - slug: abc\n    verifier: hmac\n    verifier_config: {a: 1, a: 2}\n", []string{"verifier_config"}},
		{"anchor", "inboxes:\n  - &a\n    slug: abc\n  - *a\n", []string{"anchors and aliases"}},
		{"second document", "inboxes:\n  - slug: abc\n---\ninboxes:\n  - slug: def\n", []string{"line 3", "only one YAML document"}},
		{"entry not a mapping", "inboxes:\n  - abc\n", []string{"line 2", "mapping"}},
		{"several problems are all reported", "inboxes:\n  - slug: abc\n    nope: 1\n  - slug: ZZZ\n", []string{"nope", "slug must be"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs, err := applyfile.Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("accepted: %+v", specs)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
			if strings.Contains(err.Error(), literal) || strings.Contains(err.Error(), "whsec_a/b") {
				t.Errorf("the error repeats a value from the file: %q", err)
			}
		})
	}
}

func TestParseRefusesAHugeFile(t *testing.T) {
	if _, err := applyfile.Parse([]byte("inboxes:\n  - slug: abc\n# " + strings.Repeat("x", applyfile.MaxFileBytes))); err == nil {
		t.Fatal("accepted a file over the size cap")
	}
}

func TestResolveSecrets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", "file-secret\n")
	empty := write("empty", "\n")
	huge := write("huge", strings.Repeat("a", admin.MaxSecretBytes+10))
	exact := write("exact", strings.Repeat("a", admin.MaxSecretBytes)+"\n")
	env := map[string]string{"HAVE": "env-secret"}
	getenv := func(k string) string { return env[k] }

	specs := []applyfile.Spec{
		{Slug: "from-env", SecretEnv: "HAVE"},
		{Slug: "from-file", SecretFile: good},
		{Slug: "no-source"},
		{Slug: "at-the-limit", SecretFile: exact},
	}
	got, err := applyfile.ResolveSecrets(specs, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got["from-env"] != "env-secret" || got["from-file"] != "file-secret" || len(got["at-the-limit"]) != admin.MaxSecretBytes {
		t.Errorf("secrets wrong (newline trimmed once for the file?): lens %d %d", len(got["from-env"]), len(got["from-file"]))
	}
	if _, ok := got["no-source"]; ok {
		t.Error("an inbox with no source got a secret")
	}

	_, err = applyfile.ResolveSecrets([]applyfile.Spec{
		{Slug: "unset-one", SecretEnv: "NOPE"},
		{Slug: "empty-file", SecretFile: empty},
		{Slug: "huge-file", SecretFile: huge},
		{Slug: "missing-file", SecretFile: filepath.Join(dir, "absent")},
		{Slug: "a-directory", SecretFile: dir},
	}, getenv)
	if err == nil {
		t.Fatal("bad sources accepted")
	}
	for _, w := range []string{"NOPE", empty, "larger than", "absent", "not a regular file"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error does not name %q:\n%v", w, err)
		}
	}
	if strings.Contains(err.Error(), "aaaaaaaa") {
		t.Error("the error contains secret bytes")
	}
}

func view(mod func(*admin.View)) *admin.View {
	v := &admin.View{
		Slug: "abc", Name: "abc", Verifier: "stripe", VerifierConfig: json.RawMessage(`{"tolerance_seconds": 300}`),
		OnVerifyFail: "reject", MaxBodyBytes: 1 << 20, RetentionDays: 30, RetentionMaxCount: 100, EventTypePointer: "/type",
	}
	if mod != nil {
		mod(v)
	}
	return v
}

func ptr[T any](v T) *T { return &v }

func TestPlan(t *testing.T) {
	cfg := json.RawMessage(`{"tolerance_seconds":300}`)
	full := applyfile.Spec{
		Slug: "abc", Name: ptr("abc"), Verifier: ptr("stripe"), VerifierConfig: &cfg, OnVerifyFail: ptr("reject"),
		MaxBodyBytes: ptr(int64(1 << 20)), RetentionDays: ptr(30), RetentionMaxCount: ptr(int64(100)),
		EventTypePointer: ptr("/type"), Disabled: ptr(false),
	}
	changed := func(mod func(*applyfile.Spec)) applyfile.Spec { s := full; mod(&s); return s }
	otherCfg := json.RawMessage(`{"tolerance_seconds": 600}`)
	reordered := json.RawMessage(`{ "tolerance_seconds" : 300.0 }`)

	for _, tc := range []struct {
		name       string
		spec       applyfile.Spec
		cur        *admin.View
		action     applyfile.Action
		fields     []string
		wantSecret bool
	}{
		{"identical", full, view(nil), applyfile.Unchanged, nil, false},
		{"only the slug declared", applyfile.Spec{Slug: "abc"}, view(nil), applyfile.Unchanged, nil, false},
		{"config spelled differently", changed(func(s *applyfile.Spec) { s.VerifierConfig = &reordered }), view(nil), applyfile.Unchanged, nil, false},
		{"empty config equals none stored", changed(func(s *applyfile.Spec) { e := json.RawMessage(`{}`); s.VerifierConfig = &e }), view(func(v *admin.View) { v.VerifierConfig = nil }), applyfile.Unchanged, nil, false},
		{"one field", changed(func(s *applyfile.Spec) { s.RetentionDays = ptr(7) }), view(nil), applyfile.Update, []string{"retention_days"}, false},
		{"several, in a stable order", changed(func(s *applyfile.Spec) {
			s.Disabled = ptr(true)
			s.Name = ptr("Other")
			s.VerifierConfig = &otherCfg
			s.EventTypePointer = ptr("")
		}), view(nil), applyfile.Update, []string{"name", "verifier_config", "event_type_pointer", "disabled"}, false},
		{"undeclared fields are not touched", applyfile.Spec{Slug: "abc", RetentionDays: ptr(1)}, view(func(v *admin.View) { v.Disabled = true; v.Name = "kept" }), applyfile.Update, []string{"retention_days"}, false},
		{"secret on an unchanged inbox", changed(func(s *applyfile.Spec) { s.SecretEnv = "X" }), view(nil), applyfile.Unchanged, nil, true},
		{"secret on an updated inbox", changed(func(s *applyfile.Spec) { s.SecretFile = "/x"; s.OnVerifyFail = ptr("store") }), view(nil), applyfile.Update, []string{"on_verify_fail"}, true},
		{"create", full, nil, applyfile.Create, nil, false},
		{"create with a secret", changed(func(s *applyfile.Spec) { s.SecretEnv = "X" }), nil, applyfile.Create, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := applyfile.Plan(tc.spec, tc.cur)
			if it.Action != tc.action || !reflect.DeepEqual(it.Fields, tc.fields) || it.SetSecret != tc.wantSecret {
				t.Fatalf("got action %d fields %v secret %v; want %d %v %v", it.Action, it.Fields, it.SetSecret, tc.action, tc.fields, tc.wantSecret)
			}
			if tc.action == applyfile.Unchanged && it.Patch != (admin.Patch{}) {
				t.Errorf("an unchanged inbox has a patch: %+v", it.Patch)
			}
		})
	}

	t.Run("the patch carries exactly the differing fields", func(t *testing.T) {
		it := applyfile.Plan(changed(func(s *applyfile.Spec) { s.RetentionDays = ptr(7); s.Name = ptr("N") }), view(nil))
		p := it.Patch
		if p.RetentionDays == nil || *p.RetentionDays != 7 || p.Name == nil || *p.Name != "N" {
			t.Fatalf("patch %+v", p)
		}
		if p.Verifier != nil || p.VerifierConfig != nil || p.OnVerifyFail != nil || p.MaxBodyBytes != nil ||
			p.RetentionMaxCount != nil || p.EventTypePointer != nil || p.Disabled != nil {
			t.Errorf("the patch carries fields that did not differ: %+v", p)
		}
	})
	t.Run("create parameters hold only what was declared", func(t *testing.T) {
		it := applyfile.Plan(applyfile.Spec{Slug: "abc", RetentionDays: ptr(3)}, nil)
		want := admin.CreateParams{Slug: "abc", RetentionDays: 3, VerifierConfig: json.RawMessage(`{}`)}
		if !reflect.DeepEqual(it.Create, want) {
			t.Errorf("got %+v", it.Create)
		}
	})
	t.Run("a new inbox declared disabled is created and then switched", func(t *testing.T) {
		it := applyfile.Plan(applyfile.Spec{Slug: "abc", Disabled: ptr(true)}, nil)
		if it.Patch.Disabled == nil || !*it.Patch.Disabled {
			t.Errorf("no follow-up patch: %+v", it.Patch)
		}
	})
}
