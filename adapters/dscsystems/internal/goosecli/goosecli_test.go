// SPDX-License-Identifier: GPL-3.0-only

package goosecli

import (
	"bytes"
	"strings"
	"testing"
)

func TestSimulationBit(t *testing.T) {
	apdu := []byte{0x10, 0x01, 0x00, 0x20, 0x00, 0x7f, 0x00, 0x00}
	if Simulation(apdu) {
		t.Fatal("fresh APDU reports simulation")
	}
	SetSimulation(apdu, true)
	if !Simulation(apdu) || apdu[simulationOctet] != 0x80 {
		t.Fatalf("set: octet 4 = %#x", apdu[simulationOctet])
	}
	if want := []byte{0x10, 0x01, 0x00, 0x20, 0x80, 0x7f, 0x00, 0x00}; !bytes.Equal(apdu, want) {
		t.Fatalf("other octets changed: %x", apdu)
	}
	apdu[simulationOctet] |= 0x01 // the other Reserved1 bits survive a clear
	SetSimulation(apdu, false)
	if Simulation(apdu) || apdu[simulationOctet] != 0x01 {
		t.Fatalf("clear: octet 4 = %#x", apdu[simulationOctet])
	}
}

func TestSimulationShortAPDU(t *testing.T) {
	short := []byte{1, 2, 3, 4}
	SetSimulation(short, true)
	if Simulation(short) || !bytes.Equal(short, []byte{1, 2, 3, 4}) {
		t.Errorf("short APDU touched: %x", short)
	}
	if Simulation(nil) {
		t.Error("nil APDU reports simulation")
	}
}

func TestParseAppID(t *testing.T) {
	cases := []struct {
		in   string
		want uint16
		bad  bool
	}{
		{in: "0x1001", want: 0x1001},
		{in: " 0x1001 ", want: 0x1001},
		{in: "4097", want: 4097},
		{in: "1001", want: 1001}, // a bare number is decimal
		{in: "0", want: 0},
		{in: "0xffff", want: 0xffff},
		{in: "0x10000", bad: true},
		{in: "65536", bad: true},
		{in: "-1", bad: true},
		{in: "abcd", bad: true},
		{in: "", bad: true},
	}
	for _, c := range cases {
		got, err := ParseAppID(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("%q: got %#x, %v", c.in, got, err)
		}
		if err != nil && !strings.Contains(err.Error(), "app-id") {
			t.Errorf("%q: error does not name the flag: %v", c.in, err)
		}
	}
}

func TestAppIDAndMACRender(t *testing.T) {
	if got := FormatAppID(0x1); got != "0x0001" {
		t.Errorf("FormatAppID: %s", got)
	}
	m, err := ParseMAC("01-0C-CD-01-00-01")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatMAC(m); got != "01:0c:cd:01:00:01" {
		t.Errorf("FormatMAC: %s", got)
	}
}

func TestParseMACRejects(t *testing.T) {
	for _, in := range []string{"", "01:02:03", "zz:0c:cd:01:00:01", "01:02:03:04:05:06:07:08"} {
		if _, err := ParseMAC(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestInterfaceMACUnknown(t *testing.T) {
	if _, err := InterfaceMAC("no-such-interface0"); err == nil {
		t.Error("unknown interface accepted")
	}
}
