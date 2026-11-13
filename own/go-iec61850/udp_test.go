// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otfabric/go-iec61850/rsession"
)

const (
	testKey  = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	rogueKey = "ff0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

func udpFlagsOf(t *testing.T, args ...string) *udpFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := addUDPFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUDPTransportFlag(t *testing.T) {
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
		udp, err := udpFlagsOf(t, c.args...).udp()
		if udp != c.udp || (err != nil) != c.err {
			t.Errorf("%v: %v, %v", c.args, udp, err)
		}
	}
}

func TestUDPKeys(t *testing.T) {
	if k, err := udpFlagsOf(t).keys(); k != nil || err != nil {
		t.Fatalf("no key: %v, %v", k, err)
	}
	k, err := udpFlagsOf(t, "--key-id", "7", "--key", testKey, "--mac", "HMAC-SHA256-256").keys()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := k.LookupKey(7); !ok || got.MAC != rsession.MACHMACSHA256_256 {
		t.Fatalf("key 7: %+v %v", got, ok)
	}
	for _, args := range [][]string{
		{"--key", testKey},
		{"--key-id", "1"},
		{"--key-id", "1", "--key", "zz"},
		{"--key-id", "1", "--key", testKey, "--mac", "aes-gmac-128"},
		{"--key-id", "1", "--key", "0011"}, // shorter than 16 octets
		{"--key-id", "4294967296", "--key", testKey},
	} {
		if _, err := udpFlagsOf(t, args...).keys(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestUDPOpenErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--remote", "127.0.0.1:1", "--session-version", "3"},
		{"--remote", "127.0.0.1:1", "--ttl", "0"},
		{"--remote", "127.0.0.1:x"},
		{"--remote", "127.0.0.1:1", "--key", testKey},
	} {
		if tr, _, err := udpFlagsOf(t, args...).openSender(rsession.SIGOOSE); err == nil {
			_ = tr.Close()
			t.Errorf("sender %v opened", args)
		}
	}
	for _, args := range [][]string{
		{"--key-id", "1"},
		{"--group", "10.0.0.1"},
	} {
		if tr, _, err := udpFlagsOf(t, args...).openReceiver(rsession.SIGOOSE); err == nil {
			_ = tr.Close()
			t.Errorf("receiver %v opened", args)
		}
	}
}

func TestResolve(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1":       "127.0.0.1:102",
		"127.0.0.1:1102":  "127.0.0.1:1102",
		"localhost:5":     "127.0.0.1:5",
		"[::1]:7":         "[::1]:7",
		"239.192.0.1":     "239.192.0.1:102",
		"[ff02::1]":       "[ff02::1]:102",
		"::ffff:10.0.0.1": "10.0.0.1:102",
	} {
		got, err := resolve(in)
		if err != nil || got.String() != want {
			t.Errorf("resolve(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	if got, err := resolve(":102"); err != nil || got.Addr().IsValid() || got.Port() != 102 {
		t.Errorf(":102: %v %v", got, err)
	}
}

func TestRejectReason(t *testing.T) {
	for err, want := range map[error]string{
		rsession.ErrUnknownKey:                          "unknown-key",
		fmt.Errorf("x: %w", rsession.ErrAuthentication): "authentication",
		rsession.ErrUnsecured:                           "unsecured",
		rsession.ErrReplay:                              "replay",
		rsession.ErrMalformed:                           "malformed",
		rsession.ErrUnsupported:                         "malformed",
		errors.New("other"):                             "malformed",
	} {
		if got := rejectReason(err); got != want {
			t.Errorf("rejectReason(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestSenderOf(t *testing.T) {
	for in, want := range map[netip.AddrPort]string{
		netip.MustParseAddrPort("172.18.0.3:40102"):   "172.18.0.3:40102",
		netip.MustParseAddrPort("[2001:db8::3]:4010"): "[2001:db8::3]:4010",
		{}: "",
	} {
		if got := senderOf(in); got != want {
			t.Errorf("senderOf(%v) = %q, want %q", in, got, want)
		}
	}
}

func freeUDPPort(t *testing.T) int { return freeUDPPortOn(t, netip.MustParseAddr("127.0.0.1")) }

// freeUDPPortOn returns a free UDP port on the loopback address a; it
// skips when a's family is not available.
func freeUDPPortOn(t *testing.T, a netip.Addr) int {
	t.Helper()
	c, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(a, 0)))
	if err != nil {
		if a.Is6() {
			t.Skipf("no IPv6 loopback: %v", err)
		}
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// startSubscriber runs a subscriber command until the test stops it.
func startSubscriber(t *testing.T, out *syncBuffer, run func([]string) int, args []string) func() int {
	t.Helper()
	subCtx, subStop := context.WithCancel(context.Background())
	stdoutMu.Lock()
	harnessNotify := notifyInterrupt
	notifyInterrupt = func(context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(subCtx) }
	stdoutMu.Unlock()
	done := make(chan int, 1)
	go func() { done <- run(args) }()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "ready" && e["role"] == "subscriber" })
	stdoutMu.Lock()
	notifyInterrupt = harnessNotify
	stdoutMu.Unlock()
	return func() int { subStop(); return <-done }
}

// TestRGOOSEPublisherToSubscriber runs the publisher and the subscriber
// over the loopback with --transport udp: a rogue publisher (same key ID,
// other key) is refused with reason authentication, the real one's frames
// arrive with zero addresses and from.
func TestRGOOSEPublisherToSubscriber(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) { testRGOOSEPublisherToSubscriber(t, netip.MustParseAddr(host)) })
	}
}

// testRGOOSEPublisherToSubscriber runs the exchange on the loopback
// address host: from is the publisher's address:port.
func testRGOOSEPublisherToSubscriber(t *testing.T, host netip.Addr) {
	addr := netip.AddrPortFrom(host, uint16(freeUDPPortOn(t, host))).String()
	out, stdinW, _ := l2Harness(t)
	stop := startSubscriber(t, out, runGOOSESubscriber, []string{"--transport", "udp", "--listen", addr,
		"--key-id", "1", "--key", testKey, "--app-id", "0x1002", "--dst", "01:0c:cd:01:00:02"})
	pubArgs := func(key string) []string {
		return []string{"--transport", "udp", "--remote", addr, "--key-id", "1", "--key", key,
			"--gocb-ref", "IEDLD/LLN0$GO$gcbBool", "--go-id", "G", "--dataset", "IEDLD/LLN0$ds", "--app-id", "0x1002",
			"--max-time", "50", "--values", `[{"type":"boolean","value":false}]`}
	}

	// The rogue publisher: its first frame, then stop.
	rogueIn, rogueW := io.Pipe()
	stdoutMu.Lock()
	stdin = rogueIn
	stdoutMu.Unlock()
	rogueDone := make(chan int, 1)
	go func() { rogueDone <- runGOOSEPublisher(pubArgs(rogueKey)) }()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "reject" })
	_, _ = io.WriteString(rogueW, `{"cmd":"stop"}`+"\n")
	if code := <-rogueDone; code != 0 {
		t.Fatalf("rogue publisher exit %d", code)
	}
	_ = rogueW.Close()

	pr, pw := io.Pipe()
	stdoutMu.Lock()
	stdin = pr
	stdoutMu.Unlock()
	_ = stdinW.Close()
	pubDone := make(chan int, 1)
	go func() { pubDone <- runGOOSEPublisher(pubArgs(testKey)) }()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "goose" })
	_, _ = io.WriteString(pw, `{"cmd":"set","values":[{"type":"boolean","value":true}]}`+"\n")
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "goose" && e["stNum"] == 2.0 })
	_, _ = io.WriteString(pw, `{"cmd":"stop"}`+"\n")
	if code := <-pubDone; code != 0 {
		t.Fatalf("publisher exit %d", code)
	}
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "supervision" && e["state"] == "timeout" })
	if code := stop(); code != 0 {
		t.Fatalf("subscriber exit %d", code)
	}

	var gooseEvents, rejects int
	for _, e := range out.events(t) {
		switch e["event"] {
		case "ready":
			if e["address"] != "udp:"+addr || e["iface"] != e["address"] || e["src"] != zeroMAC {
				t.Errorf("ready: %v", e)
			}
		case "goose":
			gooseEvents++
			from, err := netip.ParseAddrPort(e["from"].(string))
			if err != nil || from.Addr() != host || from.Port() == 0 {
				t.Fatalf("goose event from %q: %v", e["from"], err)
			}
			if e["dst"] != zeroMAC || e["src"] != zeroMAC || e["appID"] != "0x1002" {
				t.Fatalf("goose event: %v", e)
			}
			if v := e["values"].([]any)[0].(map[string]any)["value"]; (e["stNum"] == 1.0) != (v == false) {
				t.Errorf("stNum %v value %v", e["stNum"], v)
			}
		case "reject":
			rejects++
			from, err := netip.ParseAddrPort(e["from"].(string))
			if e["reason"] != "authentication" || err != nil || from.Addr() != host {
				t.Errorf("reject: %v", e)
			}
		case "error":
			t.Errorf("error event: %v", e)
		}
	}
	if rejects == 0 || gooseEvents < 2 {
		t.Errorf("rejects %d, goose events %d", rejects, gooseEvents)
	}
}

// TestRSVPublisherToSubscriber runs R-SV over the loopback, unsecured,
// with --allow-unsecured on the subscriber and session protocol version 1.
func TestRSVPublisherToSubscriber(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "sv.json")
	if err := os.WriteFile(fixture, []byte(`{"streams":{"MSVCB02":{"svID":"Gen","smpCntWrap":1000,
		"members":[{"type":"float32"},{"type":"bit-string"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stdinW, _ := l2Harness(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	stop := startSubscriber(t, out, runSVSubscriber, []string{"--transport", "udp", "--listen", addr, "--allow-unsecured",
		"--fixture", fixture, "--sample-every", "100", "--dst", "01:0c:cd:04:00:01"})
	pubDone := make(chan int, 1)
	go func() {
		pubDone <- runSVPublisher([]string{"--transport", "udp", "--remote", addr, "--session-version", "1",
			"--app-id", "0x4001", "--sv-id", "Gen", "--dataset", "IEDLD/LLN0$ds", "--smp-rate", "1000",
			"--smp-mod", "samples-per-second", "--profile", "generic",
			"--values", `[{"type":"float32","value":1.5},{"type":"bit-string","value":"0000000000000"}]`})
	}()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "sv" })
	_, _ = io.WriteString(stdinW, `{"cmd":"stop"}`+"\n")
	if code := <-pubDone; code != 0 {
		t.Fatalf("publisher exit %d", code)
	}
	if code := stop(); code != 0 {
		t.Fatalf("subscriber exit %d", code)
	}
	for _, e := range out.events(t) {
		switch e["event"] {
		case "sv":
			if e["dst"] != zeroMAC || e["src"] != zeroMAC || e["svID"] != "Gen" || e["appID"] != "0x4001" {
				t.Fatalf("sv event: %v", e)
			}
		case "reject", "error":
			t.Errorf("%v", e)
		}
	}
}

// TestRGOOSESubscriberRefusesUnsecured: without --allow-unsecured and
// without a key, an unsecured SPDU is refused as unsecured.
func TestRGOOSESubscriberRefusesUnsecured(t *testing.T) {
	out, stdinW, _ := l2Harness(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	stop := startSubscriber(t, out, runGOOSESubscriber, []string{"--transport", "udp", "--listen", addr})
	pubDone := make(chan int, 1)
	go func() {
		pubDone <- runGOOSEPublisher([]string{"--transport", "udp", "--remote", addr, "--gocb-ref", "X/LLN0$GO$g",
			"--dataset", "X/LLN0$ds", "--values", `[{"type":"boolean","value":true}]`})
	}()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "reject" && e["reason"] == "unsecured" })
	_, _ = io.WriteString(stdinW, `{"cmd":"stop"}`+"\n")
	if code := <-pubDone; code != 0 {
		t.Fatalf("publisher exit %d", code)
	}
	time.Sleep(50 * time.Millisecond)
	if code := stop(); code != 0 {
		t.Fatalf("subscriber exit %d", code)
	}
	for _, e := range out.events(t) {
		if e["event"] == "goose" {
			t.Fatalf("unsecured SPDU accepted: %v", e)
		}
	}
}
