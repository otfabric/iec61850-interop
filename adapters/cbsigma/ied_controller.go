// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"strings"

	iec61850 "github.com/cB-Sigma/iec61850"
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

// runIEDController is cbsigma-ied-controller: read ctlModel, then
//
//	SPCSO1 (direct-normal): operate
//	SPCSO2 (sbo-normal):    select, operate
//	SPCSO3 (sbo-enhanced):  select-with-value, operate, CommandTermination
//
// then read stVal back. cB-Sigma's Client.Operate runs the whole service
// sequence of the control model (IEC 61850-7-2 clause 20) in one call: the
// select line reports its select step (a ControlError of step "select"
// fails it), the operate line the rest. The sequence always completes
// (exit 0); each line's ok is the result.
func runIEDController(args []string) int {
	cf := newClientFlags("cbsigma-ied-controller")
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
	controlSequence(ctx, c, ld+"/GGIO1."+*doName, ctlval)
	conclude(c)
	return exitOK
}

func controlSequence(ctx context.Context, c *iec61850.Client, ctlRef string, ctlval bool) {
	target := ctlRef + ".ctlModel[CF]"
	do, err := iec61850.ParseRef(ctlRef, "")
	if err != nil {
		emit(opError("read-ctlmodel", target, err))
		return
	}
	model, err := c.ControlModel(ctx, do)
	if err != nil {
		emit(opError("read-ctlmodel", target, err))
		return
	}
	emit(opLine("read-ctlmodel", target, true, map[string]any{"value": model}))

	selectOp := ""
	switch model {
	case iec61850.SBONormal:
		selectOp = "select"
	case iec61850.SBOEnhanced:
		selectOp = "select-with-value"
	}
	// No interlock or synchrocheck: the fixture's SPCs have no process
	// behind them to check.
	err = c.Operate(ctx, do, ctlval, iec61850.ControlOptions{})
	var ce *iec61850.ControlError
	switch {
	case err != nil && errors.As(err, &ce) && ce.Step == "select" && selectOp != "":
		emit(opError(selectOp, ctlRef, err))
	case err != nil:
		if selectOp != "" {
			emit(opLine(selectOp, ctlRef, true, nil))
		}
		emit(opError("operate", ctlRef, err))
	default:
		if selectOp != "" {
			emit(opLine(selectOp, ctlRef, true, nil))
		}
		emit(opLine("operate", ctlRef, true, map[string]any{"ctlval": ctlval}))
	}

	target = ctlRef + ".stVal[ST]"
	v, err := readRef(ctx, c, target)
	if err != nil {
		emit(opError("read-stval", target, err))
		return
	}
	b, err := plain(v)
	if _, isBool := b.(bool); err != nil || !isBool {
		emit(opError("read-stval", target, errors.New("stVal is not a boolean")))
		return
	}
	emit(opLine("read-stval", target, true, map[string]any{"value": b}))
}
