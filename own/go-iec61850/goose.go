// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/otfabric/go-iec61850/goose"
	"github.com/otfabric/go-iec61850/rsession"
	mms "github.com/otfabric/go-mms"
)

const gooseFixture = "goose-v1"

// The layer-2 commands read stdin and open interfaces through these, so
// that the tests can replace them.
var (
	stdin           io.Reader = os.Stdin
	openGOOSE                 = goose.Open
	notifyInterrupt           = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
)

// ---------------------------------------------------------------------------
// Typed value JSON (docs/goose.md#value-json), shared with sv.go
// ---------------------------------------------------------------------------

// typedValue is {"type":T,"value":V}.
type typedValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// parseTypedList parses a JSON array of typed values.
func parseTypedList(raw []byte) ([]*mms.Value, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("values is not a JSON array")
	}
	out := make([]*mms.Value, 0, len(list))
	for i, e := range list {
		v, err := parseTyped(e)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// parseTyped converts one typed value to an MMS value. go-mms has one
// floating-point type: float32 and float64 both become a Float, which the
// GOOSE encoder of go-iec61850 sends in the shortest exact width.
func parseTyped(raw json.RawMessage) (*mms.Value, error) {
	var tv typedValue
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&tv); err != nil || tv.Type == "" || tv.Value == nil {
		return nil, errors.New(`value is not {"type":..,"value":..}`)
	}
	str := func() (string, error) {
		var s string
		if err := json.Unmarshal(tv.Value, &s); err != nil {
			return "", fmt.Errorf("%s: not a JSON string", tv.Type)
		}
		return s, nil
	}
	num := func() (json.Number, error) {
		d := json.NewDecoder(strings.NewReader(string(tv.Value)))
		d.UseNumber()
		var n json.Number
		if err := d.Decode(&n); err != nil {
			return "", fmt.Errorf("%s: not a JSON number", tv.Type)
		}
		return n, nil
	}
	switch tv.Type {
	case "boolean":
		var b bool
		if err := json.Unmarshal(tv.Value, &b); err != nil {
			return nil, errors.New("boolean: not a JSON boolean")
		}
		return mms.NewBoolean(b), nil
	case "integer":
		n, err := num()
		if err != nil {
			return nil, err
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return nil, errors.New("integer: not a JSON integer")
		}
		return mms.NewInteger(i), nil
	case "unsigned":
		n, err := num()
		if err != nil {
			return nil, err
		}
		u, err := strconv.ParseUint(n.String(), 10, 32)
		if err != nil {
			return nil, errors.New("unsigned: not a 32-bit unsigned integer")
		}
		return mms.NewUnsigned(u), nil
	case "float32", "float64":
		n, err := num()
		if err != nil {
			return nil, err
		}
		f, err := n.Float64()
		if err != nil {
			return nil, fmt.Errorf("%s: %v", tv.Type, err)
		}
		if tv.Type == "float32" {
			f = float64(float32(f))
		}
		return mms.NewFloat(f), nil
	case "visible-string":
		s, err := str()
		if err != nil {
			return nil, err
		}
		return mms.NewVisibleString(s), nil
	case "mms-string":
		s, err := str()
		if err != nil {
			return nil, err
		}
		return mms.NewMmsString(s), nil
	case "octet-string":
		s, err := str()
		if err != nil {
			return nil, err
		}
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, errors.New("octet-string: not hex")
		}
		return mms.NewOctetString(b), nil
	case "bit-string":
		s, err := str()
		if err != nil {
			return nil, err
		}
		bits, n, ok := parseBits(s)
		if !ok {
			return nil, errors.New("bit-string: not binary digits")
		}
		return mms.NewBitStringWithLength(bits, n), nil
	case "utc-time", "binary-time":
		s, err := str()
		if err != nil {
			return nil, err
		}
		t, err := parseTime(s)
		if err != nil {
			return nil, err
		}
		if tv.Type == "utc-time" {
			return mms.NewUTCTime(t), nil
		}
		return mms.NewBinaryTime(t.UnixMilli()), nil
	case "array", "structure":
		items, err := parseTypedList(tv.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tv.Type, err)
		}
		if tv.Type == "array" {
			return mms.NewArray(items), nil
		}
		return mms.NewStructure(items), nil
	}
	return nil, fmt.Errorf("unsupported type %q", tv.Type)
}

// parseBits parses MSB-first binary digits.
func parseBits(s string) ([]byte, int, bool) {
	bits := make([]byte, (len(s)+7)/8)
	for i, c := range s {
		switch c {
		case '1':
			bits[i/8] |= 0x80 >> (i % 8)
		case '0':
		default:
			return nil, 0, false
		}
	}
	return bits, len(s), true
}

// formatBits formats the first n bits MSB-first.
func formatBits(bits []byte, n int) string {
	var b strings.Builder
	for i := 0; i < n && i/8 < len(bits); i++ {
		if bits[i/8]&(0x80>>(i%8)) != 0 {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

// parseTime parses "YYYY-MM-DDThh:mm:ss[.f]Z", rounded to the millisecond.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || !strings.HasSuffix(s, "Z") {
		return time.Time{}, errors.New("time: not YYYY-MM-DDThh:mm:ss[.fff]Z")
	}
	return t.UTC().Round(time.Millisecond), nil
}

// formatMs formats a time as YYYY-MM-DDThh:mm:ss.mmmZ, rounded.
func formatMs(t time.Time) string {
	return t.UTC().Round(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

// floatJSON is a float as a JSON number in the shortest form that reads
// back as the same value of its width; NaN and infinities are null.
func floatJSON(f float64, bits int) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return json.Number(strconv.FormatFloat(f, 'g', -1, bits))
}

// toTyped converts an MMS value to typed JSON. floatType names the width of
// a Float when the caller knows it ("float32", "float64"); otherwise a Float
// that is exactly a float32 is reported as float32 (go-mms keeps no width).
func toTyped(v *mms.Value, floatType string) map[string]any {
	tv := func(t string, x any) map[string]any { return map[string]any{"type": t, "value": x} }
	if v == nil {
		return tv("unknown", nil)
	}
	switch v.Type() {
	case mms.ValueTypeBoolean:
		b, _ := v.Bool()
		return tv("boolean", b)
	case mms.ValueTypeInteger:
		i, _ := v.Int64()
		return tv("integer", i)
	case mms.ValueTypeUnsigned:
		u, _ := v.Uint64()
		return tv("unsigned", u)
	case mms.ValueTypeFloat:
		f, _ := v.Float64()
		t := floatType
		if t == "" {
			t = "float64"
			if float64(float32(f)) == f || math.IsNaN(f) {
				t = "float32"
			}
		}
		if t == "float32" {
			return tv(t, floatJSON(float64(float32(f)), 32))
		}
		return tv(t, floatJSON(f, 64))
	case mms.ValueTypeVisibleString:
		s, _ := v.VisibleString()
		return tv("visible-string", s)
	case mms.ValueTypeMmsString:
		s, _ := v.MmsString()
		return tv("mms-string", s)
	case mms.ValueTypeOctetString:
		b, _ := v.OctetString()
		return tv("octet-string", hex.EncodeToString(b))
	case mms.ValueTypeBitString:
		b, _ := v.BitString()
		n, _ := v.BitStringLength()
		return tv("bit-string", formatBits(b, n))
	case mms.ValueTypeBooleanArray:
		b, n, _ := v.BooleanArray()
		return tv("bit-string", formatBits(b, n))
	case mms.ValueTypeUTCTime:
		t, _ := v.UTCTime()
		return tv("utc-time", formatMs(t))
	case mms.ValueTypeBinaryTime:
		ms, _ := v.BinaryTime()
		return tv("binary-time", formatMs(time.UnixMilli(ms)))
	case mms.ValueTypeArray, mms.ValueTypeStructure:
		var elems []*mms.Value
		t := "structure"
		if v.Type() == mms.ValueTypeArray {
			elems, _ = v.ArrayElements()
			t = "array"
		} else {
			elems, _ = v.Structure()
		}
		items := make([]map[string]any, len(elems))
		for i, e := range elems {
			items[i] = toTyped(e, "")
		}
		return tv(t, items)
	}
	return tv("unknown", v.String())
}

func toTypedList(vs []*mms.Value) []map[string]any {
	out := make([]map[string]any, len(vs))
	for i, v := range vs {
		out[i] = toTyped(v, "")
	}
	return out
}

// ---------------------------------------------------------------------------
// Shared flag helpers of the layer-2 commands
// ---------------------------------------------------------------------------

// parseAppID parses "0x1001" or a decimal (strtoul base 0) 16-bit APPID.
func parseAppID(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 0, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid APPID %q", s)
	}
	return uint16(n), nil
}

func appIDString(id uint16) string { return fmt.Sprintf("0x%04x", id) }

// readCommands reads JSON Lines from stdin and hands each object to fn.
// EOF is not a stop: the publisher keeps going until a signal.
func readCommands(fn func(cmd string, line map[string]json.RawMessage)) {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]json.RawMessage
		var cmd string
		if err := json.Unmarshal([]byte(line), &obj); err == nil {
			_ = json.Unmarshal(obj["cmd"], &cmd)
		}
		if cmd == "" {
			emit(map[string]any{"event": "error", "error": "stdin line is not {\"cmd\":...}: " + truncate(line, 120)})
			continue
		}
		fn(cmd, obj)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------
// otfabric-goose-publisher
// ---------------------------------------------------------------------------

// publishedTransport reports every frame the publisher puts on the wire.
type publishedTransport struct {
	goose.Transport
}

func (t publishedTransport) Send(frame []byte) error {
	if err := t.Transport.Send(frame); err != nil {
		emit(map[string]any{"event": "error", "error": "send: " + err.Error()})
		return err
	}
	if f, err := goose.Decode(frame); err == nil {
		emit(map[string]any{"event": "published", "stNum": f.PDU.StNum, "sqNum": f.PDU.SqNum, "timeAllowedToLive": f.PDU.TimeAllowedToLive})
	}
	return nil
}

// runGOOSEPublisher is otfabric-goose-publisher on go-iec61850's
// goose.Publisher: state 1 from --values, a new state per
// {"cmd":"set"}, the retransmission schedule of the library (MinTime,
// doubling, capped at MaxTime).
func runGOOSEPublisher(args []string) int {
	fs := flag.NewFlagSet("otfabric-goose-publisher", flag.ContinueOnError)
	fs.SetOutput(stderr)
	iface := fs.String("iface", "eth0", "network interface")
	gocbRef := fs.String("gocb-ref", "", "GoCB reference")
	goID := fs.String("go-id", "", "goID (default: the gocbRef)")
	dataset := fs.String("dataset", "", "dataset reference")
	appID := fs.String("app-id", "0", "APPID, 0xNNNN or decimal")
	dst := fs.String("dst", "", "destination MAC")
	vlanID := fs.Uint("vlan-id", 0, "802.1Q VLAN ID")
	vlanPrio := fs.Uint("vlan-prio", 4, "802.1Q priority")
	confRev := fs.Uint("conf-rev", 1, "confRev")
	minTime := fs.Uint("min-time", 10, "first retransmission, ms")
	maxTime := fs.Uint("max-time", 1000, "heartbeat, ms")
	test := fs.Bool("test", false, "goosePdu test field [7]")
	simulation := fs.Bool("simulation", false, "Edition 2 simulation (S) bit")
	ndsCom := fs.Bool("nds-com", false, "needs commissioning")
	values := fs.String("values", "", "JSON array of typed values")
	uf := addUDPFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("goose-publisher: unexpected argument %q", fs.Arg(0))
	}
	udp, err := uf.udp()
	if err != nil {
		return argError("goose-publisher: %v", err)
	}
	if udp && *dst == "" {
		*dst = "01:0c:cd:01:00:00" // not sent: an SPDU has no Ethernet addresses
	}
	if *gocbRef == "" {
		return argError("goose-publisher: --gocb-ref is required")
	}
	id, err := parseAppID(*appID)
	if err != nil {
		return argError("goose-publisher: --app-id: %v", err)
	}
	dstMAC, err := goose.ParseMAC(*dst)
	if err != nil {
		return argError("goose-publisher: --dst: %v", err)
	}
	if *vlanID > 0x0fff || *vlanPrio > 7 || *confRev > math.MaxUint32 {
		return argError("goose-publisher: --vlan-id, --vlan-prio or --conf-rev out of range")
	}
	vals, err := parseTypedList([]byte(*values))
	if err != nil {
		return argError("goose-publisher: --values: %v", err)
	}
	var t goose.Transport
	label := *iface
	if udp {
		t, label, err = uf.openSender(rsession.SIGOOSE)
		if err != nil {
			return argError("goose-publisher: %v", err)
		}
	} else if t, err = openGOOSE(*iface, goose.TransportOptions{}); err != nil {
		return argError("goose-publisher: open %s: %v", *iface, err)
	}
	defer t.Close()
	p, err := goose.NewPublisher(publishedTransport{t}, goose.PublisherConfig{
		Dst:        dstMAC,
		VLAN:       &goose.VLAN{Priority: uint8(*vlanPrio), ID: uint16(*vlanID)},
		AppID:      id,
		GocbRef:    *gocbRef,
		DatSet:     *dataset,
		GoID:       *goID,
		ConfRev:    uint32(*confRev),
		MinTime:    time.Duration(*minTime) * time.Millisecond,
		MaxTime:    time.Duration(*maxTime) * time.Millisecond,
		Test:       *test,
		Simulation: *simulation,
		NdsCom:     *ndsCom,
		Values:     vals,
	})
	if err != nil {
		return argError("goose-publisher: %v", err)
	}
	defer p.Close()

	ctx, stop := notifyInterrupt(context.Background())
	defer stop()
	emit(map[string]any{
		"event": "ready", "address": label, "fixture": gooseFixture, "adapter": adapterName, "version": adapterVersion,
		"role": "publisher", "iface": label, "src": t.HardwareAddr().String(), "gocbRef": *gocbRef,
	})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go readCommands(func(cmd string, line map[string]json.RawMessage) {
		switch cmd {
		case "set":
			vs, err := parseTypedList(line["values"])
			if err == nil {
				err = p.Publish(vs)
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
	if err := p.Run(ctx); err != nil {
		logf("goose-publisher: %v", err)
		return exitArgs
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// otfabric-goose-subscriber
// ---------------------------------------------------------------------------

// gooseReporter turns the events of the library's subscriptions into the
// contract's goose and supervision events.
//
// Two subscriptions share one goose.Subscriber: one that drops simulated
// frames and one that accepts them (LPHD.Sim false and true), so the
// command reports every frame; the second one ignores the non-simulated
// frames the first one reports. A frame with ndsCom set is reported too:
// the library marks it invalid (needs commissioning), the contract
// reports what was received.
type gooseReporter struct {
	mu       sync.Mutex
	last     map[string]int  // gocbRef -> the subscription that reported its last frame
	timedOut map[string]bool // gocbRef -> a timeout was reported, no frame since
	udp      bool            // R-GOOSE: zero addresses, and from
}

func (r *gooseReporter) handle(sub int, simulated bool) func(goose.Event) {
	return func(ev goose.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		gocb := ev.State.Stream.GocbRef
		switch ev.Kind {
		case goose.EventMessage, goose.EventInvalid:
			f := ev.Frame
			if f == nil || f.Simulation != simulated {
				return
			}
			if r.timedOut[f.PDU.GocbRef] {
				emit(map[string]any{"event": "supervision", "gocbRef": f.PDU.GocbRef, "state": "ok"})
				delete(r.timedOut, f.PDU.GocbRef)
			}
			r.last[f.PDU.GocbRef] = sub
			ev := gooseEvent(f)
			if r.udp {
				ev["dst"], ev["src"], ev["from"] = zeroMAC, zeroMAC, senderOf(f.Src)
			}
			emit(ev)
		case goose.EventTimeout:
			if last, ok := r.last[gocb]; !ok || last != sub || r.timedOut[gocb] {
				return
			}
			r.timedOut[gocb] = true
			emit(map[string]any{"event": "supervision", "gocbRef": gocb, "state": "timeout"})
		}
	}
}

func gooseEvent(f *goose.Frame) map[string]any {
	p := &f.PDU
	return map[string]any{
		"event": "goose", "gocbRef": p.GocbRef, "datSet": p.DatSet, "goID": p.GoID,
		"appID": appIDString(f.AppID), "dst": f.Dst.String(), "src": f.Src.String(),
		"stNum": p.StNum, "sqNum": p.SqNum, "timeAllowedToLive": p.TimeAllowedToLive, "confRev": p.ConfRev,
		"test": p.Test, "simulation": f.Simulation, "ndsCom": p.NdsCom, "numDatSetEntries": p.NumDatSetEntries,
		"t": p.T.UTC().Format(time.RFC3339Nano), "values": toTypedList(p.AllData),
	}
}

// runGOOSESubscriber is otfabric-goose-subscriber on go-iec61850's
// goose.Subscriber, with its filters and its timeAllowedToLive
// supervision.
func runGOOSESubscriber(args []string) int {
	fs := flag.NewFlagSet("otfabric-goose-subscriber", flag.ContinueOnError)
	fs.SetOutput(stderr)
	iface := fs.String("iface", "eth0", "network interface")
	appID := fs.String("app-id", "", "APPID filter")
	gocbRef := fs.String("gocb-ref", "", "gocbRef filter")
	dst := fs.String("dst", "", "destination MAC filter")
	uf := addUDPFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("goose-subscriber: unexpected argument %q", fs.Arg(0))
	}
	udp, err := uf.udp()
	if err != nil {
		return argError("goose-subscriber: %v", err)
	}
	var base goose.SubscriptionConfig
	base.GocbRef = *gocbRef
	if *appID != "" {
		id, err := parseAppID(*appID)
		if err != nil {
			return argError("goose-subscriber: --app-id: %v", err)
		}
		base.AppIDs = []uint16{id}
	}
	if *dst != "" {
		m, err := goose.ParseMAC(*dst)
		if err != nil {
			return argError("goose-subscriber: --dst: %v", err)
		}
		if !udp { // accepted and not used: an SPDU has no Ethernet addresses
			base.Dst = m
		}
	}
	var t goose.Transport
	label := *iface
	if udp {
		t, label, err = uf.openReceiver(rsession.SIGOOSE)
		if err != nil {
			return argError("goose-subscriber: %v", err)
		}
	} else if t, err = openGOOSE(*iface, goose.TransportOptions{}); err != nil {
		return argError("goose-subscriber: open %s: %v", *iface, err)
	}
	defer t.Close()
	s, err := goose.NewSubscriber(t, goose.SubscriberConfig{})
	if err != nil {
		return argError("goose-subscriber: %v", err)
	}
	defer s.Close()
	r := &gooseReporter{last: map[string]int{}, timedOut: map[string]bool{}, udp: udp}
	for i, sim := range []bool{false, true} {
		cfg := base
		cfg.AcceptSimulation = sim
		cfg.Handler = r.handle(i, sim)
		if _, err := s.Subscribe(cfg); err != nil {
			return argError("goose-subscriber: %v", err)
		}
	}
	ctx, stop := notifyInterrupt(context.Background())
	defer stop()
	ready := map[string]any{
		"event": "ready", "address": label, "fixture": gooseFixture, "adapter": adapterName, "version": adapterVersion,
		"role": "subscriber", "iface": label, "src": t.HardwareAddr().String(),
	}
	if *gocbRef != "" {
		ready["gocbRef"] = *gocbRef
	}
	emit(ready)
	if err := s.Run(ctx); err != nil {
		logf("goose-subscriber: %v", err)
		return exitArgs
	}
	return exitOK
}
