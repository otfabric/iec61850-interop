// SPDX-License-Identifier: GPL-3.0-only

// Command dscsystems-ied-reporter is the dscsystems/go-iec61850 URCB
// adapter for mms-interop, the counterpart of libiec61850-ied-reporter.
//
// It connects to --host/--port, enables InteropLD/LLN0.RP.urcb01, writes
// GGIO1.SPS1.stVal[ST] to raise a dchg report, waits for it, emits it,
// then disables the URCB and concludes:
//
//	get-rcb, enable-rcb, write (ST), receive-report (10 s), disable-rcb,
//	conclude
//
// References are fixed to the bare InteropLD, as in the C.
//
// Arguments: --host, --port, --sps1-initial 0|1 (default 1, as the C; the
// value written is its negation).
//
// Exit codes: 0 sequence completed (results in stdout), 1 argument error,
// 2 connection failure.
package main

import (
	"flag"
	"strconv"
	"time"

	"github.com/dscsystems/go-iec61850/client"
	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/model"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/ied"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/jl"
)

const (
	rcbRef  = "InteropLD/LLN0.RP.urcb01"
	sps1Ref = "InteropLD/GGIO1.SPS1.stVal"
	dsSize  = 2 // dsInterop: SPS1.stVal, Mod.stVal
)

func main() {
	jl.HandleMetaFlags()
	initialArg := flag.String("sps1-initial", "1", "initial SPS1.stVal, 0 or 1")
	addr := ied.Flags("ied-reporter")
	n, _ := strconv.Atoi(*initialArg)
	initial := n != 0

	// A connection failure exits 2 with no JSON line, as the C does.
	c, err := ied.Dial(addr)
	if err != nil {
		jl.Fatal(2, "ied-reporter: connect %s: %v", addr, err)
	}
	run(c, initial)
}

func run(c *client.Client, initial bool) {
	// The C closes without a conclude line when get-rcb or enable-rcb fails.
	concluded := false
	defer func() {
		c.Close()
		if concluded {
			ied.OK("conclude", "")
		}
	}()

	// 1. get-rcb
	ctx, cancel := ied.Ctx()
	rcb, err := c.GetRCB(ctx, rcbRef)
	var rptEna bool
	if err == nil {
		// client.RCB has no RptEna; read it alongside, as GetRCBValues does.
		domain, item := model.ObjectReference(rcbRef).ToMMS(model.FCNone)
		var vals []*mms.Value
		if vals, err = c.MMS().Read(ctx, domain, item+"$RptEna"); err == nil && len(vals) > 0 {
			if code, isErr := vals[0].AccessError(); isErr {
				err = code
			} else {
				rptEna = vals[0].Bool()
			}
		}
	}
	cancel()
	if err != nil {
		ied.Fail("get-rcb", rcbRef, ied.ErrString(err))
		return
	}
	ied.OK("get-rcb", rcbRef, ied.Field{K: "rptID", V: rcb.RptID}, ied.Field{K: "rptEna", V: rptEna})

	// 2. enable-rcb. The C writes RptEna alone; leaving the configuration
	// fields zero keeps EnableReporting from writing them back. It still
	// reserves the URCB (Resv=true) first, which the C does not.
	rcb.OptFlds, rcb.TrgOps, rcb.BufTm, rcb.IntgPd = 0, 0, 0, 0
	reports := make(chan *client.Report, 1)
	ctx, cancel = ied.Ctx()
	sub, err := c.EnableReporting(ctx, rcb, func(r *client.Report) {
		select {
		case reports <- r:
		default: // only the first report is emitted
		}
	})
	cancel()
	if err != nil {
		ied.Fail("enable-rcb", rcbRef, ied.ErrString(err))
		return
	}
	ied.OK("enable-rcb", rcbRef)
	concluded = true

	// 3. write: toggle SPS1.stVal to trigger dchg; continue on failure so
	// the URCB is still disabled.
	ctx, cancel = ied.Ctx()
	err = c.Write(ctx, sps1Ref, model.ST, mms.NewBool(!initial))
	cancel()
	if err != nil {
		ied.Fail("write", sps1Ref+"[ST]", ied.ErrString(err))
	} else {
		ied.OK("write", sps1Ref+"[ST]")
	}

	// 4. receive-report
	select {
	case r := <-reports:
		emitReport(r)
	case <-time.After(10 * time.Second):
		ied.Fail("receive-report", "", "timeout-waiting-for-dchg-report")
	}

	// 5. disable-rcb (RptEna=false, then Resv=false).
	ctx, cancel = ied.Ctx()
	err = sub.Disable(ctx)
	cancel()
	if err != nil {
		ied.Fail("disable-rcb", rcbRef, ied.ErrString(err))
	} else {
		ied.OK("disable-rcb", rcbRef)
	}
}

// emitReport prints the report in the C's shape: inclusion over the two
// dataset members, values of the included ones (SPS1.stVal boolean,
// Mod.stVal integer, else null), and one reason per included member.
func emitReport(r *client.Report) {
	inclusion := make([]bool, dsSize)
	values := []any{}
	reasons := []string{}
	for _, e := range r.Entries {
		if e.Index < 0 || e.Index >= dsSize {
			continue
		}
		inclusion[e.Index] = true
	}
	for i := 0; i < dsSize; i++ {
		if !inclusion[i] {
			continue
		}
		var e *client.ReportEntry
		for k := range r.Entries {
			if r.Entries[k].Index == i {
				e = &r.Entries[k]
			}
		}
		var v any
		switch {
		case i == 0 && e.Value != nil && e.Value.Type() == mms.TypeBoolean:
			v = e.Value.Bool()
		case i == 1 && e.Value != nil && e.Value.Type() == mms.TypeInteger:
			v = int32(e.Value.Int64())
		}
		values = append(values, v)
		reasons = append(reasons, reasonString(e.Reason))
	}
	ied.OK("receive-report", "",
		ied.Field{K: "rptID", V: r.RptID},
		ied.Field{K: "seqNum", V: r.SeqNum},
		ied.Field{K: "inclusion", V: inclusion},
		ied.Field{K: "values", V: values},
		ied.Field{K: "reasons", V: reasons},
	)
}

// reasonString names a reason for inclusion as the C's reason_str: a single
// reason by name, anything else "unknown".
func reasonString(rc model.ReasonCode) string {
	switch rc {
	case model.ReasonDataChange:
		return "data-change"
	case model.ReasonQualityChange:
		return "quality-change"
	case model.ReasonDataUpdate:
		return "data-update"
	case model.ReasonIntegrity:
		return "integrity"
	case model.ReasonGI:
		return "gi"
	}
	return "unknown"
}
