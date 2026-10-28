// SPDX-License-Identifier: GPL-3.0-only

//go:build !linux

package rawsock

import "errors"

// Socket is a raw AF_PACKET socket; Linux only.
type Socket struct{}

var errUnsupported = errors.New("rawsock: AF_PACKET needs Linux")

// Open fails off Linux.
func Open(string, uint16) (*Socket, int, error) { return nil, 0, errUnsupported }

// Read fails off Linux.
func (*Socket) Read() ([]byte, error) { return nil, errUnsupported }

// Drops is zero off Linux.
func (*Socket) Drops() uint32 { return 0 }

// Close does nothing off Linux.
func (*Socket) Close() error { return nil }
