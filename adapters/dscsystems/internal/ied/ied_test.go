// SPDX-License-Identifier: GPL-3.0-only

package ied

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/dscsystems/go-iec61850/client"
	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/model"
)

func stdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestObjKeepsOrderAndEscapes(t *testing.T) {
	b, err := json.Marshal(Obj{{"z", 1}, {`q"`, "x\n"}, {"a", []int{1}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"q\"":"x\n","a":[1]}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
	if b, _ := json.Marshal(Obj{}); string(b) != "{}" {
		t.Errorf("empty: %s", b)
	}
}

func TestObjMarshalError(t *testing.T) {
	if _, err := json.Marshal(Obj{{"c", make(chan int)}}); err == nil {
		t.Error("unencodable value accepted")
	}
}

func TestOKAndFailLines(t *testing.T) {
	cases := []struct {
		f    func()
		want string
	}{
		{func() { OK("read", "LD/X", Field{"value", 1}) }, `{"operation":"read","target":"LD/X","ok":true,"value":1}`},
		{func() { OK("connect", "") }, `{"operation":"connect","ok":true}`},
		{func() { Fail("read", "LD/X", "timeout") }, `{"operation":"read","target":"LD/X","ok":false,"error":"timeout"}`},
		{func() { Fail("close", "", "e") }, `{"operation":"close","ok":false,"error":"e"}`},
	}
	for _, c := range cases {
		if got := stdout(t, c.f); got != c.want+"\n" {
			t.Errorf("got  %swant %s", got, c.want)
		}
	}
}

// timeoutErr is a net.Error that is not one of the wrapped sentinels.
type netErr struct{}

func (netErr) Error() string   { return "no route" }
func (netErr) Timeout() bool   { return false }
func (netErr) Temporary() bool { return false }

func TestErrString(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("op: %w", e) }
	cases := []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{wrap(mms.AccessObjectAccessDenied), "access-denied"},
		{wrap(mms.AccessObjectNonExistent), "object-does-not-exist"},
		{wrap(mms.AccessObjectAccessUnsupported), "object-access-unsupported"},
		{wrap(mms.AccessTypeInconsistent), "type-inconsistent"},
		{wrap(mms.AccessHardwareFault), mms.AccessHardwareFault.String()},
		{&client.ControlError{AddCause: model.AddCauseBlockedByInterlocking}, model.AddCauseBlockedByInterlocking.String()},
		{wrap(context.DeadlineExceeded), "timeout"},
		{os.ErrDeadlineExceeded, "timeout"},
		{wrap(net.ErrClosed), "connection-lost"},
		{io.EOF, "connection-lost"},
		{wrap(io.ErrUnexpectedEOF), "connection-lost"},
		{netErr{}, "not-connected: no route"},
		{errors.New("own text"), "own text"},
	}
	for _, c := range cases {
		if got := ErrString(c.err); got != c.want {
			t.Errorf("%v: got %q, want %q", c.err, got, c.want)
		}
	}
	if got := ErrString(&mms.ServiceError{Class: 7, Code: 2}); !strings.HasPrefix(got, "service-error: ") {
		t.Errorf("service error: %q", got)
	}
}

// A ControlError with an underlying error is named by that error, not by
// its AddCause.
func TestErrStringControlErrorWithCause(t *testing.T) {
	err := &client.ControlError{Stage: "operate", Err: context.DeadlineExceeded}
	if got := ErrString(err); got != "timeout" {
		t.Errorf("got %q", got)
	}
}

func TestResolveInteropLD(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"Other"}, "Other"},
		{[]string{"A", "IEDInteropLD"}, "IEDInteropLD"},
		{[]string{"IEDInteropLD", "InteropLD"}, "InteropLD"},
		{[]string{"InteropLDx", "B"}, "InteropLDx"}, // no suffix match: first name
	}
	for _, c := range cases {
		if got := ResolveInteropLD(c.in); got != c.want {
			t.Errorf("%v: got %q, want %q", c.in, got, c.want)
		}
	}
}
