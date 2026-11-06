// SPDX-License-Identifier: MIT

package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/cB-Sigma/iec61850/mms"
)

// Typed value JSON (docs/goose.md#value-json): {"type":T,"value":V}.
type typedValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// parseTypedList parses a JSON array of typed values.
func parseTypedList(raw []byte) ([]mms.Data, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("values is not a JSON array")
	}
	out := make([]mms.Data, 0, len(list))
	for i, e := range list {
		v, err := parseTyped(e)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// parseTyped converts one typed value to an MMS value of cB-Sigma's codec.
func parseTyped(raw json.RawMessage) (mms.Data, error) {
	var tv typedValue
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&tv); err != nil || tv.Type == "" || tv.Value == nil {
		return mms.Data{}, errors.New(`value is not {"type":..,"value":..}`)
	}
	str := func() (string, error) {
		var s string
		if err := json.Unmarshal(tv.Value, &s); err != nil {
			return "", fmt.Errorf("%s: not a JSON string", tv.Type)
		}
		return s, nil
	}
	num := func() (json.Number, error) {
		d := json.NewDecoder(strings.NewReader(string(tv.Value)))
		d.UseNumber()
		var n json.Number
		if err := d.Decode(&n); err != nil {
			return "", fmt.Errorf("%s: not a JSON number", tv.Type)
		}
		return n, nil
	}
	switch tv.Type {
	case "boolean":
		var b bool
		if err := json.Unmarshal(tv.Value, &b); err != nil {
			return mms.Data{}, errors.New("boolean: not a JSON boolean")
		}
		return mms.Boolean(b), nil
	case "integer":
		n, err := num()
		if err != nil {
			return mms.Data{}, err
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return mms.Data{}, errors.New("integer: not a JSON integer")
		}
		return mms.Integer(i), nil
	case "unsigned":
		n, err := num()
		if err != nil {
			return mms.Data{}, err
		}
		u, err := strconv.ParseUint(n.String(), 10, 32)
		if err != nil {
			return mms.Data{}, errors.New("unsigned: not a 32-bit unsigned integer")
		}
		return mms.Unsigned(u), nil
	case "float32", "float64":
		n, err := num()
		if err != nil {
			return mms.Data{}, err
		}
		f, err := n.Float64()
		if err != nil {
			return mms.Data{}, fmt.Errorf("%s: %w", tv.Type, err)
		}
		if tv.Type == "float32" {
			return mms.Float32(float64(float32(f))), nil
		}
		return mms.Float64(f), nil
	case "visible-string":
		s, err := str()
		if err != nil {
			return mms.Data{}, err
		}
		return mms.Visible(s), nil
	case "mms-string":
		s, err := str()
		if err != nil {
			return mms.Data{}, err
		}
		return mms.MMSString(s), nil
	case "octet-string":
		s, err := str()
		if err != nil {
			return mms.Data{}, err
		}
		b, err := hex.DecodeString(s)
		if err != nil {
			return mms.Data{}, errors.New("octet-string: not hex")
		}
		return mms.Octets(b), nil
	case "bit-string":
		s, err := str()
		if err != nil {
			return mms.Data{}, err
		}
		b := mms.NewBitString(len(s))
		for i, c := range s {
			switch c {
			case '1':
				b.Set(i, true)
			case '0':
			default:
				return mms.Data{}, errors.New("bit-string: not binary digits")
			}
		}
		return mms.Bits(b), nil
	case "utc-time", "binary-time":
		s, err := str()
		if err != nil {
			return mms.Data{}, err
		}
		t, err := parseTime(s)
		if err != nil {
			return mms.Data{}, err
		}
		if tv.Type == "utc-time" {
			return mms.Data{Kind: mms.KUTCTime, Time: mms.UTCTime{T: t}}, nil
		}
		return mms.BinaryTime(t), nil
	case "array", "structure":
		items, err := parseTypedList(tv.Value)
		if err != nil {
			return mms.Data{}, fmt.Errorf("%s: %w", tv.Type, err)
		}
		if tv.Type == "array" {
			return mms.Array(items...), nil
		}
		return mms.Structure(items...), nil
	}
	return mms.Data{}, fmt.Errorf("unsupported type %q", tv.Type)
}

// parseTime parses "YYYY-MM-DDThh:mm:ss[.f]Z", rounded to the millisecond.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || !strings.HasSuffix(s, "Z") {
		return time.Time{}, errors.New("time: not YYYY-MM-DDThh:mm:ss[.fff]Z")
	}
	return t.UTC().Round(time.Millisecond), nil
}

// formatMs formats a time as YYYY-MM-DDThh:mm:ss.mmmZ, rounded.
func formatMs(t time.Time) string {
	return t.UTC().Round(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

// floatJSON is a float as a JSON number in the shortest form that reads
// back as the same value of its width; NaN and infinities are null.
func floatJSON(f float64, bits int) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return json.Number(strconv.FormatFloat(f, 'g', -1, bits))
}

// formatBits formats the bits MSB-first.
func formatBits(b mms.BitString) string {
	var sb strings.Builder
	for i := 0; i < b.NBits; i++ {
		if b.Get(i) {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	}
	return sb.String()
}

// toTyped converts a decoded MMS value to typed JSON. cB-Sigma's decoder
// keeps the width of a floating-point value (Data.Double).
func toTyped(d mms.Data) map[string]any {
	tv := func(t string, x any) map[string]any { return map[string]any{"type": t, "value": x} }
	switch d.Kind {
	case mms.KBool:
		return tv("boolean", d.Bool)
	case mms.KInt:
		return tv("integer", d.Int)
	case mms.KUint:
		return tv("unsigned", d.Uint)
	case mms.KFloat:
		if d.Double {
			return tv("float64", floatJSON(d.Float, 64))
		}
		return tv("float32", floatJSON(float64(float32(d.Float)), 32))
	case mms.KVisible:
		return tv("visible-string", d.Str)
	case mms.KMMSString:
		return tv("mms-string", d.Str)
	case mms.KOctets:
		return tv("octet-string", hex.EncodeToString(d.Bytes))
	case mms.KBitString:
		return tv("bit-string", formatBits(d.Bits))
	case mms.KBoolArray:
		b := mms.NewBitString(len(d.Elems))
		for i, e := range d.Elems {
			b.Set(i, e.Bool)
		}
		return tv("bit-string", formatBits(b))
	case mms.KUTCTime:
		return tv("utc-time", formatMs(d.Time.T))
	case mms.KBinaryTime:
		t, err := mms.DecodeBinaryTime(d.Bytes)
		if err != nil {
			return tv("binary-time", nil)
		}
		return tv("binary-time", formatMs(t))
	case mms.KArray, mms.KStructure:
		items := make([]map[string]any, len(d.Elems))
		for i, e := range d.Elems {
			items[i] = toTyped(e)
		}
		if d.Kind == mms.KArray {
			return tv("array", items)
		}
		return tv("structure", items)
	}
	return tv("unknown", nil)
}

func toTypedList(ds []mms.Data) []map[string]any {
	out := make([]map[string]any, len(ds))
	for i, d := range ds {
		out[i] = toTyped(d)
	}
	return out
}
