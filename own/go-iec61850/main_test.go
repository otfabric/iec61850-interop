// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	iec61850 "github.com/otfabric/go-iec61850"
	mms "github.com/otfabric/go-mms"
)

// captureMain redirects stdout and stderr for one test.
func captureMain(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out, errb bytes.Buffer
	stdoutMu.Lock()
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &out, &errb
	stdoutMu.Unlock()
	t.Cleanup(func() {
		stdoutMu.Lock()
		stdout, stderr = oldOut, oldErr
		stdoutMu.Unlock()
		if t.Failed() {
			t.Logf("stderr: %s", errb.String())
		}
	})
	return &out
}

func decodeLine(t *testing.T, b *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b.Bytes()), &m); err != nil {
		t.Fatalf("not one JSON line: %q: %v", b.String(), err)
	}
	return m
}

func TestDispatchByName(t *testing.T) {
	out := captureMain(t)
	if code := run("otfabric-ied-server", []string{"--version"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	m := decodeLine(t, out)
	if m["event"] != "version" || m["fixtureRevision"] != iedFixture {
		t.Errorf("version event: %v", m)
	}
	up, _ := m["upstream"].(map[string]any)
	if up["name"] != "go-iec61850" || up["independentEngine"] != false {
		t.Errorf("upstream: %v", up)
	}
}

func TestDispatchByArgument(t *testing.T) {
	out := captureMain(t)
	if code := run("iec61850-interop", []string{"otfabric-sv-subscriber", "--capabilities"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	m := decodeLine(t, out)
	cmds, _ := m["commands"].([]any)
	if m["event"] != "capabilities" || len(cmds) != len(commands)-len(unsupported) {
		t.Errorf("capabilities: %v", m)
	}
	for _, c := range cmds {
		if !strings.HasPrefix(c.(string), prefix) {
			t.Errorf("command %v without the prefix", c)
		}
	}
}

func TestUnknownCommandAndNoCommand(t *testing.T) {
	captureMain(t)
	if code := run("otfabric-frobnicate", nil); code != exitArgs {
		t.Errorf("unknown command: exit %d", code)
	}
	if code := run("iec61850-interop", nil); code != exitArgs {
		t.Errorf("no command: exit %d", code)
	}
}

func TestUnsupportedCommand(t *testing.T) {
	out := captureMain(t)
	unsupported["ied-client"] = "test"
	t.Cleanup(func() { delete(unsupported, "ied-client") })
	if code := run("otfabric-ied-client", nil); code != exitUnsupported {
		t.Fatalf("exit %d", code)
	}
	m := decodeLine(t, out)
	if m["event"] != "error" || m["code"] != "unsupported" || m["command"] != "otfabric-ied-client" {
		t.Errorf("unsupported event: %v", m)
	}
}

func TestClientArgumentErrors(t *testing.T) {
	captureMain(t)
	for _, args := range [][]string{
		{"--remote-tsel", "0000000001"}, {"--local-tsel", "0"}, {"--remote-tsel", "zz"},
		{"--port", "0"}, {"--port", "70000"}, {"stray"}, {"--frobnicate"},
	} {
		if code := runIEDClient(args); code != exitArgs {
			t.Errorf("ied-client %v: exit %d, want 1", args, code)
		}
	}
	if code := runIEDController([]string{"--do", "SPCSO9"}); code != exitArgs {
		t.Errorf("controller --do SPCSO9: exit %d", code)
	}
	if code := runIEDController([]string{"--ctlval", "maybe"}); code != exitArgs {
		t.Errorf("controller --ctlval maybe: exit %d", code)
	}
	if code := runIEDReporter([]string{"--sps1-initial", "2"}); code != exitArgs {
		t.Errorf("reporter --sps1-initial 2: exit %d", code)
	}
}

func TestConnectionFailureIsExit2(t *testing.T) {
	out := captureMain(t)
	// Port 1 on the loopback address: nothing listens, the dial fails at once.
	if code := runIEDClient([]string{"--host", "127.0.0.1", "--port", "1"}); code != exitConnect {
		t.Fatalf("exit %d", code)
	}
	m := decodeLine(t, out)
	if m["operation"] != "associate" || m["ok"] != false {
		t.Errorf("line: %v", m)
	}
}

func TestParseTSel(t *testing.T) {
	for in, want := range map[string]int{"01": 1, "0001": 2, "00000001": 4} {
		b, err := parseTSel(in)
		if err != nil || len(b) != want {
			t.Errorf("%s: %x %v", in, b, err)
		}
	}
}

func TestResolveLD(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"Other", "InteropLD"}, "InteropLD"},
		{[]string{"Other", "InteropIEDInteropLD"}, "InteropIEDInteropLD"},
		{[]string{"First", "Second"}, "First"},
		{nil, "InteropLD"},
	}
	for _, c := range cases {
		if got := resolveLD(c.names); got != c.want {
			t.Errorf("%v: %s, want %s", c.names, got, c.want)
		}
	}
}

func TestRCBItem(t *testing.T) {
	if it, ok := rcbItem([]string{"LLN0$BR$brcb0101", "LLN0$RP$urcb0101"}); !ok || it != "LLN0$RP$urcb0101" {
		t.Errorf("instance: %s %v", it, ok)
	}
	if it, ok := rcbItem([]string{"LLN0$RP$urcb0101", "LLN0$RP$urcb01"}); !ok || it != "LLN0$RP$urcb01" {
		t.Errorf("exact first: %s %v", it, ok)
	}
	if _, ok := rcbItem([]string{"LLN0$BR$brcb01"}); ok {
		t.Error("found a URCB that is not there")
	}
}

func TestReasonName(t *testing.T) {
	if got := reasonName(iec61850.ReasonDataChanged | iec61850.ReasonGI); got != "data-change" {
		t.Errorf("dchg|gi: %s", got)
	}
	if got := reasonName(iec61850.ReasonGI); got != "gi" {
		t.Errorf("gi: %s", got)
	}
	if got := reasonName(0); got != "unknown" {
		t.Errorf("0: %s", got)
	}
}

func TestSCLValue(t *testing.T) {
	cases := []struct {
		btype, raw string
		want       mms.ValueType
		n          *int64
	}{
		{"BOOLEAN", "true", mms.ValueTypeBoolean, nil},
		{"Enum", "4", mms.ValueTypeInteger, ptr(4)},
		{"INT32", "-7", mms.ValueTypeInteger, ptr(-7)},
		{"INT32U", "42", mms.ValueTypeUnsigned, nil},
		{"FLOAT32", "1234.5", mms.ValueTypeFloat, nil},
		{"VisString255", `"mode"`, mms.ValueTypeVisibleString, nil},
		{"Unicode255", `"ü"`, mms.ValueTypeMmsString, nil},
		{"Dbpos", "2", mms.ValueTypeBitString, nil},
	}
	for _, c := range cases {
		v, n, err := sclValue(c.btype, json.RawMessage(c.raw))
		if err != nil || v.Type() != c.want || (c.n == nil) != (n == nil) || (n != nil && *n != *c.n) {
			t.Errorf("%s %s: %v %v %v", c.btype, c.raw, v, n, err)
		}
	}
	if bits, n, _ := mustBits(t, "Dbpos", "2"); n != 2 || bits[0] != 0x80 {
		t.Errorf("Dbpos 2: %x/%d", bits, n)
	}
	for _, bad := range [][2]string{{"BOOLEAN", "1"}, {"INT32U", "-1"}, {"Dbpos", "4"}, {"Quality", "0"}, {"VisString64", "3"}} {
		if _, _, err := sclValue(bad[0], json.RawMessage(bad[1])); err == nil {
			t.Errorf("%s %s accepted", bad[0], bad[1])
		}
	}
}

func mustBits(t *testing.T, btype, raw string) ([]byte, int, error) {
	t.Helper()
	v, _, err := sclValue(btype, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, ok := v.BitString()
	n, ok2 := v.BitStringLength()
	if !ok || !ok2 {
		t.Fatalf("%s %s is not a bit string", btype, raw)
	}
	return b, n, nil
}

func ptr(n int64) *int64 { return &n }

func TestFormatG(t *testing.T) {
	for in, want := range map[float64]string{1234.5: "1234.5", 0.1: "0.1", 1e7: "1e+07", 42: "42"} {
		if got := formatG(in); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}
