// SPDX-License-Identifier: GPL-3.0-only

// Command dscsystems-ied-controller is the dscsystems/go-iec61850 control
// adapter for iec61850-interop, the counterpart of libiec61850-ied-controller.
//
// It connects to --host/--port, runs the control sequence of the target DO
// and reads stVal back:
//
//	SPCSO1 direct (ctlModel 1):       read-ctlmodel, operate, read-stval
//	SPCSO2 SBO normal (ctlModel 2):   read-ctlmodel, select, operate, read-stval
//	SPCSO3 SBO enhanced (ctlModel 4): read-ctlmodel, select-with-value,
//	                                  operate, read-stval
//
// then concludes. References are fixed to the bare InteropLD, as in the C.
//
// Arguments: --host, --port, --ctlval 0|1 (default 1), --do SPCSO1|2|3
// (default SPCSO1).
//
// Exit codes: 0 sequence completed (results in stdout), 1 argument error,
// 2 connection failure.
package main

import (
	"flag"
	"strconv"

	"github.com/dscsystems/go-iec61850/client"
	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/model"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/ied"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

// expectedModel is each DO's ctlModel in the fixture; it picks the sequence.
var expectedModel = map[string]model.CtlModel{
	"SPCSO1": model.CtlDirectNormal,
	"SPCSO2": model.CtlSBONormal,
	"SPCSO3": model.CtlSBOEnhanced,
}

func main() {
	jl.HandleMetaFlags()
	ctlvalArg := flag.String("ctlval", "1", "control value, 0 or 1")
	doName := flag.String("do", "SPCSO1", "target data object: SPCSO1, SPCSO2 or SPCSO3")
	addr := ied.Flags("ied-controller")

	n, _ := strconv.Atoi(*ctlvalArg) // atoi semantics: junk is 0
	ctlVal := n != 0
	cm, known := expectedModel[*doName]
	if !known {
		jl.Fatal(1, "ied-controller: unknown --do %s (supported: SPCSO1, SPCSO2, SPCSO3)", *doName)
	}
	ctlRef := "InteropLD/GGIO1." + *doName

	// A connection failure exits 2 with no JSON line, as the C does.
	c, err := ied.Dial(addr)
	if err != nil {
		jl.Fatal(2, "ied-controller: connect %s: %v", addr, err)
	}
	defer func() {
		c.Close()
		ied.OK("conclude", "")
	}()

	readCtlModel(c, ctlRef)

	switch cm {
	case model.CtlSBONormal:
		// Select, then operate on the same handle: the operate reuses the
		// select's ctlNum. If the select fails the C concludes at once.
		co, ok := selectSBO(c, ctlRef)
		if !ok {
			return
		}
		operate(co, ctlRef, ctlVal, model.CtlDirectNormal)
	case model.CtlSBOEnhanced:
		// SelectWithValue and operate share one handle (one ctlNum); a
		// failed select skips the operate but still reads stVal.
		if co, ok := selectWithValue(c, ctlRef, ctlVal); ok {
			operate(co, ctlRef, ctlVal, model.CtlDirectEnhanced)
		}
	default:
		if co, ok := control(c, "operate", ctlRef); ok {
			operate(co, ctlRef, ctlVal, model.CtlDirectNormal)
		}
	}
	readStVal(c, ctlRef)
}

func readCtlModel(c *client.Client, ctlRef string) {
	ref := ctlRef + ".ctlModel"
	target := ref + "[CF]"
	ctx, cancel := ied.Ctx()
	defer cancel()
	v, err := c.Read(ctx, model.ObjectReference(ref), model.CF)
	if err == nil && v.Type() != mms.TypeInteger && v.Type() != mms.TypeUnsigned {
		err = ied.TypeError(ref, "integer", v)
	}
	if err != nil {
		ied.Fail("read-ctlmodel", target, ied.ErrString(err))
		return
	}
	ied.OK("read-ctlmodel", target, ied.Field{K: "value", V: int32(v.Int64())})
}

// control opens a control handle, as ControlObjectClient_create does: it
// reads ctlModel and the type of Oper. ControlFor alone takes a missing
// ctlModel for direct control, so the Oper type read is what tells a
// missing object; either failing, op fails like a failed create.
func control(c *client.Client, op, ctlRef string) (*client.ControlObject, bool) {
	ctx, cancel := ied.Ctx()
	defer cancel()
	co, err := c.ControlFor(ctx, model.ObjectReference(ctlRef))
	if err == nil {
		_, err = co.CtlValSpec(ctx)
	}
	if err != nil {
		jl.Log("ied-controller: %v", err)
		ied.Fail(op, ctlRef, "failed to create ControlObjectClient")
		return nil, false
	}
	return co, true
}

func selectSBO(c *client.Client, ctlRef string) (*client.ControlObject, bool) {
	co, ok := control(c, "select", ctlRef)
	if !ok {
		return nil, false
	}
	ctx, cancel := ied.Ctx()
	defer cancel()
	if err := co.Select(ctx); err != nil {
		ied.Fail("select", ctlRef, ied.ErrString(err))
		return nil, false
	}
	ied.OK("select", ctlRef)
	return co, true
}

func selectWithValue(c *client.Client, ctlRef string, ctlVal bool) (*client.ControlObject, bool) {
	co, ok := control(c, "select-with-value", ctlRef)
	if !ok {
		return nil, false
	}
	ctx, cancel := ied.Ctx()
	defer cancel()
	if err := co.SelectWithValue(ctx, mms.NewBool(ctlVal)); err != nil {
		ied.Fail("select-with-value", ctlRef, ied.ErrString(err))
		return nil, false
	}
	ied.OK("select-with-value", ctlRef)
	return co, true
}

// operate writes Oper. The select step, when there is one, is already done:
// the handle is switched to the direct form of its security level so that
// Operate does not select again. Enhanced security still waits for the
// CommandTermination.
func operate(co *client.ControlObject, ctlRef string, ctlVal bool, direct model.CtlModel) {
	ctx, cancel := ied.Ctx()
	defer cancel()
	if err := co.Operate(ctx, mms.NewBool(ctlVal), client.WithModel(direct)); err != nil {
		ied.Fail("operate", ctlRef, ied.ErrString(err))
		return
	}
	ied.OK("operate", ctlRef, ied.Field{K: "ctlval", V: ctlVal})
}

func readStVal(c *client.Client, ctlRef string) {
	ref := ctlRef + ".stVal"
	target := ref + "[ST]"
	ctx, cancel := ied.Ctx()
	defer cancel()
	v, err := c.Read(ctx, model.ObjectReference(ref), model.ST)
	if err == nil && v.Type() != mms.TypeBoolean {
		err = ied.TypeError(ref, "boolean", v)
	}
	if err != nil {
		ied.Fail("read-stval", target, ied.ErrString(err))
		return
	}
	ied.OK("read-stval", target, ied.Field{K: "value", V: v.Bool()})
}
