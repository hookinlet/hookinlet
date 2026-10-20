// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import (
	"time"
)

// MaxReaderNameLen bounds the name of a named reader, in characters.
const MaxReaderNameLen = 64

// MaxNamedReaders is the largest number of named readers one inbox may be given,
// whatever a server is configured with. A Store may refuse a larger cap.
const MaxNamedReaders = 1000

// ReaderPosition is how far a named reader has read an inbox: the last sequence
// number it said it has handled, in one epoch of the inbox. A position means
// nothing in another epoch, and a Store never reports one from an earlier epoch
// than the inbox's current one.
type ReaderPosition struct {
	// Name is the reader's name: 1 to MaxReaderNameLen characters of ASCII letters
	// and digits, '.', '_' and '-', starting with a letter or digit (see
	// ValidateReaderName). Names are unique within an inbox.
	Name string
	// Seq is the last sequence number the reader has handled. 0 means none yet.
	Seq int64
	// Epoch is the inbox's epoch (Inbox.Epoch) that Seq belongs to.
	Epoch string
	// UpdatedAt is when the position was last set.
	UpdatedAt time.Time
}

// ValidateReaderName checks a reader's name. Errors wrap ErrInvalid. The set is
// small on purpose: a name is used in a URL path and in logs and is never
// interpreted, so nothing that needs escaping or could pass for something else is
// allowed.
func ValidateReaderName(name string) error {
	if name == "" || len(name) > MaxReaderNameLen {
		return Invalidf("reader name must be 1-%d characters", MaxReaderNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '.' && c != '_' && c != '-') {
			return Invalidf("reader name may contain only letters, digits, '.', '_' and '-', and must start with a letter or digit")
		}
	}
	return nil
}
