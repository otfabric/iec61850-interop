// SPDX-License-Identifier: GPL-3.0-only

// Package rudp is the UDP transport of the GOOSE and SV commands
// (--transport udp, contract 1.4): R-GOOSE and R-SV, the session protocol
// of IEC TR 61850-90-5, through dscsystems/go-iec61850's rsession. A
// Session is an ethernet.Interface, so the commands run their publishers
// and decoders over it as they do over a raw socket.
package rudp

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"strings"

	"github.com/dscsystems/go-iec61850/ethernet"
	"github.com/dscsystems/go-iec61850/rsession"

	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

// Flags are the transport flags of a GOOSE or SV command.
type Flags struct {
	transport      *string
	remote         *string
	listen         *string
	group          *string
	mcastIface     *string
	ttl            *int
	keyID          *uint
	key            *string
	mac            *string
	version        *int
	allowUnsecured *bool
}

// Register defines the transport flags on the default flag set; call it
// before flag.Parse.
func Register() *Flags {
	return &Flags{
		transport:      flag.String("transport", "ethernet", "ethernet (GOOSE/SV on --iface) or udp (R-GOOSE/R-SV, IEC 61850-90-5)"),
		remote:         flag.String("remote", "", "udp: address:port to send to, unicast or multicast (port 102 when omitted)"),
		listen:         flag.String("listen", "", "udp: local address:port to receive on (:102 when neither --listen nor --group)"),
		group:          flag.String("group", "", "udp: multicast group[:port] to join"),
		mcastIface:     flag.String("mcast-iface", "", "udp: interface to join --group and send multicast on"),
		ttl:            flag.Int("ttl", 0, "udp: multicast hop limit (1 when 0)"),
		keyID:          flag.Uint("key-id", 0, "udp: key identifier; 0 sends unsecured"),
		key:            flag.String("key", "", "udp: key material, hex (at least 16 octets)"),
		mac:            flag.String("mac", "hmac-sha256-128", "udp: hmac-sha256-80, hmac-sha256-128 or hmac-sha256-256"),
		version:        flag.Int("session-version", 2, "udp: session protocol version sent, 1 or 2"),
		allowUnsecured: flag.Bool("allow-unsecured", false, "udp: accept SPDUs that carry no key"),
	}
}

// UDP reports whether --transport udp was given; any value other than
// ethernet and udp is an argument error.
func (f *Flags) UDP() (bool, error) {
	switch *f.transport {
	case "ethernet":
		return false, nil
	case "udp":
		return true, nil
	}
	return false, fmt.Errorf("--transport %q: want ethernet or udp", *f.transport)
}

var macs = map[string]rsession.SigAlgorithm{
	"hmac-sha256-80":  rsession.SigHMACSHA256_80,
	"hmac-sha256-128": rsession.SigHMACSHA256_128,
	"hmac-sha256-256": rsession.SigHMACSHA256_256,
}

// keys builds the key store of --key-id, --key and --mac; nil for key ID 0.
func (f *Flags) keys() (*rsession.KeyStore, error) {
	if *f.keyID == 0 {
		if *f.key != "" {
			return nil, errors.New("--key without --key-id")
		}
		return nil, nil
	}
	if *f.keyID > 0xFFFFFFFF {
		return nil, fmt.Errorf("--key-id %d out of range", *f.keyID)
	}
	material, err := hex.DecodeString(*f.key)
	if err != nil {
		return nil, fmt.Errorf("--key: %w", err)
	}
	if len(material) == 0 {
		return nil, errors.New("--key: want hex key material")
	}
	sig, ok := macs[strings.ToLower(*f.mac)]
	if !ok {
		return nil, fmt.Errorf("--mac %q: want hmac-sha256-80, hmac-sha256-128 or hmac-sha256-256", *f.mac)
	}
	return rsession.NewKeyStore(rsession.Key{ID: uint32(*f.keyID), Material: material, Sig: sig})
}

// OpenSender opens a send-only session to --remote.
func (f *Flags) OpenSender() (*Session, error) {
	if *f.remote == "" {
		return nil, errors.New("--transport udp needs --remote")
	}
	keys, err := f.keys()
	if err != nil {
		return nil, err
	}
	s, err := rsession.Open(rsession.Config{
		Remote: *f.remote, Interface: *f.mcastIface, TTL: *f.ttl,
		Keys: keys, Version: *f.version,
	})
	if err != nil {
		return nil, err
	}
	return &Session{s: s, label: "udp:" + *f.remote}, nil
}

// OpenReceiver opens a receive-only session on --listen and --group. Every
// SPDU it refuses is reported as a reject event.
func (f *Flags) OpenReceiver() (*Session, error) {
	keys, err := f.keys()
	if err != nil {
		return nil, err
	}
	listen := *f.listen
	var groups []string
	if *f.group != "" {
		groups = []string{*f.group}
	} else if listen == "" {
		listen = fmt.Sprintf(":%d", rsession.DefaultPort)
	}
	s, err := rsession.Open(rsession.Config{
		Listen: listen, Groups: groups, Interface: *f.mcastIface,
		Keys: keys, AllowUnsecured: *f.allowUnsecured, OnReject: emitReject,
	})
	if err != nil {
		return nil, err
	}
	label := "udp:" + listen
	if len(groups) > 0 {
		label = "udp:" + groups[0]
	}
	return &Session{s: s, label: label}, nil
}

// Reject is the event of an SPDU the receiver refused.
type Reject struct {
	Event  string `json:"event"`
	From   string `json:"from"`
	Reason string `json:"reason"`
	Error  string `json:"error"`
}

// Reason classifies a refusal: unknown-key, authentication, unsecured,
// replay or malformed.
func Reason(err error) string {
	switch {
	case errors.Is(err, rsession.ErrUnknownKey):
		return "unknown-key"
	case errors.Is(err, rsession.ErrAuthentication), errors.Is(err, rsession.ErrAlgorithm):
		return "authentication"
	case errors.Is(err, rsession.ErrUnsecured):
		return "unsecured"
	case errors.Is(err, rsession.ErrReplay):
		return "replay"
	}
	return "malformed"
}

func emitReject(r rsession.Reject) {
	from := ""
	if r.From != nil {
		from = r.From.String()
	}
	jl.Emit(Reject{"reject", from, Reason(r.Err), r.Err.Error()})
}

// Session is an rsession.Session that remembers the sender of the last
// frame it read.
type Session struct {
	s       *rsession.Session
	label   string
	pending []*ethernet.Frame
	from    string
}

// Label names the session for the ready event: udp:ADDRESS.
func (s *Session) Label() string { return s.label }

// From is the UDP sender of the last frame ReadFrame returned.
func (s *Session) From() string { return s.from }

// WriteFrame sends the frame's APDU as an SPDU (rsession.Session.WriteFrame).
func (s *Session) WriteFrame(f *ethernet.Frame) error { return s.s.WriteFrame(f) }

// ReadFrame returns the next GOOSE or SV payload of an accepted SPDU as a
// frame of its EtherType; one SPDU may carry several. The addresses are
// zero: an SPDU has none. Not safe for concurrent use.
func (s *Session) ReadFrame() (*ethernet.Frame, error) {
	for len(s.pending) == 0 {
		spdu, from, err := s.s.Receive()
		if err != nil {
			return nil, err
		}
		s.from = addr(from)
		s.pending = Frames(spdu)
	}
	f := s.pending[0]
	s.pending = s.pending[1:]
	return f, nil
}

// Close closes the session.
func (s *Session) Close() error { return s.s.Close() }

func addr(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

// Frames rebuilds the GOOSE and SV frames of an SPDU: the layer-2 header
// (APPID, length, Reserved 1 with the simulation bit, Reserved 2) and the
// APDU, as on Ethernet. Payloads of other types are skipped.
func Frames(spdu *rsession.SPDU) []*ethernet.Frame {
	var out []*ethernet.Frame
	for _, p := range spdu.Payloads {
		var et uint16
		switch p.Type {
		case rsession.PayloadGOOSE:
			et = ethernet.EtherTypeGOOSE
		case rsession.PayloadSV:
			et = ethernet.EtherTypeSV
		default:
			continue
		}
		n := 8 + len(p.APDU)
		if n > 0xFFFF {
			continue
		}
		var r1 byte
		if p.Simulation {
			r1 = 0x80
		}
		b := make([]byte, 0, n)
		b = append(b, byte(p.AppID>>8), byte(p.AppID), byte(n>>8), byte(n), r1, 0, 0, 0)
		b = append(b, p.APDU...)
		out = append(out, &ethernet.Frame{EtherType: et, Payload: b})
	}
	return out
}

var _ ethernet.Interface = (*Session)(nil)
