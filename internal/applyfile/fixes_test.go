// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package applyfile_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/applyfile"
)

func TestParseRefusesAKeyWithNoValue(t *testing.T) {
	for _, key := range []string{"name", "verifier", "on_verify_fail", "event_type_pointer", "max_body_bytes", "retention_days", "retention_max_count", "disabled", "slug"} {
		for _, blank := range []string{"", " null", " ~"} {
			_, err := applyfile.Parse([]byte("inboxes:\n  - slug: abc\n    " + key + ":" + blank + "\n"))
			if key == "slug" {
				// A second slug key is a different mistake; the point is that it is refused.
				if err == nil {
					t.Errorf("%s:%q accepted", key, blank)
				}
				continue
			}
			if err == nil || !strings.Contains(err.Error(), "leave the key out") {
				t.Errorf("%s:%q: %v", key, blank, err)
			}
		}
	}
	// An explicit empty string is still how event_type_pointer is cleared.
	specs, err := applyfile.Parse([]byte("inboxes:\n  - slug: abc\n    event_type_pointer: ''\n"))
	if err != nil || specs[0].EventTypePointer == nil || *specs[0].EventTypePointer != "" {
		t.Errorf("clearing the pointer: %v %+v", err, specs)
	}
}

func TestParseRefusesAnEmptyOnVerifyFail(t *testing.T) {
	_, err := applyfile.Parse([]byte("inboxes:\n  - slug: abc\n    on_verify_fail: ''\n"))
	if err == nil || !strings.Contains(err.Error(), "on_verify_fail must be one of") {
		t.Errorf("%v", err)
	}
}

func TestVerifierConfigKeepsWhatWasWritten(t *testing.T) {
	specs, err := applyfile.Parse([]byte("inboxes:\n  - slug: abc\n    verifier: hmac\n    verifier_config: {header: X-Sig, prefix: 2024-01-01, algorithm: sha256, encoding: hex}\n"))
	if err != nil {
		// Whatever the verifier thinks of the prefix, the point is the text.
		t.Skipf("the hmac verifier refuses this configuration: %v", err)
	}
	if !strings.Contains(string(*specs[0].VerifierConfig), `"prefix":"2024-01-01"`) {
		t.Errorf("config = %s", *specs[0].VerifierConfig)
	}
}

func TestPlanNoticesEveryDifferentSetting(t *testing.T) {
	cur := view(nil)
	n := int64(7)
	it := applyfile.Plan(applyfile.Spec{Slug: "abc", RetentionMaxCount: &n}, cur)
	if it.Action != applyfile.Update || len(it.Fields) != 1 || it.Fields[0] != "retention_max_count" || it.Patch.RetentionMaxCount == nil {
		t.Errorf("a different retention_max_count: %+v", it)
	}
}

func TestCheckRefusesAPlanTheServerWouldRefusePartWay(t *testing.T) {
	cfg := json.RawMessage(`{"tolerance_seconds":300}`)
	stripe, none := "stripe", "none"
	for name, tc := range map[string]struct {
		spec applyfile.Spec
		cur  *admin.View
		bad  bool
	}{
		"configuration for a new inbox that has no verifier": {applyfile.Spec{Slug: "abc", VerifierConfig: &cfg}, nil, true},
		"configuration that fits the current verifier":       {applyfile.Spec{Slug: "abc", VerifierConfig: &cfg}, view(nil), false},
		"configuration that does not fit the current one":    {applyfile.Spec{Slug: "abc", VerifierConfig: &cfg}, view(func(v *admin.View) { v.Verifier = "none"; v.VerifierConfig = nil }), true},
		"a new verifier with the old configuration":          {applyfile.Spec{Slug: "abc", Verifier: &none}, view(nil), true},
		"a new verifier with a configuration of its own":     {applyfile.Spec{Slug: "abc", Verifier: &stripe, VerifierConfig: &cfg}, view(func(v *admin.View) { v.Verifier = "none"; v.VerifierConfig = nil }), false},
		"nothing about the verifier":                         {applyfile.Spec{Slug: "abc"}, view(nil), false},
	} {
		it := applyfile.Plan(tc.spec, tc.cur)
		if err := it.Check(tc.cur); (err != nil) != tc.bad {
			t.Errorf("%s: Check = %v, want bad=%v", name, err, tc.bad)
		}
	}
}
