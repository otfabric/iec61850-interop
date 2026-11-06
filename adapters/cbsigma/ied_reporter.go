// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"strings"
	"time"

	iec61850 "github.com/cB-Sigma/iec61850"
	"github.com/cB-Sigma/iec61850/mms"
)

const (
	reporterRCB     = "urcb01"
	reporterRptID   = "interop_urcb01"
	reportTimeout   = 10 * time.Second
	reporterTimeout = 60 * time.Second
)

// rcbOf finds the fixture's URCB among the discovered report control
// blocks ("LLN0.RP.urcb01"): the block itself or its first instance
// ("LLN0.RP.urcb0101", as libiec61850 and iec61850-sim name indexed
// blocks).
func rcbOf(reports []string) (string, bool) {
	want := "LLN0.RP." + reporterRCB
	for _, r := range reports {
		if r == want {
			return r, true
		}
	}
	for _, r := range reports {
		if strings.HasPrefix(r, want) {
			return r, true
		}
	}
	return "", false
}

// runIEDReporter is cbsigma-ied-reporter: get-rcb urcb01, enable-rcb,
// write GGIO1.SPS1.stVal[ST] = !initial, receive-report (10 s), disable-rcb,
// conclude, on cB-Sigma's report client. The sequence always completes
// (exit 0); each line's ok is the result.
func runIEDReporter(args []string) int {
	cf := newClientFlags("cbsigma-ied-reporter")
	initialFlag := cf.fs.String("sps1-initial", "1", "initial GGIO1.SPS1.stVal: 0|1")
	if code := cf.parse(args); code >= 0 {
		return code
	}
	initial, ok := parseBool(*initialFlag)
	if !ok {
		return argError("ied-reporter: invalid --sps1-initial %q", *initialFlag)
	}

	ctx, cancel := context.WithTimeout(context.Background(), reporterTimeout)
	defer cancel()
	c, err := dial(ctx, cf.addr(), nil, nil)
	if err != nil {
		logf("ied-reporter: connect %s: %v", cf.addr(), err)
		return exitConnect
	}
	reportSequence(ctx, c, initial)
	conclude(c)
	return exitOK
}

func reportSequence(ctx context.Context, c *iec61850.Client, initial bool) {
	ld, _, err := ldOf(ctx, c)
	if err != nil {
		logf("ied-reporter: server directory: %v (using %s)", err, ld)
	}
	rcbTarget := ld + "/LLN0.RP." + reporterRCB

	// 1. get-rcb: discovery, then GetURCBValues.
	m, err := c.Discover(ctx, ld)
	if err != nil {
		emit(opError("get-rcb", rcbTarget, err))
		return
	}
	name, ok := rcbOf(m.Reports)
	if !ok {
		emit(opError("get-rcb", rcbTarget, errors.New("object-does-not-exist")))
		return
	}
	rcb, err := iec61850.ParseRef(ld+"/"+strings.ReplaceAll(name, ".", "$"), "")
	if err != nil {
		emit(opError("get-rcb", rcbTarget, err))
		return
	}
	vals, err := c.RCBValues(ctx, rcb)
	if err != nil {
		emit(opError("get-rcb", rcbTarget, err))
		return
	}
	rptID := vals["RptID"].Str
	emit(opLine("get-rcb", rcbTarget, true, map[string]any{"rptID": rptID, "rptEna": vals["RptEna"].Bool}))
	if rptID == "" {
		rptID = reporterRptID
	}
	// The inclusion bitstring has one bit per data set member.
	members := 0
	if ds := vals["DatSet"].Str; ds != "" {
		if list, err := c.DataSetMembers(ctx, ds); err == nil {
			members = len(list)
		}
	}

	// 2. enable-rcb: the library's EnableReport (OptFlds it needs, then
	// RptEna).
	if err := c.EnableReport(ctx, rcb, iec61850.RCBOptions{}); err != nil {
		emit(opError("enable-rcb", rcbTarget, err))
		return
	}
	emit(opLine("enable-rcb", rcbTarget, true, nil))

	// 3. write the data change.
	target := ld + "/GGIO1.SPS1.stVal[ST]"
	if ref, err := parseTarget(target); err != nil {
		emit(opError("write", target, err))
	} else if err := c.MMS().WriteOne(ctx, ref.MMS(), mms.Boolean(!initial)); err != nil {
		emit(opError("write", target, err))
	} else {
		emit(opLine("write", target, true, nil))
	}

	// 4. receive-report: the first report of the block.
	wait, stop := context.WithTimeout(ctx, reportTimeout)
	emit(receive(wait, c, rptID, members))
	stop()

	// 5. disable-rcb
	if err := c.MMS().WriteOne(ctx, rcb.Child("RptEna").MMS(), mms.Boolean(false)); err != nil {
		emit(opError("disable-rcb", rcbTarget, err))
	} else {
		emit(opLine("disable-rcb", rcbTarget, true, nil))
	}
}

// receive waits for the first report with rptID and returns its line.
func receive(ctx context.Context, c *iec61850.Client, rptID string, members int) map[string]any {
	for {
		select {
		case r := <-c.Reports():
			rep, err := iec61850.ParseReport(r)
			if err != nil {
				return opError("receive-report", "", err)
			}
			if rep.RptID != rptID {
				continue
			}
			return reportLine(rep, members)
		case <-ctx.Done():
			return opError("receive-report", "", errors.New("timeout-waiting-for-dchg-report"))
		}
	}
}

// reportLine is the receive-report line: the inclusion bitmap, and the
// value and reason of each included member.
func reportLine(rep iec61850.ParsedReport, members int) map[string]any {
	for _, e := range rep.Entries {
		members = max(members, e.Index+1)
	}
	inclusion := make([]bool, members)
	values := make([]any, 0, len(rep.Entries))
	reasons := make([]string, 0, len(rep.Entries))
	for _, e := range rep.Entries {
		inclusion[e.Index] = true
		p, err := plain(e.Value)
		if err != nil {
			p = nil
		}
		values = append(values, p)
		reasons = append(reasons, reasonName(e.Reasons))
	}
	line := opLine("receive-report", "", true, map[string]any{
		"rptID": rep.RptID, "inclusion": inclusion, "values": values, "reasons": reasons,
	})
	if rep.SeqNum != nil {
		line["seqNum"] = *rep.SeqNum
	}
	return line
}

// reasonName is the contract's name of a reason for inclusion; a member
// included for several reasons gets the first of the contract's order.
func reasonName(reasons []string) string {
	for _, r := range []struct{ lib, name string }{
		{"dchg", "data-change"},
		{"qchg", "quality-change"},
		{"dupd", "data-update"},
		{"integrity", "integrity"},
		{"gi", "gi"},
	} {
		for _, got := range reasons {
			if got == r.lib {
				return r.name
			}
		}
	}
	return "unknown"
}
