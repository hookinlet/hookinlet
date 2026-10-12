// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build !unix

package sqlite

import "math"

// Where free space cannot be measured, only a write that fails says the disk is full.
func freeBytes(string) (uint64, error) { return math.MaxUint64, nil }
