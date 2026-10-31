// SPDX-License-Identifier: GPL-3.0-only

// dscsystems-mms-client — MMS client adapter for iec61850-interop on
// github.com/dscsystems/go-iec61850 (package mms).
//
// Port of adapters/libiec61850/src/mms_client.c. Connects to a host:port
// MMS server, executes the operation sequence and emits one JSON Line per
// operation to stdout, with the operation/target pairs, value encodings
// and error names of the libiec61850 client adapter.
//
// Phase 1A operations (connection 1):
//
//	identify, get-name-list (domains+variables), read bool/int/float32,
//	write float32, read float32 (verify), write octet-string (read-only),
//	read bool (server still alive)
//
// Phase 1C additions (connection 1):
//
//	read unsigned, read+write visible-string (verify), read octet-string,
//	bit-string, utc-time, array, structure; get-var-access-attr,
//	read-multiple, write-multiple, NVL define/read/delete; negative tests
//	(unknown domain, unknown variable, wrong type)
//
// Fixture mms-v2 services (connection 1, services.go): status, name
// lists, type specs, alternate access, named variable lists, files,
// journals, InformationReport reception; then conclude.
//
// Connection 2: read bool + conclude (reconnect verification).
// Connection 3: read bool + abort.
// Connection 4: write -1 to report-trigger; the server aborts.
//
// Steps the dscsystems client API has no call for (alternate access,
// association-specific named variable lists, writing a named variable
// list) emit {"ok":false,"error":"unsupported"}.
//
// Exit codes:
//
//	0  sequence completed (individual ok flags determine test outcome)
//	1  startup/argument error
//	2  connection failure
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

// requestTimeout bounds every confirmed request, as libiec61850's default
// request timeout does.
const requestTimeout = 5 * time.Second

// connectTimeout bounds the association handshake.
const connectTimeout = 10 * time.Second

// client wraps an association with the per-request timeout.
type client struct {
	c *mms.Conn
}

// ctx returns the context of one request.
func reqCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), requestTimeout)
}

func dial(addr string) (*mms.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	return mms.Dial(ctx, addr, mms.Options{})
}

// readItem reads one variable of domain "interop". A per-variable data
// access error is returned as an error.
func (cl *client) readItem(domain, item string) (*mms.Value, error) {
	ctx, cancel := reqCtx()
	defer cancel()
	vals, err := cl.c.Read(ctx, domain, item)
	if err != nil {
		return nil, err
	}
	if len(vals) != 1 {
		return nil, fmt.Errorf("read: %d results for one variable", len(vals))
	}
	if dae, ok := vals[0].AccessError(); ok {
		return nil, dae
	}
	return vals[0], nil
}

// writeItem writes one variable; a per-variable failure is returned as
// its DataAccessError.
func (cl *client) writeItem(domain, item string, v *mms.Value) error {
	ctx, cancel := reqCtx()
	defer cancel()
	res, err := cl.c.Write(ctx, domain, []string{item}, []*mms.Value{v})
	if err != nil {
		return err
	}
	if len(res) != 1 {
		return fmt.Errorf("write: %d results for one variable", len(res))
	}
	return res[0]
}

// ---------------------------------------------------------------------------
// Phase 1 JSON lines (mms_client.c shapes: no target where C has none)
// ---------------------------------------------------------------------------

func jlOK(op string) { jl.Emit(line{{"operation", op}, {"ok", true}}) }

func jlFail(op, reason string) {
	jl.Emit(line{{"operation", op}, {"ok", false}, {"error", reason}})
}

// readResult emits a phase 1 read: the value rendered by render, a data
// access error by name, any other failure by its coarse name.
func readResult(target string, v *mms.Value, err error, render func(*mms.Value) raw) {
	if err != nil {
		var dae mms.DataAccessError
		if errors.As(err, &dae) {
			emitFail("read", target, daeName(dae))
		} else {
			emitFail("read", target, coarseErrName(err))
		}
		return
	}
	jl.Emit(append(begin("read", target, true), field{"value", render(v)}))
}

// writeResult is emit_write_result: data access errors by name, other
// failures by their coarse name.
func writeResult(target string, err error) {
	if err == nil {
		emitOK("write", target)
		return
	}
	var dae mms.DataAccessError
	if errors.As(err, &dae) {
		emitFail("write", target, daeName(dae))
		return
	}
	emitFail("write", target, coarseErrName(err))
}

// Phase 1 renderings (jl_read_* in mms_client.c).
func renderBool(v *mms.Value) raw   { return raw(strconv.FormatBool(v.Bool())) }
func renderInt(v *mms.Value) raw    { return raw(strconv.FormatInt(int64(v.Int32()), 10)) }
func renderUint(v *mms.Value) raw   { return raw(strconv.FormatUint(uint64(uint32(v.Uint64())), 10)) }
func renderFloat(v *mms.Value) raw  { return raw(fmt.Sprintf("%.6g", float64(v.Float32()))) }
func renderString(v *mms.Value) raw { return raw(jsonString(v.Text())) }
func renderAsIs(v *mms.Value) raw   { return valueJSON(v) }

func renderArrayInts(v *mms.Value) raw {
	s := "["
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			s += ","
		}
		s += strconv.FormatInt(int64(v.Index(i).Int32()), 10)
	}
	return raw(s + "]")
}

func renderStructureBoolInt(v *mms.Value) raw {
	return raw(fmt.Sprintf("[%t,%d]", v.Index(0).Bool(), v.Index(1).Int32()))
}

// valuesArray is jl_values_array: booleans, integers, unsigned and floats,
// null for anything else.
func valuesArray(op, target string, vals []*mms.Value) {
	s := "["
	for i, v := range vals {
		if i > 0 {
			s += ","
		}
		switch v.Type() {
		case mms.TypeBoolean:
			s += string(renderBool(v))
		case mms.TypeInteger:
			s += string(renderInt(v))
		case mms.TypeUnsigned:
			s += string(renderUint(v))
		case mms.TypeFloat32, mms.TypeFloat64:
			s += string(renderFloat(v))
		default:
			s += "null"
		}
	}
	jl.Emit(append(begin(op, target, true), field{"values", raw(s + "]")}))
}

// ---------------------------------------------------------------------------
// Single connection run
// ---------------------------------------------------------------------------

// runSequence connects, executes the operation sequence and concludes.
// full selects the whole sequence; otherwise only read bool + conclude.
// It returns 2 when the connection cannot be established.
func runSequence(addr string, full bool) int {
	conn, err := dial(addr)
	if err != nil {
		jl.Log("connect to %s failed: %v", addr, err)
		return 2
	}
	jl.Log("connected to %s", addr)
	cl := &client{c: conn}
	if full {
		installReportHandler(conn)
	}

	if full {
		// --- Identify ---
		ctx, cancel := reqCtx()
		vendor, model, revision, err := conn.Identify(ctx)
		cancel()
		if err != nil {
			jlFail("identify", coarseErrName(err))
		} else {
			jl.Emit(line{{"operation", "identify"}, {"ok", true},
				{"value", line{{"vendor", vendor}, {"model", model}, {"revision", revision}}}})
		}

		// --- GetNameList: domains ---
		ctx, cancel = reqCtx()
		domains, err := conn.GetNameList(ctx, mms.ClassDomain, "")
		cancel()
		if err != nil {
			jlFail("get-name-list", coarseErrName(err))
		} else {
			if domains == nil {
				domains = []string{}
			}
			jl.Emit(line{{"operation", "get-name-list"}, {"ok", true}, {"names", domains}})
		}

		// --- GetNameList: variables in "interop" ---
		ctx, cancel = reqCtx()
		vars, err := conn.GetNameList(ctx, mms.ClassNamedVariable, "interop")
		cancel()
		if err != nil {
			emitFail("get-name-list", "interop", coarseErrName(err))
		} else {
			emitNames("interop", vars)
		}
	}

	// --- Read boolean ---
	v, err := cl.readItem("interop", "boolean")
	readResult("interop/boolean", v, err, renderBool)

	if full {
		phase1(cl)
		runServices(cl)
	}

	// --- Conclude ---
	if err := conn.Err(); err != nil {
		jlFail("conclude", coarseErrName(err))
	} else {
		_ = conn.Close()
		jlOK("conclude")
	}
	return 0
}

// phase1 is the Phase 1A remainder and Phase 1C of mms_client.c.
func phase1(cl *client) {
	conn := cl.c
	read := func(item string, render func(*mms.Value) raw) {
		v, err := cl.readItem("interop", item)
		readResult("interop/"+item, v, err, render)
	}

	read("integer", renderInt)
	read("float32", renderFloat)
	// --- Write float32 = 99.0, read back ---
	writeResult("interop/float32", cl.writeItem("interop", "float32", mms.NewFloat32(99)))
	read("float32", renderFloat)
	// --- Write octet-string (expect rejection — read-only) ---
	writeResult("interop/octet-string", cl.writeItem("interop", "octet-string", mms.NewOctetString([]byte{0xde, 0xad})))
	// --- Read boolean (server still connected after rejection) ---
	read("boolean", renderBool)

	// --- Phase 1C, step 5: remaining scalar types ---
	read("unsigned", renderUint)
	read("visible-string", renderString)
	writeResult("interop/visible-string", cl.writeItem("interop", "visible-string", mms.NewVisibleString("hello")))
	read("visible-string", renderString)
	read("octet-string", renderAsIs)
	read("bit-string", renderAsIs)
	read("utc-time", renderAsIs)
	read("array", renderArrayInts)
	read("structure", renderStructureBoolInt)

	// --- Phase 1C, step 6: data-model operations ---
	{
		ctx, cancel := reqCtx()
		ts, err := conn.GetVariableAccessAttributes(ctx, "interop", "boolean")
		cancel()
		if err != nil {
			emitFail("get-var-access-attr", "interop/boolean", coarseErrName(err))
		} else {
			// spec_type_str of mms_client.c has no mms-string/binary-time
			name := specTypeName(ts.Kind)
			if name == "mms-string" || name == "binary-time" {
				name = "unknown"
			}
			jl.Emit(append(begin("get-var-access-attr", "interop/boolean", true), field{"type", name}))
		}
	}
	{
		ctx, cancel := reqCtx()
		vals, err := conn.Read(ctx, "interop", "boolean", "integer")
		cancel()
		if err != nil {
			emitFail("read-multiple", "interop", coarseErrName(err))
		} else {
			valuesArray("read-multiple", "interop", vals)
		}
	}
	{
		ctx, cancel := reqCtx()
		res, err := conn.Write(ctx, "interop", []string{"boolean", "integer"},
			[]*mms.Value{mms.NewBool(false), mms.NewInt32(0)})
		cancel()
		switch {
		case err != nil:
			emitFail("write-multiple", "interop", coarseErrName(err))
		case anyFailed(res):
			emitFail("write-multiple", "interop", "partial-failure")
		default:
			emitOK("write-multiple", "interop")
		}
	}
	members := []mms.VarRef{{Domain: "interop", Item: "boolean"}, {Domain: "interop", Item: "integer"}}
	{
		ctx, cancel := reqCtx()
		err := conn.DefineNamedVariableList(ctx, "interop", "testlist", members)
		cancel()
		if err != nil {
			emitFail("define-nvl", "interop/testlist", coarseErrName(err))
		} else {
			emitOK("define-nvl", "interop/testlist")
		}
	}
	{
		ctx, cancel := reqCtx()
		vals, err := conn.ReadNamedVariableList(ctx, "interop", "testlist")
		cancel()
		if err != nil {
			emitFail("read-nvl", "interop/testlist", coarseErrName(err))
		} else {
			valuesArray("read-nvl", "interop/testlist", vals)
		}
	}
	{
		ctx, cancel := reqCtx()
		err := conn.DeleteNamedVariableList(ctx, "interop", "testlist")
		cancel()
		if err != nil {
			emitFail("delete-nvl", "interop/testlist", deleteErrName(err, coarseErrName))
		} else {
			emitOK("delete-nvl", "interop/testlist")
		}
	}

	// --- Phase 1C, step 7: negative tests ---
	v, err := cl.readItem("unknown-domain", "boolean")
	negativeRead("unknown-domain/boolean", v, err)
	v, err = cl.readItem("interop", "nonexistent")
	negativeRead("interop/nonexistent", v, err)
	// write a boolean to the integer variable
	writeResult("interop/integer-wrong-type", cl.writeItem("interop", "integer", mms.NewBool(true)))
}

// negativeRead is check_read_val: an expected failure by name, or an
// unexpected success without a value.
func negativeRead(target string, v *mms.Value, err error) {
	if err == nil {
		emitOK("read", target)
		return
	}
	readResult(target, v, err, nil)
}

func anyFailed(res []error) bool {
	for _, e := range res {
		if e != nil {
			return true
		}
	}
	return false
}

// deleteErrName names a DeleteNamedVariableList failure. libiec61850
// reports a list that was not deleted as a false result with MmsError
// none; the library reports it as an error.
func deleteErrName(err error, name func(error) string) string {
	var dae mms.DataAccessError
	if errors.As(err, &dae) || errors.Is(err, mms.ErrNotDeleted) {
		return "none"
	}
	return name(err)
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	jl.HandleMetaFlags()
	host := "127.0.0.1"
	port := 1102

	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--host":
			host = args[i+1]
		case "--port":
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				jl.Fatal(1, "error: invalid --port %q", args[i+1])
			}
			port = p
		}
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	jl.Log("connecting to %s", addr)

	// Connection 1: full sequence
	if runSequence(addr, true) == 2 {
		jl.Fatal(2, "fatal: connection 1 failed to connect to %s", addr)
	}
	// Connection 2: reconnect verification
	if runSequence(addr, false) == 2 {
		jl.Fatal(2, "fatal: connection 2 (reconnect) failed to connect to %s", addr)
	}
	// Connection 3: abort instead of conclude
	runAbort(addr)
	// Connection 4: server-initiated abort
	runServerAbort(addr)
}
