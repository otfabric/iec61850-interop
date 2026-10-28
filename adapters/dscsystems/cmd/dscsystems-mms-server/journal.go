// SPDX-License-Identifier: GPL-3.0-only

// journal.go — ReadJournal over the fixture journals.
//
// Selection follows the C adapter's in-memory LogStorage: a time range
// returns the entries with start <= time <= end; entryToStartAfter returns
// the entries after the one with the given id whose time is at or after
// the given time. Each entry carries its data variable and the ReasonCode
// variable (a 7-bit bit string) libiec61850's journal service adds.
// entryID is the fixture id as 8 little-endian bytes.
package main

import (
	"encoding/binary"
	"math"
	"time"

	"github.com/dscsystems/go-iec61850/asn1"
	"github.com/dscsystems/go-iec61850/mms"
)

// parseTimeOfDay decodes a TimeOfDay (4 or 6 octets).
func parseTimeOfDay(b []byte) (time.Time, error) {
	v, err := mms.NewBinaryTimeRaw(b)
	if err != nil {
		return time.Time{}, err
	}
	return v.Time(), nil
}

// ReadJournal-Request ::= SEQUENCE {
//
//	journalName [0] ObjectName,
//	rangeStartSpecification [1] CHOICE { startingTime [0], startingEntry [1] } OPTIONAL,
//	rangeStopSpecification [2] CHOICE { endingTime [0], numberOfEntries [1] } OPTIONAL,
//	listOfVariables [4] OPTIONAL,
//	entryToStartAfter [5] SEQUENCE { timeSpecification [0], entrySpecification [1] } OPTIONAL }
func (h *handler) readJournal(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	jc, err := dec.Expect(asn1.ContextConstructed(0))
	if err != nil {
		return nil, errInvalidArgument
	}
	n, err := parseObjectName(asn1.NewDecoder(jc))
	if err != nil {
		return nil, errInvalidArgument
	}
	start := time.Time{}
	end := time.Unix(math.MaxInt32, 0)
	limit := -1
	var after *uint64
	for dec.More() {
		tag, c, err := dec.ReadTLV()
		if err != nil {
			return nil, errInvalidArgument
		}
		inner := asn1.NewDecoder(c)
		switch tag {
		case asn1.ContextConstructed(1):
			if b, ok, _ := inner.Optional(asn1.ContextPrimitive(0)); ok {
				if start, err = parseTimeOfDay(b); err != nil {
					return nil, errInvalidArgument
				}
			}
		case asn1.ContextConstructed(2):
			if b, ok, _ := inner.Optional(asn1.ContextPrimitive(0)); ok {
				if end, err = parseTimeOfDay(b); err != nil {
					return nil, errInvalidArgument
				}
			} else if b, ok, _ := inner.Optional(asn1.ContextPrimitive(1)); ok {
				k, _ := asn1.DecodeInt(b)
				limit = int(k)
			}
		case asn1.ContextConstructed(5):
			tb, err1 := inner.Expect(asn1.ContextPrimitive(0))
			eb, err2 := inner.Expect(asn1.ContextPrimitive(1))
			if err1 != nil || err2 != nil {
				return nil, errInvalidArgument
			}
			if start, err = parseTimeOfDay(tb); err != nil {
				return nil, errInvalidArgument
			}
			var id8 [8]byte
			copy(id8[:], eb)
			id := binary.LittleEndian.Uint64(id8[:])
			after = &id
		}
	}

	var j *journalDef
	if n.scope == scopeDomain {
		if d := h.s.domains[n.domain]; d != nil {
			j = d.journals[n.item]
		}
	}
	if j == nil {
		return nil, mms.AccessObjectNonExistent
	}

	var selected []journalEntry
	found := false
	for _, e := range j.entries {
		switch {
		case after != nil:
			if found && !e.time.Before(start) {
				selected = append(selected, e)
			}
			if e.entryID == *after {
				found = true
			}
		case !e.time.Before(start) && !e.time.After(end):
			selected = append(selected, e)
		}
	}
	if limit >= 0 && len(selected) > limit {
		selected = selected[:limit]
	}

	// One page: as many entries as fit the negotiated PDU size.
	budget := h.sc.MaxPDU - 32
	list := asn1.Cons(asn1.ContextConstructed(0))
	more, used := false, 0
	for i, e := range selected {
		el := journalEntryElem(e)
		if h.sc.MaxPDU > 0 && i > 0 && used+el.Size() > budget {
			more = true
			break
		}
		used += el.Size()
		list.Add(el)
	}
	resp := asn1.Cons(asn1.ContextConstructed(svcReadJournal), list)
	if more {
		resp.Add(asn1.BoolElem(asn1.ContextPrimitive(1), true))
	}
	return resp, nil
}

// journalEntryElem encodes
//
//	JournalEntry ::= SEQUENCE {
//	  entryIdentification [0] OCTET STRING,
//	  originatingApplication [1] ApplicationReference,   -- empty
//	  entryContent [2] SEQUENCE {
//	    occurrenceTime [0] TimeOfDay,
//	    data [2] SEQUENCE { listOfVariables [1] SEQUENCE OF SEQUENCE {
//	      variableTag [0] VisibleString, valueSpecification [1] Data } } } }
func journalEntryElem(e journalEntry) *asn1.Element {
	var id [8]byte
	binary.LittleEndian.PutUint64(id[:], e.entryID)
	reason := mms.NewBitString(7)
	for i := 0; i < 7; i++ {
		reason.SetBit(i, e.reasonCode&(1<<i) != 0)
	}
	variable := func(tag string, v *mms.Value) *asn1.Element {
		return asn1.Cons(asn1.TagSequence,
			asn1.Prim(asn1.ContextPrimitive(0), []byte(tag)),
			asn1.Cons(asn1.ContextConstructed(1), mms.DataElement(v)))
	}
	return asn1.Cons(asn1.TagSequence,
		asn1.Prim(asn1.ContextPrimitive(0), id[:]),
		asn1.Cons(asn1.ContextConstructed(1), asn1.Cons(asn1.TagSequence)),
		asn1.Cons(asn1.ContextConstructed(2),
			asn1.Prim(asn1.ContextPrimitive(0), mms.NewBinaryTime(e.time).Bytes()),
			asn1.Cons(asn1.ContextConstructed(2),
				asn1.Cons(asn1.ContextConstructed(1),
					variable(e.tag, e.value),
					variable("ReasonCode", reason)))),
	)
}
