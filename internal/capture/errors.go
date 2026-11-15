package capture

import (
	"fmt"
	"time"
)

// Code is the machine-readable reason a capture failed. An agent's loop
// branches on it, so the strings are part of the wire contract.
type Code string

// The failure codes.
const (
	CodeDisabled             Code = "disabled"
	CodeInvalidArgument      Code = "invalid_argument"
	CodeIncomparable         Code = "incomparable"
	CodeUnknownBaseline      Code = "unknown_baseline"
	CodeBusy                 Code = "busy"
	CodeUnknownCapture       Code = "unknown_capture"
	CodeTargetChanged        Code = "target_changed"
	CodeOwnerMismatch        Code = "owner_mismatch"
	CodeBudget               Code = "budget"
	CodeDisk                 Code = "disk"
	CodeExists               Code = "exists"
	CodeTargetRefused        Code = "target_refused"
	CodeTargetNotFound       Code = "target_not_found"
	CodeTargetExited         Code = "target_exited"
	CodePIDReused            Code = "pid_reused"
	CodeGPUNotInjected       Code = "gpu_not_injected"
	CodePerfAgentRefusedArgs Code = "perf_agent_refused_args"
	CodePerfAgentFailed      Code = "perf_agent_failed"
	CodePerfAgentUnstartable Code = "perf_agent_unstartable"
	CodeBinaryChanged        Code = "binary_changed"
	CodeStartTimeout         Code = "start_timeout"
	CodeFinishTimeout        Code = "finish_timeout"
	CodeTooLarge             Code = "too_large"
	CodeNoOutput             Code = "no_output"
	CodeStuckChild           Code = "stuck_child"
	CodeCancelled            Code = "cancelled"
)

// DefaultRetryable says, per code, whether the same call may succeed
// later. perf_agent_failed defaults to true; a capability failure sets
// Retryable false on its own Error.
var DefaultRetryable = map[Code]bool{
	CodeDisabled:             false,
	CodeInvalidArgument:      false,
	CodeIncomparable:         false,
	CodeUnknownBaseline:      false,
	CodeBusy:                 true,
	CodeUnknownCapture:       false,
	CodeTargetChanged:        true,
	CodeOwnerMismatch:        false,
	CodeBudget:               true,
	CodeDisk:                 false,
	CodeExists:               false,
	CodeTargetRefused:        false,
	CodeTargetNotFound:       false,
	CodeTargetExited:         true,
	CodePIDReused:            true,
	CodeGPUNotInjected:       false,
	CodePerfAgentRefusedArgs: false,
	CodePerfAgentFailed:      true,
	CodePerfAgentUnstartable: false,
	CodeBinaryChanged:        false,
	CodeStartTimeout:         true,
	CodeFinishTimeout:        true,
	CodeTooLarge:             false,
	CodeNoOutput:             true,
	CodeStuckChild:           false,
	CodeCancelled:            false,
}

// Error is a failure an agent can act on: what happened, whether to retry
// and when, and what to do next.
type Error struct {
	Code       Code
	Message    string
	Next       string
	Retryable  bool
	RetryAfter time.Duration

	// PerfAgentExit is perf-agent's exit status, when it ran.
	PerfAgentExit string
	// StderrTail is the end of perf-agent's stderr, on failure only.
	StderrTail string
	// Changed names what differed, for binary_changed and target_changed.
	Changed string
	// Survivors are the processes left after the stop grace, for stuck_child.
	Survivors []Survivor
}

// Error returns "code: message", then "; next" when there is a next step.
func (e *Error) Error() string {
	s := string(e.Code) + ": " + e.Message
	if e.Next != "" {
		s += "; " + e.Next
	}
	return s
}

// Errorf builds an Error whose message is formatted from format and args,
// with Retryable taken from DefaultRetryable.
func Errorf(code Code, next, format string, args ...any) *Error {
	return &Error{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Next:      next,
		Retryable: DefaultRetryable[code],
	}
}

// Survivor is a process that outlived the stop grace.
type Survivor struct {
	PID       int
	Comm      string
	StartTime uint64
	State     string
}
