// SPDX-License-Identifier: GPL-3.0-only

// output.go — JSON line and value rendering for the MMS client adapter.
//
// The JSON shapes are those of the libiec61850 client adapter
// (mms_client.c, mms_client_services.c), which consumer test suites assert
// on: key order, untyped values (integers as numbers, octet strings as hex,
// bit strings as "0101" bit 0 first, times as milliseconds since the
// epoch, arrays and structures as JSON arrays, per-variable data access
// errors as {"error":"<name>"}), and the libiec61850 error names.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

// raw is a pre-rendered JSON value.
type raw string

// field is one key of a JSON line.
type field struct {
	key   string
	value any
}

// line is a JSON object with ordered keys.
type line []field

func (l line) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range l {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(f.key))
		b.WriteByte(':')
		switch v := f.value.(type) {
		case raw:
			b.WriteString(string(v))
		default:
			enc, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			b.Write(enc)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(b.String(), "\n")
}

// begin starts a line {"operation":op,"target":target,"ok":ok}.
func begin(op, target string, ok bool) line {
	return line{{"operation", op}, {"target", target}, {"ok", ok}}
}

func emitOK(op, target string) { jl.Emit(begin(op, target, true)) }

func emitFail(op, target, reason string) {
	jl.Emit(append(begin(op, target, false), field{"error", reason}))
}

func emitValue(op, target string, v *mms.Value) {
	jl.Emit(append(begin(op, target, true), field{"value", valueJSON(v)}))
}

func emitNames(target string, names []string) {
	if names == nil {
		names = []string{}
	}
	jl.Emit(append(begin("get-name-list", target, true), field{"names", names}))
}

// unsupported is the error of a step the dscsystems client API cannot
// perform.
const unsupported = "unsupported"

func emitUnsupported(op, target string) {
	jl.Log("%s %s: not supported by the dscsystems client", op, target)
	emitFail(op, target, unsupported)
}

// valueJSON renders a value as the C adapter's sb_value does.
func valueJSON(v *mms.Value) raw {
	if v == nil {
		return "null"
	}
	switch v.Type() {
	case mms.TypeBoolean:
		return raw(strconv.FormatBool(v.Bool()))
	case mms.TypeInteger:
		return raw(strconv.FormatInt(v.Int64(), 10))
	case mms.TypeUnsigned:
		return raw(strconv.FormatUint(uint64(uint32(v.Uint64())), 10))
	case mms.TypeFloat32:
		return raw(fmt.Sprintf("%.9g", float64(v.Float32())))
	case mms.TypeFloat64:
		return raw(fmt.Sprintf("%.17g", v.Float64()))
	case mms.TypeVisibleString, mms.TypeMMSString:
		return raw(jsonString(v.Text()))
	case mms.TypeOctetString:
		return raw(`"` + hex.EncodeToString(v.Bytes()) + `"`)
	case mms.TypeBitString:
		return raw(`"` + bits(v) + `"`)
	case mms.TypeUTCTime, mms.TypeBinaryTime:
		return raw(strconv.FormatInt(v.Time().UnixMilli(), 10))
	case mms.TypeArray, mms.TypeStructure:
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = string(valueJSON(v.Index(i)))
		}
		return raw("[" + strings.Join(parts, ",") + "]")
	case mms.TypeDataAccessError:
		code, _ := v.AccessError()
		return raw(`{"error":"` + daeName(code) + `"}`)
	}
	return "null"
}

// bits renders a bit string bit 0 first.
func bits(v *mms.Value) string {
	var b strings.Builder
	for i := 0; i < v.BitLen(); i++ {
		if v.Bit(i) {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

// specTypeName is the C adapter's spec_type_name.
func specTypeName(k mms.Type) string {
	switch k {
	case mms.TypeBoolean:
		return "boolean"
	case mms.TypeInteger:
		return "integer"
	case mms.TypeUnsigned:
		return "unsigned"
	case mms.TypeFloat32, mms.TypeFloat64:
		return "float32"
	case mms.TypeVisibleString:
		return "visible-string"
	case mms.TypeMMSString:
		return "mms-string"
	case mms.TypeOctetString:
		return "octet-string"
	case mms.TypeBitString:
		return "bit-string"
	case mms.TypeUTCTime:
		return "utc-time"
	case mms.TypeBinaryTime:
		return "binary-time"
	case mms.TypeArray:
		return "array"
	case mms.TypeStructure:
		return "structure"
	}
	return "unknown"
}

// specJSON renders a type specification as the C adapter's sb_spec does.
func specJSON(ts *mms.TypeSpec) raw {
	if ts == nil {
		return "null"
	}
	l := line{{"type", specTypeName(ts.Kind)}}
	switch ts.Kind {
	case mms.TypeArray:
		l = append(l, field{"count", ts.Elements}, field{"element", specJSON(ts.Element)})
	case mms.TypeStructure:
		comps := make([]string, len(ts.Components))
		for i, c := range ts.Components {
			b, _ := line{{"name", c.Name}, {"spec", specJSON(c.Spec)}}.MarshalJSON()
			comps[i] = string(b)
		}
		l = append(l, field{"components", raw("[" + strings.Join(comps, ",") + "]")})
	}
	b, _ := l.MarshalJSON()
	return raw(b)
}

// daeName is the C adapter's dae_name.
func daeName(e mms.DataAccessError) string {
	if e <= mms.AccessObjectValueInvalid {
		return e.String()
	}
	return "unknown"
}

// errName maps an error to the name of the libiec61850 MmsError the C
// adapter reports (mms_err_name): ServiceErrors by class and code,
// rejects by category and reason, transport failures as connection-lost,
// timeouts as service-timeout and undecodable answers as
// parsing-response. Errors without a libiec61850 name are "mms-error".
func errName(err error) string {
	if err == nil {
		return "none"
	}
	var dae mms.DataAccessError
	if errors.As(err, &dae) {
		return daeName(dae)
	}
	var se *mms.ServiceError
	if errors.As(err, &se) {
		if se.Rejected {
			switch {
			case se.Class == 1 && se.Code == 1:
				return "reject-unrecognized-service"
			case se.Class == 5 && se.Code == 1:
				return "reject-invalid-pdu"
			}
			return "mms-error"
		}
		switch {
		case se.Class == 7 && se.Code == 2:
			return "object-non-existent"
		case se.Class == 7 && se.Code == 3:
			return "object-access-denied"
		case se.Class == 2 && se.Code == 1:
			return "object-undefined"
		case se.Class == 11 && se.Code == 7:
			return "file-non-existent"
		case se.Class == 11 && se.Code == 0:
			return "file-other"
		case se.Class == 4 && se.Code == 0:
			return "service-other"
		}
		return "mms-error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "service-timeout"
	}
	if lost(err) {
		return "connection-lost"
	}
	return "parsing-response"
}

// coarseErrName is the C adapter's mms_error_str, used by the phase 1
// operations of mms_client.c: only none, connection-lost and
// service-timeout have names there.
func coarseErrName(err error) string {
	switch n := errName(err); n {
	case "none", "connection-lost", "service-timeout":
		return n
	}
	return "mms-error"
}

// lost reports a transport failure or a closed association.
func lost(err error) bool {
	var ne net.Error
	var oe *net.OpError
	var ab *mms.AbortedError
	return errors.Is(err, mms.ErrAborted) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.As(err, &oe) || errors.As(err, &ab) ||
		(errors.As(err, &ne) && !ne.Timeout())
}

func emitLine(l line) { jl.Emit(l) }
