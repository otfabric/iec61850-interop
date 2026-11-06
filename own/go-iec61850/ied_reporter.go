// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	iec61850 "github.com/otfabric/go-iec61850"
	mms "github.com/otfabric/go-mms"
)

const (
	reporterRCB     = "urcb01"
	reporterRptID   = "interop_urcb01"
	reportTimeout   = 10 * time.Second
	reporterTimeout = 60 * time.Second
)

// rcbItem finds the item ID of the fixture's URCB: LLN0$RP$urcb01 or its
// first instance (LLN0$RP$urcb0101, as libiec61850 names instances).
func rcbItem(items []string) (string, bool) {
	want := "LLN0$RP$" + reporterRCB
	for _, it := range items {
		if it == want {
			return it, true
		}
	}
	for _, it := range items {
		if strings.HasPrefix(it, want) {
			return it, true
		}
	}
	return "", false
}

// runIEDReporter is otfabric-ied-reporter: get-rcb urcb01, enable-rcb,
// write GGIO1.SPS1.stVal[ST] = !initial, receive-report (10 s), disable-rcb,
// conclude, on go-iec61850's report client. The sequence always completes
// (exit 0); each line's ok is the result.
func runIEDReporter(args []string) int {
	cf := newClientFlags("otfabric-ied-reporter")
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
	conclude("ied-reporter", c)
	return exitOK
}

func reportSequence(ctx context.Context, c *iec61850.Client, initial bool) {
	ld, _, err := ldOf(ctx, c)
	if err != nil {
		logf("ied-reporter: server directory: %v (using %s)", err, ld)
	}
	rcbRef := ld + "/LLN0.RP." + reporterRCB

	// 1. get-rcb
	items, err := c.ListReports(ctx, ld)
	if err != nil {
		emit(opError("get-rcb", rcbRef, err))
		return
	}
	item, ok := rcbItem(items)
	if !ok {
		emit(opError("get-rcb", rcbRef, fmt.Errorf("object-does-not-exist")))
		return
	}
	rcb, err := c.GetReportControlBlock(ctx, ld, item)
	if err != nil {
		emit(opError("get-rcb", rcbRef, err))
		return
	}
	emit(opLine("get-rcb", rcbRef, true, map[string]any{"rptID": rcb.RptID, "rptEna": rcb.RptEna}))

	// 2. enable-rcb, subscribed first so that no report is missed.
	rptID := rcb.RptID
	if rptID == "" {
		rptID = reporterRptID
	}
	sub, err := c.SubscribeReport(ctx, rptID, iec61850.SubscribeReportOptions{})
	if err != nil {
		emit(opError("enable-rcb", rcbRef, err))
		return
	}
	defer func() { _ = sub.Close() }()
	if err := c.SetReportControlBlock(ctx, ld, item, iec61850.RCBUpdate{Fields: iec61850.RCBFieldRptEna, RptEna: true}); err != nil {
		emit(opError("enable-rcb", rcbRef, err))
		return
	}
	emit(opLine("enable-rcb", rcbRef, true, nil))

	// 3. write the data change.
	target := ld + "/GGIO1.SPS1.stVal[ST]"
	if ref, err := iec61850.ParseRef(target); err != nil {
		emit(opError("write", target, err))
	} else if err := c.Write(ctx, ref, mms.NewBoolean(!initial)); err != nil {
		emit(opError("write", target, err))
	} else {
		emit(opLine("write", target, true, nil))
	}

	// 4. receive-report
	wait, stop := context.WithTimeout(ctx, reportTimeout)
	select {
	case r, open := <-sub.Reports():
		if !open {
			emit(opError("receive-report", "", fmt.Errorf("subscription closed")))
		} else {
			emit(reportLine(r))
		}
	case <-wait.Done():
		emit(opError("receive-report", "", fmt.Errorf("timeout-waiting-for-dchg-report")))
	}
	stop()

	// 5. disable-rcb
	if err := c.SetReportControlBlock(ctx, ld, item, iec61850.RCBUpdate{Fields: iec61850.RCBFieldRptEna, RptEna: false}); err != nil {
		emit(opError("disable-rcb", rcbRef, err))
	} else {
		emit(opLine("disable-rcb", rcbRef, true, nil))
	}
}

// reportLine is the receive-report line: the inclusion bitmap, and the
// value and reason of each included member.
func reportLine(r *iec61850.ReportIndication) map[string]any {
	values := make([]any, 0, len(r.Values))
	for _, v := range r.Values {
		if v == nil {
			values = append(values, nil)
			continue
		}
		p, err := plain(v)
		if err != nil {
			p = nil
		}
		values = append(values, p)
	}
	reasons := make([]string, 0, len(r.ReasonCodes))
	for _, rc := range r.ReasonCodes {
		reasons = append(reasons, reasonName(rc))
	}
	inclusion := r.Inclusion
	if inclusion == nil {
		inclusion = []bool{}
	}
	return opLine("receive-report", "", true, map[string]any{
		"rptID": r.RptID, "seqNum": r.SeqNum, "inclusion": inclusion, "values": values, "reasons": reasons,
	})
}

// reasonName is the contract's name of a reason for inclusion; a member
// included for several reasons gets the first of the contract's order.
func reasonName(rc iec61850.ReasonCode) string {
	for _, r := range []struct {
		code iec61850.ReasonCode
		name string
	}{
		{iec61850.ReasonDataChanged, "data-change"},
		{iec61850.ReasonQualityChanged, "quality-change"},
		{iec61850.ReasonDataUpdate, "data-update"},
		{iec61850.ReasonIntegrity, "integrity"},
		{iec61850.ReasonGI, "gi"},
	} {
		if rc&r.code != 0 {
			return r.name
		}
	}
	return "unknown"
}
