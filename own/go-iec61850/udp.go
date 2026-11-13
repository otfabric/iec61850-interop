// SPDX-License-Identifier: MIT

package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/otfabric/go-iec61850/l2"
	"github.com/otfabric/go-iec61850/rsession"
)

// The UDP transport of the GOOSE and SV commands (--transport udp,
// contract 1.4, docs/routable.md): R-GOOSE and R-SV, the session protocol
// of IEC TR 61850-90-5, through go-iec61850's rsession. An
// rsession.Transport is an l2.Transport, so the commands run the same
// goose and sv publishers and subscribers over it as over a raw socket.

// openRSession opens the UDP transport; the tests replace it.
var openRSession = rsession.Open

// udpFlags are the transport flags shared by the GOOSE and SV commands.
type udpFlags struct {
	transport      *string
	remote         *string
	listen         *string
	group          *string
	mcastIface     *string
	ttl            *int
	keyID          *uint64
	key            *string
	mac            *string
	version        *int
	allowUnsecured *bool
}

func addUDPFlags(fs *flag.FlagSet) *udpFlags {
	return &udpFlags{
		transport:      fs.String("transport", "ethernet", "ethernet (GOOSE/SV on --iface) or udp (R-GOOSE/R-SV, IEC 61850-90-5)"),
		remote:         fs.String("remote", "", "udp: address:port to send to, unicast or multicast (port 102 when omitted)"),
		listen:         fs.String("listen", "", "udp: local address:port to receive on (:102 when there is no --group)"),
		group:          fs.String("group", "", "udp: multicast group[:port] to join"),
		mcastIface:     fs.String("mcast-iface", "", "udp: interface to join --group and send multicast on"),
		ttl:            fs.Int("ttl", 1, "udp: multicast hop limit"),
		keyID:          fs.Uint64("key-id", 0, "udp: key identifier; 0 sends unsecured"),
		key:            fs.String("key", "", "udp: key material, hex (at least 16 octets)"),
		mac:            fs.String("mac", "hmac-sha256-128", "udp: hmac-sha256-80, hmac-sha256-128 or hmac-sha256-256"),
		version:        fs.Int("session-version", 2, "udp: session protocol version sent, 1 or 2"),
		allowUnsecured: fs.Bool("allow-unsecured", false, "udp: accept SPDUs that carry no key"),
	}
}

// udp reports whether --transport udp was given; any value other than
// ethernet and udp is an argument error.
func (f *udpFlags) udp() (bool, error) {
	switch *f.transport {
	case "ethernet":
		return false, nil
	case "udp":
		return true, nil
	}
	return false, fmt.Errorf("--transport %q: want ethernet or udp", *f.transport)
}

var macAlgorithms = map[string]rsession.MACAlgorithm{
	"hmac-sha256-80":  rsession.MACHMACSHA256_80,
	"hmac-sha256-128": rsession.MACHMACSHA256_128,
	"hmac-sha256-256": rsession.MACHMACSHA256_256,
}

// keys returns the key ring of --key-id, --key and --mac: nil for key ID 0.
func (f *udpFlags) keys() (*rsession.KeyRing, error) {
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
	if err != nil || len(material) == 0 {
		return nil, errors.New("--key: want hex key material")
	}
	alg, ok := macAlgorithms[strings.ToLower(*f.mac)]
	if !ok {
		return nil, fmt.Errorf("--mac %q: want hmac-sha256-80, hmac-sha256-128 or hmac-sha256-256", *f.mac)
	}
	r, err := rsession.NewKeyRing(rsession.Key{ID: uint32(*f.keyID), Material: material, MAC: alg})
	if err != nil {
		return nil, fmt.Errorf("--key: %w", err)
	}
	return r, nil
}

// resolve resolves HOST[:PORT] (a name or an address; port 102 when
// omitted) to an address, IPv4 first. An empty host is the wildcard
// address, returned invalid.
func resolve(s string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), strconv.Itoa(rsession.DefaultPort)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%q: invalid port", s)
	}
	if host == "" {
		return netip.AddrPortFrom(netip.Addr{}, uint16(p)), nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(a.Unmap(), uint16(p)), nil
	}
	ua, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(host, port))
	if err != nil {
		if ua, err = net.ResolveUDPAddr("udp", net.JoinHostPort(host, port)); err != nil {
			return netip.AddrPort{}, err
		}
	}
	ap := ua.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
}

// openSender opens a send-only transport to --remote. It returns the
// transport and the label of its ready event (udp:REMOTE).
func (f *udpFlags) openSender(si rsession.SessionID) (*rsession.Transport, string, error) {
	if *f.remote == "" {
		return nil, "", errors.New("--transport udp needs --remote")
	}
	if *f.version != 1 && *f.version != 2 {
		return nil, "", fmt.Errorf("--session-version %d: want 1 or 2", *f.version)
	}
	if *f.ttl < 1 || *f.ttl > 255 {
		return nil, "", fmt.Errorf("--ttl %d: want 1 to 255", *f.ttl)
	}
	keys, err := f.keys()
	if err != nil {
		return nil, "", err
	}
	remote, err := resolve(*f.remote)
	if err != nil || !remote.Addr().IsValid() {
		return nil, "", fmt.Errorf("--remote %q: %v", *f.remote, err)
	}
	cfg := rsession.Config{
		SI: si, RemoteAddr: remote, Interface: *f.mcastIface, MulticastTTL: *f.ttl,
		Version: uint16(*f.version), Insecure: keys == nil,
	}
	if keys != nil {
		cfg.Keys = keys
	}
	t, err := openRSession(cfg)
	if err != nil {
		return nil, "", err
	}
	return t, "udp:" + *f.remote, nil
}

// openReceiver opens a receive-only transport on --listen and --group.
// Every SPDU it refuses is reported as a reject event. It returns the
// transport and the label of its ready event (udp:LISTEN or udp:GROUP).
func (f *udpFlags) openReceiver(si rsession.SessionID) (*rsession.Transport, string, error) {
	keys, err := f.keys()
	if err != nil {
		return nil, "", err
	}
	cfg := rsession.Config{SI: si, Interface: *f.mcastIface, Insecure: *f.allowUnsecured, OnReject: emitReject}
	// The library needs a key provider unless unsecured SPDUs are
	// accepted; without --key-id an empty ring refuses every secured SPDU
	// as unknown-key.
	if keys == nil && !*f.allowUnsecured {
		keys, _ = rsession.NewKeyRing()
	}
	if keys != nil {
		cfg.Keys = keys
	}
	listen, label := *f.listen, ""
	if *f.group != "" {
		g, err := resolve(*f.group)
		if err != nil || !g.Addr().IsMulticast() {
			return nil, "", fmt.Errorf("--group %q: not a multicast address", *f.group)
		}
		cfg.Groups = []netip.Addr{g.Addr()}
		cfg.LocalAddr = netip.AddrPortFrom(netip.Addr{}, g.Port())
		label = "udp:" + *f.group
	} else if listen == "" {
		listen = fmt.Sprintf(":%d", rsession.DefaultPort)
	}
	if listen != "" {
		l, err := resolve(listen)
		if err != nil {
			return nil, "", fmt.Errorf("--listen %q: %v", listen, err)
		}
		cfg.LocalAddr = l
		if label == "" {
			label = "udp:" + listen
		}
	}
	t, err := openRSession(cfg)
	if err != nil {
		return nil, "", err
	}
	return t, label, nil
}

// rejectReason classifies a refusal as the contract's reject reason.
func rejectReason(err error) string {
	switch {
	case errors.Is(err, rsession.ErrUnknownKey):
		return "unknown-key"
	case errors.Is(err, rsession.ErrAuthentication):
		return "authentication"
	case errors.Is(err, rsession.ErrUnsecured):
		return "unsecured"
	case errors.Is(err, rsession.ErrReplay):
		return "replay"
	}
	return "malformed"
}

func emitReject(from netip.AddrPort, err error) {
	emit(map[string]any{"event": "reject", "from": from.String(), "reason": rejectReason(err), "error": err.Error()})
}

// zeroMAC is the dst and src of the events of the UDP transport: an SPDU
// carries no Ethernet addresses.
var zeroMAC = l2.MAC{}.String()
