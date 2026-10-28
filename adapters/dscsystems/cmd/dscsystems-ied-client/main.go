// SPDX-License-Identifier: GPL-3.0-only

// Command dscsystems-ied-client is the dscsystems/go-iec61850 IED client
// adapter for mms-interop, the counterpart of libiec61850-ied-client.
//
// It runs the fixed sequence against --host/--port and emits one JSON Line
// per operation:
//
//  0. associate
//  1. get-server-directory  logical devices; resolves the interop LD
//  2. get-ld-directory      logical nodes in the LD
//  3. get-ln-directory      data objects in LD/GGIO1
//  4. read (ST)             LD/GGIO1.SPS1.stVal
//  5. read (MX)             LD/MMXU1.TotW.mag.f
//  5a. read (ST)            LD/GGIO1.DPS1.stVal (Dbpos as 0..3)
//  5b. read (ST)            LD/MMTR1.TotVAh.actVal
//  6. read (CF)             LD/LLN0.Mod.ctlModel
//  7. read (DC)             LD/LLN0.Mod.d
//  8. write (ST)            LD/LLN0.Mod.stVal = 5
//  9. read-dataset          LD/LLN0$dsInterop
//  10. conclude
//
// Exit codes: 0 all ok, 1 argument error, 2 connection failure, 3 one or
// more operations emitted ok:false (conclude still attempted).
package main

import (
	"os"
	"strings"

	"github.com/dscsystems/go-iec61850/client"
	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/model"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/ied"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/jl"
)

func main() {
	jl.HandleMetaFlags()
	addr := ied.Flags("ied-client")

	c, err := ied.Dial(addr)
	if err != nil {
		jl.Log("ied-client: connect %s: %v", addr, err)
		ied.Fail("associate", "", ied.ErrString(err))
		os.Exit(2)
	}
	ied.OK("associate", "")

	// Continue after failures; always conclude. The LD comes from the
	// server directory: bare InteropLD against go-iec61850, IED-prefixed
	// against libiec61850 / iec61850bean / dscsystems.
	ok := true
	ld, good := getServerDirectory(c)
	ok = ok && good
	if ld == "" {
		ld = "InteropLD"
	}
	ok = getLDDirectory(c, ld) && ok
	ok = getLNDirectory(c, ld+"/GGIO1") && ok
	ok = read(c, ld+"/GGIO1.SPS1.stVal", model.ST, readBool) && ok
	ok = read(c, ld+"/MMXU1.TotW.mag.f", model.MX, readFloat) && ok
	ok = read(c, ld+"/GGIO1.DPS1.stVal", model.ST, readDbpos) && ok
	ok = read(c, ld+"/MMTR1.TotVAh.actVal", model.ST, readUint) && ok
	ok = read(c, ld+"/LLN0.Mod.ctlModel", model.CF, readInt) && ok
	ok = read(c, ld+"/LLN0.Mod.d", model.DC, readString) && ok
	ok = write(c, ld+"/LLN0.Mod.stVal", model.ST, mms.NewInt32(5)) && ok
	ok = readDataSet(c, ld+"/LLN0$dsInterop") && ok

	c.Close()
	ied.OK("conclude", "")
	if !ok {
		os.Exit(3)
	}
}

func getServerDirectory(c *client.Client) (string, bool) {
	ctx, cancel := ied.Ctx()
	defer cancel()
	names, err := c.LogicalDevices(ctx)
	if err != nil {
		ied.Fail("get-server-directory", "", ied.ErrString(err))
		return "", false
	}
	ied.OK("get-server-directory", "", ied.Field{K: "names", V: nonNil(names)})
	ld := ied.ResolveInteropLD(names)
	if ld == "" {
		ied.Fail("get-server-directory", "", "no logical device found")
		return "", false
	}
	return ld, true
}

func getLDDirectory(c *client.Client, ld string) bool {
	ctx, cancel := ied.Ctx()
	defer cancel()
	names, err := c.LogicalNodes(ctx, ld)
	if err != nil {
		ied.Fail("get-ld-directory", ld, ied.ErrString(err))
		return false
	}
	ied.OK("get-ld-directory", ld, ied.Field{K: "names", V: nonNil(names)})
	return true
}

func getLNDirectory(c *client.Client, ldln string) bool {
	ctx, cancel := ied.Ctx()
	defer cancel()
	// Data objects only, as IedConnection_getLogicalNodeDirectory with
	// ACSI_CLASS_DATA_OBJECT.
	names, err := c.LogicalNodeDirectory(ctx, model.ObjectReference(ldln), client.ACSIDataObject)
	if err != nil {
		ied.Fail("get-ln-directory", ldln, ied.ErrString(err))
		return false
	}
	ied.OK("get-ln-directory", ldln, ied.Field{K: "names", V: nonNil(names)})
	return true
}

// A decoder turns the value read into its JSON form, or fails like the
// typed IedConnection_read*Value calls when the type is wrong.
type decoder func(ref string, v *mms.Value) (any, error)

func readBool(ref string, v *mms.Value) (any, error) {
	if v.Type() != mms.TypeBoolean {
		return nil, ied.TypeError(ref, "boolean", v)
	}
	return v.Bool(), nil
}

func readFloat(ref string, v *mms.Value) (any, error) {
	switch v.Type() {
	case mms.TypeFloat32:
		return float64(v.Float32()), nil
	case mms.TypeFloat64:
		return v.Float64(), nil
	}
	return nil, ied.TypeError(ref, "float", v)
}

func readInt(ref string, v *mms.Value) (any, error) {
	switch v.Type() {
	case mms.TypeInteger, mms.TypeUnsigned:
		return int32(v.Int64()), nil
	}
	return nil, ied.TypeError(ref, "integer", v)
}

// readDbpos reports a Dbpos as its enumeration value (2 = on, bits "10").
func readDbpos(ref string, v *mms.Value) (any, error) {
	if v.Type() != mms.TypeBitString {
		return nil, ied.TypeError(ref, "bit-string", v)
	}
	return int(model.DbposFromValue(v)), nil
}

func readUint(ref string, v *mms.Value) (any, error) {
	switch v.Type() {
	case mms.TypeInteger, mms.TypeUnsigned:
		return uint32(v.Int64()), nil
	}
	return nil, ied.TypeError(ref, "unsigned", v)
}

func readString(ref string, v *mms.Value) (any, error) {
	switch v.Type() {
	case mms.TypeVisibleString, mms.TypeMMSString:
		return v.Text(), nil
	}
	return nil, ied.TypeError(ref, "string", v)
}

func read(c *client.Client, ref string, fc model.FC, dec decoder) bool {
	target := ref + "[" + fc.String() + "]"
	ctx, cancel := ied.Ctx()
	defer cancel()
	v, err := c.Read(ctx, model.ObjectReference(ref), fc)
	var out any
	if err == nil {
		out, err = dec(ref, v)
	}
	if err != nil {
		ied.Fail("read", target, ied.ErrString(err))
		return false
	}
	ied.OK("read", target, ied.Field{K: "value", V: out})
	return true
}

func write(c *client.Client, ref string, fc model.FC, v *mms.Value) bool {
	target := ref + "[" + fc.String() + "]"
	ctx, cancel := ied.Ctx()
	defer cancel()
	if err := c.Write(ctx, model.ObjectReference(ref), fc, v); err != nil {
		ied.Fail("write", target, ied.ErrString(err))
		return false
	}
	ied.OK("write", target)
	return true
}

func readDataSet(c *client.Client, ref string) bool {
	ctx, cancel := ied.Ctx()
	defer cancel()
	// One Read of the named variable list, with specificationWithResult
	// set. client.ReadDataSet sends it without, which iec61850bean's
	// server answers with a service error (others); libiec61850 and
	// go-iec61850 servers take either form.
	domain, list, _ := strings.Cut(ref, "/")
	list = strings.ReplaceAll(list, ".", "$")
	_, vals, err := c.MMS().ReadNamedVariableListWithSpec(ctx, domain, list)
	if err != nil {
		ied.Fail("read-dataset", ref, ied.ErrString(err))
		return false
	}
	values := make([]any, len(vals))
	for i, v := range vals {
		values[i] = ied.Scalar(v)
	}
	ied.OK("read-dataset", ref, ied.Field{K: "values", V: values})
	return true
}

// nonNil keeps an empty list a JSON [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
