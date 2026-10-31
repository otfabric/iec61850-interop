// SPDX-License-Identifier: GPL-3.0-only

// dscsystems-sv-publisher — IEC 61850-9-2 Sampled Values publisher on
// dscsystems/go-iec61850.
//
//	dscsystems-sv-publisher --iface IF --app-id 0xNNNN --dst MAC
//	    --vlan-id N --vlan-prio N --sv-id ID --dataset REF --conf-rev N
//	    --smp-rate N --smp-mod MODE --no-asdu N [--smp-synch 0|1|2]
//	    [--simulation] [--profile 9-2LE|generic] [--frequency HZ]
//	    [--report-every N] --values JSON
//
// Lifecycle (docs/sv.md):
//  1. Open a raw socket on IF (needs CAP_NET_RAW), emit {"event":"ready"}.
//  2. Publish continuously, --no-asdu samples per frame, frames paced
//     against absolute deadlines (a late frame goes out at once; the
//     schedule is not shifted). smpCnt starts at 0 and wraps at the
//     samples per second (65536 for seconds-per-sample).
//  3. stdin JSON Lines: {"cmd":"set","values":[...]} changes the values
//     of every following frame; {"cmd":"stop"} (or SIGTERM/SIGINT) emits
//     {"event":"stopped","frames":F,"samples":S} and exits 0. EOF on stdin
//     keeps publishing.
//  4. {"event":"published",...} for the first frame (start), the first
//     frame after each set (set) and each frame that takes samples past a
//     multiple of --report-every (periodic).
//
// The ASDUs are built and encoded with the library's sv.PDU and its 9-2LE
// sample helpers; the pacing loop is the adapter's own, because
// sv.LEPublisher.Run ticks with a time.Ticker (which drops ticks rather
// than catching up), always stamps smpSynch 2, and has no generic profile.
// The library leaves Reserved1 zero, so --simulation sets the S bit in
// each encoded APDU before it goes to the socket.
//
// Exit codes: 0 stop/signal, 1 argument or socket error.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dscsystems/go-iec61850/ethernet"
	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/sv"

	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/goosecli"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/mmsjson"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/svcli"
)

// dataset is one value set, encoded once when it is set.
type dataset struct {
	seqData []byte
	gen     uint64 // 0 for --values, n for the n-th accepted set
}

type publisher struct {
	profile string
	types   []string // member types of --values; a set must match
	current atomic.Pointer[dataset]
	sets    atomic.Uint64
}

func (p *publisher) encode(vals []*mms.Value) ([]byte, error) {
	if p.profile == svcli.Profile92LE {
		return svcli.Encode92LE(vals)
	}
	return svcli.EncodeGeneric(vals)
}

func main() {
	jl.HandleMetaFlags()
	iface := flag.String("iface", "eth0", "network interface")
	appID := flag.String("app-id", "0x4000", "APPID, 0xNNNN or decimal")
	dst := flag.String("dst", "01:0c:cd:04:00:00", "destination MAC")
	vlanID := flag.Uint("vlan-id", 0, "802.1Q VLAN ID")
	vlanPrio := flag.Uint("vlan-prio", 4, "802.1Q priority")
	svID := flag.String("sv-id", "", "svID")
	datSet := flag.String("dataset", "", "datSet reference (sent by the generic profile)")
	confRev := flag.Uint("conf-rev", 1, "confRev")
	smpRate := flag.Uint("smp-rate", 80, "smpRate")
	smpModS := flag.String("smp-mod", "samples-per-period", "samples-per-period|samples-per-second|seconds-per-sample")
	noASDU := flag.Uint("no-asdu", 1, "ASDUs per frame")
	smpSynch := flag.Uint("smp-synch", 0, "smpSynch 0|1|2")
	simulation := flag.Bool("simulation", false, "set the Edition 2 S bit of Reserved1")
	profile := flag.String("profile", svcli.Profile92LE, "9-2LE|generic")
	frequency := flag.Float64("frequency", 50, "nominal frequency, Hz (samples-per-period)")
	reportEvery := flag.Uint64("report-every", 1000, "emit a periodic published event every N samples")
	valuesJSON := flag.String("values", "", "seqData values, typed JSON array")
	flag.Parse()

	fail := func(format string, args ...any) { jl.Fatal(1, "sv-publisher: "+format, args...) }
	if *svID == "" {
		fail("--sv-id is required")
	}
	if *profile != svcli.Profile92LE && *profile != svcli.ProfileGeneric {
		fail("--profile %q: want 9-2LE or generic", *profile)
	}
	smpMod, err := svcli.ParseSmpMod(*smpModS)
	if err != nil {
		fail("%v", err)
	}
	id, err := goosecli.ParseAppID(*appID)
	if err != nil {
		fail("%v", err)
	}
	dstMAC, err := goosecli.ParseMAC(*dst)
	if err != nil {
		fail("--dst: %v", err)
	}
	if *vlanID > 4095 || *vlanPrio > 7 {
		fail("--vlan-id must be 0..4095 and --vlan-prio 0..7")
	}
	if *smpSynch > 255 {
		fail("--smp-synch %d does not fit an octet", *smpSynch)
	}
	if *confRev > math.MaxUint32 {
		fail("--conf-rev %d does not fit 32 bits", *confRev)
	}
	if *smpRate == 0 || *smpRate > 0xffff {
		fail("--smp-rate must be 1..65535")
	}
	if *noASDU == 0 || *noASDU > 255 {
		fail("--no-asdu must be 1..255")
	}
	if *reportEvery == 0 {
		fail("--report-every must be positive")
	}

	// Sample interval and smpCnt wrap.
	var interval time.Duration
	var wrap int
	switch smpMod {
	case sv.SmpPerPeriod:
		if *frequency <= 0 {
			fail("--frequency must be positive")
		}
		perSec := float64(*smpRate) * *frequency
		wrap = int(math.Round(perSec))
		interval = time.Duration(float64(time.Second) / perSec)
	case sv.SmpPerSec:
		wrap = int(*smpRate)
		interval = time.Second / time.Duration(*smpRate)
	case sv.SecPerSmp:
		wrap = 1 << 16
		interval = time.Duration(*smpRate) * time.Second
	}
	if wrap < 1 || wrap > 1<<16 {
		fail("%d samples per second do not fit the 16-bit smpCnt", wrap)
	}

	vals, err := mmsjson.ParseValues(*valuesJSON)
	if err != nil {
		fail("--values: %v", err)
	}
	p := &publisher{profile: *profile, types: svcli.Types(vals)}
	seq, err := p.encode(vals)
	if err != nil {
		fail("--values: %v", err)
	}
	p.current.Store(&dataset{seqData: seq})

	srcMAC, err := goosecli.InterfaceMAC(*iface)
	if err != nil {
		fail("interface %s: %v", *iface, err)
	}
	sock, err := ethernet.Open(*iface, ethernet.EtherTypeSV)
	if err != nil {
		fail("%v (needs CAP_NET_RAW)", err)
	}
	defer func() { _ = sock.Close() }()

	svcli.EmitReady("publisher", *iface, goosecli.FormatMAC(srcMAC), *svID)

	stop := make(chan struct{}, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		select {
		case stop <- struct{}{}:
		default:
		}
	}()
	go p.readCommands(stop)

	// One reusable PDU; only smpCnt, refrTm and the sample change.
	generic := *profile == svcli.ProfileGeneric
	n := int(*noASDU)
	pdu := &sv.PDU{AppID: id, ASDUs: make([]*sv.ASDU, n)}
	for i := range pdu.ASDUs {
		a := &sv.ASDU{SvID: *svID, ConfRev: uint32(*confRev), SmpSynch: uint8(*smpSynch)}
		if generic {
			a.DatSet = *datSet
			a.SmpRate = uint16(*smpRate)
			a.SmpMod = smpMod
			a.HasSmpMod = true
		}
		pdu.ASDUs[i] = a
	}
	frame := &ethernet.Frame{
		Dst:       dstMAC,
		Src:       srcMAC,
		EtherType: ethernet.EtherTypeSV,
		VLAN:      &ethernet.VLANTag{VID: uint16(*vlanID), Priority: uint8(*vlanPrio)},
	}

	var frames, samples uint64
	smpCnt := 0
	lastGen := uint64(0)
	start := time.Now()
	frameInterval := interval * time.Duration(n)
	for k := int64(0); ; k++ {
		deadline := start.Add(time.Duration(k) * frameInterval)
		if !waitUntil(deadline, stop) {
			break
		}
		ds := p.current.Load()
		for i, a := range pdu.ASDUs {
			a.SmpCnt = uint16(smpCnt)
			a.Sample = ds.seqData
			if generic {
				a.RefrTm = start.Add(time.Duration(k*int64(n)+int64(i)) * interval)
			}
			if smpCnt++; smpCnt >= wrap {
				smpCnt = 0
			}
		}
		apdu := pdu.Marshal()
		goosecli.SetSimulation(apdu, *simulation)
		frame.Payload = apdu
		if err := sock.WriteFrame(frame); err != nil {
			goosecli.EmitError("send: %v", err)
			continue
		}
		before := samples
		frames++
		samples += uint64(n)
		reason := ""
		switch {
		case frames == 1:
			reason = "start"
		case ds.gen != lastGen:
			reason = "set"
		case samples/(*reportEvery) != before/(*reportEvery):
			reason = "periodic"
		}
		lastGen = ds.gen
		if reason != "" {
			jl.Emit(struct {
				Event   string `json:"event"`
				Reason  string `json:"reason"`
				Frames  uint64 `json:"frames"`
				Samples uint64 `json:"samples"`
				SmpCnt  uint16 `json:"smpCnt"`
			}{"published", reason, frames, samples, pdu.ASDUs[n-1].SmpCnt})
		}
	}
	jl.Emit(struct {
		Event   string `json:"event"`
		Frames  uint64 `json:"frames"`
		Samples uint64 `json:"samples"`
	}{"stopped", frames, samples})
	_ = sock.Close()
	os.Exit(0)
}

// waitUntil returns at deadline, or false when stop fires first. Go
// sleeps have about a millisecond of granularity, so the last stretch
// before a deadline is a yielding busy-wait.
func waitUntil(deadline time.Time, stop <-chan struct{}) bool {
	const spin = 1200 * time.Microsecond
	for {
		select {
		case <-stop:
			return false
		default:
		}
		d := time.Until(deadline)
		switch {
		case d <= 0:
			return true
		case d > spin+time.Millisecond:
			t := time.NewTimer(d - spin)
			select {
			case <-stop:
				t.Stop()
				return false
			case <-t.C:
			}
		case d > spin:
			time.Sleep(d - spin)
		default:
			runtime.Gosched()
		}
	}
}

// command is one stdin line.
type command struct {
	Cmd    string          `json:"cmd"`
	Values []mmsjson.Typed `json:"values"`
}

func (p *publisher) readCommands(stop chan<- struct{}) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c command
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			goosecli.EmitError("stdin: %v", err)
			continue
		}
		switch c.Cmd {
		case "set":
			if err := p.set(c.Values); err != nil {
				goosecli.EmitError("set: %v", err)
			}
		case "stop":
			select {
			case stop <- struct{}{}:
			default:
			}
			return
		default:
			goosecli.EmitError("stdin: unknown cmd %q", c.Cmd)
		}
	}
	jl.Log("sv-publisher: stdin closed; publishing until SIGTERM")
}

// set encodes a new value set; it must have the member types of --values.
func (p *publisher) set(ts []mmsjson.Typed) error {
	vals, err := mmsjson.ToValues(ts)
	if err != nil {
		return err
	}
	types := svcli.Types(vals)
	if len(types) != len(p.types) {
		return fmt.Errorf("%d values, the dataset has %d", len(types), len(p.types))
	}
	for i := range types {
		if types[i] != p.types[i] {
			return fmt.Errorf("value %d is %s, the dataset member is %s", i, types[i], p.types[i])
		}
	}
	seq, err := p.encode(vals)
	if err != nil {
		return err
	}
	p.current.Store(&dataset{seqData: seq, gen: p.sets.Add(1)})
	return nil
}
