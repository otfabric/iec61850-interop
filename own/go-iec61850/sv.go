// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/otfabric/go-iec61850/sv"
)

const svFixture = "sv-v1"

var openSV = sv.Open

// smpMods maps the contract's names of smpMod to go-iec61850's.
var smpMods = map[string]sv.SmpMod{
	"samples-per-period": sv.SmpPerPeriod,
	"samples-per-second": sv.SmpPerSec,
	"seconds-per-sample": sv.SecPerSmp,
}

func smpModName(m sv.SmpMod) string {
	for k, v := range smpMods {
		if v == m {
			return k
		}
	}
	return m.String()
}

// kindOfType is the 9-2 sample encoding of a typed-value type (docs/sv.md,
// generic profile). A bit-string is a quality.
func kindOfType(t string) (sv.Kind, bool) {
	switch t {
	case "integer":
		return sv.KindInt32, true
	case "unsigned":
		return sv.KindUint32, true
	case "float32":
		return sv.KindFloat32, true
	case "float64":
		return sv.KindFloat64, true
	case "boolean":
		return sv.KindBoolean, true
	case "bit-string":
		return sv.KindQuality, true
	case "utc-time":
		return sv.KindTimestamp, true
	}
	return sv.KindInvalid, false
}

// typesOf are the typed-value types of a JSON array of typed values.
func typesOf(raw []byte) ([]string, error) {
	var list []typedValue
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("values is not a JSON array of typed values")
	}
	out := make([]string, len(list))
	for i, tv := range list {
		out[i] = tv.Type
	}
	return out, nil
}

// layoutOf is the sample layout of a list of types.
func layoutOf(types []string) (sv.Layout, error) {
	l := make(sv.Layout, len(types))
	for i, t := range types {
		k, ok := kindOfType(t)
		if !ok {
			return nil, fmt.Errorf("value %d: type %q cannot be sampled", i, t)
		}
		l[i] = k
	}
	return l, nil
}

// ---------------------------------------------------------------------------
// otfabric-sv-publisher
// ---------------------------------------------------------------------------

// svPublished reports the frames the publisher puts on the wire: the
// first one, the first one that carries a set's sample, and those that
// take the sample count past a multiple of --report-every.
type svPublished struct {
	sv.Transport
	reportEvery uint64

	mu      sync.Mutex
	frames  uint64
	samples uint64
	started bool
	pending []byte // the sample of the last set, until a frame carries it
}

func (t *svPublished) Send(frame []byte) error {
	if err := t.Transport.Send(frame); err != nil {
		emit(map[string]any{"event": "error", "error": "send: " + err.Error()})
		return err
	}
	f, err := sv.Decode(frame)
	if err != nil || len(f.PDU.ASDUs) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	before := t.samples
	t.frames++
	t.samples += uint64(len(f.PDU.ASDUs))
	reason := ""
	switch {
	case !t.started:
		t.started, reason = true, "start"
	case t.pending != nil && bytes.Equal(f.PDU.ASDUs[0].Sample, t.pending):
		t.pending, reason = nil, "set"
	case t.reportEvery > 0 && before/t.reportEvery != t.samples/t.reportEvery:
		reason = "periodic"
	}
	if reason != "" {
		last := f.PDU.ASDUs[len(f.PDU.ASDUs)-1].SmpCnt
		emit(map[string]any{"event": "published", "reason": reason, "frames": t.frames, "samples": t.samples, "smpCnt": last})
	}
	return nil
}

func (t *svPublished) set(sample []byte) {
	t.mu.Lock()
	t.pending = bytes.Clone(sample)
	t.mu.Unlock()
}

func (t *svPublished) stopped() {
	t.mu.Lock()
	defer t.mu.Unlock()
	emit(map[string]any{"event": "stopped", "frames": t.frames, "samples": t.samples})
}

// runSVPublisher is otfabric-sv-publisher on go-iec61850's sv.Publisher,
// which paces the frames against absolute deadlines and wraps smpCnt at
// the samples per second (65536 for seconds-per-sample).
func runSVPublisher(args []string) int {
	fs := flag.NewFlagSet("otfabric-sv-publisher", flag.ContinueOnError)
	fs.SetOutput(stderr)
	iface := fs.String("iface", "eth0", "network interface")
	appID := fs.String("app-id", "0x4000", "APPID, 0xNNNN or decimal")
	dst := fs.String("dst", "", "destination MAC")
	vlanID := fs.Uint("vlan-id", 0, "802.1Q VLAN ID")
	vlanPrio := fs.Uint("vlan-prio", 4, "802.1Q priority")
	svID := fs.String("sv-id", "", "svID")
	dataset := fs.String("dataset", "", "datSet reference (generic profile)")
	confRev := fs.Uint("conf-rev", 1, "confRev")
	smpRate := fs.Uint("smp-rate", 80, "smpRate")
	smpMod := fs.String("smp-mod", "samples-per-period", "samples-per-period, samples-per-second or seconds-per-sample")
	noASDU := fs.Uint("no-asdu", 1, "ASDUs per frame")
	smpSynch := fs.Uint("smp-synch", 0, "smpSynch 0, 1 or 2")
	simulation := fs.Bool("simulation", false, "Edition 2 simulation (S) bit")
	profile := fs.String("profile", "9-2LE", "9-2LE or generic")
	frequency := fs.Float64("frequency", 50, "nominal frequency, Hz")
	reportEvery := fs.Uint64("report-every", 1000, "published event every N samples")
	values := fs.String("values", "", "JSON array of typed values")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("sv-publisher: unexpected argument %q", fs.Arg(0))
	}
	id, err := parseAppID(*appID)
	if err != nil {
		return argError("sv-publisher: --app-id: %v", err)
	}
	dstMAC, err := sv.ParseMAC(*dst)
	if err != nil {
		return argError("sv-publisher: --dst: %v", err)
	}
	mod, ok := smpMods[*smpMod]
	if !ok {
		return argError("sv-publisher: --smp-mod %s: samples-per-period, samples-per-second or seconds-per-sample", *smpMod)
	}
	switch {
	case *vlanID > 0x0fff || *vlanPrio > 7:
		return argError("sv-publisher: --vlan-id or --vlan-prio out of range")
	case *confRev > math.MaxUint32, *smpRate == 0 || *smpRate > math.MaxUint16, *noASDU == 0 || *noASDU > math.MaxUint16:
		return argError("sv-publisher: --conf-rev, --smp-rate or --no-asdu out of range")
	case *smpSynch > 2:
		return argError("sv-publisher: --smp-synch must be 0, 1 or 2")
	case !(*frequency > 0) || math.IsInf(*frequency, 0):
		return argError("sv-publisher: --frequency must be positive")
	case *profile != "9-2LE" && *profile != "generic":
		return argError("sv-publisher: --profile must be 9-2LE or generic")
	}
	types, err := typesOf([]byte(*values))
	if err != nil {
		return argError("sv-publisher: --values: %v", err)
	}
	vals, err := parseTypedList([]byte(*values))
	if err != nil {
		return argError("sv-publisher: --values: %v", err)
	}
	generic := *profile == "generic"
	layout := sv.LELayout
	if generic {
		if layout, err = layoutOf(types); err != nil {
			return argError("sv-publisher: --values: %v", err)
		}
	} else if len(vals) != len(sv.LELayout) {
		return argError("sv-publisher: 9-2LE needs %d values (eight integer/bit-string pairs), got %d", len(sv.LELayout), len(vals))
	}
	sample, err := layout.Encode(nil, vals)
	if err != nil {
		return argError("sv-publisher: --values: %v", err)
	}
	cfg := sv.PublisherConfig{
		Dst: dstMAC, VLAN: &sv.VLAN{Priority: uint8(*vlanPrio), ID: uint16(*vlanID)},
		AppID: id, Simulation: *simulation, SvID: *svID, ConfRev: uint32(*confRev),
		SmpRate: uint16(*smpRate), SmpMod: mod, NominalFrequency: *frequency,
		NoASDU: int(*noASDU), SmpSynch: uint8(*smpSynch), Layout: layout, Sample: sample,
	}
	if generic {
		cfg.DatSet, cfg.RefrTm, cfg.SendSmpRate, cfg.SendSmpMod = *dataset, true, true, true
	}
	t, err := openSV(*iface, sv.TransportOptions{})
	if err != nil {
		return argError("sv-publisher: open %s: %v", *iface, err)
	}
	defer t.Close()
	out := &svPublished{Transport: t, reportEvery: *reportEvery}
	p, err := sv.NewPublisher(out, cfg)
	if err != nil {
		return argError("sv-publisher: %v", err)
	}
	defer p.Close()

	ctx, stop := notifyInterrupt(context.Background())
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	emit(map[string]any{
		"event": "ready", "address": *iface, "fixture": svFixture, "adapter": adapterName, "version": adapterVersion,
		"role": "publisher", "iface": *iface, "src": t.HardwareAddr().String(), "svID": *svID,
	})
	go readCommands(func(cmd string, line map[string]json.RawMessage) {
		switch cmd {
		case "set":
			vs, err := parseTypedList(line["values"])
			var b []byte
			if err == nil {
				b, err = layout.Encode(nil, vs)
			}
			if err == nil {
				out.set(b)
				err = p.UpdateSample(b)
			}
			if err != nil {
				emit(map[string]any{"event": "error", "error": "set: " + err.Error()})
			}
		case "stop":
			cancel()
		default:
			emit(map[string]any{"event": "error", "error": "unknown cmd " + strconv.Quote(cmd)})
		}
	})
	err = p.Run(ctx)
	out.stopped()
	if err != nil {
		logf("sv-publisher: %v", err)
		return exitArgs
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// otfabric-sv-subscriber
// ---------------------------------------------------------------------------

// svStream is a stream of the fixture: its member types and smpCnt wrap.
type svStream struct {
	types  []string
	layout sv.Layout
	wrap   int
}

// loadSVFixture reads the streams of sv.json by svID.
func loadSVFixture(path string) (map[string]svStream, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Streams map[string]struct {
			SvID       string `json:"svID"`
			SmpCntWrap int    `json:"smpCntWrap"`
			Members    []struct {
				Type string `json:"type"`
			} `json:"members"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]svStream{}
	for name, s := range doc.Streams {
		types := make([]string, len(s.Members))
		for i, m := range s.Members {
			types[i] = m.Type
		}
		l, err := layoutOf(types)
		if err != nil {
			return nil, fmt.Errorf("%s: stream %s: %w", path, name, err)
		}
		out[s.SvID] = svStream{types: types, layout: l, wrap: s.SmpCntWrap}
	}
	return out, nil
}

// svEvent is the contract's sv event of one ASDU.
func svEvent(smp *sv.Sample, known map[string]svStream) map[string]any {
	f, a := smp.Frame, smp.ASDU
	null := func(ok bool, v any) any {
		if ok {
			return v
		}
		return nil
	}
	ev := map[string]any{
		"event": "sv", "appID": appIDString(f.AppID), "dst": f.Dst.String(), "src": f.Src.String(),
		"svID": a.SvID, "datSet": null(a.DatSet != "", a.DatSet), "smpCnt": a.SmpCnt, "confRev": a.ConfRev,
		"refrTm":   null(a.HasRefrTm, a.RefrTm.UTC().Format(time.RFC3339Nano)),
		"smpSynch": a.SmpSynch, "smpRate": null(a.HasSmpRate, a.SmpRate), "smpMod": null(a.HasSmpMod, smpModName(a.SmpMod)),
		"simulation": f.Simulation, "asduIndex": smp.Index, "noASDU": len(f.PDU.ASDUs), "values": nil,
	}
	st, ok := known[a.SvID]
	if !ok {
		ev["seqData"] = hex.EncodeToString(a.Sample)
		return ev
	}
	vs, err := st.layout.Decode(a.Sample)
	if err != nil {
		ev["seqData"] = hex.EncodeToString(a.Sample)
		return ev
	}
	out := make([]map[string]any, len(vs))
	for i, v := range vs {
		ft := ""
		if t := st.types[i]; t == "float32" || t == "float64" {
			ft = t
		}
		out[i] = toTyped(v, ft)
	}
	ev["values"] = out
	return ev
}

// runSVSubscriber is otfabric-sv-subscriber on go-iec61850's
// sv.Subscriber. The counts of the stats event are the library's per
// stream counters (sv.Stats), summed: received = ASDUs, lost, duplicates
// and out-of-order; the smpCnt wrap of a fixture stream is its
// smpCntWrap, of any other stream the one the library learns.
func runSVSubscriber(args []string) int {
	fs := flag.NewFlagSet("otfabric-sv-subscriber", flag.ContinueOnError)
	fs.SetOutput(stderr)
	iface := fs.String("iface", "eth0", "network interface")
	appID := fs.String("app-id", "", "APPID filter")
	svID := fs.String("sv-id", "", "svID filter")
	dst := fs.String("dst", "", "destination MAC filter")
	every := fs.Uint("sample-every", 1, "report ASDUs whose smpCnt is a multiple of N")
	fixture := fs.String("fixture", "/fixtures/sv/sv.json", "SV fixture")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("sv-subscriber: unexpected argument %q", fs.Arg(0))
	}
	if *every == 0 {
		return argError("sv-subscriber: --sample-every must be positive")
	}
	known, err := loadSVFixture(*fixture)
	if err != nil {
		return argError("sv-subscriber: %v", err)
	}
	var base sv.SubscriptionConfig
	if *appID != "" {
		id, err := parseAppID(*appID)
		if err != nil {
			return argError("sv-subscriber: --app-id: %v", err)
		}
		base.AppIDs = []uint16{id}
	}
	if *dst != "" {
		m, err := sv.ParseMAC(*dst)
		if err != nil {
			return argError("sv-subscriber: --dst: %v", err)
		}
		base.Dst = m
	}
	t, err := openSV(*iface, sv.TransportOptions{})
	if err != nil {
		return argError("sv-subscriber: open %s: %v", *iface, err)
	}
	defer t.Close()
	s, err := sv.NewSubscriber(t, sv.SubscriberConfig{AcceptSimulation: true})
	if err != nil {
		return argError("sv-subscriber: %v", err)
	}
	defer s.Close()

	report := func(smp *sv.Sample) {
		if uint(smp.ASDU.SmpCnt)%*every == 0 {
			emit(svEvent(smp, known))
		}
	}
	// One subscription per fixture stream (its smpCnt wrap), and one for
	// every other svID; each counts only its own streams.
	type counted struct {
		sub   *sv.Subscription
		other bool
	}
	var subs []counted
	add := func(cfg sv.SubscriptionConfig, other bool) error {
		sub, err := s.Subscribe(cfg)
		if err == nil {
			subs = append(subs, counted{sub, other})
		}
		return err
	}
	for id, st := range known {
		if *svID != "" && id != *svID {
			continue
		}
		cfg := base
		cfg.SvID, cfg.SmpCntWrap, cfg.OnASDU = id, st.wrap, report
		if err := add(cfg, false); err != nil {
			return argError("sv-subscriber: %v", err)
		}
	}
	if _, isKnown := known[*svID]; !isKnown || *svID == "" {
		cfg := base
		cfg.SvID = *svID
		cfg.OnASDU = func(smp *sv.Sample) {
			if _, k := known[smp.ASDU.SvID]; !k {
				report(smp)
			}
		}
		if err := add(cfg, true); err != nil {
			return argError("sv-subscriber: %v", err)
		}
	}
	stats := func() map[string]any {
		var recv, lost, dup, ooo uint64
		for _, c := range subs {
			for _, st := range c.sub.Stats() {
				if _, k := known[st.Stream.SvID]; c.other && k {
					continue
				}
				recv += st.ASDUs
				lost += st.Lost
				dup += st.Duplicates
				ooo += st.OutOfOrder
			}
		}
		return map[string]any{"event": "stats", "received": recv, "lost": lost, "duplicates": dup, "outOfOrder": ooo}
	}

	ctx, stop := notifyInterrupt(context.Background())
	defer stop()
	emit(map[string]any{
		"event": "ready", "address": *iface, "fixture": svFixture, "adapter": adapterName, "version": adapterVersion,
		"role": "subscriber", "iface": *iface, "src": t.HardwareAddr().String(),
	})
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				emit(stats())
			}
		}
	}()
	if err := s.Run(ctx); err != nil {
		logf("sv-subscriber: %v", err)
		return exitArgs
	}
	return exitOK
}
