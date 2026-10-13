// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package applyfile_test

import (
	"encoding/json"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/hookinlet/hookinlet/internal/applyfile"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// The fuzz target here runs its seeds in a normal `go test`. To fuzz it:
//
//	go test -run='^$' -fuzz=FuzzParse -fuzztime=90s ./internal/applyfile

const secretMarker = "zQxJ7markerValue" //nolint:gosec // a marker to look for in errors, not a credential

// markerIsData reports whether the marker sits somewhere an error may name: a
// key or an inbox's slug. Anything else the file says must not be repeated.
func markerIsData(n *yaml.Node) bool {
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && strings.Contains(c.Value, secretMarker) {
			return true
		}
		if n.Kind == yaml.MappingNode && i%2 == 1 && n.Content[i-1].Value == "slug" && strings.Contains(c.Value, secretMarker) {
			return true
		}
		if markerIsData(c) {
			return true
		}
	}
	return false
}

// FuzzParse: Parse must not panic or run away on any file, must not repeat a
// value from the file in an error, and what it accepts must be what it promises.
func FuzzParse(f *testing.F) {
	f.Add([]byte("inboxes:\n  - slug: stripe-prod\n    verifier: stripe\n    secret_env: STRIPE_SECRET\n    max_body_bytes: 1048576\n    verifier_config:\n      tolerance_seconds: 300\n"))
	f.Add([]byte("inboxes:\n  - slug: a-b\n    secret: " + secretMarker + "\n"))
	f.Add([]byte("inboxes:\n  - slug: a-b\n    name: " + secretMarker + "\n    retention_days: x" + secretMarker + "\n"))
	f.Add([]byte("inboxes:\n  - slug: a-b\n    verifier_config: {k: " + secretMarker + ", n: [1, 2e400, 2024-01-01, null, ~]}\n"))
	f.Add([]byte("inboxes: &a\n  - slug: abc\n"))
	f.Add([]byte("inboxes:\n  - slug: abc\n---\ninboxes: []\n"))
	f.Add([]byte("inboxes: [" + strings.Repeat("[", 200) + strings.Repeat("]", 200) + "]\n"))
	f.Add([]byte("inboxes:\n  - slug: abc\n    secret_env: " + secretMarker + " x\n    secret_file: /p\n"))
	f.Add([]byte("\xff\xfe\x00inboxes"))
	f.Fuzz(func(t *testing.T, data []byte) {
		specs, err := applyfile.Parse(data)
		if err != nil {
			if strings.Contains(err.Error(), secretMarker) {
				var doc yaml.Node
				if yaml.Unmarshal(data, &doc) != nil || !markerIsData(&doc) {
					t.Fatalf("an error repeats a value from the file: %v", err)
				}
			}
			return
		}
		if len(specs) == 0 {
			t.Fatal("no error and no inboxes")
		}
		seen := map[string]bool{}
		for _, s := range specs {
			if !inbox.ValidSlug(s.Slug) || seen[s.Slug] {
				t.Fatalf("accepted the slug %q (seen: %v)", s.Slug, seen[s.Slug])
			}
			seen[s.Slug] = true
			if s.SecretEnv != "" && s.SecretFile != "" {
				t.Fatal("two secret sources")
			}
			if s.VerifierConfig != nil {
				var obj map[string]json.RawMessage
				if json.Unmarshal(*s.VerifierConfig, &obj) != nil || obj == nil {
					t.Fatalf("verifier_config is not a JSON object: %s", *s.VerifierConfig)
				}
			}
			if s.Line < 1 {
				t.Fatalf("line %d", s.Line)
			}
		}
	})
}
