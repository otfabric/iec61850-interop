// SPDX-License-Identifier: GPL-3.0-only

// Package goosecli holds what the two GOOSE commands share: flag value
// parsing, the on-wire helpers the dscsystems goose package does not
// expose (the Edition 2 simulation bit), and the event shapes.
package goosecli

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

// simulationBit is the S bit of Reserved1 (IEC 61850-8-1 Ed2, Annex C):
// the most significant bit of the first reserved octet, which is octet 4
// of the APDU (after APPID and Length).
const (
	simulationOctet = 4
	simulationMask  = 0x80
)

// SetSimulation sets or clears the S bit in a GOOSE APDU in place.
func SetSimulation(apdu []byte, on bool) {
	if len(apdu) <= simulationOctet {
		return
	}
	if on {
		apdu[simulationOctet] |= simulationMask
	} else {
		apdu[simulationOctet] &^= simulationMask
	}
}

// Simulation reports the S bit of a GOOSE APDU.
func Simulation(apdu []byte) bool {
	return len(apdu) > simulationOctet && apdu[simulationOctet]&simulationMask != 0
}

// ParseAppID parses "0x1001", "1001" (hex, as in SCL) or "4097" with an
// explicit "0x" required for hex. A bare number is decimal.
func ParseAppID(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 0, 16)
	if err != nil {
		return 0, fmt.Errorf("app-id %q: %w", s, err)
	}
	return uint16(n), nil
}

// FormatAppID renders an APPID as the contract does: "0x1001".
func FormatAppID(id uint16) string { return fmt.Sprintf("0x%04x", id) }

// ParseMAC parses a MAC address written with ':' or '-'.
func ParseMAC(s string) ([6]byte, error) {
	var out [6]byte
	hw, err := net.ParseMAC(strings.ReplaceAll(s, "-", ":"))
	if err != nil || len(hw) != 6 {
		return out, fmt.Errorf("MAC %q: not a 6-octet address", s)
	}
	copy(out[:], hw)
	return out, nil
}

// FormatMAC renders a MAC as the contract does: lower-case, ':'-separated.
func FormatMAC(m [6]byte) string { return net.HardwareAddr(m[:]).String() }

// InterfaceMAC returns the hardware address of the named interface.
func InterfaceMAC(iface string) ([6]byte, error) {
	var out [6]byte
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return out, err
	}
	copy(out[:], ifi.HardwareAddr)
	return out, nil
}

// Ready is the readiness event of both GOOSE commands. address is the
// interface name, so the generic ready check of the smoke test applies.
type Ready struct {
	Event   string `json:"event"`
	Address string `json:"address"`
	Fixture string `json:"fixture"`
	Adapter string `json:"adapter"`
	Version string `json:"version"`
	Role    string `json:"role"`
	Iface   string `json:"iface"`
	Src     string `json:"src"`
	GoCbRef string `json:"gocbRef,omitempty"`
}

// EmitReady writes the readiness event.
func EmitReady(role, iface, src, gocbRef string) {
	jl.Emit(Ready{"ready", iface, jl.GooseFixtureRevision, jl.Adapter, jl.Version(), role, iface, src, gocbRef})
}

// EmitError reports a rejected stdin command or a send failure; the
// command keeps running.
func EmitError(format string, args ...any) {
	jl.Emit(struct {
		Event string `json:"event"`
		Error string `json:"error"`
	}{"error", fmt.Sprintf(format, args...)})
}
