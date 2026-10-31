// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
)

func TestValueJSONMatchesTheCAdapter(t *testing.T) {
	bitstr := mms.NewBitString(4)
	bitstr.SetBit(1, true)
	stamp := time.Date(2024, 1, 2, 3, 4, 5, 250000000, time.UTC)
	cases := []struct {
		v    *mms.Value
		want raw
	}{
		{nil, "null"},
		{mms.NewBool(true), "true"},
		{mms.NewInt64(-5), "-5"},
		{mms.NewUint32(4294967295), "4294967295"},
		{mms.NewFloat32(0.1), "0.100000001"},
		{mms.NewFloat64(0.5), "0.5"},
		{mms.NewVisibleString(`a"<b`), `"a\"<b"`},
		{mms.NewMMSString("ä"), `"ä"`},
		{mms.NewOctetString([]byte{0, 0xff}), `"00ff"`},
		{bitstr, `"0100"`},
		{mms.NewBinaryTime(stamp), "1704164645250"},
		{mms.NewArray(mms.NewBool(false), mms.NewInt32(1)), "[false,1]"},
		{mms.NewStructure(mms.NewArray(), mms.NewStructure(mms.NewBool(true))), "[[],[true]]"},
		{mms.NewDataAccessError(mms.AccessObjectNonExistent), raw(`{"error":"` + mms.AccessObjectNonExistent.String() + `"}`)},
	}
	for _, c := range cases {
		if got := valueJSON(c.v); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

func TestLineKeepsOrderAndRawValues(t *testing.T) {
	b, err := json.Marshal(line{{"z", 1}, {"raw", raw("[1,2]")}, {"a", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"raw":[1,2],"a":"x"}`; string(b) != want {
		t.Errorf("got %s", b)
	}
	if _, err := json.Marshal(line{{"c", make(chan int)}}); err == nil {
		t.Error("unencodable value accepted")
	}
}

func TestJSONStringDoesNotEscapeHTML(t *testing.T) {
	if got := jsonString(`<&>"`); got != `"<&>\""` {
		t.Errorf("got %s", got)
	}
}

func TestDaeName(t *testing.T) {
	if got := daeName(mms.AccessObjectValueInvalid); got != mms.AccessObjectValueInvalid.String() {
		t.Errorf("got %s", got)
	}
	if got := daeName(mms.AccessObjectValueInvalid + 1); got != "unknown" {
		t.Errorf("past the last code: %s", got)
	}
}

func TestErrName(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("op: %w", e) }
	cases := []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{wrap(mms.AccessObjectNonExistent), mms.AccessObjectNonExistent.String()},
		{&mms.ServiceError{Class: 1, Code: 1, Rejected: true}, "reject-unrecognized-service"},
		{&mms.ServiceError{Class: 5, Code: 1, Rejected: true}, "reject-invalid-pdu"},
		{&mms.ServiceError{Class: 9, Code: 9, Rejected: true}, "mms-error"},
		{&mms.ServiceError{Class: 7, Code: 2}, "object-non-existent"},
		{&mms.ServiceError{Class: 7, Code: 3}, "object-access-denied"},
		{&mms.ServiceError{Class: 2, Code: 1}, "object-undefined"},
		{&mms.ServiceError{Class: 11, Code: 7}, "file-non-existent"},
		{&mms.ServiceError{Class: 11, Code: 0}, "file-other"},
		{&mms.ServiceError{Class: 4, Code: 0}, "service-other"},
		{&mms.ServiceError{Class: 9, Code: 9}, "mms-error"},
		{wrap(context.DeadlineExceeded), "service-timeout"},
		{wrap(io.EOF), "connection-lost"},
		{wrap(mms.ErrAborted), "connection-lost"},
		{wrap(&mms.AbortedError{}), "connection-lost"},
		{wrap(syscall.ECONNRESET), "connection-lost"},
		{wrap(syscall.EPIPE), "connection-lost"},
		{&net.OpError{Op: "read", Err: errors.New("x")}, "connection-lost"},
		{errors.New("short PDU"), "parsing-response"},
	}
	for _, c := range cases {
		if got := errName(c.err); got != c.want {
			t.Errorf("%v: got %s, want %s", c.err, got, c.want)
		}
	}
}

func TestCoarseErrName(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{io.EOF, "connection-lost"},
		{context.DeadlineExceeded, "service-timeout"},
		{&mms.ServiceError{Class: 7, Code: 2}, "mms-error"},
		{errors.New("x"), "mms-error"},
	}
	for _, c := range cases {
		if got := coarseErrName(c.err); got != c.want {
			t.Errorf("%v: got %s, want %s", c.err, got, c.want)
		}
	}
}

// A refused delete is a false result with MmsError none, however the
// library wraps it.
func TestDeleteErrName(t *testing.T) {
	name := func(error) string { return "other" }
	for _, err := range []error{
		mms.AccessObjectAccessDenied,
		fmt.Errorf("delete: %w", mms.AccessObjectAccessDenied),
		mms.ErrNotDeleted,
		fmt.Errorf("delete: %w", mms.ErrNotDeleted),
	} {
		if got := deleteErrName(err, name); got != "none" {
			t.Errorf("%v: got %s", err, got)
		}
	}
	if got := deleteErrName(io.EOF, name); got != "other" {
		t.Errorf("transport failure: got %s", got)
	}
}
