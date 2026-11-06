// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	iec61850 "github.com/cB-Sigma/iec61850"
	"github.com/cB-Sigma/iec61850-sim/sim"
	"github.com/cB-Sigma/iec61850/mms"
)

// capture runs fn with stdout and stderr redirected and returns the lines
// of stdout as JSON objects.
func capture(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	stdout, stderr = &out, &errOut
	t.Cleanup(func() { stdout, stderr = os.Stdout, os.Stderr })
	fn()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestRun_VersionCapabilitiesUnsupported(t *testing.T) {
	var code int
	lines := capture(t, func() { code = run("cbsigma-ied-server", []string{"--version"}) })
	if code != exitOK || len(lines) != 1 || lines[0]["event"] != "version" || lines[0]["fixtureRevision"] != iedFixture {
		t.Fatalf("--version: code %d, %v", code, lines)
	}
	if _, ok := lines[0]["upstream"]; ok {
		t.Error("a reference image carries no upstream object")
	}

	lines = capture(t, func() { code = run("iec61850-interop", []string{"cbsigma-ied-client", "--capabilities"}) })
	if code != exitOK || len(lines) != 1 {
		t.Fatalf("--capabilities: code %d, %v", code, lines)
	}
	var got []string
	for _, c := range lines[0]["commands"].([]any) {
		got = append(got, c.(string))
	}
	want := "cbsigma-goose-publisher cbsigma-goose-subscriber cbsigma-ied-client cbsigma-ied-controller cbsigma-ied-reporter cbsigma-ied-server"
	if strings.Join(got, " ") != want {
		t.Errorf("commands %v, want %s", got, want)
	}

	for _, c := range []string{"mms-server", "mms-client", "sv-publisher", "sv-subscriber"} {
		lines = capture(t, func() { code = run("cbsigma-"+c, nil) })
		if code != exitUnsupported || len(lines) != 1 || lines[0]["code"] != "unsupported" || lines[0]["command"] != "cbsigma-"+c {
			t.Errorf("%s: code %d, %v", c, code, lines)
		}
	}

	capture(t, func() { code = run("cbsigma-nope", nil) })
	if code != exitArgs {
		t.Errorf("unknown command: code %d", code)
	}
	capture(t, func() { code = run("iec61850-interop", nil) })
	if code != exitArgs {
		t.Errorf("no command: code %d", code)
	}
}

func TestClientFlags(t *testing.T) {
	for _, args := range [][]string{{"--port", "0"}, {"--port", "70000"}, {"extra"}, {"--nope"}} {
		var code int
		capture(t, func() { code = run("cbsigma-ied-client", args) })
		if code != exitArgs {
			t.Errorf("%v: code %d, want %d", args, code, exitArgs)
		}
	}
	for _, args := range [][]string{{"--local-tsel", "0"}, {"--remote-tsel", "0102030405"}, {"--local-tsel", "zz"}} {
		var code int
		capture(t, func() { code = run("cbsigma-ied-client", args) })
		if code != exitArgs {
			t.Errorf("%v: code %d, want %d", args, code, exitArgs)
		}
	}
	var code int
	capture(t, func() { code = run("cbsigma-ied-controller", []string{"--do", "SPCSO9"}) })
	if code != exitArgs {
		t.Errorf("--do SPCSO9: code %d", code)
	}
	capture(t, func() { code = run("cbsigma-ied-controller", []string{"--ctlval", "maybe"}) })
	if code != exitArgs {
		t.Errorf("--ctlval maybe: code %d", code)
	}
	capture(t, func() { code = run("cbsigma-ied-reporter", []string{"--sps1-initial", "2"}) })
	if code != exitArgs {
		t.Errorf("--sps1-initial 2: code %d", code)
	}
}

func TestConnectFailure(t *testing.T) {
	var code int
	lines := capture(t, func() { code = run("cbsigma-ied-client", []string{"--port", "1"}) })
	if code != exitConnect || len(lines) != 1 || lines[0]["operation"] != "associate" || lines[0]["ok"] != false {
		t.Fatalf("code %d, %v", code, lines)
	}
}

func TestHelpers(t *testing.T) {
	if got := resolveLD([]string{"X", "InteropIEDInteropLD"}); got != "InteropIEDInteropLD" {
		t.Errorf("resolveLD: %s", got)
	}
	if got := resolveLD([]string{"InteropLD", "InteropIEDInteropLD"}); got != "InteropLD" {
		t.Errorf("resolveLD exact: %s", got)
	}
	if got := resolveLD(nil); got != ldInst {
		t.Errorf("resolveLD empty: %s", got)
	}
	if r, ok := rcbOf([]string{"LLN0.BR.brcb0101", "LLN0.RP.urcb0101"}); !ok || r != "LLN0.RP.urcb0101" {
		t.Errorf("rcbOf instance: %s %v", r, ok)
	}
	if r, ok := rcbOf([]string{"LLN0.RP.urcb01"}); !ok || r != "LLN0.RP.urcb01" {
		t.Errorf("rcbOf: %s %v", r, ok)
	}
	if _, ok := rcbOf([]string{"LLN0.BR.brcb01"}); ok {
		t.Error("rcbOf found a BRCB")
	}
	if reasonName([]string{"gi", "dchg"}) != "data-change" || reasonName([]string{"qchg"}) != "quality-change" ||
		reasonName([]string{"dupd"}) != "data-update" || reasonName([]string{"integrity"}) != "integrity" ||
		reasonName([]string{"gi"}) != "gi" || reasonName(nil) != "unknown" {
		t.Error("reasonName")
	}
	if _, err := parseTarget("LD/LN.DO.DA"); err == nil {
		t.Error("parseTarget without [FC]")
	}
	r, err := parseTarget("InteropIEDInteropLD/GGIO1.SPS1.stVal[ST]")
	if err != nil || r.MMS().Item != "GGIO1$ST$SPS1$stVal" {
		t.Errorf("parseTarget: %v %v", r, err)
	}
	if b, ok := parseBool("TRUE"); !ok || !b {
		t.Error("parseBool")
	}
	if id, err := parseAppID("0x1001"); err != nil || id != 0x1001 {
		t.Error("parseAppID hex")
	}
	if _, err := parseAppID("0x10000"); err == nil {
		t.Error("parseAppID range")
	}
	if appIDString(0x1001) != "0x1001" {
		t.Error("appIDString")
	}
	if truncate("abcdef", 3) != "abc" || truncate("ab", 3) != "ab" {
		t.Error("truncate")
	}
	if errString(mms.ErrObjectAccessDenied) != "access-denied" || errString(mms.ErrObjectNonExistent) != "object-does-not-exist" ||
		errString(mms.ErrTypeInconsistent) != "type-inconsistent" || errString(mms.ErrObjectAccessUnsupport) != "object-access-unsupported" ||
		errString(context.DeadlineExceeded) != "timeout" {
		t.Error("errString")
	}
}

func TestPlain(t *testing.T) {
	dbpos := mms.NewBitString(2)
	dbpos.Set(0, true)
	for _, c := range []struct {
		in   mms.Data
		want string
	}{
		{mms.Boolean(true), "true"},
		{mms.Integer(-3), "-3"},
		{mms.Unsigned(42), "42"},
		{mms.Float32(1234.5), "1234.5"},
		{mms.Visible("mode"), `"mode"`},
		{mms.MMSString("é"), `"é"`},
		{mms.Bits(dbpos), "2"},
	} {
		v, err := plain(c.in)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		b, _ := json.Marshal(v)
		if string(b) != c.want {
			t.Errorf("plain(%v) = %s, want %s", c.in, b, c.want)
		}
	}
	if _, err := plain(mms.Structure()); err == nil {
		t.Error("plain of a structure")
	}
	if _, err := plain(mms.Bits(mms.NewBitString(0))); err == nil {
		t.Error("plain of an empty bit-string")
	}
}

// The typed values of fixtures/goose/goose.json (gcbAll) read and written
// back unchanged.
func TestTypedRoundTrip(t *testing.T) {
	in := `[{"type":"boolean","value":true},{"type":"integer","value":-5},{"type":"unsigned","value":42},
		{"type":"float32","value":0.1},{"type":"float64","value":0.1},{"type":"bit-string","value":"0000000000000"},
		{"type":"utc-time","value":"2026-01-02T03:04:05.006Z"},{"type":"binary-time","value":"2026-01-02T03:04:05.006Z"},
		{"type":"visible-string","value":"x"},{"type":"mms-string","value":"y"},{"type":"octet-string","value":"0aff"},
		{"type":"structure","value":[{"type":"structure","value":[{"type":"float32","value":1234.5}]},{"type":"bit-string","value":"10"}]},
		{"type":"array","value":[{"type":"integer","value":1}]}]`
	vals, err := parseTypedList([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	// Through the wire encoding and back, as the subscriber sees them.
	for i, v := range vals {
		e, err := mms.ParseAll(v.Encode())
		if err != nil || len(e) != 1 {
			t.Fatalf("value %d: %v", i, err)
		}
		if vals[i], err = mms.DecodeData(e[0]); err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
	}
	out, _ := json.Marshal(toTypedList(vals))
	var a, b any
	_ = json.Unmarshal([]byte(in), &a)
	_ = json.Unmarshal(out, &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("round trip:\n got %s\nwant %s", jb, ja)
	}
	for _, bad := range []string{`{}`, `[{"type":"integer","value":1.5}]`, `[{"type":"unsigned","value":-1}]`,
		`[{"type":"bit-string","value":"012"}]`, `[{"type":"utc-time","value":"yesterday"}]`, `[{"type":"nope","value":1}]`,
		`[{"type":"octet-string","value":"zz"}]`, `[{"type":"boolean","value":1}]`, `[{"type":"visible-string","value":1}]`,
		`[{"type":"structure","value":1}]`, `[{"value":1}]`} {
		if _, err := parseTypedList([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestApplyValues(t *testing.T) {
	s := sim.New()
	s.AddValue("InteropIEDInteropLD", "GGIO1", "ST", "SPS1", "stVal", mms.Boolean(false))
	s.AddValue("InteropIEDInteropLD", "GGIO1", "ST", "DPS1", "stVal", mms.Bits(mms.NewBitString(2)))
	s.AddValue("InteropIEDInteropLD", "MMXU1", "MX", "TotW", "mag$f", mms.Float32(0))
	s.AddValue("InteropIEDInteropLD", "MMTR1", "ST", "TotVAh", "actVal", mms.Integer(0))
	s.AddValue("InteropIEDInteropLD", "LLN0", "DC", "Mod", "d", mms.Visible(""))
	s.AddValue("InteropIEDInteropLD", "LLN0", "CF", "Mod", "ctlModel", mms.Integer(0))
	s.AddValue("InteropIEDInteropLD", "LLN0", "SP", "X", "u", mms.Unsigned(0))
	s.AddValue("InteropIEDInteropLD", "LLN0", "SP", "X", "m", mms.MMSString(""))
	err := applyValues(s, strings.NewReader(`{"values":{"InteropLD/GGIO1.SPS1.stVal":true,"InteropLD/GGIO1.DPS1.stVal":2,
		"InteropLD/MMXU1.TotW.mag.f":1234.5,"InteropLD/MMTR1.TotVAh.actVal":42,"InteropLD/LLN0.Mod.d":"mode",
		"InteropLD/LLN0.Mod.ctlModel":1,"InteropLD/LLN0.X.u":7,"InteropLD/LLN0.X.m":"m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("InteropIEDInteropLD", "GGIO1$ST$DPS1$stVal"); v.Bits.Bits[0]>>6 != 2 {
		t.Errorf("DPS1 %v", v)
	}
	if v, _ := s.Get("InteropIEDInteropLD", "MMTR1$ST$TotVAh$actVal"); v.Int != 42 {
		t.Errorf("TotVAh %v", v)
	}
	for _, bad := range []string{`{}`, `x`, `{"values":{"nope":1}}`, `{"values":{"InteropLD/GGIO1.Nope.stVal":1}}`,
		`{"values":{"InteropLD/GGIO1.SPS1.stVal":1}}`, `{"values":{"InteropLD/GGIO1.DPS1.stVal":4}}`,
		`{"values":{"InteropLD/MMTR1.TotVAh.actVal":"a"}}`, `{"values":{"InteropLD/LLN0.X.u":-1}}`,
		`{"values":{"InteropLD/MMXU1.TotW.mag.f":"a"}}`, `{"values":{"InteropLD/LLN0.Mod.d":1}}`,
		`{"values":{"InteropLD/LLN0.X.m":1}}`} {
		if err := applyValues(s, strings.NewReader(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// The commands against the fixture IED in one process, when the
// repository's fixtures are at hand (not in the image build).
func TestEndToEnd(t *testing.T) {
	const icd, values = "../../fixtures/iec61850/interop.icd", "../../fixtures/iec61850/values.json"
	if _, err := os.Stat(icd); err != nil {
		t.Skip("fixtures not found")
	}
	srv, err := newIEDServer(icd, values)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	port := addr[strings.LastIndex(addr, ":")+1:]

	var code int
	lines := capture(t, func() {
		code = run("cbsigma-ied-client", []string{"--port", port, "--local-tsel", "0001", "--remote-tsel", "0001"})
	})
	ops := map[string]map[string]any{}
	for _, l := range lines {
		ops[l["operation"].(string)+" "+asString(l["target"])] = l
	}
	if code != exitOpFailed { // the ST write is refused, as IEC 61850-7-2 says
		t.Errorf("ied-client exit %d", code)
	}
	if l := ops["read InteropIEDInteropLD/GGIO1.DPS1.stVal[ST]"]; l == nil || l["value"] != float64(2) {
		t.Errorf("DPS1: %v", l)
	}
	if l := ops["read-dataset InteropIEDInteropLD/LLN0$dsInterop"]; l == nil || l["ok"] != true {
		t.Errorf("dataset: %v", l)
	}
	if l := ops["write InteropIEDInteropLD/LLN0.Mod.stVal[ST]"]; l == nil || l["error"] != "access-denied" {
		t.Errorf("write: %v", l)
	}

	for _, do := range []string{"SPCSO1", "SPCSO2", "SPCSO3"} {
		lines = capture(t, func() { code = run("cbsigma-ied-controller", []string{"--port", port, "--do", do}) })
		last := lines[len(lines)-2]
		if code != exitOK || last["operation"] != "read-stval" || last["value"] != true {
			t.Errorf("%s: code %d, %v", do, code, lines)
		}
	}

	// The reporter's data change is an ST write, which the simulator
	// refuses: no report.
	lines = capture(t, func() { code = run("cbsigma-ied-reporter", []string{"--port", port}) })
	if code != exitOK || len(lines) != 6 || lines[0]["rptID"] != "interop_urcb01" || lines[1]["ok"] != true || lines[4]["ok"] != true {
		t.Errorf("reporter: code %d, %v", code, lines)
	}

	// The general interrogation of contract 1.3: a report of both members,
	// whatever the server allows clients to write.
	lines = capture(t, func() { code = run("cbsigma-ied-reporter", []string{"--port", port, "--trigger", "gi"}) })
	if code != exitOK || len(lines) != 6 || lines[2]["operation"] != "trigger-gi" || lines[2]["ok"] != true {
		t.Fatalf("reporter --trigger gi: code %d, %v", code, lines)
	}
	if r := lines[3]; r["ok"] != true || fmt.Sprint(r["reasons"]) != "[gi gi]" || fmt.Sprint(r["inclusion"]) != "[true true]" ||
		fmt.Sprint(r["values"]) != "[false 1]" {
		t.Errorf("GI report: %v", r)
	}
	capture(t, func() { code = run("cbsigma-ied-reporter", []string{"--trigger", "x"}) })
	if code != exitArgs {
		t.Errorf("--trigger x: code %d", code)
	}

	// A report the simulator does send: a data change from its process.
	c, err := dial(context.Background(), addr, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	rcb, _ := iec61850.ParseRef("InteropIEDInteropLD/LLN0$RP$urcb0101", "") // indexed: the first instance
	if err := c.EnableReport(context.Background(), rcb, iec61850.RCBOptions{}); err != nil {
		t.Fatal(err)
	}
	srv.Set("InteropIEDInteropLD", "GGIO1$ST$SPS1$stVal", mms.Boolean(true))
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	l := receive(ctx, c, "interop_urcb01", 2)
	if l["ok"] != true || l["reasons"].([]string)[0] != "data-change" || !l["inclusion"].([]bool)[0] {
		t.Errorf("report: %v", l)
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
