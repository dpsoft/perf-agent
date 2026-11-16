package perfmcp

import (
	"cmp"
	"strconv"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// wireError is a capture failure as an agent reads it (§4.4): the text of
// an isError result is {"error": <wireError>} (as resultError, whose
// survivors carry numbers), and a failure manifest keeps the same object,
// where every integer is a decimal string. A duration is a Go duration
// string; empty fields are left out, but retryable is always there, since
// a loop branches on it.
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

// The files keep every integer as a decimal string (wireSettings,
// manifestTarget, wireSurvivor); what an agent reads back carries them as
// JSON numbers, so a setting it read can be passed again as it is. These
// are those answers' shapes.

// resultSettings are a capture's settings as an answer shows them.
type resultSettings struct {
	Modes        []string `json:"modes"`
	Duration     string   `json:"duration"`
	SampleRate   int      `json:"sample_rate"`
	Unwind       string   `json:"unwind"`
	KernelStacks bool     `json:"kernel_stacks"`
}

func toResultSettings(w wireSettings) resultSettings {
	rate, _ := strconv.Atoi(w.SampleRate)
	return resultSettings{Modes: w.Modes, Duration: w.Duration, SampleRate: rate, Unwind: w.Unwind, KernelStacks: w.KernelStacks}
}

// resultTarget is the profiled process as an answer shows it.
type resultTarget struct {
	PID       int    `json:"pid,omitzero"`
	Comm      string `json:"comm,omitzero"`
	Exe       string `json:"exe,omitzero"`
	BuildID   string `json:"build_id,omitzero"`
	StartTime uint64 `json:"start_time,omitzero"`
	Launched  bool   `json:"launched,omitzero"`
	Exit      string `json:"exit,omitzero"`
}

func toResultTarget(m manifestTarget) resultTarget {
	pid, _ := strconv.Atoi(m.PID)
	start, _ := strconv.ParseUint(m.StartTime, 10, 64)
	return resultTarget{PID: pid, Comm: m.Comm, Exe: m.Exe, BuildID: m.BuildID, StartTime: start, Launched: m.Launched, Exit: m.Exit}
}

// resultSurvivor is a process that outlived the stop grace, as an answer
// shows it.
type resultSurvivor struct {
	PID       int    `json:"pid"`
	Comm      string `json:"comm"`
	StartTime uint64 `json:"start_time"`
	State     string `json:"state"`
}

func toResultSurvivors(ws []wireSurvivor) []resultSurvivor {
	var out []resultSurvivor
	for _, w := range ws {
		pid, _ := strconv.Atoi(w.PID)
		start, _ := strconv.ParseUint(w.StartTime, 10, 64)
		out = append(out, resultSurvivor{PID: pid, Comm: w.Comm, StartTime: start, State: w.State})
	}
	return out
}

// resultError is a wire error as an answer shows it: its survivors with
// numbers, and perf-agent's exit status as a number, or the signal that
// ended it as perf_agent_signal.
type resultError struct {
	*wireError
	PerfAgentExit   *int             `json:"perf_agent_exit,omitzero"`
	PerfAgentSignal string           `json:"perf_agent_signal,omitzero"`
	Survivors       []resultSurvivor `json:"survivors,omitzero"`
}

func toResultError(w *wireError) resultError {
	r := resultError{wireError: w, Survivors: toResultSurvivors(w.Survivors)}
	if w.PerfAgentExit != "" {
		if n, err := strconv.Atoi(w.PerfAgentExit); err == nil {
			r.PerfAgentExit = new(n)
		} else {
			r.PerfAgentSignal = w.PerfAgentExit
		}
	}
	return r
}

// defaultNext is what to do after a failure of each code, for an error
// whose own next says nothing more precise: every wire error has a next.
var defaultNext = map[capture.Code]string{
	capture.CodeDisabled:             "the message says what this server lacks: tell the user, who restarts perf-agent-mcp with it",
	capture.CodeInvalidArgument:      "fix the argument as the message says; the tool's input schema gives each argument's type and range",
	capture.CodeIncomparable:         "drop the setting to inherit the baseline's, or start a new series",
	capture.CodeUnknownBaseline:      "pass compare_with an id of this series as capture_list shows it",
	capture.CodeBusy:                 "retry after retry_after",
	capture.CodeUnknownCapture:       "pass the job id capture answered, or a capture id <series>/NNN as capture_list shows it",
	capture.CodeTargetChanged:        "capture again; the target changed while it was profiled",
	capture.CodeOwnerMismatch:        "give this server its own directory with --captures",
	capture.CodeBudget:               "retry after retry_after",
	capture.CodeDisk:                 "free space in the capture directory, or tell the user to start perf-agent-mcp with --captures naming another directory",
	capture.CodeExists:               "pass another label",
	capture.CodeTargetRefused:        "capture an ordinary process of yours, or launch the program with command",
	capture.CodeTargetNotFound:       "check the pid, or capture a command instead",
	capture.CodeTargetExited:         "give the command more work, or capture the process again by its new pid",
	capture.CodePIDReused:            "capture the process again by its current pid",
	capture.CodeGPUNotInjected:       "launch the CUDA program with command and modes gpu, so the CUPTI adapter is injected at its start",
	capture.CodePerfAgentRefusedArgs: "tell the user: this server and its perf-agent do not match; use a perf-agent of the version this server was built with",
	capture.CodePerfAgentFailed:      "capture again; if it fails the same way, tell the user what stderr_tail says",
	capture.CodePerfAgentUnstartable: "tell the user: perf-agent cannot start; check the binary and its libraries, then restart perf-agent-mcp",
	capture.CodeBinaryChanged:        "tell the user to restart perf-agent-mcp",
	capture.CodeStartTimeout:         "capture again; if it times out again, tell the user",
	capture.CodeFinishTimeout:        "capture again; if it times out again, tell the user",
	capture.CodeTooLarge:             "capture with a shorter duration or a lower sample_rate",
	capture.CodeNoOutput:             "capture again",
	capture.CodeStuckChild:           "do not retry; tell the user which processes survived",
	capture.CodeCancelled:            "capture again if you still need it",
	capture.CodeClosing:              "start a new perf-agent-mcp to capture",
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
		Next:          cmp.Or(e.Next, defaultNext[e.Code], "tell the user what the message says"),
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
