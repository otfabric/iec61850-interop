// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mms "github.com/otfabric/go-mms"
	"github.com/otfabric/go-mms/transport/iso"
)

// The repository's fixtures are not in the build stage, so the tests carry
// a copy of fixture mms-v2 (fixtures/mms/interop.json, comments dropped).
const testMMSFixture = `{
  "revision": "mms-v2",
  "identity": {"vendor": "OTFabric", "model": "libiec61850-mms-server", "revision": "1.0"},
  "status": {"logical": 1, "physical": 1},
  "domains": {
    "interop": {
      "variables": {
        "boolean": {"type": "boolean", "value": true, "writable": true},
        "integer": {"type": "integer", "size": 32, "value": -123, "writable": true},
        "unsigned": {"type": "unsigned", "size": 32, "value": 456, "writable": true},
        "float32": {"type": "float32", "value": 21.5, "writable": true},
        "visible-string": {"type": "visible-string", "size": 64, "value": "interop", "writable": true},
        "octet-string": {"type": "octet-string", "value": "deadbeef"},
        "bit-string": {"type": "bit-string", "value": "10110"},
        "utc-time": {"type": "utc-time", "value": "2024-01-01T00:00:00.000Z"},
        "array": {"type": "array", "elementType": "integer", "count": 5, "value": [1, 2, 3, 4, 5]},
        "structure": {"type": "structure", "value": [{"type": "boolean", "value": true}, {"type": "integer", "value": 42}]},
        "array-rw": {"type": "array", "elementType": "integer", "count": 4, "value": [10, 20, 30, 40], "writable": true},
        "structure-rw": {"type": "structure", "value": [{"name": "flag", "type": "boolean", "value": false}, {"name": "count", "type": "integer", "value": 7}], "writable": true},
        "report-trigger": {"type": "integer", "size": 32, "value": 0, "writable": true}
      },
      "journals": {
        "log1": {"entries": [
          {"entryId": 1, "occurrenceTime": "2024-01-01T00:00:01.000Z", "tag": "interop/integer", "type": "integer", "value": 1, "reasonCode": 2},
          {"entryId": 2, "occurrenceTime": "2024-01-01T00:00:02.000Z", "tag": "interop/integer", "type": "integer", "value": 2, "reasonCode": 2},
          {"entryId": 3, "occurrenceTime": "2024-01-01T00:00:03.000Z", "tag": "interop/boolean", "type": "boolean", "value": false, "reasonCode": 4},
          {"entryId": 4, "occurrenceTime": "2024-01-01T00:00:04.000Z", "tag": "interop/integer", "type": "integer", "value": 4, "reasonCode": 2}
        ]}
      }
    }
  },
  "informationReports": [
    {"variables": ["interop/integer", "interop/boolean"]},
    {"vmdList": "interop-report", "variables": ["interop/integer", "interop/boolean"]}
  ],
  "files": {
    "REC01.CFG": {"content": "station,device,1999\n1,1A,0D\n", "lastModified": "2024-01-02T03:04:05Z"},
    "REC01.DAT": {"hex": "000102030405060708090a0b0c0d0e0f", "lastModified": "2024-01-02T03:04:05Z"},
    "LARGE.BIN": {"size": 150000, "lastModified": "2024-01-02T03:04:05Z"},
    "COMTRADE/REC02.CFG": {"content": "nested file in a subdirectory\n", "lastModified": "2024-01-02T03:04:05Z"},
    "DELETE.ME": {"content": "target of the FileDelete test\n", "lastModified": "2024-01-02T03:04:05Z"},
    "RENAME.ME": {"content": "target of the FileRename test\n", "lastModified": "2024-01-02T03:04:05Z"},
    "NOTIME.TXT": {"content": "file without a fixture modification time\n"}
  }
}`

var mmsTestStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testMMSFixtureFile(t *testing.T) *mmsFixtureFile {
	t.Helper()
	p := filepath.Join(t.TempDir(), "interop.json")
	if err := os.WriteFile(p, []byte(testMMSFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	fix, err := loadMMSFixture(p)
	if err != nil {
		t.Fatal(err)
	}
	return fix
}

type testMMSServer struct {
	addr string
	srv  *mms.Server
}

func startTestMMSServer(t *testing.T) *testMMSServer {
	t.Helper()
	fix := testMMSFixtureFile(t)
	srv, err := newMMSServer(fix, mmsTestStart)
	if err != nil {
		t.Fatalf("newMMSServer: %v", err)
	}
	ln, err := iso.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(context.Background(), ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-served; !errors.Is(err, mms.ErrServerClosed) {
			t.Errorf("ListenAndServe = %v, want ErrServerClosed", err)
		}
	})
	return &testMMSServer{addr: ln.Addr().String(), srv: srv}
}

// mmsLine is one operation line of the client.
type mmsLine struct {
	Operation string   `json:"operation"`
	Target    *string  `json:"target"`
	OK        bool     `json:"ok"`
	Error     string   `json:"error"`
	Value     any      `json:"value"`
	Values    []any    `json:"values"`
	Names     []string `json:"names"`
	Type      string   `json:"type"`
}

func (l mmsLine) target() string {
	if l.Target == nil {
		return ""
	}
	return *l.Target
}

var mmsCaptureMu sync.Mutex

// runMMSTestClient runs otfabric-mms-client against addr and returns its exit
// code and lines.
func runMMSTestClient(t *testing.T, addr string) (int, []mmsLine) {
	t.Helper()
	mmsCaptureMu.Lock()
	defer mmsCaptureMu.Unlock()
	var out, errb bytes.Buffer
	stdoutMu.Lock()
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &out, &errb
	stdoutMu.Unlock()
	defer func() {
		stdoutMu.Lock()
		stdout, stderr = oldOut, oldErr
		stdoutMu.Unlock()
		if t.Failed() {
			t.Logf("stderr: %s", errb.String())
		}
	}()
	host, port, _ := net.SplitHostPort(addr)
	code := run("otfabric-mms-client", []string{"--host", host, "--port", port})
	var lines []mmsLine
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var l mmsLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("not a JSON line: %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	return code, lines
}

func mmsFind(t *testing.T, lines []mmsLine, operation, target string) mmsLine {
	t.Helper()
	for _, l := range lines {
		if l.Operation == operation && l.target() == target {
			return l
		}
	}
	t.Fatalf("no %s %q line", operation, target)
	return mmsLine{}
}

func mmsOK(t *testing.T, lines []mmsLine, operation, target string) mmsLine {
	t.Helper()
	l := mmsFind(t, lines, operation, target)
	if !l.OK {
		t.Errorf("%s %q failed: %s", operation, target, l.Error)
	}
	return l
}

func mmsFail(t *testing.T, lines []mmsLine, operation, target, want string) {
	t.Helper()
	l := mmsFind(t, lines, operation, target)
	if l.OK || l.Error != want {
		t.Errorf("%s %q: ok=%v error=%q, want error %q", operation, target, l.OK, l.Error, want)
	}
}

func mmsEqualJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	g, _ := json.Marshal(got)
	var gg any
	_ = json.Unmarshal(g, &gg)
	if !reflect.DeepEqual(gg, w) {
		t.Errorf("%s = %s, want %s", what, g, want)
	}
}

// TestMMSClientAgainstServer runs the client's whole sequence against the
// server and checks what go-mms's interop suite checks of a peer client.
func TestMMSClientAgainstServer(t *testing.T) {
	s := startTestMMSServer(t)
	code, lines := runMMSTestClient(t, s.addr)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}

	mmsOK(t, lines, "identify", "")
	if names := mmsOK(t, lines, "get-name-list", "").Names; !reflect.DeepEqual(names, []string{"interop"}) {
		t.Errorf("domains = %v", names)
	}
	vars := mmsOK(t, lines, "get-name-list", "interop").Names
	for _, w := range []string{"boolean", "integer", "float32", "unsigned", "visible-string", "octet-string", "bit-string", "utc-time", "array", "structure"} {
		if !strings.Contains(","+strings.Join(vars, ",")+",", ","+w+",") {
			t.Errorf("variable %s not listed: %v", w, vars)
		}
	}
	mmsEqualJSON(t, "integer", mmsOK(t, lines, "read", "interop/integer").Value, `-123`)
	mmsEqualJSON(t, "float32", mmsOK(t, lines, "read", "interop/float32").Value, `21.5`)
	mmsOK(t, lines, "write", "interop/float32")
	mmsFail(t, lines, "write", "interop/octet-string", "object-access-denied")
	mmsEqualJSON(t, "unsigned", mmsOK(t, lines, "read", "interop/unsigned").Value, `456`)
	mmsEqualJSON(t, "octet-string", mmsOK(t, lines, "read", "interop/octet-string").Value, `"deadbeef"`)
	mmsEqualJSON(t, "bit-string", mmsOK(t, lines, "read", "interop/bit-string").Value, `"10110"`)
	mmsEqualJSON(t, "utc-time", mmsOK(t, lines, "read", "interop/utc-time").Value, `1704067200000`)
	mmsEqualJSON(t, "array", mmsOK(t, lines, "read", "interop/array").Value, `[1,2,3,4,5]`)
	mmsEqualJSON(t, "structure", mmsOK(t, lines, "read", "interop/structure").Value, `[true,42]`)
	if typ := mmsOK(t, lines, "get-var-access-attr", "interop/boolean").Type; typ != "boolean" {
		t.Errorf("boolean type %q", typ)
	}
	mmsEqualJSON(t, "read-multiple", mmsOK(t, lines, "read-multiple", "interop").Values, `[true,-123]`)
	mmsOK(t, lines, "write-multiple", "interop")
	mmsOK(t, lines, "define-nvl", "interop/testlist")
	mmsEqualJSON(t, "testlist", mmsOK(t, lines, "read-nvl", "interop/testlist").Values, `[false,0]`)
	mmsOK(t, lines, "delete-nvl", "interop/testlist")
	mmsFail(t, lines, "read", "unknown-domain/boolean", "object-non-existent")
	mmsFail(t, lines, "read", "interop/nonexistent", "object-non-existent")
	mmsFail(t, lines, "write", "interop/integer-wrong-type", "type-inconsistent")

	mmsEqualJSON(t, "status", mmsOK(t, lines, "status", "").Value, `{"logical":1,"physical":1}`)
	mmsEqualJSON(t, "status extended", mmsOK(t, lines, "status", "extended").Value, `{"logical":1,"physical":1}`)
	mmsEqualJSON(t, "journals", mmsOK(t, lines, "get-name-list", "interop/journals").Names, `["log1"]`)
	mmsEqualJSON(t, "vmd variables", mmsOK(t, lines, "get-name-list", "vmd/variables").Names, `[]`)
	mmsFail(t, lines, "get-name-list", "unknown-domain/variables", "object-non-existent")
	mmsEqualJSON(t, "array-rw spec", mmsOK(t, lines, "get-var-access-attr", "interop/array-rw").Value,
		`{"type":"array","count":4,"element":{"type":"integer"}}`)
	mmsEqualJSON(t, "structure-rw spec", mmsOK(t, lines, "get-var-access-attr", "interop/structure-rw").Value,
		`{"type":"structure","components":[{"name":"flag","spec":{"type":"boolean"}},{"name":"count","spec":{"type":"integer"}}]}`)
	mmsEqualJSON(t, "array[1:4]", mmsOK(t, lines, "read-array-elements", "interop/array[1:4]").Value, `[2,3,4]`)
	mmsEqualJSON(t, "array[2]", mmsOK(t, lines, "read-array-elements", "interop/array[2]").Value, `3`)
	mmsEqualJSON(t, "count", mmsOK(t, lines, "read-component", "interop/structure-rw$count").Value, `7`)
	mmsOK(t, lines, "write-array-elements", "interop/array-rw[1]")
	mmsOK(t, lines, "write-array-elements", "interop/array-rw[2:4]")
	mmsOK(t, lines, "write-component", "interop/structure-rw$count")
	mmsEqualJSON(t, "array-rw", mmsOK(t, lines, "read", "interop/array-rw").Value, `[10,21,33,44]`)
	mmsEqualJSON(t, "structure-rw", mmsOK(t, lines, "read", "interop/structure-rw").Value, `[false,99]`)

	mmsOK(t, lines, "define-nvl", "interop/svclist")
	mmsEqualJSON(t, "domain lists", mmsOK(t, lines, "get-name-list", "interop/nvl").Names, `["svclist"]`)
	members := `{"deletable":true,"variables":["interop/boolean","interop/integer"]}`
	mmsEqualJSON(t, "svclist attrs", mmsOK(t, lines, "get-nvl-attrs", "interop/svclist").Value, members)
	mmsEqualJSON(t, "svclist spec", mmsOK(t, lines, "read-nvl", "interop/svclist?spec").Value, `[false,0]`)
	mmsOK(t, lines, "write-nvl", "interop/svclist")
	mmsEqualJSON(t, "svclist", mmsOK(t, lines, "read-nvl", "interop/svclist").Value, `[true,77]`)
	mmsOK(t, lines, "delete-nvl", "interop/svclist")
	mmsOK(t, lines, "define-nvl", "vmd/vmdlist")
	mmsEqualJSON(t, "vmdlist attrs", mmsOK(t, lines, "get-nvl-attrs", "vmd/vmdlist").Value, members)
	mmsEqualJSON(t, "vmdlist", mmsOK(t, lines, "read-nvl", "vmd/vmdlist").Value, `[true,77]`)
	mmsOK(t, lines, "delete-nvl", "vmd/vmdlist")
	// Not defined by this server, as by the other stacks' servers.
	mmsFail(t, lines, "get-nvl-attrs", "vmd/interop-report", "object-non-existent")
	mmsOK(t, lines, "define-nvl", "aa/aalist")
	mmsEqualJSON(t, "aa lists", mmsOK(t, lines, "get-name-list", "aa/nvl").Names, `["aalist"]`)
	mmsEqualJSON(t, "aalist attrs", mmsOK(t, lines, "get-nvl-attrs", "aa/aalist").Value, members)
	mmsEqualJSON(t, "aalist", mmsOK(t, lines, "read-nvl", "aa/aalist").Value, `[true,77]`)
	mmsOK(t, lines, "write-nvl", "aa/aalist")
	mmsOK(t, lines, "delete-nvl", "aa/aalist")

	root := mmsOK(t, lines, "file-directory", "").Value.([]any)
	if len(root) != 7 {
		t.Errorf("root listing has %d entries, want the 7 files: %v", len(root), root)
	}
	for _, e := range root {
		m := e.(map[string]any)
		if m["lastModified"].(float64) == 0 {
			t.Errorf("%v has no lastModified", m["name"])
		}
		if m["name"] == "NOTIME.TXT" && int64(m["lastModified"].(float64)) != mmsTestStart.UnixMilli() {
			t.Errorf("NOTIME.TXT lastModified %v, want the server start", m["lastModified"])
		}
	}
	for _, spec := range []string{"COMTRADE", `COMTRADE\`} {
		l := mmsOK(t, lines, "file-directory", spec).Value.([]any)
		if len(l) != 1 || l[0].(map[string]any)["name"] != "COMTRADE/REC02.CFG" {
			t.Errorf("%s listing = %v", spec, l)
		}
	}
	rec := mmsOK(t, lines, "file-read", "REC01.CFG").Value.(map[string]any)
	if rec["data"] != "73746174696f6e2c6465766963652c313939390a312c31412c30440a" || rec["lastModified"].(float64) != 1704164645000 {
		t.Errorf("REC01.CFG = %v", rec)
	}
	mmsEqualJSON(t, "REC01.DAT@4", mmsOK(t, lines, "file-read", "REC01.DAT@4").Value.(map[string]any)["data"], `"0405060708090a0b0c0d0e0f"`)
	large := mmsOK(t, lines, "file-read", "LARGE.BIN").Value.(map[string]any)
	if large["length"].(float64) != 150000 || large["data"] != nil {
		t.Errorf("LARGE.BIN = %v", large)
	}
	mmsOK(t, lines, "file-close", "LARGE.BIN")
	mmsFail(t, lines, "file-read", "NO-SUCH.FILE", "file-non-existent")
	mmsOK(t, lines, "file-delete", "DELETE.ME")
	mmsOK(t, lines, "file-rename", "RENAME.ME->RENAMED.ME")
	mmsOK(t, lines, "obtain-file", "REC01.CFG->OBTAINED.CFG")
	var after []string
	for _, e := range mmsOK(t, lines, "file-directory", "after-changes").Value.([]any) {
		after = append(after, e.(map[string]any)["name"].(string))
	}
	mmsEqualJSON(t, "after changes", after, `["COMTRADE/REC02.CFG","LARGE.BIN","NOTIME.TXT","OBTAINED.CFG","REC01.CFG","REC01.DAT","RENAMED.ME"]`)

	entry := func(id string, ms int64, tag, value, reason string) string {
		return `{"id":"` + id + `","time":` + strconv.FormatInt(ms, 10) + `,"variables":[{"tag":"` + tag + `","value":` + value + `},{"tag":"ReasonCode","value":"` + reason + `"}]}`
	}
	e1 := entry("0100000000000000", 1704067201000, "interop/integer", "1", "0100000")
	e2 := entry("0200000000000000", 1704067202000, "interop/integer", "2", "0100000")
	e3 := entry("0300000000000000", 1704067203000, "interop/boolean", "false", "0010000")
	e4 := entry("0400000000000000", 1704067204000, "interop/integer", "4", "0100000")
	mmsEqualJSON(t, "log1", mmsOK(t, lines, "read-journal", "interop/log1").Value, `{"moreFollows":false,"entries":[`+e1+`,`+e2+`,`+e3+`,`+e4+`]}`)
	mmsEqualJSON(t, "log1 after 2", mmsOK(t, lines, "read-journal", "interop/log1@after-2").Value, `{"moreFollows":false,"entries":[`+e3+`,`+e4+`]}`)
	mmsFail(t, lines, "read-journal", "interop/nolog", "object-non-existent")

	mmsOK(t, lines, "write", "interop/report-trigger")
	mmsEqualJSON(t, "var integer", mmsOK(t, lines, "information-report", "var:interop/integer").Value, `-123`)
	mmsEqualJSON(t, "var boolean", mmsOK(t, lines, "information-report", "var:interop/boolean").Value, `true`)
	mmsEqualJSON(t, "named list", mmsOK(t, lines, "information-report", "list:interop-report").Value, `[-123,true]`)
	mmsOK(t, lines, "read", "interop/boolean#after-reports")

	conclude := 0
	for _, l := range lines {
		if l.Operation == "conclude" {
			conclude++
			if !l.OK || l.Target != nil {
				t.Errorf("conclude line %+v", l)
			}
		}
	}
	if conclude != 2 {
		t.Errorf("%d conclude lines, want 2 (full sequence and reconnect)", conclude)
	}
	mmsOK(t, lines, "read", "interop/boolean#before-abort")
	mmsOK(t, lines, "abort", "")
	mmsOK(t, lines, "server-abort", "")
}

func TestMMSClientConnectFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if code, lines := runMMSTestClient(t, addr); code != exitConnect || len(lines) != 0 {
		t.Errorf("exit %d with %d lines, want %d and none", code, len(lines), exitConnect)
	}
}

func TestMMSArguments(t *testing.T) {
	for _, args := range [][]string{{"--port", "0"}, {"--port", "70000"}, {"extra"}, {"--frobnicate"}} {
		if code := runMMSClient(args); code != exitArgs {
			t.Errorf("mms-client %v: exit %d", args, code)
		}
		if code := runMMSServer(args); code != exitArgs {
			t.Errorf("mms-server %v: exit %d", args, code)
		}
	}
	if code := runMMSServer([]string{"--fixture", filepath.Join(t.TempDir(), "missing.json")}); code != exitArgs {
		t.Errorf("missing fixture: exit %d", code)
	}
}

// TestMMSServerPeerExpectations checks the server behaviours go-mms's client
// suite expects of a peer server.
func TestMMSServerPeerExpectations(t *testing.T) {
	s := startTestMMSServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := iso.Dial(ctx, s.addr, iso.WithClientDialOptions(mms.DialOptions{MMS: mms.MMSOptions{MaxPDUSize: 200}}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	first, err := c.FileDirectory(ctx, mms.FileDirectoryRequest{})
	if err != nil || !first.MoreFollows {
		t.Errorf("FileDirectory with a 200-byte PDU: %+v, %v; want MoreFollows", first, err)
	}
	all, err := c.FileDirectoryAll(ctx, "")
	if err != nil || len(all) != 7 {
		t.Errorf("FileDirectoryAll = %d entries, %v; want 7", len(all), err)
	}
	f, err := c.FileOpen(ctx, "REC01.DAT", mms.FileOpenOptions{InitialPosition: 4})
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.FileReadAll(ctx, f.FrsmID)
	if err != nil || !bytes.Equal(data, []byte{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}) {
		t.Errorf("REC01.DAT from 4 = %x, %v", data, err)
	}
	_ = c.FileClose(ctx, f.FrsmID)

	err = c.ObtainFile(ctx, "local.cfg", "OBTAINED.CFG")
	if se, ok := errors.AsType[*mms.ServiceError](err); !ok || se.Class != mms.ErrorClassFile {
		t.Errorf("ObtainFile of a file the server lacks = %v, want a File-class service error", err)
	}

	if err := c.DefineNamedVariableList(ctx, mms.DefineNamedVariableListRequest{ListName: mmsVMD("vmdlist"), Variables: mmsMembers()}); err != nil {
		t.Fatal(err)
	}
	names, err := c.GetNameList(ctx, mms.NameListRequest{ObjectClass: mms.ObjectClassNamedVariableList, Scope: mms.ObjectScopeVMD})
	if err != nil || !reflect.DeepEqual(names.Names, []string{"vmdlist"}) {
		t.Errorf("VMD lists = %+v, %v; want [vmdlist]", names, err)
	}

	got := make(chan *mms.InformationReportIndication, 8)
	c.OnInformationReport(func(r *mms.InformationReportIndication) { got <- r })
	if _, err := c.Write(ctx, mms.WriteRequest{DomainID: "interop", ItemID: "report-trigger", Value: mms.NewInteger(1)}); err != nil {
		t.Fatal(err)
	}
	for i, wantList := range []bool{false, true} {
		select {
		case r := <-got:
			if (r.ListName != nil) != wantList || len(r.Values) != 2 {
				t.Errorf("report %d = %+v", i, r)
			}
			if n, _ := r.Values[0].Int64(); n != -123 {
				t.Errorf("report %d integer %v", i, r.Values[0])
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("report %d not received", i)
		}
	}
}

func TestMMSValueJSON(t *testing.T) {
	cases := []struct {
		v    *mms.Value
		want string
	}{
		{mms.NewFloat(0.1), `0.1`},
		{mms.NewFloat(float64(float32(0.1))), `0.1`},
		{mms.NewFloat(0.1 + 1e-12), `0.100000000001`},
		{mms.NewBitStringWithLength([]byte{0xb0}, 5), `"10110"`},
		{mms.NewOctetString([]byte{0xde, 0xad}), `"dead"`},
		{mms.NewArray([]*mms.Value{mms.NewInteger(-1), mms.NewUnsigned(2)}), `[-1,2]`},
		{mms.NewDataAccessError(mms.DataAccessErrorObjectNonExistent), `{"error":"object-non-existent"}`},
	}
	for _, c := range cases {
		mmsEqualJSON(t, c.v.String(), mmsJSON(c.v), c.want)
	}
	if got := mmsErrName(&mms.ServiceError{Class: mms.ErrorClassFile, Code: 7}); got != "file-non-existent" {
		t.Errorf("file error name %q", got)
	}
	if got := mmsErrName(&mms.ServiceError{Class: mms.ErrorClassResource, Code: 1}); got != "memory-unavailable" {
		t.Errorf("resource error name %q", got)
	}
	if got := mmsErrName(mms.ErrClosed); got != "connection-lost" {
		t.Errorf("closed error name %q", got)
	}
}
