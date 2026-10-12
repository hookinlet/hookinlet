// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build unix

package server

import "syscall"

type rawConn = syscall.RawConn

// smallReceiveBuffer gives a socket a receive buffer so small that a response cannot be
// taken into it, which is what makes a client that does not read hold its answer.
func smallReceiveBuffer(c syscall.RawConn) error {
	return c.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
}
