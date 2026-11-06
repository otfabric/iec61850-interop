// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cB-Sigma/iec61850/goose"
	"github.com/cB-Sigma/iec61850/mms"
)

const gooseFixture = "goose-v1"

// The layer-2 commands read stdin and open interfaces through these, so
// that the tests can replace them.
var (
	stdin           io.Reader = os.Stdin
	openSend                  = goose.OpenSendInterface
	openCapture               = goose.OpenInterface
	notifyInterrupt           = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
)

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
// cbsigma-goose-publisher
// ---------------------------------------------------------------------------

// gooseState is the publisher's state: the frame template, the current
// values and counters, and the retransmission schedule.
type gooseState struct {
	mu       sync.Mutex
	frame    goose.Frame
	min, max time.Duration
	interval time.Duration // time from the last frame to the next
	next     time.Time
}

// send encodes the current message with cB-Sigma's GOOSE encoder, puts it
// on the wire and schedules the next one. Each frame's timeAllowedToLive is
// twice the interval to the next one (IEC 61850-8-1 18.1.2.5). Called with
// mu held.
func (s *gooseState) send(sink goose.Sink, now time.Time) {
	g := s.frame.GOOSE
	g.TAL = 2 * s.interval
	if err := sink.WriteFrame(goose.EncodeGOOSE(s.frame)); err != nil {
		emit(map[string]any{"event": "error", "error": "send: " + err.Error()})
	} else {
		emit(map[string]any{"event": "published", "stNum": g.StNum, "sqNum": g.SqNum, "timeAllowedToLive": g.TAL.Milliseconds()})
	}
	s.next = now.Add(s.interval)
}

// change starts a new state: stNum+1, sqNum 0, t now, the schedule from
// MinTime. Called with mu held.
func (s *gooseState) change(values []mms.Data, now time.Time) {
	g := s.frame.GOOSE
	g.AllData = values
	g.StNum = goose.NextCount(g.StNum)
	g.SqNum = 0
	g.T = now
	s.interval = s.min
}

// retransmit sends the next repetition: sqNum+1, the interval doubled up
// to MaxTime. Called with mu held.
func (s *gooseState) retransmit() {
	g := s.frame.GOOSE
	g.SqNum = goose.NextCount(g.SqNum)
	s.interval = min(2*s.interval, s.max)
}

// runGOOSEPublisher is cbsigma-goose-publisher: frames built by
// cB-Sigma's goose.EncodeGOOSE and sent with goose.OpenSendInterface, on
// the schedule of the contract. The library's own goose.Publisher takes
// members of IEC 61850-7-2 basic types only and sets the PDU test field
// and the S bit together, while the contract publishes any typed value
// (structures among them) and sets --test and --simulation apart: the
// adapter keeps the counters and the schedule and the library encodes.
func runGOOSEPublisher(args []string) int {
	fs := flag.NewFlagSet("cbsigma-goose-publisher", flag.ContinueOnError)
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
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("goose-publisher: unexpected argument %q", fs.Arg(0))
	}
	if *gocbRef == "" {
		return argError("goose-publisher: --gocb-ref is required")
	}
	id, err := parseAppID(*appID)
	if err != nil {
		return argError("goose-publisher: --app-id: %v", err)
	}
	dstMAC, err := goose.ParseMAC(*dst)
	if err != nil || dstMAC == nil {
		return argError("goose-publisher: --dst: invalid MAC %q", *dst)
	}
	if *vlanID > 0x0fff || *vlanPrio > 7 || *confRev > math.MaxUint32 {
		return argError("goose-publisher: --vlan-id, --vlan-prio or --conf-rev out of range")
	}
	if *minTime == 0 || *maxTime < *minTime {
		return argError("goose-publisher: --min-time must be positive and at most --max-time")
	}
	vals, err := parseTypedList([]byte(*values))
	if err != nil {
		return argError("goose-publisher: --values: %v", err)
	}
	if *goID == "" {
		*goID = *gocbRef
	}
	sink, src, err := openSend(*iface)
	if err != nil {
		return argError("goose-publisher: open %s: %v", *iface, err)
	}
	defer func() { _ = sink.Close() }()

	now := time.Now()
	st := &gooseState{
		frame: goose.Frame{
			Dst: dstMAC, Src: src, VLAN: int(*vlanID), Priority: int(*vlanPrio), APPID: id, Simulated: *simulation,
			GOOSE: &goose.GOOSE{
				GoCBRef: *gocbRef, DatSet: *dataset, GoID: *goID, TQuality: 0x0a,
				Simulation: *test, ConfRev: uint32(*confRev), NdsCom: *ndsCom,
			},
		},
		min: time.Duration(*minTime) * time.Millisecond,
		max: time.Duration(*maxTime) * time.Millisecond,
	}

	ctx, stop := notifyInterrupt(context.Background())
	defer stop()
	emit(map[string]any{
		"event": "ready", "address": *iface, "fixture": gooseFixture, "adapter": adapterName, "version": adapterVersion,
		"role": "publisher", "iface": *iface, "src": net.HardwareAddr(src).String(), "gocbRef": *gocbRef,
	})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// State 1: stNum 1, sqNum 0 (the counter after 0 is 1).
	st.mu.Lock()
	st.change(vals, now)
	st.send(sink, now)
	st.mu.Unlock()

	wake := make(chan struct{}, 1)
	go readCommands(func(cmd string, line map[string]json.RawMessage) {
		switch cmd {
		case "set":
			vs, err := parseTypedList(line["values"])
			if err != nil {
				emit(map[string]any{"event": "error", "error": "set: " + err.Error()})
				return
			}
			now := time.Now()
			st.mu.Lock()
			st.change(vs, now)
			st.send(sink, now)
			st.mu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
		case "stop":
			cancel()
		default:
			emit(map[string]any{"event": "error", "error": "unknown cmd " + strconv.Quote(cmd)})
		}
	})
	for {
		st.mu.Lock()
		wait := time.Until(st.next)
		st.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return exitOK
		case <-wake:
			timer.Stop()
		case <-timer.C:
			now := time.Now()
			st.mu.Lock()
			if !now.Before(st.next) {
				st.retransmit()
				st.send(sink, now)
			}
			st.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// cbsigma-goose-subscriber
// ---------------------------------------------------------------------------

// received is one frame read from the interface.
type received struct {
	frame *goose.Frame
	at    time.Time
}

// runGOOSESubscriber is cbsigma-goose-subscriber: cB-Sigma's raw capture
// (goose.OpenInterface) and decoder (goose.Decode), with the contract's
// filters and timeAllowedToLive supervision per gocbRef.
func runGOOSESubscriber(args []string) int {
	fs := flag.NewFlagSet("cbsigma-goose-subscriber", flag.ContinueOnError)
	fs.SetOutput(stderr)
	iface := fs.String("iface", "eth0", "network interface")
	appIDFlag := fs.String("app-id", "", "APPID filter")
	gocbRef := fs.String("gocb-ref", "", "gocbRef filter")
	dstFlag := fs.String("dst", "", "destination MAC filter")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("goose-subscriber: unexpected argument %q", fs.Arg(0))
	}
	var appID *uint16
	if *appIDFlag != "" {
		id, err := parseAppID(*appIDFlag)
		if err != nil {
			return argError("goose-subscriber: --app-id: %v", err)
		}
		appID = &id
	}
	var dst net.HardwareAddr
	var macs []net.HardwareAddr
	if *dstFlag != "" {
		m, err := goose.ParseMAC(*dstFlag)
		if err != nil || m == nil {
			return argError("goose-subscriber: --dst: invalid MAC %q", *dstFlag)
		}
		dst, macs = m, []net.HardwareAddr{m}
	}
	ifi, err := net.InterfaceByName(*iface)
	if err != nil {
		return argError("goose-subscriber: %v", err)
	}
	src, err := openCapture(*iface, macs)
	if err != nil {
		return argError("goose-subscriber: open %s: %v", *iface, err)
	}
	defer func() { _ = src.Close() }()

	ctx, stop := notifyInterrupt(context.Background())
	defer stop()
	ready := map[string]any{
		"event": "ready", "address": *iface, "fixture": gooseFixture, "adapter": adapterName, "version": adapterVersion,
		"role": "subscriber", "iface": *iface, "src": ifi.HardwareAddr.String(),
	}
	if *gocbRef != "" {
		ready["gocbRef"] = *gocbRef
	}
	emit(ready)

	frames := make(chan received, 256)
	go func() {
		buf := make([]byte, 9000)
		for {
			n, at, err := src.ReadFrame(buf)
			if err != nil {
				close(frames)
				return
			}
			f, err := goose.Decode(buf[:n])
			if err != nil || f.GOOSE == nil {
				continue
			}
			frames <- received{f, at}
		}
	}()

	accept := func(f *goose.Frame) bool {
		if appID != nil && f.APPID != *appID {
			return false
		}
		if *gocbRef != "" && f.GOOSE.GoCBRef != *gocbRef {
			return false
		}
		if dst != nil && f.Dst.String() != dst.String() {
			return false
		}
		return true
	}
	deadline := map[string]time.Time{} // gocbRef -> end of timeAllowedToLive
	timedOut := map[string]bool{}
	for {
		var next time.Time
		var nextRef string
		for ref, d := range deadline {
			if next.IsZero() || d.Before(next) {
				next, nextRef = d, ref
			}
		}
		var timer <-chan time.Time
		var t *time.Timer
		if !next.IsZero() {
			t = time.NewTimer(time.Until(next))
			timer = t.C
		}
		stopTimer := func() {
			if t != nil {
				t.Stop()
			}
		}
		select {
		case <-ctx.Done():
			stopTimer()
			return exitOK
		case <-timer:
			delete(deadline, nextRef)
			timedOut[nextRef] = true
			emit(map[string]any{"event": "supervision", "gocbRef": nextRef, "state": "timeout"})
		case r, open := <-frames:
			stopTimer()
			if !open {
				logf("goose-subscriber: capture on %s ended", *iface)
				return exitArgs
			}
			if !accept(r.frame) {
				continue
			}
			ref := r.frame.GOOSE.GoCBRef
			if timedOut[ref] {
				emit(map[string]any{"event": "supervision", "gocbRef": ref, "state": "ok"})
				delete(timedOut, ref)
			}
			deadline[ref] = time.Now().Add(r.frame.GOOSE.TAL)
			emit(gooseEvent(r.frame))
		}
	}
}

func gooseEvent(f *goose.Frame) map[string]any {
	g := f.GOOSE
	return map[string]any{
		"event": "goose", "gocbRef": g.GoCBRef, "datSet": g.DatSet, "goID": g.GoID,
		"appID": appIDString(f.APPID), "dst": f.Dst.String(), "src": f.Src.String(),
		"stNum": g.StNum, "sqNum": g.SqNum, "timeAllowedToLive": g.TAL.Milliseconds(), "confRev": g.ConfRev,
		"test": g.Simulation, "simulation": f.Simulated, "ndsCom": g.NdsCom, "numDatSetEntries": g.NumEntries,
		"t": g.T.UTC().Format(time.RFC3339Nano), "values": toTypedList(g.AllData),
	}
}
