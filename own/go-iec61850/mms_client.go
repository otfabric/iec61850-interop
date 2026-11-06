// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	mms "github.com/otfabric/go-mms"
	"github.com/otfabric/go-mms/transport/iso"
)

// Timeouts of the mms-client: a connection attempt, and the whole of one
// connection's sequence.
const (
	mmsConnectTimeout  = 10 * time.Second
	mmsSequenceTimeout = 120 * time.Second
	mmsServerAbortWait = 3 * time.Second
)

// runMMSClient is otfabric-mms-client: the fixed service sequence of
// fixture mms-v2 on go-mms's client, over four connections as the other
// stacks run it: (1) the full sequence, (2) reconnect (read, conclude),
// (3) read and abort, (4) the server-initiated abort. One JSON line per
// operation; operation names, targets and value encodings are those of
// docs/contract.md and the libiec61850 client.
func runMMSClient(args []string) int {
	fs := flag.NewFlagSet("otfabric-mms-client", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "127.0.0.1", "server host")
	port := fs.Int("port", 1102, "server port")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("mms-client: unexpected argument %q", fs.Arg(0))
	}
	if *port <= 0 || *port > 65535 {
		return argError("mms-client: invalid port %d", *port)
	}
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))

	r := &mmsRun{addr: addr}
	if !r.sequence(true) {
		logf("mms-client: connection 1 failed to connect to %s", addr)
		return exitConnect
	}
	if !r.sequence(false) {
		logf("mms-client: connection 2 (reconnect) failed to connect to %s", addr)
		return exitConnect
	}
	r.abort()
	r.serverAbort()
	return exitOK
}

// mmsRun is one run of the mms-client against one server.
type mmsRun struct {
	addr string

	mu      sync.Mutex
	reports int
}

func (r *mmsRun) dial(ctx context.Context) (*mms.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, mmsConnectTimeout)
	defer cancel()
	return iso.Dial(dctx, r.addr)
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

// mmsOp is one operation line; target is left out when omitTarget is set.
type mmsOp struct {
	name, target string
	omitTarget   bool
}

func (o mmsOp) line(ok bool) map[string]any {
	m := map[string]any{"operation": o.name, "ok": ok}
	if !o.omitTarget {
		m["target"] = o.target
	}
	return m
}

func (o mmsOp) ok(extra ...any) {
	m := o.line(true)
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	emit(m)
}

func (o mmsOp) fail(reason string) {
	m := o.line(false)
	m["error"] = reason
	emit(m)
}

func (o mmsOp) err(err error) { o.fail(mmsErrName(err)) }

// read emits a read result: the value, or the failure.
func (o mmsOp) read(v *mms.Value, err error) {
	switch {
	case err != nil:
		o.err(err)
	case v == nil:
		o.fail("no-value")
	default:
		if code, isErr := v.DataAccessErr(); isErr {
			o.fail(mmsDAEName(code))
			return
		}
		o.ok("value", mmsJSON(v))
	}
}

// writeResults emits the outcome of a multi-variable write: ok, or the
// data access error of a failed member (the last one).
func (o mmsOp) writeResults(results []mms.WriteAccessResult, err error) {
	if err != nil {
		o.err(err)
		return
	}
	failure := ""
	for _, w := range results {
		if !w.Success {
			failure = mmsDAEName(w.ErrorCode)
		}
	}
	if failure != "" {
		o.fail(failure)
		return
	}
	o.ok()
}

// mmsDAEName is the libiec61850 name of a data access error.
func mmsDAEName(c mms.DataAccessErrorCode) string {
	switch c {
	case mms.DataAccessErrorObjectInvalidated:
		return "object-invalidated"
	case mms.DataAccessErrorHardwareFault:
		return "hardware-fault"
	case mms.DataAccessErrorTemporarilyUnavailable:
		return "temporarily-unavailable"
	case mms.DataAccessErrorObjectAccessDenied:
		return "object-access-denied"
	case mms.DataAccessErrorObjectUndefined:
		return "object-undefined"
	case mms.DataAccessErrorInvalidAddress:
		return "invalid-address"
	case mms.DataAccessErrorTypeUnsupported:
		return "type-unsupported"
	case mms.DataAccessErrorTypeInconsistent:
		return "type-inconsistent"
	case mms.DataAccessErrorObjectAttributeInconsistent:
		return "object-attribute-inconsistent"
	case mms.DataAccessErrorObjectAccessUnsupported:
		return "object-access-unsupported"
	case mms.DataAccessErrorObjectNonExistent:
		return "object-non-existent"
	case mms.DataAccessErrorObjectValueInvalid:
		return "object-value-invalid"
	case mms.DataAccessErrorNone:
		return "success"
	}
	return "unknown"
}

// mmsErrName is the libiec61850 name of a go-mms error (the MmsError
// names of mms_client_services.c), the ISO 9506-2 code name of other
// service errors, or "mms-error".
func mmsErrName(err error) string {
	if dae, ok := errors.AsType[*mms.DataAccessError](err); ok {
		return mmsDAEName(dae.Code)
	}
	if se, ok := errors.AsType[*mms.ServiceError](err); ok {
		switch {
		case se.Class == mms.ErrorClassAccess && se.Code == 2:
			return "object-non-existent"
		case se.Class == mms.ErrorClassAccess && se.Code == 3:
			return "object-access-denied"
		case se.Class == mms.ErrorClassDefinition && se.Code == 1:
			return "object-undefined"
		case se.Class == mms.ErrorClassFile && se.Code == 7:
			return "file-non-existent"
		case se.Class == mms.ErrorClassFile:
			return "file-other"
		case se.Class == mms.ErrorClassService && se.Code == 0:
			return "service-other"
		}
		if n := se.CodeName(); n != "" {
			return n
		}
		return "mms-error"
	}
	var rej *mms.RejectError
	switch {
	case errors.As(err, &rej):
		return "reject"
	case errors.Is(err, mms.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, mms.ErrInvokeTimeout), errors.Is(err, context.DeadlineExceeded):
		return "service-timeout"
	case errors.Is(err, mms.ErrClosed), errors.Is(err, mms.ErrServerConnClosed):
		return "connection-lost"
	}
	return "mms-error"
}

// mmsJSON is the value JSON of the libiec61850 client: numbers, octet
// strings in hex, bit strings as binary digits (bit 0 first), times in
// milliseconds since the epoch, arrays and structures as JSON arrays,
// per-variable data access errors as {"error": name}.
func mmsJSON(v *mms.Value) any {
	if v == nil {
		return nil
	}
	switch v.Type() {
	case mms.ValueTypeBoolean:
		b, _ := v.Bool()
		return b
	case mms.ValueTypeInteger:
		n, _ := v.Int64()
		return n
	case mms.ValueTypeUnsigned:
		n, _ := v.Uint64()
		return n
	case mms.ValueTypeFloat:
		f, _ := v.Float64()
		return mmsFloatJSON(f)
	case mms.ValueTypeVisibleString:
		s, _ := v.VisibleString()
		return s
	case mms.ValueTypeMmsString:
		s, _ := v.MmsString()
		return s
	case mms.ValueTypeOctetString:
		b, _ := v.OctetString()
		return hex.EncodeToString(b)
	case mms.ValueTypeBitString:
		b, _ := v.BitString()
		n, _ := v.BitStringLength()
		return mmsBitDigits(b, n)
	case mms.ValueTypeUTCTime:
		t, _ := v.UTCTime()
		return t.UnixMilli()
	case mms.ValueTypeBinaryTime:
		ms, _ := v.BinaryTime()
		return ms
	case mms.ValueTypeArray:
		e, _ := v.ArrayElements()
		return mmsJSONList(e)
	case mms.ValueTypeStructure:
		e, _ := v.Structure()
		return mmsJSONList(e)
	case mms.ValueTypeDataAccessError:
		c, _ := v.DataAccessErr()
		return map[string]any{"error": mmsDAEName(c)}
	}
	return nil
}

func mmsJSONList(vs []*mms.Value) []any {
	out := make([]any, 0, len(vs))
	for _, e := range vs {
		out = append(out, mmsJSON(e))
	}
	return out
}

// mmsFloatJSON is a float in the shortest form that reads back as the same
// float32 when it is one (0.1, not 0.10000000149011612), else as float64.
func mmsFloatJSON(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	if float64(float32(f)) == f {
		return json.Number(strconv.FormatFloat(f, 'g', -1, 32))
	}
	return f
}

func mmsBitDigits(b []byte, n int) string {
	out := make([]byte, n)
	for i := range n {
		out[i] = '0'
		if i/8 < len(b) && b[i/8]&(0x80>>(i%8)) != 0 {
			out[i] = '1'
		}
	}
	return string(out)
}

func mmsTypeName(t mms.ValueType) string {
	switch t {
	case mms.ValueTypeBoolean:
		return "boolean"
	case mms.ValueTypeInteger:
		return "integer"
	case mms.ValueTypeUnsigned:
		return "unsigned"
	case mms.ValueTypeFloat:
		return "float32"
	case mms.ValueTypeVisibleString:
		return "visible-string"
	case mms.ValueTypeMmsString:
		return "mms-string"
	case mms.ValueTypeOctetString:
		return "octet-string"
	case mms.ValueTypeBitString:
		return "bit-string"
	case mms.ValueTypeUTCTime:
		return "utc-time"
	case mms.ValueTypeBinaryTime:
		return "binary-time"
	case mms.ValueTypeArray:
		return "array"
	case mms.ValueTypeStructure:
		return "structure"
	}
	return "unknown"
}

// mmsSpecJSON is a type specification: {"type"}, with "count" and "element"
// for an array and "components" [{"name","spec"}] for a structure.
func mmsSpecJSON(ts mms.TypeSpec) map[string]any {
	m := map[string]any{"type": mmsTypeName(ts.Type)}
	switch ts.Type {
	case mms.ValueTypeArray:
		m["count"] = ts.Count
		if ts.Element != nil {
			m["element"] = mmsSpecJSON(*ts.Element)
		} else {
			m["element"] = map[string]any{"type": "unknown"}
		}
	case mms.ValueTypeStructure:
		comps := make([]any, 0, len(ts.Elements))
		for _, e := range ts.Elements {
			comps = append(comps, map[string]any{"name": e.Name, "spec": mmsSpecJSON(e.Type)})
		}
		m["components"] = comps
	}
	return m
}

// mmsScalars is the values array of read-multiple and of the first
// read-nvl: scalars, null for anything else and for failed members.
func mmsScalars(results []mms.AccessResult) []any {
	out := make([]any, 0, len(results))
	for _, a := range results {
		if a.Value == nil {
			out = append(out, nil)
			continue
		}
		switch a.Value.Type() {
		case mms.ValueTypeBoolean, mms.ValueTypeInteger, mms.ValueTypeUnsigned, mms.ValueTypeFloat:
			out = append(out, mmsJSON(a.Value))
		default:
			out = append(out, nil)
		}
	}
	return out
}

// mmsAccessJSON is an NVL read result as one array value: each member's
// value, or {"error": name}.
func mmsAccessJSON(results []mms.AccessResult) []any {
	out := make([]any, 0, len(results))
	for _, a := range results {
		if a.Value == nil {
			out = append(out, map[string]any{"error": mmsDAEName(a.ErrorCode)})
			continue
		}
		out = append(out, mmsJSON(a.Value))
	}
	return out
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

func mmsDV(item string) mms.ObjectName {
	return mms.ObjectName{Scope: mms.ObjectScopeDomain, Domain: "interop", ItemID: mms.ItemID(item)}
}

func mmsVMD(item string) mms.ObjectName {
	return mms.ObjectName{Scope: mms.ObjectScopeVMD, ItemID: mms.ItemID(item)}
}

func mmsAA(item string) mms.ObjectName {
	return mms.ObjectName{Scope: mms.ObjectScopeAssociation, ItemID: mms.ItemID(item)}
}

func mmsMembers() []mms.VariableSpec {
	return []mms.VariableSpec{{Name: mmsDV("boolean")}, {Name: mmsDV("integer")}}
}

// mmsRef is "domain/item", or "item" outside a domain.
func mmsRef(n mms.ObjectName) string {
	if n.Scope == mms.ObjectScopeDomain {
		return string(n.Domain) + "/" + string(n.ItemID)
	}
	return string(n.ItemID)
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

// sequence runs one connection: the full sequence or the reconnect check.
// It returns false when the connection could not be made.
func (r *mmsRun) sequence(full bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), mmsSequenceTimeout)
	defer cancel()
	c, err := r.dial(ctx)
	if err != nil {
		logf("mms-client: connect %s: %v", r.addr, err)
		return false
	}
	if full {
		c.OnInformationReport(r.onReport)
		r.phase1(ctx, c)
		r.services(ctx, c)
	} else {
		v, err := r.readValue(ctx, c, "boolean")
		mmsOp{name: "read", target: "interop/boolean"}.read(v, err)
	}
	if err := c.Close(ctx); err != nil {
		mmsOp{name: "conclude", omitTarget: true}.err(err)
	} else {
		mmsOp{name: "conclude", omitTarget: true}.ok()
	}
	return true
}

func (r *mmsRun) readValue(ctx context.Context, c *mms.Client, item string) (*mms.Value, error) {
	res, err := c.Read(ctx, mms.ReadRequest{DomainID: "interop", ItemID: mms.ItemID(item)})
	if err != nil {
		return nil, err
	}
	return res.Value, nil
}

func (r *mmsRun) read(ctx context.Context, c *mms.Client, item, target string) {
	v, err := r.readValue(ctx, c, item)
	mmsOp{name: "read", target: target}.read(v, err)
}

func (r *mmsRun) write(ctx context.Context, c *mms.Client, item, target string, v *mms.Value) {
	_, err := c.Write(ctx, mms.WriteRequest{DomainID: "interop", ItemID: mms.ItemID(item), Value: v})
	o := mmsOp{name: "write", target: target}
	if err != nil {
		o.err(err)
		return
	}
	o.ok()
}

// phase1 is the first part of the full sequence: identify, name lists,
// reads and writes of every fixture type, access attributes, multiple
// access, a domain NVL and the negative cases.
func (r *mmsRun) phase1(ctx context.Context, c *mms.Client) {
	if id, err := c.Identify(ctx); err != nil {
		mmsOp{name: "identify", omitTarget: true}.err(err)
	} else {
		mmsOp{name: "identify", omitTarget: true}.ok("value", map[string]any{"vendor": id.Vendor, "model": id.Model, "revision": id.Revision})
	}
	r.nameList(ctx, c, mmsOp{name: "get-name-list", omitTarget: true}, mms.NameListRequest{ObjectClass: mms.ObjectClassDomain, Scope: mms.ObjectScopeVMD})
	r.nameList(ctx, c, mmsOp{name: "get-name-list", target: "interop"}, mms.NameListRequest{ObjectClass: mms.ObjectClassNamedVariable, Scope: mms.ObjectScopeDomain, DomainID: "interop"})

	r.read(ctx, c, "boolean", "interop/boolean")
	r.read(ctx, c, "integer", "interop/integer")
	r.read(ctx, c, "float32", "interop/float32")
	r.write(ctx, c, "float32", "interop/float32", mms.NewFloat(99))
	r.read(ctx, c, "float32", "interop/float32")
	r.write(ctx, c, "octet-string", "interop/octet-string", mms.NewOctetString([]byte{0xde, 0xad}))
	r.read(ctx, c, "boolean", "interop/boolean")

	r.read(ctx, c, "unsigned", "interop/unsigned")
	r.read(ctx, c, "visible-string", "interop/visible-string")
	r.write(ctx, c, "visible-string", "interop/visible-string", mms.NewVisibleString("hello"))
	r.read(ctx, c, "visible-string", "interop/visible-string")
	r.read(ctx, c, "octet-string", "interop/octet-string")
	r.read(ctx, c, "bit-string", "interop/bit-string")
	r.read(ctx, c, "utc-time", "interop/utc-time")
	r.read(ctx, c, "array", "interop/array")
	r.read(ctx, c, "structure", "interop/structure")

	if a, err := c.GetVariableAccessAttributes(ctx, mmsDV("boolean")); err != nil {
		mmsOp{name: "get-var-access-attr", target: "interop/boolean"}.err(err)
	} else {
		mmsOp{name: "get-var-access-attr", target: "interop/boolean"}.ok("type", mmsTypeName(a.TypeSpec.Type))
	}

	if res, err := c.ReadMultiple(ctx, []mms.ObjectName{mmsDV("boolean"), mmsDV("integer")}); err != nil {
		mmsOp{name: "read-multiple", target: "interop"}.err(err)
	} else {
		mmsOp{name: "read-multiple", target: "interop"}.ok("values", mmsScalars(res))
	}
	if res, err := c.WriteVariables(ctx, mmsMembers(), []*mms.Value{mms.NewBoolean(false), mms.NewInteger(0)}); err != nil {
		mmsOp{name: "write-multiple", target: "interop"}.err(err)
	} else {
		failed := false
		for _, w := range res {
			failed = failed || !w.Success
		}
		if failed {
			mmsOp{name: "write-multiple", target: "interop"}.fail("partial-failure")
		} else {
			mmsOp{name: "write-multiple", target: "interop"}.ok()
		}
	}

	list := mmsDV("testlist")
	r.defineNVL(ctx, c, "interop/testlist", list)
	if res, err := c.ReadNamedVariableList(ctx, list); err != nil {
		mmsOp{name: "read-nvl", target: "interop/testlist"}.err(err)
	} else {
		mmsOp{name: "read-nvl", target: "interop/testlist"}.ok("values", mmsScalars(res))
	}
	r.deleteNVL(ctx, c, "interop/testlist", list)

	v, err := c.Read(ctx, mms.ReadRequest{DomainID: "unknown-domain", ItemID: "boolean"})
	r.readOutcome(mmsOp{name: "read", target: "unknown-domain/boolean"}, v, err)
	v, err = c.Read(ctx, mms.ReadRequest{DomainID: "interop", ItemID: "nonexistent"})
	r.readOutcome(mmsOp{name: "read", target: "interop/nonexistent"}, v, err)
	r.write(ctx, c, "integer", "interop/integer-wrong-type", mms.NewBoolean(true))
}

func (r *mmsRun) readOutcome(o mmsOp, res *mms.ReadResult, err error) {
	if err != nil {
		o.err(err)
		return
	}
	o.read(res.Value, nil)
}

func (r *mmsRun) nameList(ctx context.Context, c *mms.Client, o mmsOp, req mms.NameListRequest) {
	names, err := c.GetNameListAll(ctx, req)
	if err != nil {
		o.err(err)
		return
	}
	if names == nil {
		names = []string{}
	}
	o.ok("names", names)
}

func (r *mmsRun) defineNVL(ctx context.Context, c *mms.Client, target string, list mms.ObjectName) {
	o := mmsOp{name: "define-nvl", target: target}
	if err := c.DefineNamedVariableList(ctx, mms.DefineNamedVariableListRequest{ListName: list, Variables: mmsMembers()}); err != nil {
		o.err(err)
		return
	}
	o.ok()
}

func (r *mmsRun) deleteNVL(ctx context.Context, c *mms.Client, target string, list mms.ObjectName) {
	o := mmsOp{name: "delete-nvl", target: target}
	res, err := c.DeleteNamedVariableList(ctx, []mms.ObjectName{list})
	switch {
	case err != nil:
		o.err(err)
	case res.NumberDeleted != 1:
		o.fail(fmt.Sprintf("deleted %d of %d", res.NumberDeleted, res.NumberMatched))
	default:
		o.ok()
	}
}

func (r *mmsRun) nvlAttributes(ctx context.Context, c *mms.Client, target string, list mms.ObjectName) {
	o := mmsOp{name: "get-nvl-attrs", target: target}
	a, err := c.GetNamedVariableListAttributes(ctx, list)
	if err != nil {
		o.err(err)
		return
	}
	vars := make([]any, 0, len(a.Variables))
	for _, v := range a.Variables {
		vars = append(vars, mmsRef(v.Name))
	}
	o.ok("value", map[string]any{"deletable": a.Deletable, "variables": vars})
}

func (r *mmsRun) readNVL(ctx context.Context, c *mms.Client, target string, list mms.ObjectName, spec bool) {
	o := mmsOp{name: "read-nvl", target: target}
	res, err := c.ReadNamedVariableList(ctx, list, mms.ReadNamedVariableListOptions{SpecificationWithResult: spec})
	if err != nil {
		o.err(err)
		return
	}
	o.ok("value", mmsAccessJSON(res))
}

// services is the second part of the full sequence: the mms-v2 services.
func (r *mmsRun) services(ctx context.Context, c *mms.Client) {
	// Status
	for _, ext := range []bool{false, true} {
		o := mmsOp{name: "status"}
		if ext {
			o.target = "extended"
		}
		if st, err := c.StatusWithOptions(ctx, mms.ClientStatusRequest{ExtendedDerivation: ext}); err != nil {
			o.err(err)
		} else {
			o.ok("value", map[string]any{"logical": int(st.Logical), "physical": int(st.Physical)})
		}
	}

	// Name lists
	r.nameList(ctx, c, mmsOp{name: "get-name-list", target: "interop/journals"}, mms.NameListRequest{ObjectClass: mms.ObjectClassJournal, Scope: mms.ObjectScopeDomain, DomainID: "interop"})
	r.nameList(ctx, c, mmsOp{name: "get-name-list", target: "vmd/variables"}, mms.NameListRequest{ObjectClass: mms.ObjectClassNamedVariable, Scope: mms.ObjectScopeVMD})
	r.nameList(ctx, c, mmsOp{name: "get-name-list", target: "unknown-domain/variables"}, mms.NameListRequest{ObjectClass: mms.ObjectClassNamedVariable, Scope: mms.ObjectScopeDomain, DomainID: "unknown-domain"})

	// Type specifications
	for _, item := range []string{"array-rw", "structure-rw"} {
		o := mmsOp{name: "get-var-access-attr", target: "interop/" + item}
		if a, err := c.GetVariableAccessAttributes(ctx, mmsDV(item)); err != nil {
			o.err(err)
		} else {
			o.ok("type", mmsTypeName(a.TypeSpec.Type), "value", mmsSpecJSON(a.TypeSpec))
		}
	}

	// Alternate access
	res, err := c.ReadArrayRange(ctx, mmsDV("array"), 1, 3)
	r.readOutcome(mmsOp{name: "read-array-elements", target: "interop/array[1:4]"}, res, err)
	res, err = c.ReadArrayElement(ctx, mmsDV("array"), 2)
	r.readOutcome(mmsOp{name: "read-array-elements", target: "interop/array[2]"}, res, err)
	res, err = c.ReadComponent(ctx, mmsDV("structure-rw"), "count")
	r.readOutcome(mmsOp{name: "read-component", target: "interop/structure-rw$count"}, res, err)
	if err := c.WriteArrayElement(ctx, mmsDV("array-rw"), 1, mms.NewInteger(21)); err != nil {
		mmsOp{name: "write-array-elements", target: "interop/array-rw[1]"}.err(err)
	} else {
		mmsOp{name: "write-array-elements", target: "interop/array-rw[1]"}.ok()
	}
	wres, err := c.WriteVariables(ctx,
		[]mms.VariableSpec{{Name: mmsDV("array-rw"), AlternateAccess: []mms.AccessSelector{mms.SelectRange(2, 2)}}},
		[]*mms.Value{mms.NewArray([]*mms.Value{mms.NewInteger(33), mms.NewInteger(44)})})
	mmsOp{name: "write-array-elements", target: "interop/array-rw[2:4]"}.writeResults(wres, err)
	if err := c.WriteComponent(ctx, mmsDV("structure-rw"), "count", mms.NewInteger(99)); err != nil {
		mmsOp{name: "write-component", target: "interop/structure-rw$count"}.err(err)
	} else {
		mmsOp{name: "write-component", target: "interop/structure-rw$count"}.ok()
	}
	r.read(ctx, c, "array-rw", "interop/array-rw")
	r.read(ctx, c, "structure-rw", "interop/structure-rw")

	// Named variable lists: domain scope
	list := mmsDV("svclist")
	r.defineNVL(ctx, c, "interop/svclist", list)
	r.nameList(ctx, c, mmsOp{name: "get-name-list", target: "interop/nvl"}, mms.NameListRequest{ObjectClass: mms.ObjectClassNamedVariableList, Scope: mms.ObjectScopeDomain, DomainID: "interop"})
	r.nvlAttributes(ctx, c, "interop/svclist", list)
	r.readNVL(ctx, c, "interop/svclist?spec", list, true)
	wres, err = c.WriteNamedVariableList(ctx, list, []*mms.Value{mms.NewBoolean(true), mms.NewInteger(77)})
	mmsOp{name: "write-nvl", target: "interop/svclist"}.writeResults(wres, err)
	r.readNVL(ctx, c, "interop/svclist", list, false)
	r.deleteNVL(ctx, c, "interop/svclist", list)

	// VMD scope
	list = mmsVMD("vmdlist")
	r.defineNVL(ctx, c, "vmd/vmdlist", list)
	r.nvlAttributes(ctx, c, "vmd/vmdlist", list)
	r.readNVL(ctx, c, "vmd/vmdlist", list, false)
	r.deleteNVL(ctx, c, "vmd/vmdlist", list)

	// The server-defined list behind the fixture's named-list report
	r.nvlAttributes(ctx, c, "vmd/interop-report", mmsVMD("interop-report"))
	r.readNVL(ctx, c, "vmd/interop-report", mmsVMD("interop-report"), false)

	// Association scope
	list = mmsAA("aalist")
	r.defineNVL(ctx, c, "aa/aalist", list)
	r.nameList(ctx, c, mmsOp{name: "get-name-list", target: "aa/nvl"}, mms.NameListRequest{ObjectClass: mms.ObjectClassNamedVariableList, Scope: mms.ObjectScopeAssociation})
	r.nvlAttributes(ctx, c, "aa/aalist", list)
	r.readNVL(ctx, c, "aa/aalist", list, false)
	wres, err = c.WriteNamedVariableList(ctx, list, []*mms.Value{mms.NewBoolean(true), mms.NewInteger(-123)})
	mmsOp{name: "write-nvl", target: "aa/aalist"}.writeResults(wres, err)
	r.deleteNVL(ctx, c, "aa/aalist", list)

	r.files(ctx, c)
	r.journals(ctx, c)
	r.reportsStep(ctx, c)
}

// files are the file services: directories, reads, delete, rename, obtain.
func (r *mmsRun) files(ctx context.Context, c *mms.Client) {
	r.fileDirectory(ctx, c, "", "")
	r.fileDirectory(ctx, c, "COMTRADE", "COMTRADE")
	// The name of the zero-size entry a per-level server lists for the
	// subdirectory.
	r.fileDirectory(ctx, c, `COMTRADE\`, `COMTRADE\`)

	r.fileRead(ctx, c, "REC01.CFG", 0, "REC01.CFG")
	r.fileRead(ctx, c, "REC01.DAT", 4, "REC01.DAT@4")
	r.fileRead(ctx, c, "LARGE.BIN", 0, "LARGE.BIN")
	r.fileRead(ctx, c, "NOTIME.TXT", 0, "NOTIME.TXT")
	r.fileRead(ctx, c, "NO-SUCH.FILE", 0, "NO-SUCH.FILE")

	if err := c.FileDelete(ctx, "DELETE.ME"); err != nil {
		mmsOp{name: "file-delete", target: "DELETE.ME"}.err(err)
	} else {
		mmsOp{name: "file-delete", target: "DELETE.ME"}.ok()
	}
	if err := c.FileRename(ctx, "RENAME.ME", "RENAMED.ME"); err != nil {
		mmsOp{name: "file-rename", target: "RENAME.ME->RENAMED.ME"}.err(err)
	} else {
		mmsOp{name: "file-rename", target: "RENAME.ME->RENAMED.ME"}.ok()
	}
	if err := c.ObtainFile(ctx, "REC01.CFG", "OBTAINED.CFG"); err != nil {
		mmsOp{name: "obtain-file", target: "REC01.CFG->OBTAINED.CFG"}.err(err)
	} else {
		mmsOp{name: "obtain-file", target: "REC01.CFG->OBTAINED.CFG"}.ok()
	}
	r.fileDirectory(ctx, c, "", "after-changes")
}

func mmsMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (r *mmsRun) fileDirectory(ctx context.Context, c *mms.Client, spec, target string) {
	o := mmsOp{name: "file-directory", target: target}
	entries, err := c.FileDirectoryAll(ctx, spec)
	if err != nil {
		o.err(err)
		return
	}
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{"name": e.Name, "size": e.Size, "lastModified": mmsMillis(e.LastModified)})
	}
	o.ok("value", out)
}

// fileRead opens, reads to the end and closes a file: "file-read" with
// {size, lastModified, length, fnv1a, data (hex, files up to 256 bytes)},
// then "file-close" when the open succeeded.
func (r *mmsRun) fileRead(ctx context.Context, c *mms.Client, name string, position uint32, target string) {
	o := mmsOp{name: "file-read", target: target}
	f, err := c.FileOpen(ctx, name, mms.FileOpenOptions{InitialPosition: position})
	if err != nil {
		o.err(err)
		return
	}
	var data []byte
	for {
		chunk, err := c.FileRead(ctx, f.FrsmID)
		if err != nil {
			o.err(err)
			data = nil
			break
		}
		data = append(data, chunk.Data...)
		if !chunk.MoreFollows {
			v := map[string]any{"size": f.Size, "lastModified": mmsMillis(f.LastModified), "length": len(data), "fnv1a": mmsFNV1a(data)}
			if len(data) <= 256 {
				v["data"] = hex.EncodeToString(data)
			}
			o.ok("value", v)
			break
		}
		if len(chunk.Data) == 0 {
			o.fail("file-read made no progress")
			break
		}
	}
	co := mmsOp{name: "file-close", target: target}
	if err := c.FileClose(ctx, f.FrsmID); err != nil {
		co.err(err)
	} else {
		co.ok()
	}
}

func mmsFNV1a(d []byte) string {
	h := uint64(0xcbf29ce484222325)
	for _, b := range d {
		h ^= uint64(b)
		h *= 0x100000001b3
	}
	return fmt.Sprintf("%016x", h)
}

// journals reads log1 by time range and after entry 2, and a journal the
// server does not have.
func (r *mmsRun) journals(ctx context.Context, c *mms.Client) {
	t1 := time.UnixMilli(1704067201000).UTC() // fixture log1 entry 1
	t4 := time.UnixMilli(1704067204000).UTC() // entry 4
	res, err := c.ReadJournalTimeRange(ctx, "interop", "log1", t1, t4)
	mmsJournalLine("interop/log1", res, err)

	id2 := make([]byte, 8)
	binary.LittleEndian.PutUint64(id2, 2)
	res, err = c.ReadJournalStartAfter(ctx, "interop", "log1", t1.Add(time.Second), id2)
	mmsJournalLine("interop/log1@after-2", res, err)

	res, err = c.ReadJournalTimeRange(ctx, "interop", "nolog", t1, t4)
	mmsJournalLine("interop/nolog", res, err)
}

func mmsJournalLine(target string, res *mms.JournalResult, err error) {
	o := mmsOp{name: "read-journal", target: target}
	if err != nil {
		o.err(err)
		return
	}
	entries := make([]any, 0, len(res.Entries))
	for _, e := range res.Entries {
		vars := make([]any, 0, len(e.Variables))
		for _, v := range e.Variables {
			vars = append(vars, map[string]any{"tag": v.Tag, "value": mmsJSON(v.Value)})
		}
		entries = append(entries, map[string]any{"id": hex.EncodeToString(e.EntryID), "time": e.OccurrenceTime.UnixMilli(), "variables": vars})
	}
	o.ok("value", map[string]any{"moreFollows": res.MoreFollows, "entries": entries})
}

// onReport emits an InformationReport: a named-list report as one line
// (target "list:<name>", the values as an array), a listOfVariable report
// as one line per variable (target "var:<domain>/<item>").
func (r *mmsRun) onReport(rep *mms.InformationReportIndication) {
	if rep.ListName != nil {
		mmsOp{name: "information-report", target: "list:" + mmsRef(*rep.ListName)}.ok("value", mmsJSONList(rep.Values))
		r.countReport()
		return
	}
	for i, v := range rep.Values {
		target := "var:"
		if i < len(rep.Variables) {
			target += mmsRef(rep.Variables[i])
		}
		mmsOp{name: "information-report", target: target}.ok("value", mmsJSON(v))
		r.countReport()
	}
}

func (r *mmsRun) countReport() {
	r.mu.Lock()
	r.reports++
	r.mu.Unlock()
}

func (r *mmsRun) reportCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reports
}

// reportsStep writes interop/report-trigger, waits up to 3 s for the
// reports (then 300 ms of quiet after the last), and checks that the
// association survived the unconfirmed PDUs.
func (r *mmsRun) reportsStep(ctx context.Context, c *mms.Client) {
	before := r.reportCount()
	r.write(ctx, c, "report-trigger", "interop/report-trigger", mms.NewInteger(1))
	last, quiet := before, 0
	for waited := 0; waited < 3000 && quiet < 300; waited += 50 {
		time.Sleep(50 * time.Millisecond)
		switch n := r.reportCount(); {
		case n != last:
			last, quiet = n, 0
		case n > before:
			quiet += 50
		}
	}
	r.read(ctx, c, "boolean", "interop/boolean#after-reports")
}

// abort is connection 3: a read, then an abort instead of a conclude.
func (r *mmsRun) abort() {
	ctx, cancel := context.WithTimeout(context.Background(), mmsSequenceTimeout)
	defer cancel()
	o := mmsOp{name: "abort"}
	c, err := r.dial(ctx)
	if err != nil {
		o.err(err)
		return
	}
	r.read(ctx, c, "boolean", "interop/boolean#before-abort")
	if err := c.Abort(ctx); err != nil {
		o.err(err)
		return
	}
	o.ok()
}

// serverAbort is connection 4: writing -1 to interop/report-trigger asks
// the server to abort the association; ok when the association is lost
// and the write did not succeed.
func (r *mmsRun) serverAbort() {
	ctx, cancel := context.WithTimeout(context.Background(), mmsSequenceTimeout)
	defer cancel()
	o := mmsOp{name: "server-abort"}
	c, err := r.dial(ctx)
	if err != nil {
		o.err(err)
		return
	}
	defer func() { _ = c.Abort(context.Background()) }()
	wctx, wcancel := context.WithTimeout(ctx, mmsServerAbortWait)
	defer wcancel()
	_, werr := c.Write(wctx, mms.WriteRequest{DomainID: "interop", ItemID: "report-trigger", Value: mms.NewInteger(mmsReportTriggerAbort)})
	lost := false
	select {
	case <-c.Done():
		lost = true
	case <-time.After(mmsServerAbortWait):
	}
	switch {
	case lost && werr != nil:
		o.ok()
	case !lost:
		o.fail("connection-not-lost")
	default:
		o.fail("write-succeeded")
	}
}
