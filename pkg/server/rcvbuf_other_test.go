// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build !unix

package server

import "syscall"

type rawConn = syscall.RawConn

// smallReceiveBuffer does nothing here; the test that uses it still runs, less sharply.
func smallReceiveBuffer(syscall.RawConn) error { return nil }

// smallSendBuffer does nothing here either.
func smallSendBuffer(syscall.RawConn) error { return nil }
