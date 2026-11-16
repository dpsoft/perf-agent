package mcphttp

import (
	"fmt"
	"strings"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// Class is the admission class of a parsed message. Each class has its own
// caps, so one that is full never delays another.
type Class int

const (
	// ClassControl is what must get through while the server is busy:
	// notifications, ping, initialize, the list methods, DELETE and the
	// calls of a Control tool.
	ClassControl Class = iota
	// ClassPageState is a call of a Snapshot tool or a read of a Snapshot
	// resource: answered from a snapshot, never waiting on the engine.
	ClassPageState
	// ClassCalls is everything else.
	ClassCalls
)

func (c Class) String() string {
	switch c {
	case ClassControl:
		return "control"
	case ClassPageState:
		return "page_state"
	case ClassCalls:
		return "calls"
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// classify is r's admission class. A tool or resource h does not know is
// a call.
func classify(h Handler, r *mcpstdio.Request) Class {
	switch {
	case r.Notification(), r.Method == "ping", r.Method == "initialize", strings.HasSuffix(r.Method, "/list"):
		return ClassControl
	case r.Method == "tools/call":
		if tool, ok := h.Tool(r.Tool); ok {
			switch {
			case tool.Control:
				return ClassControl
			case tool.Snapshot:
				return ClassPageState
			}
		}
	case r.Method == "resources/read":
		if res, ok := h.Resource(r.URI); ok && res.Snapshot {
			return ClassPageState
		}
	}
	return ClassCalls
}

// ticket is what an admitted message holds until release: its class, and
// for a call its session and the bytes of its arguments.
type ticket struct {
	class Class
	s     *session
	bytes int64
}

// busyText is the refusal text of the cap limit at n.
func busyText(limit string, n int64) string {
	return fmt.Sprintf("server busy: %s=%d reached; retry", limit, n)
}

// takeControl admits one control message, or returns the refusal text
// when MaxControlInFlight are in flight. Called with t.mu held.
func (t *Transport) takeControl() (ticket, string) {
	if t.control >= t.o.MaxControlInFlight {
		return ticket{}, busyText("MaxControlInFlight", int64(t.o.MaxControlInFlight))
	}
	t.control++
	return ticket{class: ClassControl}, ""
}

// take is a place in class c for a request of session s whose
// arguments are params bytes, or returns the refusal text naming the cap
// that is full. Nothing waits: a full class refuses at once. A call is
// counted from here until its handler returns, whether or not its client
// still waits for the answer. Called with t.mu held.
func (t *Transport) take(c Class, s *session, params int64) (ticket, string) {
	switch c {
	case ClassControl:
		return t.takeControl()
	case ClassPageState:
		if t.pageState >= t.o.MaxPageStateInFlight {
			return ticket{}, busyText("MaxPageStateInFlight", int64(t.o.MaxPageStateInFlight))
		}
		t.pageState++
		return ticket{class: c}, ""
	}
	switch {
	case t.calls >= t.o.MaxCalls:
		return ticket{}, busyText("MaxCalls", int64(t.o.MaxCalls))
	case s.admitted >= t.o.MaxCallsPerSession:
		return ticket{}, busyText("MaxCallsPerSession", int64(t.o.MaxCallsPerSession))
	case t.requestBytes+params > t.o.MaxRequestBytes:
		return ticket{}, busyText("MaxRequestBytes", t.o.MaxRequestBytes)
	}
	t.calls++
	s.admitted++
	t.requestBytes += params
	return ticket{class: c, s: s, bytes: params}, ""
}

// release gives back what tk holds. A call's session may have left the
// table meanwhile; its count is decremented all the same. Called with t.mu
// held.
func (t *Transport) release(tk ticket) {
	switch tk.class {
	case ClassControl:
		t.control--
	case ClassPageState:
		t.pageState--
	case ClassCalls:
		t.calls--
		tk.s.admitted--
		t.requestBytes -= tk.bytes
	}
}
