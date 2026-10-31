// SPDX-License-Identifier: GPL-3.0-only

package svcli

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/sv"

	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/mmsjson"
)

func quality(t *testing.T, bits string) *mms.Value {
	t.Helper()
	v, err := mmsjson.ToValue(mmsjson.Typed{Type: "bit-string", Value: bits})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSmpModNames(t *testing.T) {
	for _, m := range []sv.SmpMod{sv.SmpPerPeriod, sv.SmpPerSec, sv.SecPerSmp} {
		got, err := ParseSmpMod(SmpModName(m))
		if err != nil || got != m {
			t.Errorf("%d: got %d, %v", m, got, err)
		}
	}
	if got := SmpModName(sv.SmpMod(9)); got != "smpMod(9)" {
		t.Errorf("unknown: %s", got)
	}
	if _, err := ParseSmpMod("per-fortnight"); err == nil {
		t.Error("unknown name accepted")
	}
}

// Position i of the bit string is bit i from the LSB of the word.
func TestQualityWordBitOrder(t *testing.T) {
	w, err := QualityWord(quality(t, "1000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	if want := uint32(1 | 1<<12); w != want {
		t.Errorf("word %#x, want %#x", w, want)
	}
	if got := QualityString(w); got != "1000000000001" {
		t.Errorf("string %s", got)
	}
}

func TestQualityStringDropsDerivedAndReserved(t *testing.T) {
	if got := QualityString(1<<13 | 1<<31 | 1); got != "1000000000000" {
		t.Errorf("got %s", got)
	}
}

func TestQualityWordRejects(t *testing.T) {
	if _, err := QualityWord(mms.NewBool(true)); err == nil {
		t.Error("boolean accepted")
	}
	if _, err := QualityWord(quality(t, strings.Repeat("0", QualityBits+1))); err == nil {
		t.Error("14 bits accepted")
	}
}

func le92Values(t *testing.T) []*mms.Value {
	t.Helper()
	vals := make([]*mms.Value, 0, 16)
	for ch := range 8 {
		q := strings.Repeat("0", QualityBits)
		if ch == 1 {
			q = "1" + q[1:] // the legacy 9-2LE invalid bit, reported as sent
		}
		vals = append(vals, mms.NewInt32(int32(ch*1000-3500)), quality(t, q))
	}
	return vals
}

func le92Stream() *Stream {
	st := &Stream{Profile: Profile92LE, SvID: "s"}
	for range 8 {
		st.Members = append(st.Members, Member{Type: "integer"}, Member{Type: "bit-string"})
	}
	return st
}

func TestEncodeDecode92LE(t *testing.T) {
	b, err := Encode92LE(le92Values(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 64 {
		t.Fatalf("seqData of %d octets", len(b))
	}
	d, err := NewDecoder(le92Stream())
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 16 {
		t.Fatalf("%d values", len(got))
	}
	for ch := range 8 {
		if v := got[2*ch]; v.Type != "integer" || v.Value != int64(ch*1000-3500) {
			t.Errorf("channel %d: %+v", ch, v)
		}
	}
	if got[3].Value != "1000000000000" {
		t.Errorf("channel 1 quality not as sent: %+v", got[3])
	}
}

func TestEncode92LERejects(t *testing.T) {
	good := le92Values(t)
	if _, err := Encode92LE(good[:15]); err == nil {
		t.Error("15 values accepted")
	}
	replace := func(i int, v *mms.Value) []*mms.Value {
		out := append([]*mms.Value(nil), good...)
		out[i] = v
		return out
	}
	if _, err := Encode92LE(replace(0, mms.NewFloat32(1))); err == nil {
		t.Error("float channel accepted")
	}
	if _, err := Encode92LE(replace(0, mms.NewInt64(math.MaxInt32+1))); err == nil {
		t.Error("value beyond INT32 accepted")
	}
	if _, err := Encode92LE(replace(1, mms.NewBool(true))); err == nil {
		t.Error("boolean quality accepted")
	}
}

func TestEncodeDecodeGeneric(t *testing.T) {
	stamp := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	utc := mms.NewUTCTime(stamp, mms.DefaultTimeQuality)
	vals := []*mms.Value{
		mms.NewInt32(-7), mms.NewUint32(math.MaxUint32), mms.NewFloat32(1.5), mms.NewFloat64(-2.25),
		mms.NewBool(true), mms.NewBool(false), quality(t, "0100000000000"), utc,
	}
	b, err := EncodeGeneric(vals)
	if err != nil {
		t.Fatal(err)
	}
	if want := 4 + 4 + 4 + 8 + 1 + 1 + 4 + 8; len(b) != want {
		t.Fatalf("%d octets, want %d", len(b), want)
	}
	if binary.BigEndian.Uint32(b) != math.MaxUint32-6 {
		t.Errorf("integer not big-endian two's complement: %x", b[:4])
	}
	st := &Stream{Profile: ProfileGeneric, SvID: "g"}
	for _, typ := range Types(vals) {
		st.Members = append(st.Members, Member{Type: typ})
	}
	d, err := NewDecoder(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(vals) {
		t.Fatalf("%d values", len(got))
	}
	want := []mmsjson.Typed{
		{Type: "integer", Value: int64(-7)},
		{Type: "unsigned", Value: uint64(math.MaxUint32)},
	}
	if !reflect.DeepEqual(got[:2], want) {
		t.Errorf("got %+v", got[:2])
	}
	if got[2].Type != "float32" || got[3].Value != -2.25 || got[4].Value != true || got[5].Value != false {
		t.Errorf("got %+v", got[2:6])
	}
	if got[6].Value != "0100000000000" {
		t.Errorf("bit-string %+v", got[6])
	}
	if got[7].Type != "utc-time" || got[7].Value != "2024-01-02T03:04:05.000Z" {
		t.Errorf("utc-time %+v", got[7])
	}
}

func TestEncodeGenericRejects(t *testing.T) {
	cases := map[string]*mms.Value{
		"no fixed length": mms.NewVisibleString("x"),
		"beyond INT32":    mms.NewInt64(math.MinInt32 - 1),
		"long bit string": quality(t, strings.Repeat("0", QualityBits+3)),
	}
	for name, v := range cases {
		if _, err := EncodeGeneric([]*mms.Value{v}); err == nil || !strings.HasPrefix(err.Error(), "value 0:") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestNewDecoderRejects(t *testing.T) {
	if _, err := NewDecoder(&Stream{SvID: "x", Members: []Member{{Type: "visible-string"}}}); err == nil {
		t.Error("variable-length member accepted")
	}
	short := le92Stream()
	short.Members = short.Members[:14]
	if _, err := NewDecoder(short); err == nil {
		t.Error("9-2LE with 7 channels accepted")
	}
}

func TestDecodeWrongLength(t *testing.T) {
	d, err := NewDecoder(&Stream{Profile: ProfileGeneric, Members: []Member{{Type: "float64"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 7, 9} {
		if _, err := d.Decode(make([]byte, n)); err == nil {
			t.Errorf("%d octets accepted", n)
		}
	}
}

func TestLoadFixture(t *testing.T) {
	streams, err := LoadFixture(filepath.Join("..", "..", "..", "..", "fixtures", "sv", "sv.json"))
	if err != nil {
		t.Fatal(err)
	}
	le, gen := streams["InteropSv92LE"], streams["InteropSvGeneric"]
	if le == nil || gen == nil {
		t.Fatalf("streams: %v", streams)
	}
	// Both fixture streams must be decodable, or the subscriber cannot start.
	for _, st := range []*Stream{le, gen} {
		if _, err := NewDecoder(st); err != nil {
			t.Error(err)
		}
	}
	if le.Profile != Profile92LE || gen.Profile != ProfileGeneric {
		t.Errorf("profiles %s, %s", le.Profile, gen.Profile)
	}
}

func TestLoadFixtureErrors(t *testing.T) {
	if _, err := LoadFixture(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("absent file accepted")
	}
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFixture(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Errorf("err = %v", err)
	}
}

func TestDecodedValuesMarshal(t *testing.T) {
	d, err := NewDecoder(&Stream{Profile: ProfileGeneric, Members: []Member{{Type: "float32"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString("3dcccccd") // 0.1f
	got, err := d.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[{"type":"float32","value":0.1}]` {
		t.Errorf("got %s", b)
	}
}
