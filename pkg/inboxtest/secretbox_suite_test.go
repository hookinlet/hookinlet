// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

// remoteBox stands for a box that asks a key service: before each call it checks
// the context, as a network request would.
type remoteBox struct{ *secretbox.Box }

func (b remoteBox) SealContext(ctx context.Context, pt, binding []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("key service: %w", err)
	}
	return b.Seal(pt, binding)
}

func (b remoteBox) OpenContext(ctx context.Context, ct, binding []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("key service: %w", err)
	}
	return b.Open(ct, binding)
}

func TestContextSecretBoxSuiteOnABoxThatHonoursTheContext(t *testing.T) {
	newBox := func(t *testing.T) inbox.SecretBox {
		b, err := secretbox.New(bytes.Repeat([]byte{9}, secretbox.KeySize))
		if err != nil {
			t.Fatal(err)
		}
		return remoteBox{b}
	}
	SecretBoxSuite(t, newBox)
	ContextSecretBoxSuite(t, newBox)
}
