// SPDX-License-Identifier: GPL-3.0-only

// dscsystems-sv-subscriber — IEC 61850-9-2 Sampled Values subscriber on
// dscsystems/go-iec61850.
//
//	dscsystems-sv-subscriber --iface IF [--app-id 0xNNNN] [--sv-id ID]
//	    [--dst MAC] [--sample-every N] [--fixture PATH]
//
// Opens a raw socket on IF (needs CAP_NET_RAW), emits {"event":"ready"},
// then for each accepted ASDU whose smpCnt is a multiple of
// --sample-every one {"event":"sv",...} line, and once per second a
// cumulative {"event":"stats",...} line (docs/sv.md). Frames are decoded
// with sv.Parse; the adapter reads the Ethernet frame itself rather than
// sv.Subscriber, because the event carries the MACs, the Edition 2 S bit
// and the ASDU's position in its frame, which the callback does not see.
// seqData is decoded with the member list of the sv.json stream whose
// svID matches; for any other svID values is null and seqData the hex.
//
// Every ASDU is counted; only the sampled ones are decoded and printed,
// on a separate goroutine, which keeps the receive loop well ahead of a
// 4000 frames/s stream. The socket is the adapter's own (rawsock) rather
// than ethernet.Open, for a receive buffer large enough to ride out a
// scheduling stall of the reader and a read buffer that is reused.
//
// Exit codes: 0 on SIGTERM/SIGINT, 1 argument or socket error.
package main

import (
	"encoding/hex"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dscsystems/go-iec61850/ethernet"
	"github.com/dscsystems/go-iec61850/sv"

	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/goosecli"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/jl"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/mmsjson"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/rawsock"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/svcli"
)

// event is one received ASDU. Absent optional fields are null.
type event struct {
	Event      string          `json:"event"`
	AppID      string          `json:"appID"`
	Dst        string          `json:"dst"`
	Src        string          `json:"src"`
	SvID       string          `json:"svID"`
	DatSet     *string         `json:"datSet"`
	SmpCnt     uint16          `json:"smpCnt"`
	ConfRev    uint32          `json:"confRev"`
	RefrTm     *string         `json:"refrTm"`
	SmpSynch   uint8           `json:"smpSynch"`
	SmpRate    *uint16         `json:"smpRate"`
	SmpMod     *string         `json:"smpMod"`
	Simulation bool            `json:"simulation"`
	AsduIndex  int             `json:"asduIndex"`
	NoASDU     int             `json:"noASDU"`
	Values     []mmsjson.Typed `json:"values"`
	SeqData    string          `json:"seqData,omitempty"`
}

type streamKey struct {
	appID uint16
	svID  string
}

// stream is the sequence state of one (APPID, svID).
type stream struct {
	started bool
	prev    int
	maxSeen int
	wrap    int // from the fixture; 0 = largest smpCnt seen + 1
}

type stats struct {
	mu                                     sync.Mutex
	received, lost, duplicates, outOfOrder uint64
}

func (s *stats) emit() {
	s.mu.Lock()
	ev := struct {
		Event      string `json:"event"`
		Received   uint64 `json:"received"`
		Lost       uint64 `json:"lost"`
		Duplicates uint64 `json:"duplicates"`
		OutOfOrder uint64 `json:"outOfOrder"`
	}{"stats", s.received, s.lost, s.duplicates, s.outOfOrder}
	s.mu.Unlock()
	jl.Emit(ev)
}

// count applies the counting rules of docs/sv.md to one ASDU.
func (s *stats) count(st *stream, smpCnt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received++
	if smpCnt > st.maxSeen {
		st.maxSeen = smpCnt
	}
	if !st.started {
		st.started, st.prev = true, smpCnt
		return
	}
	wrap := st.wrap
	if wrap == 0 {
		wrap = st.maxSeen + 1
	}
	d := smpCnt - st.prev
	switch {
	case d == 1:
	case d == 0:
		s.duplicates++
	case d > 1:
		s.lost += uint64(d - 1)
	default: // d < 0
		if st.prev-smpCnt > wrap/2 {
			s.lost += uint64((wrap - 1 - st.prev) + smpCnt)
		} else {
			s.outOfOrder++
			return // the reference stays
		}
	}
	st.prev = smpCnt
}

func main() {
	jl.HandleMetaFlags()
	iface := flag.String("iface", "eth0", "network interface")
	appID := flag.String("app-id", "", "accept only this APPID, 0xNNNN")
	svID := flag.String("sv-id", "", "accept only this svID")
	dst := flag.String("dst", "", "accept only this destination MAC")
	sampleEvery := flag.Uint("sample-every", 1, "report ASDUs whose smpCnt is a multiple of N")
	fixturePath := flag.String("fixture", "/fixtures/sv/sv.json", "sv.json, for the seqData member lists")
	flag.Parse()

	if *sampleEvery == 0 {
		jl.Fatal(1, "sv-subscriber: --sample-every must be positive")
	}
	every := int(*sampleEvery)
	var wantAppID uint16
	haveAppID := *appID != ""
	if haveAppID {
		id, err := goosecli.ParseAppID(*appID)
		if err != nil {
			jl.Fatal(1, "sv-subscriber: %v", err)
		}
		wantAppID = id
	}
	var wantDst [6]byte
	haveDst := *dst != ""
	if haveDst {
		m, err := goosecli.ParseMAC(*dst)
		if err != nil {
			jl.Fatal(1, "sv-subscriber: --dst: %v", err)
		}
		wantDst = m
	}
	fixture, err := svcli.LoadFixture(*fixturePath)
	if err != nil {
		jl.Fatal(1, "sv-subscriber: --fixture: %v", err)
	}
	decoders := make(map[string]*svcli.Decoder, len(fixture))
	for id, st := range fixture {
		d, err := svcli.NewDecoder(st)
		if err != nil {
			jl.Fatal(1, "sv-subscriber: --fixture: %v", err)
		}
		decoders[id] = d
	}
	srcMAC, err := goosecli.InterfaceMAC(*iface)
	if err != nil {
		jl.Fatal(1, "sv-subscriber: interface %s: %v", *iface, err)
	}
	sock, rcvBuf, err := rawsock.Open(*iface, ethernet.EtherTypeSV)
	if err != nil {
		jl.Fatal(1, "sv-subscriber: %v (needs CAP_NET_RAW)", err)
	}
	jl.Log("sv-subscriber: receive buffer %d bytes", rcvBuf)

	st := &stats{}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; sock.Close(); st.emit(); os.Exit(0) }()

	svcli.EmitReady("subscriber", *iface, goosecli.FormatMAC(srcMAC), "")
	go func() {
		for range time.Tick(time.Second) {
			st.emit()
			if d := sock.Drops(); d > 0 {
				jl.Log("sv-subscriber: kernel dropped %d frames (receive buffer full)", d)
			}
		}
	}()

	// The receive loop only counts and hands sampled ASDUs over; decoding
	// and writing (a stdout pipe can block) happen on their own goroutine.
	out := make(chan sampled, 4096)
	go func() {
		for x := range out {
			jl.Emit(toEvent(&x))
		}
	}()

	streams := make(map[streamKey]*stream)
	for {
		raw, err := sock.Read()
		if err != nil {
			jl.Fatal(1, "sv-subscriber: %v", err)
		}
		fr, err := ethernet.ParseFrame(raw)
		if err != nil {
			continue
		}
		if fr.EtherType != ethernet.EtherTypeSV {
			continue
		}
		if haveDst && fr.Dst != wantDst {
			continue
		}
		pdu, err := sv.Parse(fr.Payload)
		if err != nil {
			jl.Log("sv-subscriber: %s: %v", goosecli.FormatMAC(fr.Src), err)
			continue
		}
		if haveAppID && pdu.AppID != wantAppID {
			continue
		}
		for i, a := range pdu.ASDUs {
			if *svID != "" && a.SvID != *svID {
				continue
			}
			key := streamKey{pdu.AppID, a.SvID}
			s := streams[key]
			if s == nil {
				s = &stream{}
				if f := fixture[a.SvID]; f != nil {
					s.wrap = f.SmpCntWrap
				}
				streams[key] = s
			}
			st.count(s, int(a.SmpCnt))
			if int(a.SmpCnt)%every != 0 {
				continue
			}
			select {
			case out <- sampled{fr.Dst, fr.Src, goosecli.Simulation(fr.Payload), pdu.AppID, len(pdu.ASDUs), i, a, decoders[a.SvID]}:
			default:
				jl.Log("sv-subscriber: output backlog full, sv event for smpCnt %d dropped", a.SmpCnt)
			}
		}
	}
}

// sampled is an ASDU to report, copied out of the reused read buffer
// (sv.Parse does not alias it).
type sampled struct {
	dst, src   [6]byte
	simulation bool
	appID      uint16
	noASDU     int
	i          int
	a          *sv.ASDU
	dec        *svcli.Decoder
}

func toEvent(x *sampled) event {
	a, dec := x.a, x.dec
	e := event{
		Event:      "sv",
		AppID:      goosecli.FormatAppID(x.appID),
		Dst:        goosecli.FormatMAC(x.dst),
		Src:        goosecli.FormatMAC(x.src),
		SvID:       a.SvID,
		SmpCnt:     a.SmpCnt,
		ConfRev:    a.ConfRev,
		SmpSynch:   a.SmpSynch,
		Simulation: x.simulation,
		AsduIndex:  x.i,
		NoASDU:     x.noASDU,
	}
	if a.DatSet != "" {
		e.DatSet = &a.DatSet
	}
	if !a.RefrTm.IsZero() {
		t := a.RefrTm.UTC().Format(time.RFC3339Nano)
		e.RefrTm = &t
	}
	if a.SmpRate != 0 {
		r := a.SmpRate
		e.SmpRate = &r
	}
	if a.HasSmpMod {
		m := svcli.SmpModName(a.SmpMod)
		e.SmpMod = &m
	}
	if dec != nil {
		vals, err := dec.Decode(a.Sample)
		if err == nil {
			e.Values = vals
			return e
		}
		jl.Log("sv-subscriber: %s smpCnt %d: %v", a.SvID, a.SmpCnt, err)
	}
	e.SeqData = hex.EncodeToString(a.Sample)
	return e
}
