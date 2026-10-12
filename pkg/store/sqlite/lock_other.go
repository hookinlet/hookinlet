// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build !unix

package sqlite

import "os"

// Where there is no flock the data directory is not locked: running two servers on
// one directory is then the operator's to avoid.
func lockFile(string) (*os.File, error) { return nil, nil }

func unlockFile(*os.File) {}
