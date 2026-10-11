// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package applyfile reads the declarative inbox file of "hookinlet apply" and
// works out what has to change to match it.
//
// The format is deliberately small: one list of inboxes, whose fields are the
// admin API's own. Parsing is strict, because a file that is silently
// misunderstood would be applied to production: an unknown key, a repeated
// key, a literal secret, an anchor or alias, or a second document is an error
// that names the key and the line. No error message ever repeats a value from
// the file, so a secret pasted in the wrong place is not echoed.
package applyfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// MaxFileBytes bounds the file (and standard input) that is read.
const MaxFileBytes = 1 << 20

// Spec is one inbox as the file declares it. A nil field was not declared and
// is left alone on an existing inbox.
type Spec struct {
	Line int // where the entry starts, for messages

	Slug              string
	Name              *string
	Verifier          *string
	VerifierConfig    *json.RawMessage
	OnVerifyFail      *string
	MaxBodyBytes      *int64
	RetentionDays     *int
	RetentionMaxCount *int64
	EventTypePointer  *string
	Disabled          *bool

	// SecretEnv and SecretFile say where the verifier secret is read from; at
	// most one is set. The secret itself is never in a Spec.
	SecretEnv  string
	SecretFile string
	// SecretLine is the line of the secret_env or secret_file key. Errors point
	// there, because the value of a mistyped key could be the secret itself.
	SecretLine int
}

// HasSecretSource reports whether the file says where to read a secret from.
func (s Spec) HasSecretSource() bool { return s.SecretEnv != "" || s.SecretFile != "" }

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse reads and validates a file. It returns every problem it can find in an
// entry's fields, one per line, but stops at a structural error.
func Parse(data []byte) ([]Spec, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("the file is larger than %d bytes", MaxFileBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty; it needs an \"inboxes\" list")
		}
		return nil, syntaxError(err)
	}
	var second yaml.Node
	switch err := dec.Decode(&second); {
	case err == nil:
		return nil, fmt.Errorf("line %d: only one YAML document is allowed (a second one starts here)", second.Line)
	case !errors.Is(err, io.EOF):
		return nil, syntaxError(err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("the file is not a YAML document")
	}
	if err := noAliases(doc.Content[0]); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: the file must be a mapping with one key, \"inboxes\"", root.Line)
	}
	var list *yaml.Node
	seen := map[string]bool{}
	for i := 0; i < len(root.Content); i += 2 {
		k := root.Content[i]
		if seen[k.Value] {
			return nil, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
		}
		seen[k.Value] = true
		if k.Value != "inboxes" {
			return nil, fmt.Errorf("line %d: unknown key %q (the only top-level key is \"inboxes\")", k.Line, k.Value)
		}
		list = root.Content[i+1]
	}
	if list == nil {
		return nil, errors.New("the file has no \"inboxes\" key")
	}
	if list.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: inboxes must be a list", list.Line)
	}
	if len(list.Content) == 0 {
		return nil, fmt.Errorf("line %d: inboxes is empty", list.Line)
	}

	var specs []Spec
	var errs []error
	firstLine := map[string]int{}
	for _, item := range list.Content {
		spec, err := parseEntry(item)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if prev, dup := firstLine[spec.Slug]; dup {
			errs = append(errs, fmt.Errorf("line %d: inbox %q is declared twice (first at line %d)", item.Line, spec.Slug, prev))
			continue
		}
		firstLine[spec.Slug] = item.Line
		if err := check(spec); err != nil {
			errs = append(errs, err)
			continue
		}
		specs = append(specs, spec)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return specs, nil
}

// syntaxError reports a YAML syntax error without quoting the input.
func syntaxError(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: ")
	return fmt.Errorf("the file is not valid YAML: %s", msg)
}

func noAliases(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return fmt.Errorf("line %d: anchors and aliases are not supported", n.Line)
	}
	for _, c := range n.Content {
		if err := noAliases(c); err != nil {
			return err
		}
	}
	return nil
}

func parseEntry(n *yaml.Node) (Spec, error) {
	spec := Spec{Line: n.Line}
	if n.Kind != yaml.MappingNode {
		return spec, fmt.Errorf("line %d: each inbox must be a mapping with at least a slug", n.Line)
	}
	seen := map[string]int{}
	var slugSet bool
	var errs []error
	fail := func(line int, key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("line %d: %s: %s", line, key, fmt.Sprintf(format, args...)))
	}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		key := k.Value
		if first, dup := seen[key]; dup {
			errs = append(errs, fmt.Errorf("line %d: duplicate key %q (first at line %d)", k.Line, key, first))
			continue
		}
		seen[key] = k.Line
		decode := func(out any, want string) bool {
			// A key with no value decodes to the zero value, which would be applied
			// as a setting: turning retention off, or re-enabling a disabled inbox.
			if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
				fail(v.Line, key, "has no value; leave the key out to leave the setting alone")
				return false
			}
			if v.Kind != yaml.ScalarNode && want != "mapping" {
				fail(v.Line, key, "must be %s", want)
				return false
			}
			if err := v.Decode(out); err != nil {
				fail(v.Line, key, "must be %s", want)
				return false
			}
			return true
		}
		switch key {
		case "slug":
			slugSet = decode(&spec.Slug, "a string")
		case "name":
			var s string
			if decode(&s, "a string") {
				spec.Name = &s
			}
		case "verifier":
			var s string
			if decode(&s, "a string") {
				spec.Verifier = &s
			}
		case "on_verify_fail":
			var s string
			if decode(&s, "a string") {
				spec.OnVerifyFail = &s
			}
		case "event_type_pointer":
			var s string
			if decode(&s, "a string") {
				spec.EventTypePointer = &s
			}
		case "max_body_bytes":
			var x int64
			if decode(&x, "an integer") {
				spec.MaxBodyBytes = &x
			}
		case "retention_days":
			var x int
			if decode(&x, "an integer") {
				spec.RetentionDays = &x
			}
		case "retention_max_count":
			var x int64
			if decode(&x, "an integer") {
				spec.RetentionMaxCount = &x
			}
		case "disabled":
			var x bool
			if decode(&x, "true or false") {
				spec.Disabled = &x
			}
		case "verifier_config":
			if v.Kind != yaml.MappingNode {
				fail(v.Line, key, "must be a mapping")
				break
			}
			m, err := nodeValue(v)
			if err != nil {
				fail(v.Line, key, "must be a mapping with string keys and plain values (%s)", err)
				break
			}
			raw, err := json.Marshal(m)
			if err != nil {
				fail(v.Line, key, "must be a mapping that can be written as JSON")
				break
			}
			rm := json.RawMessage(raw)
			spec.VerifierConfig = &rm
		case "secret_env":
			var s string
			if decode(&s, "the name of an environment variable") {
				if !envNameRE.MatchString(s) {
					fail(v.Line, key, "must be the name of an environment variable, not the secret itself")
				} else {
					spec.SecretEnv = s
					spec.SecretLine = k.Line
				}
			}
		case "secret_file":
			var s string
			if decode(&s, "a file path") {
				if s == "" {
					fail(v.Line, key, "must not be empty")
				} else {
					spec.SecretFile = s
					spec.SecretLine = k.Line
				}
			}
		case "secret", "verifier_secret":
			fail(k.Line, key, "a secret must not be written in the file; use secret_env (an environment variable) or secret_file (a path to a file that holds it)")
		default:
			fail(k.Line, key, "unknown key")
		}
	}
	if !slugSet && seen["slug"] == 0 {
		errs = append(errs, fmt.Errorf("line %d: slug is required", n.Line))
	}
	if spec.SecretEnv != "" && spec.SecretFile != "" {
		errs = append(errs, fmt.Errorf("line %d: secret_env and secret_file are mutually exclusive; give only one", n.Line))
	}
	return spec, errors.Join(errs...)
}

// nodeValue reads a YAML value as the plain JSON value it stands for. Unlike
// decoding into any, it keeps a timestamp-looking scalar as the text that was
// written (a header named 2024-01-01 is a string, not a date), and refuses what
// JSON cannot hold.
func nodeValue(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a key must be a plain string", k.Line)
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			x, err := nodeValue(v)
			if err != nil {
				return nil, err
			}
			m[k.Value] = x
		}
		return m, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			x, err := nodeValue(c)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str", "!!timestamp":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool", "!!int", "!!float":
			var x any
			if err := n.Decode(&x); err != nil {
				return nil, fmt.Errorf("line %d: not a plain value", n.Line)
			}
			return x, nil
		}
		return nil, fmt.Errorf("line %d: a value of type %s cannot be used here", n.Line, n.Tag)
	}
	return nil, fmt.Errorf("line %d: not a plain value", n.Line)
}

// check applies the admin API's own rules to what the entry declares. The
// fields it did not declare are given values that are always valid, so only
// declared ones can fail.
func check(s Spec) error {
	p := admin.CreateParams{Slug: s.Slug}
	if s.Name != nil {
		p.Name = *s.Name
		if p.Name == "" {
			return s.errorf("name must be 1-200 characters of valid UTF-8")
		}
	}
	if s.Verifier != nil {
		p.Verifier = *s.Verifier
		if p.Verifier == "" {
			return s.errorf("verifier must be 1-32 characters from a-z, 0-9, and hyphen")
		}
	}
	if s.VerifierConfig != nil {
		// Whether a configuration fits a verifier is checked only when the
		// verifier is declared too; otherwise it depends on the inbox's
		// current verifier, which the server will check when it is applied.
		if s.Verifier != nil {
			p.VerifierConfig = *s.VerifierConfig
		}
	}
	if s.OnVerifyFail != nil {
		// Checked here because an empty value is "the default" to the rules below,
		// and a file that declares it would never converge.
		if v := *s.OnVerifyFail; v != inbox.OnVerifyFailReject && v != inbox.OnVerifyFailStore {
			return s.errorf("on_verify_fail must be one of: reject, store")
		}
		p.OnVerifyFail = *s.OnVerifyFail
	}
	if s.MaxBodyBytes != nil {
		if *s.MaxBodyBytes == 0 {
			return s.errorf("max_body_bytes must not be 0: leave it out to get the default")
		}
		p.MaxBodyBytes = *s.MaxBodyBytes
	}
	if s.RetentionDays != nil {
		p.RetentionDays = *s.RetentionDays
	}
	if s.RetentionMaxCount != nil {
		p.RetentionMaxCount = *s.RetentionMaxCount
	}
	if s.EventTypePointer != nil {
		p.EventTypePointer = *s.EventTypePointer
	}
	if err := admin.CheckSettings(p); err != nil {
		return s.errorf("%s", strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
	}
	return nil
}

// errorAt is errorf for a message about the key on another line.
func (s Spec) errorAt(line int, format string, args ...any) error {
	return fmt.Errorf("line %d: inbox %q: %s", line, s.Slug, fmt.Sprintf(format, args...))
}

func (s Spec) errorf(format string, args ...any) error {
	return fmt.Errorf("line %d: inbox %q: %s", s.Line, s.Slug, fmt.Sprintf(format, args...))
}
