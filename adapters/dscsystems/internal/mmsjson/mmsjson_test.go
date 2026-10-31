// SPDX-License-Identifier: GPL-3.0-only

package mmsjson

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
)

// roundTrip feeds the JSON text of a Typed through ToValue and FromValue,
// the path a fixture value takes to a client's output.
func roundTrip(t *testing.T, in string) string {
	t.Helper()
	vals, err := ParseValues("[" + in + "]")
	if err != nil {
		t.Fatalf("%s: %v", in, err)
	}
	out, err := json.Marshal(FromValue(vals[0]))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestRoundTrip(t *testing.T) {
	cases := []string{
		`{"type":"boolean","value":true}`,
		`{"type":"boolean","value":false}`,
		`{"type":"integer","value":-42}`,
		`{"type":"unsigned","value":4294967295}`,
		`{"type":"float32","value":1234.5}`,
		`{"type":"float32","value":0.1}`,
		`{"type":"float64","value":0.1}`,
		`{"type":"visible-string","value":"abc"}`,
		`{"type":"mms-string","value":"äö"}`,
		`{"type":"octet-string","value":"00ff10"}`,
		`{"type":"bit-string","value":"0110000001"}`,
		`{"type":"utc-time","value":"2024-01-02T03:04:05.250Z"}`,
		`{"type":"binary-time","value":"2024-01-02T03:04:05.250Z"}`,
		`{"type":"array","value":[{"type":"integer","value":1},{"type":"integer","value":2}]}`,
		`{"type":"structure","value":[{"type":"boolean","value":true},{"type":"array","value":[{"type":"visible-string","value":"x"}]}]}`,
	}
	for _, in := range cases {
		if got := roundTrip(t, in); got != in {
			t.Errorf("\n in %s\nout %s", in, got)
		}
	}
}

// The float32 form must be the shortest that reads back as the same
// float32, not the float64 widening 0.10000000149011612.
func TestFloat32ShortestForm(t *testing.T) {
	got, err := json.Marshal(FromValue(mms.NewFloat32(0.1)).Value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0.1" {
		t.Errorf("got %s", got)
	}
}

func TestNonFiniteFloatsAreStrings(t *testing.T) {
	cases := []struct {
		v    *mms.Value
		want string
	}{
		{mms.NewFloat32(float32(math.NaN())), "NaN"},
		{mms.NewFloat32(float32(math.Inf(1))), "+Inf"},
		{mms.NewFloat64(math.NaN()), "NaN"},
		{mms.NewFloat64(math.Inf(-1)), "-Inf"},
	}
	for _, c := range cases {
		typed := FromValue(c.v)
		if typed.Value != c.want {
			t.Errorf("%s: got %v, want %v", typed.Type, typed.Value, c.want)
		}
		if _, err := json.Marshal(typed); err != nil {
			t.Errorf("%s: not encodable: %v", c.want, err)
		}
	}
}

func TestFromValueSpecial(t *testing.T) {
	if got := FromValue(nil); got.Type != "none" {
		t.Errorf("nil: %+v", got)
	}
	got := FromValue(mms.NewDataAccessError(mms.AccessObjectNonExistent))
	if got.Type != "data-access-error" || got.Value != int(mms.AccessObjectNonExistent) {
		t.Errorf("access error: %+v", got)
	}
	list := FromValues([]*mms.Value{mms.NewBool(true), nil})
	if len(list) != 2 || list[0].Type != "boolean" || list[1].Type != "none" {
		t.Errorf("FromValues: %+v", list)
	}
}

func TestFormatTimeRoundsToMillisecondInUTC(t *testing.T) {
	in := time.Date(2024, 1, 2, 4, 4, 5, 123600000, time.FixedZone("x", 3600))
	if got, want := FormatTime(in), "2024-01-02T03:04:05.124Z"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestToValueRejects(t *testing.T) {
	cases := []struct {
		name string
		in   Typed
		want string
	}{
		{"bool from number", Typed{"boolean", 1.0}, "not a JSON boolean"},
		{"integer from string", Typed{"integer", "1"}, "not a JSON number"},
		{"negative unsigned", Typed{"unsigned", -1.0}, "non-negative"},
		{"unsigned from string", Typed{"unsigned", "1"}, "non-negative"},
		{"unsigned over 32 bits", Typed{"unsigned", float64(math.MaxUint32) + 1}, "exceeds 32 bits"},
		{"float32 from string", Typed{"float32", "1"}, "float32"},
		{"float64 from bool", Typed{"float64", true}, "float64"},
		{"string from number", Typed{"visible-string", 1.0}, "not a JSON string"},
		{"odd hex", Typed{"octet-string", "abc"}, "octet-string"},
		{"octet-string from number", Typed{"octet-string", 5.0}, "not a hex string"},
		{"octet-string missing value", Typed{"octet-string", nil}, "not a hex string"},
		{"bit-string from number", Typed{"bit-string", 1.0}, "binary digits"},
		{"bit-string digit", Typed{"bit-string", "012"}, "not binary digits"},
		{"bad time", Typed{"utc-time", "yesterday"}, "utc-time"},
		{"time from number", Typed{"binary-time", 1.0}, "binary-time"},
		{"array of non-typed", Typed{"array", []any{1.0}}, "not a list of typed values"},
		{"structure bad member", Typed{"structure", []any{map[string]any{"type": "nope"}}}, "unsupported type"},
		{"unknown type", Typed{"real128", 1.0}, "unsupported type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ToValue(c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestToValueAcceptsJSONNumber(t *testing.T) {
	v, err := ToValue(Typed{"integer", json.Number("7")})
	if err != nil || v.Int64() != 7 {
		t.Errorf("v=%v err=%v", v, err)
	}
}

func TestToValuesReportsIndex(t *testing.T) {
	_, err := ToValues([]Typed{{"boolean", true}, {"boolean", 1.0}})
	if err == nil || !strings.HasPrefix(err.Error(), "value 1:") {
		t.Errorf("err = %v", err)
	}
}

func TestParseValues(t *testing.T) {
	if _, err := ParseValues(`not json`); err == nil || !strings.HasPrefix(err.Error(), "values:") {
		t.Errorf("err = %v", err)
	}
	vals, err := ParseValues(`[]`)
	if err != nil || len(vals) != 0 {
		t.Errorf("vals=%v err=%v", vals, err)
	}
}

func TestBitStringPositions(t *testing.T) {
	v, err := ToValue(Typed{"bit-string", "0100"})
	if err != nil {
		t.Fatal(err)
	}
	if v.BitLen() != 4 || v.Bit(0) || !v.Bit(1) || v.Bit(2) || v.Bit(3) {
		t.Errorf("bits wrong: len %d", v.BitLen())
	}
}

func TestStructureChildrenKeepOrder(t *testing.T) {
	v := mms.NewStructure(mms.NewInt32(1), mms.NewVisibleString("a"))
	got := FromValue(v)
	want := Typed{"structure", []Typed{{"integer", int64(1)}, {"visible-string", "a"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}
