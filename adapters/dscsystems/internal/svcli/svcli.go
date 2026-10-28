// SPDX-License-Identifier: GPL-3.0-only

// Package svcli holds what the two Sampled Values commands share: the
// seqData codec of the 9-2LE and generic profiles, the sv.json fixture,
// the smpMod names and the event shapes.
package svcli

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/model"
	"github.com/dscsystems/go-iec61850/sv"

	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/jl"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/mmsjson"
)

// QualityBits is the length of the quality bit-string; bits 13..31 of the
// on-wire word (9-2LE derived flag and reserved) are written as zero and
// not reported.
const QualityBits = 13

// Profiles.
const (
	Profile92LE    = "9-2LE"
	ProfileGeneric = "generic"
)

// SmpModName is the contract's string form of smpMod.
func SmpModName(m sv.SmpMod) string {
	switch m {
	case sv.SmpPerPeriod:
		return "samples-per-period"
	case sv.SmpPerSec:
		return "samples-per-second"
	case sv.SecPerSmp:
		return "seconds-per-sample"
	}
	return fmt.Sprintf("smpMod(%d)", uint8(m))
}

// ParseSmpMod parses the contract's string form of smpMod.
func ParseSmpMod(s string) (sv.SmpMod, error) {
	switch s {
	case "samples-per-period":
		return sv.SmpPerPeriod, nil
	case "samples-per-second":
		return sv.SmpPerSec, nil
	case "seconds-per-sample":
		return sv.SecPerSmp, nil
	}
	return 0, fmt.Errorf("smp-mod %q: want samples-per-period, samples-per-second or seconds-per-sample", s)
}

// size is the fixed-length IEC 61850-9-2 encoding size of a member type.
func size(typ string) (int, error) {
	switch typ {
	case "integer", "unsigned", "float32", "bit-string":
		return 4, nil
	case "float64", "utc-time":
		return 8, nil
	case "boolean":
		return 1, nil
	}
	return 0, fmt.Errorf("type %q has no fixed-length Sampled Values encoding", typ)
}

// QualityWord converts a quality bit-string (MSB-first, position i) to
// the 32-bit on-wire word, in which position i is bit i from the LSB.
func QualityWord(v *mms.Value) (uint32, error) {
	if v.Type() != mms.TypeBitString {
		return 0, fmt.Errorf("quality is %v, want bit-string", v.Type())
	}
	if v.BitLen() > QualityBits {
		return 0, fmt.Errorf("quality of %d bits, want at most %d", v.BitLen(), QualityBits)
	}
	var w uint32
	for i := 0; i < v.BitLen(); i++ {
		if v.Bit(i) {
			w |= 1 << i
		}
	}
	return w, nil
}

// QualityString is the 13-character bit-string of an on-wire word.
func QualityString(w uint32) string {
	var b [QualityBits]byte
	for i := range b {
		if w&(1<<i) != 0 {
			b[i] = '1'
		} else {
			b[i] = '0'
		}
	}
	return string(b[:])
}

// Types lists the contract type names of vals.
func Types(vals []*mms.Value) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = mmsjson.FromValue(v).Type
	}
	return out
}

func int32Of(v *mms.Value) (int32, error) {
	n := v.Int64()
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("integer %d does not fit INT32", n)
	}
	return int32(n), nil
}

// Encode92LE builds the 64-octet 9-2LE seqData of eight
// integer/bit-string pairs (IA IB IC IN VA VB VC VN).
func Encode92LE(vals []*mms.Value) ([]byte, error) {
	if len(vals) != 16 {
		return nil, fmt.Errorf("9-2LE needs 16 values (8 integer/bit-string pairs), got %d", len(vals))
	}
	var s sv.LESample
	for ch := 0; ch < 8; ch++ {
		v, q := vals[2*ch], vals[2*ch+1]
		if v.Type() != mms.TypeInteger {
			return nil, fmt.Errorf("value %d: 9-2LE channel value is %v, want integer", 2*ch, v.Type())
		}
		n, err := int32Of(v)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", 2*ch, err)
		}
		w, err := QualityWord(q)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", 2*ch+1, err)
		}
		if ch < 4 {
			s.I[ch] = n
		} else {
			s.V[ch-4] = n
		}
		s.SetQuality(ch, model.Quality(w))
	}
	return sv.EncodeLESample(&s), nil
}

// EncodeGeneric builds the seqData of the generic profile: the members in
// order with their IEC 61850-9-2 fixed-length encoding.
func EncodeGeneric(vals []*mms.Value) ([]byte, error) {
	var b []byte
	for i, v := range vals {
		t := mmsjson.FromValue(v).Type
		switch t {
		case "integer":
			n, err := int32Of(v)
			if err != nil {
				return nil, fmt.Errorf("value %d: %w", i, err)
			}
			b = binary.BigEndian.AppendUint32(b, uint32(n))
		case "unsigned":
			b = binary.BigEndian.AppendUint32(b, uint32(v.Uint64()))
		case "float32":
			b = binary.BigEndian.AppendUint32(b, math.Float32bits(v.Float32()))
		case "float64":
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(v.Float64()))
		case "boolean":
			if v.Bool() {
				b = append(b, 1)
			} else {
				b = append(b, 0)
			}
		case "bit-string":
			w, err := QualityWord(v)
			if err != nil {
				return nil, fmt.Errorf("value %d: %w", i, err)
			}
			b = binary.BigEndian.AppendUint32(b, w)
		case "utc-time":
			b = append(b, v.Bytes()...)
		default:
			return nil, fmt.Errorf("value %d: type %q has no fixed-length Sampled Values encoding", i, t)
		}
	}
	return b, nil
}

// Decoder decodes the seqData of one stream.
type Decoder struct {
	profile string
	types   []string
	size    int
}

// NewDecoder returns the decoder of a fixture stream.
func NewDecoder(st *Stream) (*Decoder, error) {
	d := &Decoder{profile: st.Profile}
	for _, m := range st.Members {
		n, err := size(m.Type)
		if err != nil {
			return nil, fmt.Errorf("stream %s: %w", st.SvID, err)
		}
		d.types = append(d.types, m.Type)
		d.size += n
	}
	if d.profile == Profile92LE && (len(d.types) != 16 || d.size != 64) {
		return nil, fmt.Errorf("stream %s: 9-2LE needs 8 integer/bit-string pairs", st.SvID)
	}
	return d, nil
}

// Decode returns the typed values of seqData.
func (d *Decoder) Decode(b []byte) ([]mmsjson.Typed, error) {
	if len(b) != d.size {
		return nil, fmt.Errorf("seqData of %d octets, members need %d", len(b), d.size)
	}
	out := make([]mmsjson.Typed, 0, len(d.types))
	if d.profile == Profile92LE {
		s, err := sv.DecodeLESample(b)
		if err != nil {
			return nil, err
		}
		for ch := 0; ch < 8; ch++ {
			n := s.I[ch%4]
			if ch >= 4 {
				n = s.V[ch-4]
			}
			// The raw word, not LESample.Quality: that maps the legacy
			// 9-2LE invalid (0x0001) to 0x0002, and the contract reports
			// the bits as sent.
			out = append(out, mmsjson.Typed{Type: "integer", Value: int64(n)},
				mmsjson.Typed{Type: "bit-string", Value: QualityString(s.Q[ch])})
		}
		return out, nil
	}
	off := 0
	for _, t := range d.types {
		switch t {
		case "integer":
			out = append(out, mmsjson.Typed{Type: t, Value: int64(int32(binary.BigEndian.Uint32(b[off:])))})
			off += 4
		case "unsigned":
			out = append(out, mmsjson.Typed{Type: t, Value: uint64(binary.BigEndian.Uint32(b[off:]))})
			off += 4
		case "float32":
			out = append(out, mmsjson.FromValue(mms.NewFloat32(math.Float32frombits(binary.BigEndian.Uint32(b[off:])))))
			off += 4
		case "float64":
			out = append(out, mmsjson.FromValue(mms.NewFloat64(math.Float64frombits(binary.BigEndian.Uint64(b[off:])))))
			off += 8
		case "boolean":
			out = append(out, mmsjson.Typed{Type: t, Value: b[off] != 0})
			off++
		case "bit-string":
			out = append(out, mmsjson.Typed{Type: t, Value: QualityString(binary.BigEndian.Uint32(b[off:]))})
			off += 4
		case "utc-time":
			v, err := mms.NewUTCTimeRaw(b[off : off+8])
			if err != nil {
				return nil, err
			}
			out = append(out, mmsjson.FromValue(v))
			off += 8
		}
	}
	return out, nil
}

// Member is one dataset member of a fixture stream.
type Member struct {
	Ref  string `json:"ref"`
	Type string `json:"type"`
}

// Stream is one stream of sv.json, as far as the subscriber needs it.
type Stream struct {
	Profile    string   `json:"profile"`
	SvID       string   `json:"svID"`
	SmpCntWrap int      `json:"smpCntWrap"`
	Members    []Member `json:"members"`
}

// Fixture is sv.json.
type Fixture struct {
	Revision string             `json:"revision"`
	Streams  map[string]*Stream `json:"streams"`
}

// LoadFixture reads sv.json and indexes its streams by svID.
func LoadFixture(path string) (map[string]*Stream, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]*Stream, len(f.Streams))
	for _, s := range f.Streams {
		out[s.SvID] = s
	}
	return out, nil
}

// Ready is the readiness event of both SV commands. address is the
// interface name, so the generic ready check of the smoke test applies.
type Ready struct {
	Event   string `json:"event"`
	Address string `json:"address"`
	Fixture string `json:"fixture"`
	Adapter string `json:"adapter"`
	Version string `json:"version"`
	Role    string `json:"role"`
	Iface   string `json:"iface"`
	Src     string `json:"src"`
	SvID    string `json:"svID,omitempty"`
}

// EmitReady writes the readiness event.
func EmitReady(role, iface, src, svID string) {
	jl.Emit(Ready{"ready", iface, jl.SVFixtureRevision, jl.Adapter, jl.Version(), role, iface, src, svID})
}
