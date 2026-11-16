package perfmcp

import (
	"strconv"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// wireError is a capture failure as an agent reads it (§4.4): the text of
// an isError result is {"error": <wireError>}, and a failure manifest
// keeps the same object. Every integer is a decimal string and a duration
// a Go duration string; empty fields are left out, but retryable is always
// there, since a loop branches on it.
type wireError struct {
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	Retryable     bool           `json:"retryable"`
	RetryAfter    string         `json:"retry_after,omitzero"`
	Next          string         `json:"next,omitzero"`
	CaptureID     string         `json:"capture_id,omitzero"`
	Job           string         `json:"job,omitzero"`
	Status        string         `json:"status,omitzero"`
	PerfAgentExit string         `json:"perf_agent_exit,omitzero"`
	StderrTail    string         `json:"stderr_tail,omitzero"`
	Changed       string         `json:"changed,omitzero"`
	Survivors     []wireSurvivor `json:"survivors,omitzero"`
}

// wireSurvivor is a process that outlived the stop grace, with its PID and
// start time as decimal strings.
type wireSurvivor struct {
	PID       string `json:"pid"`
	Comm      string `json:"comm"`
	StartTime string `json:"start_time"`
	State     string `json:"state"`
}

// toWireError is e as the wire shows it, nil for nil. A retryable failure
// always says when to retry, in whole seconds, "0s" meaning at once.
func toWireError(e *capture.Error) *wireError {
	if e == nil {
		return nil
	}
	w := &wireError{
		Code:          string(e.Code),
		Message:       e.Message,
		Retryable:     e.Retryable,
		Next:          e.Next,
		PerfAgentExit: e.PerfAgentExit,
		StderrTail:    e.StderrTail,
		Changed:       e.Changed,
		Survivors:     toWireSurvivors(e.Survivors),
	}
	if e.Retryable {
		w.RetryAfter = capture.CeilSecond(e.RetryAfter).String()
	}
	return w
}

func toWireSurvivors(ss []capture.Survivor) []wireSurvivor {
	var out []wireSurvivor
	for _, s := range ss {
		out = append(out, wireSurvivor{
			PID:       strconv.Itoa(s.PID),
			Comm:      s.Comm,
			StartTime: strconv.FormatUint(s.StartTime, 10),
			State:     s.State,
		})
	}
	return out
}
