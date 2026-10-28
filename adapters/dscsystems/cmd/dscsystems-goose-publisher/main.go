// SPDX-License-Identifier: GPL-3.0-only

// dscsystems-goose-publisher — GOOSE publisher on dscsystems/go-iec61850.
//
//	dscsystems-goose-publisher --iface IF --gocb-ref REF --go-id ID
//	    --dataset REF --app-id 0xNNNN --dst MAC --vlan-id N --vlan-prio N
//	    --conf-rev N --min-time ms --max-time ms [--test] [--simulation]
//	    [--nds-com] --values JSON
//
// Lifecycle:
//  1. Open a raw socket on IF (needs CAP_NET_RAW).
//  2. Emit {"event":"ready",...}.
//  3. Publish --values as the first state (stNum 1, sqNum 0), then
//     retransmit with the library's state machine: MinTime, doubling,
//     up to MaxTime (goose.RetransFromSCL).
//  4. Read stdin JSON Lines: {"cmd":"set","values":[...]} publishes a
//     new state (stNum+1, sqNum 0); {"cmd":"stop"} exits 0. EOF on stdin
//     keeps publishing until SIGTERM/SIGINT.
//
// Every frame put on the wire emits {"event":"published","stNum":N,
// "sqNum":N,"timeAllowedToLive":ms}. --test sets goosePdu field [7]
// (test in Edition 1, simulation in the Edition 2 ASN.1); --simulation
// sets the Edition 2 S bit of Reserved1, which the library leaves zero,
// so the adapter sets it in each frame on its way to the socket.
//
// Exit codes: 0 stop/signal, 1 argument or socket error.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/dscsystems/go-iec61850/ethernet"
	"github.com/dscsystems/go-iec61850/goose"

	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/goosecli"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/mmsjson"
)

// wire wraps the socket: it sets the simulation bit and reports each
// frame the publisher sends.
type wire struct {
	ethernet.Interface
	simulation bool
}

func (w *wire) WriteFrame(f *ethernet.Frame) error {
	payload := append([]byte(nil), f.Payload...)
	goosecli.SetSimulation(payload, w.simulation)
	out := *f
	out.Payload = payload
	if err := w.Interface.WriteFrame(&out); err != nil {
		goosecli.EmitError("send: %v", err)
		return err
	}
	if m, err := goose.Parse(payload); err == nil {
		jl.Emit(struct {
			Event             string `json:"event"`
			StNum             uint32 `json:"stNum"`
			SqNum             uint32 `json:"sqNum"`
			TimeAllowedToLive uint32 `json:"timeAllowedToLive"`
		}{"published", m.StNum, m.SqNum, m.TimeAllowedToLive})
	}
	return nil
}

func main() {
	jl.HandleMetaFlags()
	iface := flag.String("iface", "eth0", "network interface")
	gocbRef := flag.String("gocb-ref", "", "gocbRef, e.g. InteropIEDInteropLD/LLN0$GO$gcbAll")
	goID := flag.String("go-id", "", "goID (default: gocbRef)")
	dataset := flag.String("dataset", "", "datSet reference")
	appID := flag.String("app-id", "0x0000", "APPID, 0xNNNN")
	dst := flag.String("dst", "01:0c:cd:01:00:00", "destination MAC")
	vlanID := flag.Uint("vlan-id", 0, "802.1Q VLAN ID")
	vlanPrio := flag.Uint("vlan-prio", 4, "802.1Q priority")
	confRev := flag.Uint("conf-rev", 1, "confRev")
	minTime := flag.Uint("min-time", 10, "first retransmission interval, ms")
	maxTime := flag.Uint("max-time", 1000, "steady retransmission interval, ms")
	test := flag.Bool("test", false, "set goosePdu test field [7]")
	simulation := flag.Bool("simulation", false, "set the Edition 2 S bit of Reserved1")
	ndsCom := flag.Bool("nds-com", false, "set ndsCom")
	valuesJSON := flag.String("values", "", "data set values, typed JSON array")
	flag.Parse()

	if *gocbRef == "" {
		jl.Fatal(1, "goose-publisher: --gocb-ref is required")
	}
	values, err := mmsjson.ParseValues(*valuesJSON)
	if err != nil {
		jl.Fatal(1, "goose-publisher: --values: %v", err)
	}
	id, err := goosecli.ParseAppID(*appID)
	if err != nil {
		jl.Fatal(1, "goose-publisher: %v", err)
	}
	dstMAC, err := goosecli.ParseMAC(*dst)
	if err != nil {
		jl.Fatal(1, "goose-publisher: --dst: %v", err)
	}
	if *vlanID > 4095 || *vlanPrio > 7 {
		jl.Fatal(1, "goose-publisher: --vlan-id must be 0..4095 and --vlan-prio 0..7")
	}
	srcMAC, err := goosecli.InterfaceMAC(*iface)
	if err != nil {
		jl.Fatal(1, "goose-publisher: interface %s: %v", *iface, err)
	}
	sock, err := ethernet.Open(*iface, ethernet.EtherTypeGOOSE)
	if err != nil {
		jl.Fatal(1, "goose-publisher: %v (needs CAP_NET_RAW)", err)
	}
	defer sock.Close()

	// The frame always carries an 802.1Q tag, as libiec61850's publisher
	// does: priority tagging is how GOOSE gets its precedence.
	pub, err := goose.NewPublisher(&wire{Interface: sock, simulation: *simulation}, goose.PublisherConfig{
		DstMAC:  dstMAC,
		AppID:   id,
		VLAN:    &ethernet.VLANTag{VID: uint16(*vlanID), Priority: uint8(*vlanPrio)},
		GoCbRef: *gocbRef,
		DatSet:  *dataset,
		GoID:    *goID,
		ConfRev: uint32(*confRev),
		SrcMAC:  srcMAC,
		Retrans: goose.RetransFromSCL(uint32(*minTime), uint32(*maxTime)),
		Test:    *test,
		NdsCom:  *ndsCom,
	})
	if err != nil {
		jl.Fatal(1, "goose-publisher: %v", err)
	}

	goosecli.EmitReady("publisher", *iface, goosecli.FormatMAC(srcMAC), *gocbRef)

	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { pub.Close(); close(done) }) }

	if err := pub.Publish(values); err != nil {
		goosecli.EmitError("publish: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; stop() }()
	go readCommands(pub, stop)

	<-done
}

// command is one stdin line.
type command struct {
	Cmd    string          `json:"cmd"`
	Values []mmsjson.Typed `json:"values"`
}

func readCommands(pub *goose.Publisher, stop func()) {
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
			vals, err := mmsjson.ToValues(c.Values)
			if err != nil {
				goosecli.EmitError("set: %v", err)
				continue
			}
			if err := pub.Publish(vals); err != nil {
				goosecli.EmitError("set: %v", err)
			}
		case "stop":
			stop()
			return
		default:
			goosecli.EmitError("stdin: unknown cmd %q", c.Cmd)
		}
	}
	jl.Log("goose-publisher: stdin closed; publishing until SIGTERM")
}
