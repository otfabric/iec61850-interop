// SPDX-License-Identifier: GPL-3.0-only

// Package ied holds what the dscsystems IED client, controller and reporter
// share: the JSON Line shapes of the libiec61850 adapter (field order
// included), its error strings and the logical-device resolution.
package ied

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dscsystems/go-iec61850/client"
	"github.com/dscsystems/go-iec61850/mms"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/jl"
)

// OpTimeout bounds one request of a fixed sequence.
const OpTimeout = 10 * time.Second

// Field is one member of an ordered JSON object.
type Field struct {
	K string
	V any
}

// Obj is a JSON object that keeps its member order, so lines read like the
// libiec61850 adapter's printf output.
type Obj []Field

// MarshalJSON encodes the members in order.
func (o Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.K)
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(f.V)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// OK emits {"operation":op[,"target":target],"ok":true,extra...}.
// An empty target is left out.
func OK(op, target string, extra ...Field) {
	o := Obj{{"operation", op}}
	if target != "" {
		o = append(o, Field{"target", target})
	}
	o = append(o, Field{"ok", true})
	jl.Emit(append(o, extra...))
}

// Fail emits {"operation":op[,"target":target],"ok":false,"error":msg}.
func Fail(op, target, msg string) {
	o := Obj{{"operation", op}}
	if target != "" {
		o = append(o, Field{"target", target})
	}
	jl.Emit(append(o, Field{"ok", false}, Field{"error", msg}))
}

// ErrString renders err with the IedClientError names the libiec61850
// adapter prints (ied_error_str), so ok:false lines compare across stacks.
// Errors libiec61850 has no name for keep the library's own text.
func ErrString(err error) string {
	if err == nil {
		return "ok"
	}
	var dae mms.DataAccessError
	if errors.As(err, &dae) {
		switch dae {
		case mms.AccessObjectAccessDenied:
			return "access-denied"
		case mms.AccessObjectNonExistent:
			return "object-does-not-exist"
		case mms.AccessObjectAccessUnsupported:
			return "object-access-unsupported"
		case mms.AccessTypeInconsistent:
			return "type-inconsistent"
		}
		return dae.String()
	}
	var ce *client.ControlError
	if errors.As(err, &ce) && ce.Err == nil {
		// A refusal known only by its AddCause (select returned no name,
		// CommandTermination-).
		return ce.AddCause.String()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection-lost"
	}
	var se *mms.ServiceError
	if errors.As(err, &se) {
		return "service-error: " + se.Error()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "not-connected: " + err.Error()
	}
	return err.Error()
}

// Flags parses --host and --port (plus whatever the caller registered on
// flag.CommandLine) and returns host:port. An invalid port exits 1 like the
// C adapters.
func Flags(name string) string {
	host := flag.String("host", "localhost", "server host")
	port := flag.Int("port", 1102, "server port")
	flag.Parse()
	if *port <= 0 || *port > 65535 {
		jl.Fatal(1, "%s: invalid port %d", name, *port)
	}
	return net.JoinHostPort(*host, strconv.Itoa(*port))
}

// Dial associates with the server at addr.
func Dial(addr string) (*client.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), OpTimeout)
	defer cancel()
	return client.Dial(ctx, addr, client.WithTimeout(OpTimeout))
}

// Ctx returns the context for one request.
func Ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), OpTimeout)
}

// ResolveInteropLD prefers the bare "InteropLD" (go-iec61850), else a name
// ending in "InteropLD" (libiec61850 / iec61850bean / dscsystems prefix the
// IED name), else the first name; "" when names is empty.
func ResolveInteropLD(names []string) string {
	for _, n := range names {
		if n == "InteropLD" {
			return n
		}
	}
	for _, n := range names {
		if strings.HasSuffix(n, "InteropLD") {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// Scalar is the plain JSON form the libiec61850 adapter prints for a
// dataset member: booleans, integers, floats and visible strings as such,
// anything else null.
func Scalar(v *mms.Value) any {
	if v == nil {
		return nil
	}
	switch v.Type() {
	case mms.TypeBoolean:
		return v.Bool()
	case mms.TypeInteger:
		return int32(v.Int64())
	case mms.TypeUnsigned:
		return uint32(v.Uint64())
	case mms.TypeFloat32:
		return float64(v.Float32())
	case mms.TypeFloat64:
		return v.Float64()
	case mms.TypeVisibleString:
		return v.Text()
	}
	return nil
}

// TypeError is the error a typed read reports when the value has another
// type, as libiec61850's IedConnection_read<Type>Value does.
// The detail goes to stderr; the error is type-inconsistent.
func TypeError(ref, want string, v *mms.Value) error {
	jl.Log("%s: want %s, got %s", ref, want, v.Type())
	return mms.AccessTypeInconsistent
}
