// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build unix

package sqlite

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// lockFile takes an exclusive, advisory lock on path, creating it if need be. The
// lock belongs to the open file, so the operating system drops it when the process
// ends, however it ends: there is no stale lock to clean up. The holder's pid is
// written into the file for whoever has to find it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the data directory plus a fixed name
	if err != nil {
		return nil, fmt.Errorf("sqlite: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		holder, _ := os.ReadFile(path) //nolint:gosec // the same file
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &LockedError{Path: path, Holder: string(holder)}
		}
		return nil, fmt.Errorf("sqlite: lock %s: %w", path, err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, nil
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor fits an int
	_ = f.Close()
}
