// SPDX-License-Identifier: GPL-3.0-only

package rudp

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-iec61850/ethernet"
	"github.com/dscsystems/go-iec61850/rsession"
)

const testKey = "000102030405060708090a0b0c0d0e0f"

// flags returns Flags parsed from args on a fresh flag set.
func flags(t *testing.T, args ...string) *Flags {
	t.Helper()
	old := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	defer func() { flag.CommandLine = old }()
	f := Register()
	if err := flag.CommandLine.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUDP(t *testing.T) {
	for _, c := range []struct {
		args []string
		udp  bool
		err  bool
	}{
		{nil, false, false},
		{[]string{"--transport", "ethernet"}, false, false},
		{[]string{"--transport", "udp"}, true, false},
		{[]string{"--transport", "tcp"}, false, true},
	} {
		udp, err := flags(t, c.args...).UDP()
		if udp != c.udp || (err != nil) != c.err {
			t.Errorf("%v: %v, %v", c.args, udp, err)
		}
	}
}

func TestKeys(t *testing.T) {
	if k, err := flags(t).keys(); k != nil || err != nil {
		t.Fatalf("no key: %v, %v", k, err)
	}
	k, err := flags(t, "--key-id", "7", "--key", testKey, "--mac", "HMAC-SHA256-256").keys()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := k.Lookup(7); !ok || got.Sig != rsession.SigHMACSHA256_256 {
		t.Fatalf("key 7: %+v %v", got, ok)
	}
	for _, args := range [][]string{
		{"--key", testKey},
		{"--key-id", "1"},
		{"--key-id", "1", "--key", "zz"},
		{"--key-id", "1", "--key", testKey, "--mac", "aes-gmac-128"},
		{"--key-id", "1", "--key", "0011"},
		{"--key-id", "4294967296", "--key", testKey},
	} {
		if _, err := flags(t, args...).keys(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestReason(t *testing.T) {
	for err, want := range map[error]string{
		rsession.ErrUnknownKey:                          "unknown-key",
		fmt.Errorf("x: %w", rsession.ErrAuthentication): "authentication",
		rsession.ErrAlgorithm:                           "authentication",
		rsession.ErrUnsecured:                           "unsecured",
		rsession.ErrReplay:                              "replay",
		rsession.ErrMalformed:                           "malformed",
		errors.New("other"):                             "malformed",
	} {
		if got := Reason(err); got != want {
			t.Errorf("Reason(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestFrames(t *testing.T) {
	fs := Frames(&rsession.SPDU{Payloads: []rsession.Payload{
		{Type: rsession.PayloadGOOSE, AppID: 0x1001, Simulation: true, APDU: []byte{0x61, 0x00}},
		{Type: rsession.PayloadManagement, APDU: []byte{1}},
		{Type: rsession.PayloadSV, AppID: 0x4000, APDU: []byte{0x60, 0x00}},
		{Type: rsession.PayloadGOOSE, APDU: make([]byte, 0x10000)},
	}})
	if len(fs) != 2 {
		t.Fatalf("%d frames", len(fs))
	}
	if fs[0].EtherType != ethernet.EtherTypeGOOSE || !bytes.Equal(fs[0].Payload, []byte{0x10, 0x01, 0, 10, 0x80, 0, 0, 0, 0x61, 0}) {
		t.Errorf("GOOSE frame %x", fs[0].Payload)
	}
	if fs[1].EtherType != ethernet.EtherTypeSV || !bytes.Equal(fs[1].Payload, []byte{0x40, 0x00, 0, 10, 0, 0, 0, 0, 0x60, 0}) {
		t.Errorf("SV frame %x", fs[1].Payload)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := flags(t, "--transport", "udp").OpenSender(); err == nil {
		t.Error("sender without --remote")
	}
	if _, err := flags(t, "--remote", "127.0.0.1:1", "--key", testKey).OpenSender(); err == nil {
		t.Error("sender with --key and no --key-id")
	}
	if _, err := flags(t, "--key-id", "1").OpenReceiver(); err == nil {
		t.Error("receiver with --key-id and no --key")
	}
}

// TestLoopback sends signed R-GOOSE to a receiver on the loopback: frames
// under the shared key arrive with their APPID and simulation bit; frames
// under another key are refused.
func TestLoopback(t *testing.T) {
	rx, err := flags(t, "--listen", "127.0.0.1:0", "--key-id", "1", "--key", testKey).OpenReceiver()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rx.Close() }()
	addr := rx.s.LocalAddr().String()
	if !strings.HasPrefix(rx.Label(), "udp:127.0.0.1:") {
		t.Errorf("label %s", rx.Label())
	}

	tx, err := flags(t, "--remote", addr, "--key-id", "1", "--key", testKey, "--session-version", "1").OpenSender()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Close() }()
	if tx.Label() != "udp:"+addr {
		t.Errorf("label %s", tx.Label())
	}
	other, err := flags(t, "--remote", addr, "--key-id", "2", "--key", testKey).OpenSender()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	frame := &ethernet.Frame{EtherType: ethernet.EtherTypeGOOSE,
		Payload: []byte{0x10, 0x01, 0, 10, 0x80, 0, 0, 0, 0x61, 0}}
	if err := other.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	got := make(chan *ethernet.Frame, 1)
	go func() {
		f, err := rx.ReadFrame()
		if err != nil {
			t.Error(err)
		}
		got <- f
	}()
	select {
	case f := <-got:
		if f == nil || !bytes.Equal(f.Payload, frame.Payload) || f.EtherType != ethernet.EtherTypeGOOSE {
			t.Fatalf("frame %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	if !strings.HasPrefix(rx.From(), "127.0.0.1:") {
		t.Errorf("from %q", rx.From())
	}
	if s := rx.s.Stats(); s.UnknownKey != 1 || s.Accepted != 1 {
		t.Errorf("stats %+v", s)
	}
}
