// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"

	iec61850 "github.com/otfabric/go-iec61850"
)

// parseBool takes true/false and 1/0, as the reference controllers do.
func parseBool(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	}
	return false, false
}

// runIEDController is otfabric-ied-controller: read ctlModel, then
//
//	SPCSO1 (direct-normal): operate
//	SPCSO2 (sbo-normal):    select, operate
//	SPCSO3 (sbo-enhanced):  select-with-value, operate
//
// then read stVal back, on go-iec61850's control client. The sequence
// always completes (exit 0); each line's ok is the result.
func runIEDController(args []string) int {
	cf := newClientFlags("otfabric-ied-controller")
	ctlvalFlag := cf.fs.String("ctlval", "true", "control value: true|false (or 1|0)")
	doName := cf.fs.String("do", "SPCSO1", "data object: SPCSO1|SPCSO2|SPCSO3")
	if code := cf.parse(args); code >= 0 {
		return code
	}
	ctlval, ok := parseBool(*ctlvalFlag)
	if !ok {
		return argError("ied-controller: invalid --ctlval %q", *ctlvalFlag)
	}
	switch *doName {
	case "SPCSO1", "SPCSO2", "SPCSO3":
	default:
		return argError("ied-controller: unknown --do %s (supported: SPCSO1, SPCSO2, SPCSO3)", *doName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), iedTimeout)
	defer cancel()
	c, err := dial(ctx, cf.addr(), nil, nil)
	if err != nil {
		logf("ied-controller: connect %s: %v", cf.addr(), err)
		return exitConnect
	}
	ld, _, err := ldOf(ctx, c)
	if err != nil {
		logf("ied-controller: server directory: %v (using %s)", err, ld)
	}
	ctlRef := ld + "/GGIO1." + *doName
	controlSequence(ctx, c, ctlRef, *doName, ctlval)
	conclude("ied-controller", c)
	return exitOK
}

func controlSequence(ctx context.Context, c *iec61850.Client, ctlRef, doName string, ctlval bool) {
	ref, err := iec61850.ParseRef(ctlRef)
	if err != nil {
		emit(opError("read-ctlmodel", ctlRef+".ctlModel[CF]", err))
		return
	}
	if m, err := c.ReadCtlModel(ctx, ref); err != nil {
		emit(opError("read-ctlmodel", ctlRef+".ctlModel[CF]", err))
	} else {
		emit(opLine("read-ctlmodel", ctlRef+".ctlModel[CF]", true, map[string]any{"value": int(m)}))
	}

	// One select/operate cycle carries one ctlNum: SelectWithValue and
	// Operate each auto-increment a zero CtlNum, so it is given.
	params := iec61850.OperateParams{CtlVal: iec61850.BoolCtlVal(ctlval), CtlNum: 1}
	operate := true
	switch doName {
	case "SPCSO2":
		if _, err := c.Select(ctx, ref); err != nil {
			emit(opError("select", ctlRef, err))
			return
		}
		emit(opLine("select", ctlRef, true, nil))
	case "SPCSO3":
		if err := c.SelectWithValue(ctx, ref, params); err != nil {
			emit(opError("select-with-value", ctlRef, err))
			operate = false
		} else {
			emit(opLine("select-with-value", ctlRef, true, nil))
		}
	}
	if operate {
		if err := c.Operate(ctx, ref, params); err != nil {
			emit(opError("operate", ctlRef, err))
		} else {
			emit(opLine("operate", ctlRef, true, map[string]any{"ctlval": ctlval}))
		}
	}

	target := ctlRef + ".stVal[ST]"
	st, err := iec61850.ParseRef(target)
	if err != nil {
		emit(opError("read-stval", target, err))
		return
	}
	v, err := c.Read(ctx, st)
	if err != nil {
		emit(opError("read-stval", target, err))
		return
	}
	b, err := v.Bool()
	if err != nil {
		emit(opError("read-stval", target, err))
		return
	}
	emit(opLine("read-stval", target, true, map[string]any{"value": b}))
}
