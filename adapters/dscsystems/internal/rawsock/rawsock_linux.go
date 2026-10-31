// SPDX-License-Identifier: GPL-3.0-only

//go:build linux

package rawsock

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Socket is a raw AF_PACKET socket bound to one interface and EtherType.
type Socket struct {
	fd     int
	buf    []byte
	closed atomic.Bool
}

func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

// Open binds a socket to iface receiving etherType (matched after VLAN
// decapsulation) and returns it with the receive buffer it got, in bytes.
func Open(iface string, etherType uint16) (*Socket, int, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, 0, fmt.Errorf("rawsock: %w", err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(etherType)))
	if err != nil {
		return nil, 0, fmt.Errorf("rawsock: socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(etherType), Ifindex: ifi.Index}); err != nil {
		_ = unix.Close(fd)
		return nil, 0, fmt.Errorf("rawsock: bind %s: %w", iface, err)
	}
	// SO_RCVBUFFORCE passes rmem_max with CAP_NET_ADMIN; without it,
	// SO_RCVBUF takes what rmem_max allows.
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, RcvBuf) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, RcvBuf)
	}
	got, _ := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF)
	tv := unix.NsecToTimeval(int64(pollInterval))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return nil, 0, fmt.Errorf("rawsock: SO_RCVTIMEO: %w", err)
	}
	return &Socket{fd: fd, buf: make([]byte, 9216)}, got, nil
}

// Read returns the next frame. The slice is reused by the next Read.
func (s *Socket) Read() ([]byte, error) {
	for {
		if s.closed.Load() {
			return nil, net.ErrClosed
		}
		n, _, err := unix.Recvfrom(s.fd, s.buf, 0)
		switch err {
		case nil:
			return s.buf[:n], nil
		case unix.EAGAIN, unix.EINTR:
			continue
		}
		if s.closed.Load() {
			return nil, net.ErrClosed
		}
		return nil, fmt.Errorf("rawsock: read: %w", err)
	}
}

// Drops returns the frames the kernel dropped for this socket since the
// previous call (PACKET_STATISTICS).
func (s *Socket) Drops() uint32 {
	st, err := unix.GetsockoptTpacketStats(s.fd, unix.SOL_PACKET, unix.PACKET_STATISTICS)
	if err != nil {
		return 0
	}
	return st.Drops
}

// Close closes the socket; a concurrent Read returns within pollInterval.
func (s *Socket) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return unix.Close(s.fd)
}
