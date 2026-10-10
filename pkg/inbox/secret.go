// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import "log/slog"

const redacted = "[redacted]"

// SecretBytes holds bytes that must never reach logs, error messages, or API
// responses: sealed verifier secrets and agent-token hashes. Formatting,
// slog, and JSON encoding all emit "[redacted]". A []byte converts to and from
// SecretBytes freely, so code that genuinely needs the bytes can have them.
type SecretBytes []byte

// String implements fmt.Stringer.
func (SecretBytes) String() string { return redacted }

// GoString implements fmt.GoStringer so %#v does not leak.
func (SecretBytes) GoString() string { return "inbox.SecretBytes(" + redacted + ")" }

// LogValue implements slog.LogValuer.
func (SecretBytes) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler.
func (SecretBytes) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler, which covers YAML, TOML, and
// map-key encodings.
func (SecretBytes) MarshalText() ([]byte, error) { return []byte(redacted), nil }
