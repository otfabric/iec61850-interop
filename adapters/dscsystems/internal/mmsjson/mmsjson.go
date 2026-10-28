// SPDX-License-Identifier: GPL-3.0-only

// Package mmsjson converts MMS values to and from the typed JSON form of
// the iec61850-interop contract: {"type":"boolean","value":true}.
//
//	boolean          JSON boolean
//	integer          JSON number
//	unsigned         JSON number
//	float32/float64  JSON number (float32 in its shortest float32 form)
//	visible-string   JSON string
//	mms-string       JSON string
//	octet-string     lower-case hex string
//	bit-string       MSB-first binary digits, "0110..."; length = len(value)
//	utc-time         RFC 3339 UTC, millisecond precision ("2024-01-01T00:00:00.000Z")
//	binary-time      RFC 3339 UTC, millisecond precision
//	array            JSON array of typed values
//	structure        JSON array of typed values, in member order
package mmsjson

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
)

// TimeLayout is the utc-time text form.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// Typed is one value in its JSON form.
type Typed struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// FromValue returns the JSON form of v.
func FromValue(v *mms.Value) Typed {
	if v == nil {
		return Typed{Type: "none"}
	}
	switch v.Type() {
	case mms.TypeBoolean:
		return Typed{"boolean", v.Bool()}
	case mms.TypeInteger:
		return Typed{"integer", v.Int64()}
	case mms.TypeUnsigned:
		return Typed{"unsigned", v.Uint64()}
	case mms.TypeFloat32:
		return Typed{"float32", jsonFloat32(v.Float32())}
	case mms.TypeFloat64:
		return Typed{"float64", jsonFloat(v.Float64())}
	case mms.TypeVisibleString:
		return Typed{"visible-string", v.Text()}
	case mms.TypeMMSString:
		return Typed{"mms-string", v.Text()}
	case mms.TypeOctetString:
		return Typed{"octet-string", hex.EncodeToString(v.Bytes())}
	case mms.TypeBitString:
		var b strings.Builder
		for i := 0; i < v.BitLen(); i++ {
			if v.Bit(i) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		return Typed{"bit-string", b.String()}
	case mms.TypeUTCTime:
		return Typed{"utc-time", FormatTime(v.Time())}
	case mms.TypeBinaryTime:
		return Typed{"binary-time", FormatTime(v.Time())}
	case mms.TypeGeneralizedTime:
		return Typed{"generalized-time", v.Text()}
	case mms.TypeArray, mms.TypeStructure:
		kids := v.Children()
		out := make([]Typed, len(kids))
		for i, c := range kids {
			out[i] = FromValue(c)
		}
		return Typed{v.Type().String(), out}
	case mms.TypeDataAccessError:
		code, _ := v.AccessError()
		return Typed{"data-access-error", int(code)}
	}
	return Typed{Type: v.Type().String()}
}

// FromValues converts a list.
func FromValues(vs []*mms.Value) []Typed {
	out := make([]Typed, len(vs))
	for i, v := range vs {
		out[i] = FromValue(v)
	}
	return out
}

// FormatTime renders t as the contract's utc-time text, rounded to the
// millisecond.
func FormatTime(t time.Time) string {
	return t.UTC().Round(time.Millisecond).Format(TimeLayout)
}

// jsonFloat32 renders a float32 with the fewest digits that read back as
// the same float32 (1234.5, -0.25, 0.1), not the float64 widening of it.
func jsonFloat32(f float32) any {
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		return fmt.Sprint(f)
	}
	return json.Number(strconv.FormatFloat(float64(f), 'g', -1, 32))
}

func jsonFloat(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Sprint(f)
	}
	return f
}

// ToValue builds the MMS value of a typed JSON value.
func ToValue(t Typed) (*mms.Value, error) {
	switch t.Type {
	case "boolean":
		b, ok := t.Value.(bool)
		if !ok {
			return nil, fmt.Errorf("boolean: value %v is not a JSON boolean", t.Value)
		}
		return mms.NewBool(b), nil
	case "integer":
		n, err := number(t.Value)
		if err != nil {
			return nil, fmt.Errorf("integer: %w", err)
		}
		return mms.NewInt64(int64(n)), nil
	case "unsigned":
		n, err := number(t.Value)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("unsigned: value %v is not a non-negative number", t.Value)
		}
		if n > math.MaxUint32 {
			return nil, fmt.Errorf("unsigned: value %v exceeds 32 bits", t.Value)
		}
		return mms.NewUint32(uint32(n)), nil
	case "float32":
		n, err := number(t.Value)
		if err != nil {
			return nil, fmt.Errorf("float32: %w", err)
		}
		return mms.NewFloat32(float32(n)), nil
	case "float64":
		n, err := number(t.Value)
		if err != nil {
			return nil, fmt.Errorf("float64: %w", err)
		}
		return mms.NewFloat64(n), nil
	case "visible-string", "mms-string":
		s, ok := t.Value.(string)
		if !ok {
			return nil, fmt.Errorf("%s: value %v is not a JSON string", t.Type, t.Value)
		}
		if t.Type == "mms-string" {
			return mms.NewMMSString(s), nil
		}
		return mms.NewVisibleString(s), nil
	case "octet-string":
		s, _ := t.Value.(string)
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("octet-string: %w", err)
		}
		return mms.NewOctetString(b), nil
	case "bit-string":
		s, ok := t.Value.(string)
		if !ok {
			return nil, fmt.Errorf("bit-string: value %v is not a string of binary digits", t.Value)
		}
		v := mms.NewBitString(len(s))
		for i, c := range s {
			switch c {
			case '1':
				v.SetBit(i, true)
			case '0':
			default:
				return nil, fmt.Errorf("bit-string: %q is not binary digits", s)
			}
		}
		return v, nil
	case "utc-time", "binary-time":
		s, _ := t.Value.(string)
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Type, err)
		}
		if t.Type == "binary-time" {
			return mms.NewBinaryTime(tm), nil
		}
		return mms.NewUTCTime(tm, mms.DefaultTimeQuality), nil
	case "array", "structure":
		raw, err := json.Marshal(t.Value)
		if err != nil {
			return nil, err
		}
		var kids []Typed
		if err := json.Unmarshal(raw, &kids); err != nil {
			return nil, fmt.Errorf("%s: value is not a list of typed values: %w", t.Type, err)
		}
		vals, err := ToValues(kids)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Type, err)
		}
		if t.Type == "array" {
			return mms.NewArray(vals...), nil
		}
		return mms.NewStructure(vals...), nil
	}
	return nil, fmt.Errorf("unsupported type %q", t.Type)
}

// ToValues converts a list.
func ToValues(ts []Typed) ([]*mms.Value, error) {
	out := make([]*mms.Value, len(ts))
	for i, t := range ts {
		v, err := ToValue(t)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ParseValues decodes a JSON array of typed values.
func ParseValues(s string) ([]*mms.Value, error) {
	var ts []Typed
	if err := json.Unmarshal([]byte(s), &ts); err != nil {
		return nil, fmt.Errorf("values: %w", err)
	}
	return ToValues(ts)
}

func number(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case json.Number:
		return n.Float64()
	}
	return 0, fmt.Errorf("value %v is not a JSON number", v)
}
