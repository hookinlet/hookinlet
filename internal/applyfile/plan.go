// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package applyfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/hookinlet/hookinlet/internal/admin"
)

// Secrets maps a slug to the verifier secret read for it. Only inboxes whose
// entry names a source appear. It is never printed or logged.
type Secrets map[string]string

// ResolveSecrets reads every declared secret source. A source that is missing
// or empty is an error that names the variable or the path (never a value),
// and all of them are reported together.
func ResolveSecrets(specs []Spec, getenv func(string) string) (Secrets, error) {
	out := Secrets{}
	var errs []error
	for _, s := range specs {
		var secret string
		switch {
		case s.SecretEnv != "":
			secret = getenv(s.SecretEnv)
			if secret == "" {
				errs = append(errs, s.errorf("secret_env: the environment variable %s is not set or is empty", s.SecretEnv))
				continue
			}
		case s.SecretFile != "":
			b, err := readSecretFile(s.SecretFile)
			if err != nil {
				errs = append(errs, s.errorf("secret_file: %v", err))
				continue
			}
			// A file made by echo or a secrets manager usually ends in a newline
			// that is not part of the secret.
			secret = strings.TrimRight(string(b), "\r\n")
			if secret == "" {
				errs = append(errs, s.errorf("secret_file: %s is empty", s.SecretFile))
				continue
			}
		default:
			continue
		}
		if len(secret) > admin.MaxSecretBytes {
			errs = append(errs, s.errorf("the secret is longer than %d bytes", admin.MaxSecretBytes))
			continue
		}
		out[s.Slug] = secret
	}
	return out, errors.Join(errs...)
}

func readSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the operator's own, from their file
	if err != nil {
		// The error text names the path and the reason, not the contents.
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("cannot read %s: %w", path, pe.Err)
		}
		return nil, fmt.Errorf("cannot read %s", path)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	// One byte over what is allowed, plus room for a trailing newline, so that
	// a file that is too big is refused and not truncated.
	limit := int64(admin.MaxSecretBytes) + 3
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", path)
	}
	if int64(len(b)) >= limit {
		return nil, fmt.Errorf("%s is larger than a secret can be (%d bytes)", path, admin.MaxSecretBytes)
	}
	return b, nil
}

// Action is what has to happen to one inbox.
type Action int

const (
	Unchanged Action = iota
	Create
	Update
)

// Item is the plan for one declared inbox.
type Item struct {
	Spec   Spec
	Action Action
	// Fields lists the settings that differ, by their names in the file. It is
	// empty for Create (everything declared is set) and for Unchanged.
	Fields []string
	// Create is what to create, with no secret in it.
	Create admin.CreateParams
	// Patch is what to change on an existing inbox, or the zero Patch.
	Patch admin.Patch
	// SetSecret is true when a secret source is declared: the secret cannot be
	// read back, so it is set on every run (on a new inbox, as part of Create).
	SetSecret bool
}

// Plan works out what to do for one spec given the inbox as it is now, or nil
// if there is none.
func Plan(s Spec, cur *admin.View) Item {
	it := Item{Spec: s, SetSecret: s.HasSecretSource()}
	if cur == nil {
		it.Action = Create
		p := admin.CreateParams{Slug: s.Slug, VerifierConfig: json.RawMessage(`{}`)}
		if s.Name != nil {
			p.Name = *s.Name
		}
		if s.Verifier != nil {
			p.Verifier = *s.Verifier
		}
		if s.VerifierConfig != nil {
			p.VerifierConfig = *s.VerifierConfig
		}
		if s.OnVerifyFail != nil {
			p.OnVerifyFail = *s.OnVerifyFail
		}
		if s.MaxBodyBytes != nil {
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
		it.Create = p
		// A new inbox can be declared disabled: it is created, then switched.
		if s.Disabled != nil && *s.Disabled {
			it.Patch.Disabled = s.Disabled
		}
		return it
	}
	change := func(name string) { it.Fields = append(it.Fields, name) }
	if s.Name != nil && *s.Name != cur.Name {
		it.Patch.Name = s.Name
		change("name")
	}
	if s.Verifier != nil && *s.Verifier != cur.Verifier {
		it.Patch.Verifier = s.Verifier
		change("verifier")
	}
	if s.VerifierConfig != nil && !sameJSON(*s.VerifierConfig, cur.VerifierConfig) {
		it.Patch.VerifierConfig = s.VerifierConfig
		change("verifier_config")
	}
	if s.OnVerifyFail != nil && *s.OnVerifyFail != cur.OnVerifyFail {
		it.Patch.OnVerifyFail = s.OnVerifyFail
		change("on_verify_fail")
	}
	if s.MaxBodyBytes != nil && *s.MaxBodyBytes != cur.MaxBodyBytes {
		it.Patch.MaxBodyBytes = s.MaxBodyBytes
		change("max_body_bytes")
	}
	if s.RetentionDays != nil && *s.RetentionDays != cur.RetentionDays {
		it.Patch.RetentionDays = s.RetentionDays
		change("retention_days")
	}
	if s.RetentionMaxCount != nil && *s.RetentionMaxCount != cur.RetentionMaxCount {
		it.Patch.RetentionMaxCount = s.RetentionMaxCount
		change("retention_max_count")
	}
	if s.EventTypePointer != nil && *s.EventTypePointer != cur.EventTypePointer {
		it.Patch.EventTypePointer = s.EventTypePointer
		change("event_type_pointer")
	}
	if s.Disabled != nil && *s.Disabled != cur.Disabled {
		it.Patch.Disabled = s.Disabled
		change("disabled")
	}
	if len(it.Fields) > 0 {
		it.Action = Update
	}
	return it
}

// sameJSON compares two JSON documents by meaning: key order, spacing, and the
// difference between 300 and 300.0 do not matter.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if len(bytes.TrimSpace(a)) == 0 {
		a = json.RawMessage(`{}`)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		b = json.RawMessage(`{}`)
	}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
