// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func TestReaderNames(t *testing.T) {
	for _, name := range []string{"a", "Z", "0", "billing-worker", "a.b_c-d", "9lives", strings.Repeat("x", inbox.MaxReaderNameLen)} {
		if err := inbox.ValidateReaderName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{
		"", "-a", ".a", "_a", "a b", "a/b", "a\\b", "a?b", "a#b", "a%2fb", "a:b", "a@b", "é", "a\u202eb", "a\x00", "a\n", "..",
		strings.Repeat("x", inbox.MaxReaderNameLen+1),
	} {
		if err := inbox.ValidateReaderName(name); !errors.Is(err, inbox.ErrInvalid) {
			t.Errorf("%q: %v, want an error wrapping ErrInvalid", name, err)
		}
	}
}
