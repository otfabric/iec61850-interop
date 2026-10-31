// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

func TestLoadsTheInteropFixture(t *testing.T) {
	f, err := loadFixture(filepath.Join("..", "..", "..", "..", "fixtures", "mms", "interop.json"))
	if err != nil {
		t.Fatal(err)
	}
	if f.revision != jl.MMSFixtureRevision {
		t.Errorf("revision %s, adapter declares %s", f.revision, jl.MMSFixtureRevision)
	}
	if len(f.domains) != 1 || f.domains[0].name != "interop" {
		t.Fatalf("domains: %+v", f.domains)
	}
	vars := f.domains[0].vars
	if len(vars) == 0 {
		t.Fatal("no variables")
	}
	if !slices.IsSortedFunc(vars, func(a, b *varDef) int { return strings.Compare(a.name, b.name) }) {
		t.Error("variables not sorted: GetNameList order depends on it")
	}
	for _, v := range vars {
		if v.spec == nil || v.value == nil {
			t.Errorf("%s: spec %v value %v", v.name, v.spec, v.value)
		}
	}
	var large *fileDef
	for _, fd := range f.files {
		if fd.name == "LARGE.BIN" {
			large = fd
		}
	}
	if large == nil || len(large.data) != 150000 {
		t.Fatalf("LARGE.BIN: %+v", large)
	}
	if large.data[250] != 250 || large.data[251] != 0 || large.lastModified.IsZero() {
		t.Error("generated content is not byte i = i % 251")
	}
}

func load(t *testing.T, body string) (*fixtureDef, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadFixture(p)
}

func TestFixtureDefaults(t *testing.T) {
	f, err := load(t, `{"domains":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if f.revision != "mms-v1" || f.vendor != "OTFabric" || f.model != "iec61850-interop" || f.revisionString != "1.0" {
		t.Errorf("%+v", f)
	}
}

func TestFixtureRejects(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"not json", `{`, "parse"},
		{"no domains", `{}`, "missing 'domains'"},
		{"bad variable", `{"domains":{"d":{"variables":{"v":{"type":"nope"}}}}}`, "variable d/v"},
		{"bad journal time", `{"domains":{"d":{"journals":{"j":{"entries":[{"occurrenceTime":"x"}]}}}}}`, "journal j entry 0"},
		{"bad journal value", `{"domains":{"d":{"journals":{"j":{"entries":[{"occurrenceTime":"2024-01-01T00:00:00Z","type":"boolean","value":1}]}}}}}`, "journal j entry 0"},
		{"bad hex", `{"domains":{},"files":{"f":{"hex":"zz"}}}`, "bad hex"},
		{"empty file", `{"domains":{},"files":{"f":{}}}`, "needs content, hex or size"},
		{"bad mtime", `{"domains":{},"files":{"f":{"content":"","lastModified":"x"}}}`, "lastModified"},
		{"report without variables", `{"domains":{},"informationReports":[{}]}`, "missing 'variables'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, c.body)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestBuildVar(t *testing.T) {
	arr, err := buildVar("a", jsonVar{Type: "array", ElementType: "integer", Value: []any{1.0, 2.0}, Writable: true})
	if err != nil {
		t.Fatal(err)
	}
	if arr.spec.Elements != 2 || arr.spec.Element.Size != 32 || !arr.writable {
		t.Errorf("array spec %+v", arr.spec)
	}
	declared, err := buildVar("a", jsonVar{Type: "array", ElementType: "integer", Value: []any{1.0}, Count: 8})
	if err != nil || declared.spec.Elements != 8 {
		t.Errorf("declared count: %+v, %v", declared, err)
	}
	st, err := buildVar("s", jsonVar{Type: "structure", Value: []any{
		map[string]any{"name": "flag", "type": "boolean", "value": true},
		map[string]any{"type": "integer", "size": 16.0, "value": 3.0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if st.spec.Components[0].Name != "flag" || st.spec.Components[1].Name != "comp1" || st.spec.Components[1].Spec.Size != 16 {
		t.Errorf("components %+v", st.spec.Components)
	}
	if st.value.Len() != 2 {
		t.Errorf("value of %d members", st.value.Len())
	}
}

func TestBuildVarRejects(t *testing.T) {
	cases := map[string]jsonVar{
		"array without element type": {Type: "array", Value: []any{}},
		"array of no list":           {Type: "array", ElementType: "integer", Value: 1.0},
		"array of unknown type":      {Type: "array", ElementType: "nope", Value: []any{}},
		"array element":              {Type: "array", ElementType: "boolean", Value: []any{1.0}},
		"structure of no list":       {Type: "structure", Value: 1.0},
		"structure component type":   {Type: "structure", Value: []any{map[string]any{"type": "nope"}}},
		"structure component value":  {Type: "structure", Value: []any{map[string]any{"type": "boolean", "value": 1.0}}},
		"scalar value":               {Type: "boolean", Value: 1.0},
	}
	for name, jv := range cases {
		if _, err := buildVar("v", jv); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestScalarSpecSizes(t *testing.T) {
	cases := []struct {
		typ  string
		size int
		kind mms.Type
		want int
	}{
		{"integer", 0, mms.TypeInteger, 32},
		{"integer", 8, mms.TypeInteger, 8},
		{"unsigned", 0, mms.TypeUnsigned, 32},
		{"visible-string", 0, mms.TypeVisibleString, -1},
		{"visible-string", 32, mms.TypeVisibleString, 32},
		{"octet-string", 0, mms.TypeOctetString, -1},
		{"bit-string", 0, mms.TypeBitString, -1},
		{"boolean", 0, mms.TypeBoolean, 0},
		{"float32", 0, mms.TypeFloat32, 0},
		{"utc-time", 0, mms.TypeUTCTime, 0},
	}
	for _, c := range cases {
		s, err := scalarSpec(c.typ, c.size)
		if err != nil || s.Kind != c.kind || s.Size != c.want {
			t.Errorf("%s/%d: %+v, %v", c.typ, c.size, s, err)
		}
	}
	if _, err := scalarSpec("float64", 0); err == nil {
		t.Error("float64 accepted: fixture.c has no such scalar")
	}
}

// The file services take names from the client: none may leave the store.
func TestStoreName(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", ".", true},
		{"/", ".", true},
		{"REC01.CFG", "REC01.CFG", true},
		{"/dir/REC01.CFG", "dir/REC01.CFG", true},
		{"../etc/passwd", "", false},
		{"/../etc/passwd", "", false},
		{"dir/../../x", "", false},
		{"dir//x", "", false},
		{"./x", "", false},
		{"//etc/passwd", "", false},
	}
	for _, c := range cases {
		got, ok := storeName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q, %v", c.in, got, ok)
		}
	}
}

func TestParseTimeOfDay(t *testing.T) {
	if _, err := parseTimeOfDay([]byte{1, 2, 3}); err == nil {
		t.Error("3 octets accepted")
	}
	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := parseTimeOfDay(mms.NewBinaryTime(want).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("got %v", got)
	}
}
