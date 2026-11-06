// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otfabric/go-iec61850/goose"
	"github.com/otfabric/go-iec61850/l2"
	"github.com/otfabric/go-iec61850/sv"
	mms "github.com/otfabric/go-mms"
)

// syncBuffer is a bytes.Buffer safe for the concurrent emits of two commands.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(s.b.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

// l2Harness redirects stdout, stdin, the transports and the signal context
// of the layer-2 commands for one test.
func l2Harness(t *testing.T, ports ...*l2.HubPort) (*syncBuffer, *io.PipeWriter, context.CancelFunc) {
	t.Helper()
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	next := func() (l2.Transport, error) {
		mu.Lock()
		defer mu.Unlock()
		p := ports[0]
		ports = ports[1:]
		return p, nil
	}
	stdoutMu.Lock()
	oldOut, oldErr, oldIn := stdout, stderr, stdin
	oldG, oldS, oldN := openGOOSE, openSV, notifyInterrupt
	stdout, stderr, stdin = out, io.Discard, pr
	openGOOSE = func(string, goose.TransportOptions) (goose.Transport, error) { return next() }
	openSV = func(string, sv.TransportOptions) (sv.Transport, error) { return next() }
	notifyInterrupt = func(context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
	stdoutMu.Unlock()
	t.Cleanup(func() {
		cancel()
		_ = pw.Close()
		stdoutMu.Lock()
		stdout, stderr, stdin = oldOut, oldErr, oldIn
		openGOOSE, openSV, notifyInterrupt = oldG, oldS, oldN
		stdoutMu.Unlock()
	})
	return out, pw, cancel
}

func TestTypedRoundTrip(t *testing.T) {
	in := `[{"type":"boolean","value":true},{"type":"integer","value":-5},{"type":"unsigned","value":42},
		{"type":"float32","value":0.1},{"type":"float64","value":0.1},{"type":"visible-string","value":"a"},
		{"type":"mms-string","value":"é"},{"type":"octet-string","value":"00ff"},
		{"type":"bit-string","value":"0000000000001"},{"type":"utc-time","value":"2026-10-28T02:36:12.244Z"},
		{"type":"structure","value":[{"type":"array","value":[{"type":"integer","value":1}]}]}]`
	vs, err := parseTypedList([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(toTypedList(vs))
	want := `[{"type":"boolean","value":true},{"type":"integer","value":-5},{"type":"unsigned","value":42},` +
		`{"type":"float32","value":0.1},{"type":"float64","value":0.1},{"type":"visible-string","value":"a"},` +
		`{"type":"mms-string","value":"é"},{"type":"octet-string","value":"00ff"},` +
		`{"type":"bit-string","value":"0000000000001"},{"type":"utc-time","value":"2026-10-28T02:36:12.244Z"},` +
		`{"type":"structure","value":[{"type":"array","value":[{"type":"integer","value":1}]}]}]`
	if string(got) != want {
		t.Errorf("round trip:\n got %s\nwant %s", got, want)
	}
	for _, bad := range []string{`[{"type":"integer","value":1.5}]`, `[{"type":"unsigned","value":-1}]`,
		`[{"type":"bit-string","value":"012"}]`, `[{"type":"utc-time","value":"yesterday"}]`, `[{"type":"x","value":1}]`, `{}`} {
		if _, err := parseTypedList([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestFloatWidth(t *testing.T) {
	// go-mms keeps no float width: an exact float32 reads back as float32.
	if got := toTyped(mms.NewFloat(1234.5), ""); got["type"] != "float32" {
		t.Errorf("1234.5: %v", got)
	}
	if got := toTyped(mms.NewFloat(0.1), ""); got["type"] != "float64" {
		t.Errorf("0.1 (float64): %v", got)
	}
	if got := toTyped(mms.NewFloat(float64(float32(0.1))), "float32"); got["value"] != json.Number("0.1") {
		t.Errorf("float32 0.1: %v", got["value"])
	}
}

func TestParseAppID(t *testing.T) {
	for s, want := range map[string]uint16{"0x1001": 0x1001, "4097": 4097, "0X4000": 0x4000} {
		if got, err := parseAppID(s); err != nil || got != want {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	for _, s := range []string{"0x10000", "abc", ""} {
		if _, err := parseAppID(s); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
	if appIDString(0x1001) != "0x1001" || appIDString(7) != "0x0007" {
		t.Error("appIDString")
	}
}

func TestGOOSEPublisherToSubscriber(t *testing.T) {
	hub := l2.NewHub()
	pub := hub.NewPort(l2.MAC{0x02, 0, 0, 0, 0, 1})
	sub := hub.NewPort(l2.MAC{0x02, 0, 0, 0, 0, 2})
	out, stdinW, _ := l2Harness(t, sub, pub)

	subCtx, subStop := context.WithCancel(context.Background())
	notifySub := func(context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(subCtx) }
	stdoutMu.Lock()
	harnessNotify := notifyInterrupt
	notifyInterrupt = notifySub
	stdoutMu.Unlock()
	subDone := make(chan int)
	go func() { subDone <- runGOOSESubscriber([]string{"--iface", "x", "--app-id", "0x1002"}) }()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "ready" && e["role"] == "subscriber" })
	stdoutMu.Lock()
	notifyInterrupt = harnessNotify
	stdoutMu.Unlock()

	pubDone := make(chan int)
	go func() {
		pubDone <- runGOOSEPublisher([]string{"--iface", "x", "--gocb-ref", "IEDLD/LLN0$GO$gcbBool", "--go-id", "G",
			"--dataset", "IEDLD/LLN0$ds", "--app-id", "0x1002", "--dst", "01:0c:cd:01:00:02", "--max-time", "50",
			"--test", "--simulation", "--nds-com",
			"--values", `[{"type":"boolean","value":false},{"type":"bit-string","value":"0000000000000"}]`})
	}()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "published" })
	time.Sleep(150 * time.Millisecond)
	_, _ = io.WriteString(stdinW, `{"cmd":"set","values":[{"type":"boolean","value":true},{"type":"bit-string","value":"0000000000000"}]}`+"\n")
	time.Sleep(150 * time.Millisecond)
	_, _ = io.WriteString(stdinW, `{"cmd":"stop"}`+"\n")
	if code := <-pubDone; code != 0 {
		t.Fatalf("publisher exit %d", code)
	}
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "supervision" && e["state"] == "timeout" })
	subStop()
	if code := <-subDone; code != 0 {
		t.Fatalf("subscriber exit %d", code)
	}

	var st1, st2 int
	for _, e := range out.events(t) {
		switch e["event"] {
		case "goose":
			if e["gocbRef"] != "IEDLD/LLN0$GO$gcbBool" || e["goID"] != "G" || e["appID"] != "0x1002" ||
				e["test"] != true || e["simulation"] != true || e["ndsCom"] != true || e["numDatSetEntries"] != 2.0 ||
				e["src"] != "02:00:00:00:00:01" {
				t.Fatalf("goose event fields: %v", e)
			}
			v := e["values"].([]any)[0].(map[string]any)["value"]
			switch e["stNum"] {
			case 1.0:
				st1++
				if v != false {
					t.Errorf("stNum 1 value %v", v)
				}
			case 2.0:
				st2++
				if v != true {
					t.Errorf("stNum 2 value %v", v)
				}
			}
		case "error":
			t.Errorf("error event: %v", e)
		}
	}
	if st1 < 3 || st2 < 3 {
		t.Errorf("frames: stNum 1 %d, stNum 2 %d", st1, st2)
	}
}

func TestSVPublisherToSubscriber(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "sv.json")
	if err := os.WriteFile(fixture, []byte(`{"streams":{"MSVCB02":{"svID":"Gen","smpCntWrap":1000,
		"members":[{"type":"float32"},{"type":"bit-string"},{"type":"unsigned"},{"type":"bit-string"},{"type":"integer"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hub := l2.NewHub()
	pub := hub.NewPort(l2.MAC{0x02, 0, 0, 0, 0, 1})
	sub := hub.NewPort(l2.MAC{0x02, 0, 0, 0, 0, 2})
	out, stdinW, _ := l2Harness(t, sub, pub)

	subCtx, subStop := context.WithCancel(context.Background())
	stdoutMu.Lock()
	harnessNotify := notifyInterrupt
	notifyInterrupt = func(context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(subCtx) }
	stdoutMu.Unlock()
	subDone := make(chan int)
	go func() {
		subDone <- runSVSubscriber([]string{"--iface", "x", "--app-id", "0x4001", "--fixture", fixture, "--sample-every", "100"})
	}()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "ready" && e["role"] == "subscriber" })
	stdoutMu.Lock()
	notifyInterrupt = harnessNotify
	stdoutMu.Unlock()

	values := `[{"type":"float32","value":1234.5},{"type":"bit-string","value":"0000000000000"},{"type":"unsigned","value":42},` +
		`{"type":"bit-string","value":"0000000000000"},{"type":"integer","value":1}]`
	pubDone := make(chan int)
	go func() {
		pubDone <- runSVPublisher([]string{"--iface", "x", "--app-id", "0x4001", "--dst", "01:0c:cd:04:00:01",
			"--sv-id", "Gen", "--dataset", "IEDLD/LLN0$dsSvGeneric", "--smp-rate", "1000", "--smp-mod", "samples-per-second",
			"--no-asdu", "4", "--profile", "generic", "--report-every", "200", "--values", values})
	}()
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "published" && e["reason"] == "start" })
	time.Sleep(300 * time.Millisecond)
	_, _ = io.WriteString(stdinW, `{"cmd":"set","values":`+strings.Replace(values, "1234.5", "-1.5", 1)+`}`+"\n")
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "published" && e["reason"] == "set" })
	time.Sleep(300 * time.Millisecond)
	_, _ = io.WriteString(stdinW, `{"cmd":"stop"}`+"\n")
	if code := <-pubDone; code != 0 {
		t.Fatalf("publisher exit %d", code)
	}
	waitFor(t, out, func(e map[string]any) bool { return e["event"] == "stats" })
	subStop()
	if code := <-subDone; code != 0 {
		t.Fatalf("subscriber exit %d", code)
	}

	var before, after int
	for _, e := range out.events(t) {
		switch e["event"] {
		case "sv":
			if e["svID"] != "Gen" || e["datSet"] != "IEDLD/LLN0$dsSvGeneric" || e["smpMod"] != "samples-per-second" ||
				e["smpRate"] != 1000.0 || e["noASDU"] != 4.0 || e["refrTm"] == nil || int(e["smpCnt"].(float64))%100 != 0 {
				t.Fatalf("sv event fields: %v", e)
			}
			f := e["values"].([]any)[0].(map[string]any)
			if f["type"] != "float32" {
				t.Fatalf("value 0: %v", f)
			}
			switch f["value"] {
			case 1234.5:
				before++
			case -1.5:
				after++
			}
		case "stopped":
			if e["samples"].(float64) < 100 {
				t.Errorf("stopped: %v", e)
			}
		case "error":
			t.Errorf("error event: %v", e)
		}
	}
	if before == 0 || after == 0 {
		t.Errorf("sv events before set %d, after %d", before, after)
	}
}

func TestSVPublisherArguments(t *testing.T) {
	out, _, _ := l2Harness(t)
	for _, args := range [][]string{
		{"--dst", "01:0c:cd:04:00:00", "--sv-id", "x", "--values", `[{"type":"integer","value":1}]`}, // 9-2LE needs 16
		{"--dst", "01:0c:cd:04:00:00", "--sv-id", "x", "--smp-mod", "often", "--values", `[]`},
		{"--dst", "nope", "--sv-id", "x", "--values", `[]`},
		{"--dst", "01:0c:cd:04:00:00", "--sv-id", "x", "--profile", "generic", "--values", `[{"type":"octet-string","value":"00"}]`},
	} {
		if code := runSVPublisher(args); code != exitArgs {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
	}
	if code := runGOOSEPublisher([]string{"--dst", "01:0c:cd:01:00:01", "--values", `[]`}); code != exitArgs {
		t.Errorf("goose-publisher without --gocb-ref: exit %d", code)
	}
	if n := len(out.events(t)); n != 0 {
		t.Errorf("argument errors printed %d events", n)
	}
}

// waitFor polls the output for an event (at most 5 s).
func waitFor(t *testing.T, out *syncBuffer, match func(map[string]any) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range out.events(t) {
			if match(e) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event not seen; output: %v", out.events(t))
}
