// SPDX-License-Identifier: GPL-3.0-only

// dscsystems-goose-subscriber — GOOSE subscriber on dscsystems/go-iec61850.
//
//	dscsystems-goose-subscriber --iface IF [--app-id 0xNNNN]
//	    [--gocb-ref REF] [--dst MAC]
//
// Opens a raw socket on IF (needs CAP_NET_RAW), emits {"event":"ready"},
// then one {"event":"goose",...} line per received frame that passes the
// filters. Frames are decoded with goose.Parse; the adapter reads the
// Ethernet frame itself rather than goose.Subscriber, because the event
// carries the source and destination MAC and the Edition 2 S bit, which
// the subscriber callback does not expose.
//
// Supervision: each gocbRef is supervised with the timeAllowedToLive of
// its last frame. When it passes without a frame the command emits
// {"event":"supervision","gocbRef":REF,"state":"timeout"}; the next frame
// of that gocbRef emits "state":"ok" before its goose event.
//
// Exit codes: 0 on SIGTERM/SIGINT, 1 argument or socket error.
package main

import (
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dscsystems/go-iec61850/ethernet"
	"github.com/dscsystems/go-iec61850/goose"

	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/goosecli"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/jl"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/mmsjson"
)

// event is one received GOOSE frame.
type event struct {
	Event             string          `json:"event"`
	GoCbRef           string          `json:"gocbRef"`
	DatSet            string          `json:"datSet"`
	GoID              string          `json:"goID"`
	AppID             string          `json:"appID"`
	Dst               string          `json:"dst"`
	Src               string          `json:"src"`
	StNum             uint32          `json:"stNum"`
	SqNum             uint32          `json:"sqNum"`
	TimeAllowedToLive uint32          `json:"timeAllowedToLive"`
	ConfRev           uint32          `json:"confRev"`
	Test              bool            `json:"test"`
	Simulation        bool            `json:"simulation"`
	NdsCom            bool            `json:"ndsCom"`
	NumDatSetEntries  uint32          `json:"numDatSetEntries"`
	T                 string          `json:"t"`
	Values            []mmsjson.Typed `json:"values"`
}

// supervisor tracks timeAllowedToLive per gocbRef.
type supervisor struct {
	mu      sync.Mutex
	streams map[string]*stream
}

type stream struct {
	timer    *time.Timer
	gen      uint64
	timedOut bool
}

func emitSupervision(ref, state string) {
	jl.Emit(struct {
		Event   string `json:"event"`
		GoCbRef string `json:"gocbRef"`
		State   string `json:"state"`
	}{"supervision", ref, state})
}

// frame restarts the supervision of ref, reporting recovery first.
func (s *supervisor) frame(ref string, tal time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.streams[ref]
	if !ok {
		st = &stream{}
		s.streams[ref] = st
	}
	if st.timedOut {
		st.timedOut = false
		emitSupervision(ref, "ok")
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.gen++
	gen := st.gen
	if tal <= 0 {
		return
	}
	st.timer = time.AfterFunc(tal, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if st.gen != gen || st.timedOut {
			return
		}
		st.timedOut = true
		emitSupervision(ref, "timeout")
	})
}

func main() {
	jl.HandleMetaFlags()
	iface := flag.String("iface", "eth0", "network interface")
	appID := flag.String("app-id", "", "accept only this APPID, 0xNNNN")
	gocbRef := flag.String("gocb-ref", "", "accept only this gocbRef")
	dst := flag.String("dst", "", "accept only this destination MAC")
	flag.Parse()

	var wantAppID uint16
	haveAppID := *appID != ""
	if haveAppID {
		id, err := goosecli.ParseAppID(*appID)
		if err != nil {
			jl.Fatal(1, "goose-subscriber: %v", err)
		}
		wantAppID = id
	}
	var wantDst [6]byte
	haveDst := *dst != ""
	if haveDst {
		m, err := goosecli.ParseMAC(*dst)
		if err != nil {
			jl.Fatal(1, "goose-subscriber: --dst: %v", err)
		}
		wantDst = m
	}
	srcMAC, err := goosecli.InterfaceMAC(*iface)
	if err != nil {
		jl.Fatal(1, "goose-subscriber: interface %s: %v", *iface, err)
	}
	sock, err := ethernet.Open(*iface, ethernet.EtherTypeGOOSE)
	if err != nil {
		jl.Fatal(1, "goose-subscriber: %v (needs CAP_NET_RAW)", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; sock.Close(); os.Exit(0) }()

	goosecli.EmitReady("subscriber", *iface, goosecli.FormatMAC(srcMAC), *gocbRef)

	sup := &supervisor{streams: make(map[string]*stream)}
	for {
		fr, err := sock.ReadFrame()
		if err != nil {
			jl.Fatal(1, "goose-subscriber: %v", err)
		}
		if fr.EtherType != ethernet.EtherTypeGOOSE {
			continue
		}
		if haveDst && fr.Dst != wantDst {
			continue
		}
		m, err := goose.Parse(fr.Payload)
		if err != nil {
			jl.Log("goose-subscriber: %s: %v", goosecli.FormatMAC(fr.Src), err)
			continue
		}
		if haveAppID && m.AppID != wantAppID {
			continue
		}
		if *gocbRef != "" && m.GoCbRef != *gocbRef {
			continue
		}
		sup.frame(m.GoCbRef, time.Duration(m.TimeAllowedToLive)*time.Millisecond)
		jl.Emit(event{
			Event:             "goose",
			GoCbRef:           m.GoCbRef,
			DatSet:            m.DatSet,
			GoID:              m.GoID,
			AppID:             goosecli.FormatAppID(m.AppID),
			Dst:               goosecli.FormatMAC(fr.Dst),
			Src:               goosecli.FormatMAC(fr.Src),
			StNum:             m.StNum,
			SqNum:             m.SqNum,
			TimeAllowedToLive: m.TimeAllowedToLive,
			ConfRev:           m.ConfRev,
			Test:              m.Test,
			Simulation:        goosecli.Simulation(fr.Payload),
			NdsCom:            m.NdsCom,
			NumDatSetEntries:  m.NumDatSetEntries,
			T:                 m.T.UTC().Format(time.RFC3339Nano),
			Values:            mmsjson.FromValues(m.Values),
		})
	}
}
