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
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// Secrets maps a slug to the verifier secret read for it. Only inboxes whose
// entry names a source appear. It is never printed or logged.
type Secrets map[string]string

// ResolveSecrets reads every declared secret source. A source that is missing
// or empty is an error that points at the line of the key, and all of them are
// reported together. It never repeats the variable name or the path: a value
// written under the wrong key could be the secret itself, and that must not be
// echoed.
func ResolveSecrets(specs []Spec, getenv func(string) string) (Secrets, error) {
	out := Secrets{}
	var errs []error
	for _, s := range specs {
		var secret string
		switch {
		case s.SecretEnv != "":
			secret = getenv(s.SecretEnv)
			if secret == "" {
				errs = append(errs, s.errorAt(s.SecretLine, "secret_env: the environment variable it names is not set or is empty"))
				continue
			}
		case s.SecretFile != "":
			b, err := readSecretFile(s.SecretFile)
			if err != nil {
				errs = append(errs, s.errorAt(s.SecretLine, "secret_file: %v", err))
				continue
			}
			// A file made by echo or a secrets manager usually ends in a newline
			// that is not part of the secret.
			secret = strings.TrimRight(string(b), "\r\n")
			if secret == "" {
				errs = append(errs, s.errorAt(s.SecretLine, "secret_file: the file it names is empty"))
				continue
			}
		default:
			continue
		}
		if len(secret) > admin.MaxSecretBytes {
			errs = append(errs, s.errorAt(s.SecretLine, "the secret is longer than %d bytes", admin.MaxSecretBytes))
			continue
		}
		out[s.Slug] = secret
	}
	return out, errors.Join(errs...)
}

func readSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the operator's own, from their file
	if err != nil {
		// The reason, not the path: see ResolveSecrets.
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("cannot read the file it names: %w", pe.Err)
		}
		return nil, errors.New("cannot read the file it names")
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("the file it names is not a regular file")
	}
	// One byte over what is allowed, plus room for a trailing newline, so that
	// a file that is too big is refused and not truncated.
	limit := int64(admin.MaxSecretBytes) + 3
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, errors.New("cannot read the file it names")
	}
	if int64(len(b)) >= limit {
		return nil, fmt.Errorf("the file it names is larger than a secret can be (%d bytes)", admin.MaxSecretBytes)
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
		if s.IngestKey != nil {
			p.IngestKey = *s.IngestKey
		}
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

// Check refuses, before anything is changed, a plan the server would refuse
// part way: the verifier an inbox will have after the change, with the
// configuration it will have, must fit together. It looks at the settings after
// the change, not only the ones the file declares, because a file may change the
// verifier and leave the configuration, or the reverse.
func (it Item) Check(cur *admin.View) error {
	s := it.Spec
	if cur == nil && s.Verifier == nil {
		return s.errorf("a new inbox needs a verifier: name how its deliveries are checked, or none to accept anything unchecked")
	}
	if cur != nil && s.IngestKey != nil && *s.IngestKey != cur.IngestKey {
		return s.errorf("ingest_key cannot be changed by apply; remove it from the file, or run: hookinlet inbox rotate-ingest-key %s", s.Slug)
	}
	if s.Verifier == nil && s.VerifierConfig == nil {
		return nil
	}
	verifier, config := "none", json.RawMessage(`{}`)
	if cur != nil {
		verifier, config = cur.Verifier, cur.VerifierConfig
	}
	if s.Verifier != nil {
		verifier = *s.Verifier
	}
	if s.VerifierConfig != nil {
		config = *s.VerifierConfig
	}
	if err := admin.CheckSettings(admin.CreateParams{Slug: s.Slug, Verifier: verifier, VerifierConfig: config}); err != nil {
		return s.errorf("verifier %s with its configuration: %s", verifier, strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
	}
	return nil
}

// CheckSecret refuses, before anything is changed, a secret that the verifier the
// inbox will have cannot use. Standard Webhooks, for one, needs whsec_ followed
// by base64.
func (it Item) CheckSecret(cur *admin.View, secret string) error {
	if secret == "" {
		return nil
	}
	s := it.Spec
	verifier, config := "none", json.RawMessage(`{}`)
	if cur != nil {
		verifier, config = cur.Verifier, cur.VerifierConfig
	}
	if s.Verifier != nil {
		verifier = *s.Verifier
	}
	if s.VerifierConfig != nil {
		config = *s.VerifierConfig
	}
	if err := admin.CheckSecret(verifier, config, secret); err != nil {
		return s.errorAt(s.SecretLine, "the secret cannot be used by the %s verifier: %s", verifier, strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
	}
	return nil
}
