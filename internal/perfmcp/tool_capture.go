package perfmcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// captureToolNames are the capture tools, in the order tools/list shows
// them.
var captureToolNames = []string{"capture", "capture_wait", "capture_cancel", "capture_list"}

// The capture tools' fixed values (§4.0, §9.1): capture_wait's timeout,
// how often it reports progress, how long capture_list waits for the
// history, and the retry_after of an answer that is still loading.
const (
	waitDefault     = 30 * time.Second
	waitCap         = 45 * time.Second
	progressEvery   = 5 * time.Second
	phasePoll       = 250 * time.Millisecond
	listWaitDefault = 10 * time.Second
	loadingRetry    = time.Second
)

// The values of a capture_wait answer's handles and status that the job
// states do not name.
const (
	handlesReopen = "reopen"
	statusLoading = "loading"
)

// captureTools are the four capture tools. They run without the engine:
// capture only admits a job, capture_wait and capture_list read the
// registry and the disk, and capture_cancel is a control message.
func (s *Server) captureTools() []mcpstdio.Tool {
	return []mcpstdio.Tool{s.captureTool(), s.captureWaitTool(), s.captureCancelTool(), s.captureListTool()}
}

// captureDescription is d, after the reason capture is off when it is.
func (s *Server) captureDescription(d string) string {
	switch s.captureDisabled {
	case "":
		return d
	case captureDisabledText:
		return captureDisabledText + " " + d
	}
	return "DISABLED: " + strings.TrimSuffix(s.captureDisabled, ".") + ". " + d
}

// disabledError is every capture call's answer while capture is off.
func (s *Server) disabledError() *capture.Error {
	return capture.Errorf(capture.CodeDisabled, "", "%s", s.captureDisabled)
}

// toolError is a capture failure as the isError result an agent branches
// on: one JSON object {"error": {...}} (§4.4), with j's id, capture id and
// state when the failure is a job's.
func toolError(e *capture.Error, j *job) mcpstdio.Result {
	w := toWireError(e)
	if j != nil {
		v := j.view()
		w.Job, w.CaptureID, w.Status = v.Job, v.ID, v.Status
	}
	return errorResult(w)
}

// errorResult is w as an isError result.
func errorResult(w *wireError) mcpstdio.Result {
	b, err := marshal(struct {
		Error resultError `json:"error"`
	}{resultError{wireError: w, Survivors: toResultSurvivors(w.Survivors)}})
	if err != nil {
		b = []byte(`{"error":{"code":"` + w.Code + `","message":"the error could not be encoded","retryable":false}}`)
	}
	return mcpstdio.Result{Text: string(b), IsError: true}
}

// argsNext is the next of arguments that do not decode.
const argsNext = "fix the arguments as the message says; the tool's input schema gives each argument's JSON type"

// invalidArgument is a refused argument.
func invalidArgument(next, format string, args ...any) *capture.Error {
	return capture.Errorf(capture.CodeInvalidArgument, next, format, args...)
}

// unknownCapture is the answer for an id this server has no job or
// capture for: why it is unknown, and next what to do.
func unknownCapture(id, why, next string) *capture.Error {
	return capture.Errorf(capture.CodeUnknownCapture, next, "%q: %s", echo(id), why)
}

// unknownNext is what to do with an id that names nothing this server
// knows.
const unknownNext = "pass the job id capture answered, or a capture id <series>/NNN as capture_list's capture field shows it"

// answer encodes v as a tool result: capture tool answers are small and
// bounded, so they need no size check of their own.
func answer(v any) (mcpstdio.Result, error) {
	b, err := marshal(v)
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("internal error encoding the answer: %v", err)
	}
	return mcpstdio.Result{Text: string(b)}, nil
}

func (s *Server) captureTool() mcpstdio.Tool {
	c := s.lim.Capture
	schema := fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "pid": {"type": "integer", "minimum": 1, "maximum": %d, "description": "A running process of yours to profile. Exactly one of pid and command."},
    "command": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "A program to launch and profile: command[0] is an absolute path or a bare name looked up in PATH (never ./x), the rest its arguments. No shell. Exactly one of pid and command."},
    "cwd": {"type": "string", "description": "The command's directory, inside a --source root or the profiles root; a relative cwd is relative to the profiles root. Default: the first --source root, else the profiles root."},
    "modes": {"type": "array", "items": {"enum": ["cpu", "offcpu", "gpu"]}, "uniqueItems": true, "minItems": 1, "description": "What to profile; default [\"cpu\"]. gpu needs --gpu-shim."},
    "duration": {"type": "string", "description": "A Go duration from 1s to %v; default 10s."},
    "sample_rate": {"type": "integer", "minimum": 1, "maximum": %d, "description": "Samples per second; default 99."},
    "kernel_stacks": {"type": "boolean", "description": "Also sample kernel stacks; needs cap_syslog on perf-agent. Default false."},
    "unwind": {"enum": ["auto", "fp", "dwarf"], "description": "perf-agent's --unwind; default auto."},
    "series": {"type": "string", "description": "The series this capture belongs to, one per loop: 1-%d bytes of [A-Za-z0-9._-], not starting with a dot. Default adhoc."},
    "label": {"type": "string", "description": "Names the capture in its series, as base or cand3: 1-%d bytes of [A-Za-z0-9._-], not starting with a dot. Default run."},
    "compare_with": {"type": "string", "description": "A capture id of this series, such as 001: the capture inherits its settings and the result carries the diff with it."},
    "keep": {"type": "boolean", "description": "Exempt this capture from retention, as for a loop's baseline. Default false."}
  },
  "additionalProperties": false
}`, maxPID, c.MaxDuration, c.MaxSampleRate, maxSeriesName, maxLabel)
	return mcpstdio.Tool{
		Name:  "capture",
		Title: "Capture a profile",
		Description: s.captureDescription("Start capturing a profile of a process (pid) or a launched command with perf-agent, into a series. " +
			"It answers at once with a job id; call capture_wait with it for the state and, once done, the result: each profile's open result " +
			"with its handle and, with compare_with, the diff with that baseline. A failure is one JSON error object with a code, " +
			"whether to retry and when, and what to do next. Retention may remove older captures of the series, and a launched command runs as you."),
		InputSchema: json.RawMessage(schema),
		Annotations: mcpstdio.Annotations{Destructive: true, OpenWorld: true},
		Handler:     s.handleCapture,
	}
}

// captureArgs are capture's arguments; a setting is a pointer so a passed
// one is told from a default.
type captureArgs struct {
	PID          *int     `json:"pid"`
	Command      []string `json:"command"`
	Cwd          string   `json:"cwd"`
	Modes        []string `json:"modes"`
	Duration     *string  `json:"duration"`
	SampleRate   *int     `json:"sample_rate"`
	KernelStacks *bool    `json:"kernel_stacks"`
	Unwind       *string  `json:"unwind"`
	Series       *string  `json:"series"`
	Label        *string  `json:"label"`
	CompareWith  string   `json:"compare_with"`
	Keep         bool     `json:"keep"`
}

// acceptedAnswer is capture's answer.
type acceptedAnswer struct {
	Job    string `json:"job"`
	Status string `json:"status"`
	ETA    string `json:"eta"`
}

// handleCapture admits a capture job: every check is on the arguments,
// the cached binary and owner, the slot and the windows, in memory only.
// The job then runs on its own, and every later failure is its state.
func (s *Server) handleCapture(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.captureDisabled != "" {
		return toolError(s.disabledError(), nil), nil
	}
	if e := s.ownerMismatch(); e != nil {
		return toolError(e, nil), nil
	}
	req, e := s.captureRequestOf(raw)
	if e != nil {
		return toolError(e, nil), nil
	}
	j, e := s.startJob(req)
	if e != nil {
		return toolError(e, nil), nil
	}
	return answer(acceptedAnswer{Job: j.id, Status: string(jobAccepted), ETA: (j.reserve + etaSlack).Round(100 * time.Millisecond).String()})
}

// maxPID is the largest process id Linux hands out (PID_MAX_LIMIT).
const maxPID = 1 << 22

// The values unwind takes.
var unwinds = []string{"auto", "fp", "dwarf"}

// captureRequestOf checks capture's arguments, in the order an agent
// fixes them: the target, the modes, the settings against this server's
// limits and capabilities, the series and label, then compare_with.
func (s *Server) captureRequestOf(raw json.RawMessage) (captureRequest, *capture.Error) {
	var a captureArgs
	if err := decodeArgs("capture", raw, &a); err != nil {
		return captureRequest{}, invalidArgument(argsNext, "%s", err.Error())
	}
	req := captureRequest{Cwd: a.Cwd, Explicit: map[string]bool{}, Keep: a.Keep, Series: defaultSeries, Label: defaultLabel}
	if e := checkTarget(a); e != nil {
		return req, e
	}
	if a.PID != nil {
		req.PID = *a.PID
	}
	req.Command = slices.Clone(a.Command)

	c := s.lim.Capture
	st := &req.Settings
	if a.Modes != nil {
		modes, e := s.checkModes(a.Modes)
		if e != nil {
			return req, e
		}
		st.Modes, req.Explicit[argModes] = modes, true
	}
	if a.Duration != nil {
		d, err := time.ParseDuration(*a.Duration)
		if err != nil {
			return req, invalidArgument(fmt.Sprintf("pass a Go duration such as \"10s\", from 1s to %v", c.MaxDuration),
				"duration %q is not a Go duration", echo(*a.Duration))
		}
		if d < time.Second || d > c.MaxDuration {
			return req, invalidArgument(fmt.Sprintf("pass a duration from 1s to %v", c.MaxDuration),
				"duration %v is outside [1s, Capture.MaxDuration=%v] of this server", d, c.MaxDuration)
		}
		st.Duration, req.Explicit[argDuration] = d, true
	}
	if a.SampleRate != nil {
		if r := *a.SampleRate; r < 1 || r > c.MaxSampleRate {
			return req, invalidArgument(fmt.Sprintf("pass a sample_rate from 1 to %d; perf-agent's default is %d", c.MaxSampleRate, defaultSampleRate),
				"sample_rate %d is outside [1, Capture.MaxSampleRate=%d] of this server", r, c.MaxSampleRate)
		}
		st.SampleRate, req.Explicit[argSampleRate] = *a.SampleRate, true
	}
	if a.Unwind != nil {
		if !slices.Contains(unwinds, *a.Unwind) {
			return req, invalidArgument("pass unwind auto, fp or dwarf", "unwind %q is not one of auto, fp, dwarf", echo(*a.Unwind))
		}
		st.Unwind, req.Explicit[argUnwind] = *a.Unwind, true
	}
	if a.KernelStacks != nil {
		if *a.KernelStacks && !s.perfAgent.Caps.Has(capture.CapSyslog) {
			return req, capture.Errorf(capture.CodeDisabled, s.syslogNext(),
				"kernel_stacks needs cap_syslog in perf-agent's file capabilities, or kernel frames come out unsymbolized")
		}
		st.KernelStacks, req.Explicit[argKernelStacks] = *a.KernelStacks, true
	}
	const names = "1-%d bytes of [A-Za-z0-9._-], not starting with '.'"
	if a.Series != nil {
		if !validSeries(*a.Series) {
			return req, invalidArgument("pass a series of "+fmt.Sprintf(names, maxSeriesName), "series %q is not "+names, echo(*a.Series), maxSeriesName)
		}
		req.Series = *a.Series
	}
	if a.Label != nil {
		if !validLabel(*a.Label) {
			return req, invalidArgument("pass a label of "+fmt.Sprintf(names, maxLabel), "label %q is not "+names, echo(*a.Label), maxLabel)
		}
		req.Label = *a.Label
	}
	if a.CompareWith != "" {
		id, e := compareID(a.CompareWith, req.Series)
		if e != nil {
			return req, e
		}
		req.CompareWith = id
	}
	return req, nil
}

// syslogNext is what to do for kernel_stacks on a perf-agent without
// cap_syslog: the setcap line for this server's perf-agent, the one path a
// capture answer shows.
func (s *Server) syslogNext() string {
	return "capture without kernel_stacks, or ask the user to run " +
		strings.ReplaceAll(capture.SetcapLine, "<path>", s.perfAgent.Path) + " and restart perf-agent-mcp"
}

// checkTarget checks that exactly one of pid and command names the
// target, and the command's shape, without I/O. A relative command[0] is
// refused with the absolute path the agent most likely meant: cwd joined
// with it, as strings.
func checkTarget(a captureArgs) *capture.Error {
	const both = "pass exactly one of pid (a running process of yours) and command (a program to launch)"
	switch {
	case a.PID == nil && a.Command == nil:
		return invalidArgument(both, "capture needs a target: pid or command")
	case a.PID != nil && a.Command != nil:
		return invalidArgument(both, "capture takes pid or command, not both")
	case a.PID != nil && (*a.PID < 1 || *a.PID > maxPID):
		return invalidArgument("pass the pid of a running process of yours", "pid %d is not a process id (1 to %d)", *a.PID, maxPID)
	case a.PID != nil && a.Cwd != "":
		return invalidArgument("drop cwd: a process runs where it already runs", "cwd is for a launched command, and pid names a running process")
	case a.PID != nil:
		return nil
	case len(a.Command) == 0:
		return invalidArgument(`pass the program and its arguments, as ["/abs/path/bench", "-n", "3"]`, "command is empty")
	case a.Command[0] == "":
		return invalidArgument("pass command[0] as an absolute path or a bare name found in PATH", "command[0] is empty")
	}
	for i, arg := range a.Command {
		if strings.IndexByte(arg, 0) >= 0 {
			return invalidArgument("drop the NUL byte", "command[%d] holds a NUL byte, which no argument can carry", i)
		}
	}
	name := a.Command[0]
	if filepath.IsAbs(name) || !strings.ContainsRune(name, '/') {
		return nil
	}
	next := "pass command[0] as an absolute path (cwd joined with it), or a bare name found in PATH"
	switch {
	case strings.HasPrefix(name, "~"):
		next = "pass command[0] as an absolute path: no shell runs the command, so ~ is not expanded"
	case filepath.IsAbs(a.Cwd):
		next = fmt.Sprintf("pass %q as command[0]: cwd joined with %q", echo(filepath.Join(a.Cwd, name)), echo(name))
	}
	return invalidArgument(next, "command[0] %q is a relative path; it must be an absolute path or a bare name looked up in PATH", echo(name))
}

// checkModes checks modes: known, distinct and at least one; gpu needs
// this server's CUPTI adapter.
func (s *Server) checkModes(modes []string) ([]capture.Mode, *capture.Error) {
	const next = "pass one or more of cpu, offcpu, gpu, each once"
	if len(modes) == 0 {
		return nil, invalidArgument(next, "modes is empty")
	}
	var out []capture.Mode
	for _, m := range modes {
		mode := capture.Mode(m)
		switch {
		case mode != capture.ModeCPU && mode != capture.ModeOffCPU && mode != capture.ModeGPU:
			return nil, invalidArgument(next, "modes holds %q, which is not cpu, offcpu or gpu", echo(m))
		case slices.Contains(out, mode):
			return nil, invalidArgument(next, "modes lists %s twice", m)
		}
		out = append(out, mode)
	}
	if slices.Contains(out, capture.ModeGPU) && s.shim.Path == "" {
		return nil, capture.Errorf(capture.CodeDisabled, "start perf-agent-mcp with --gpu-shim PATH, or capture without gpu",
			"mode gpu needs the CUPTI adapter, and this server has none")
	}
	return out, nil
}

// compareID is compare_with's capture id in canonical form, NNN: it is
// given as NNN, or as <series>/NNN of the capture's own series.
func compareID(with, series string) (string, *capture.Error) {
	next := fmt.Sprintf("pass the id of a capture of series %s as capture_list shows it, such as 001", series)
	id := with
	if s, rest, found := strings.Cut(with, "/"); found {
		if s == "" {
			return "", invalidArgument(next, "compare_with %q is not a capture id such as 001", echo(with))
		}
		if s != series {
			return "", invalidArgument(fmt.Sprintf("compare within one series: capture into series %q, or start a new loop", echo(s)),
				"compare_with %q names a capture of series %q, and this capture is in series %s", echo(with), echo(s), series)
		}
		id = rest
	}
	if _, ok := parseID(id); !ok {
		return "", invalidArgument(next, "compare_with %q is not a capture id such as 001", echo(with))
	}
	return id, nil
}

func (s *Server) captureWaitTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name:  "capture_wait",
		Title: "Wait for a capture",
		Description: s.captureDescription("Wait for a capture job, by the job id capture answered or the capture id (series/NNN), " +
			"for at most timeout (a Go duration, default 30s, at most 45s). It answers the capture's result once it is done: " +
			"the capture, each profile's open result with its handle, and the compare with its baseline. Before that, the job's state " +
			"(status, phase, elapsed, eta); call again. A failed or cancelled capture is its error. A capture this server no longer holds " +
			"in memory is answered from its manifest, with the calls that reopen it (recompute)."),
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "The job id from capture, or a capture id as series/NNN."},
    "timeout": {"type": "string", "description": "How long to wait at most, a Go duration above 0s and at most 45s; default 30s."}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
		Annotations: readOnly,
		Handler:     s.handleWait,
	}
}

// waitTimeout is capture_wait's timeout: default 30s, above 0 and at most
// 45s, so a call stays well inside a client's 60s.
func waitTimeout(raw string) (time.Duration, *capture.Error) {
	if raw == "" {
		return waitDefault, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > waitCap {
		return 0, invalidArgument(`pass a timeout such as "30s", above 0s and at most 45s, and call again while the capture runs`,
			"timeout %q is not a Go duration in (0s, %v]", echo(raw), waitCap)
	}
	return d, nil
}

// waitArgs are capture_wait's arguments.
type waitArgs struct {
	ID      string `json:"id"`
	Timeout string `json:"timeout"`
}

// handleWait answers a job of this server from the registry, waiting for
// it to settle, and a capture it no longer holds from its manifest,
// through the lookup pool. It never takes the engine.
func (s *Server) handleWait(ctx context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.captureDisabled != "" {
		return toolError(s.disabledError(), nil), nil
	}
	var a waitArgs
	if err := decodeArgs("capture_wait", raw, &a); err != nil {
		return toolError(invalidArgument(argsNext, "%s", err.Error()), nil), nil
	}
	if a.ID == "" {
		return toolError(invalidArgument("pass the job id capture answered, or a capture id as <series>/NNN", "capture_wait needs id"), nil), nil
	}
	if e := bareCaptureID("capture_wait", a.ID); e != nil {
		return toolError(e, nil), nil
	}
	d, e := waitTimeout(a.Timeout)
	if e != nil {
		return toolError(e, nil), nil
	}
	d = fromReceipt(ctx, d)
	if j, ok := s.registry.get(a.ID); ok {
		return s.waitJob(ctx, j, d, a.ID), nil
	}
	key, jobID := a.ID, ""
	if al, ok := s.registry.final(a.ID); ok {
		if al.captureID == "" {
			return endedEarly(al), nil
		}
		if j, ok := s.registry.get(al.captureID); ok {
			return s.waitJob(ctx, j, d, a.ID), nil
		}
		key, jobID = al.captureID, al.jobID
	}
	if _, _, ok := splitCaptureKey(key); !ok {
		return toolError(unknownCapture(a.ID, "no job of this server, and no capture id <series>/NNN", unknownNext), nil), nil
	}
	return s.waitHistorical(ctx, key, jobID, d, a.ID), nil
}

// bareCaptureID refuses a capture's number without its series (001): a
// capture id names its series, as capture_list's capture field shows it.
func bareCaptureID(tool, id string) *capture.Error {
	if _, ok := parseID(id); !ok {
		return nil
	}
	return invalidArgument(fmt.Sprintf("pass the capture id with its series, <series>/%s, as capture_list's capture field shows it, or the job id capture answered", id),
		"%s takes a job id or a capture id <series>/%s, and %s names no series", tool, id, id)
}

// fromReceipt is what is left of a call's wait d, which counts from when
// the call was read, not from when it ran: a call queued behind another
// still answers within its own budget of the client's time.
func fromReceipt(ctx context.Context, d time.Duration) time.Duration {
	if at, ok := mcpstdio.Received(ctx); ok {
		return max(d-time.Since(at), 0)
	}
	return d
}

// splitCaptureKey splits a capture id "series/NNN".
func splitCaptureKey(key string) (series string, id int, ok bool) {
	series, n, found := strings.Cut(key, "/")
	if !found || !validSeries(series) {
		return "", 0, false
	}
	id, ok = parseID(n)
	return series, id, ok
}

// endedEarly answers a wait for a job that ended before it had a capture
// id and whose entry has since left the registry: its alias keeps the code
// it failed with, not the rest of its error.
func endedEarly(a jobAlias) mcpstdio.Result {
	status, code := jobFailed, a.code
	if a.answer == cancelAnswerCancelled {
		status, code = jobCancelled, capture.CodeCancelled
	}
	if code == "" {
		code = capture.CodeUnknownCapture
	}
	w := toWireError(capture.Errorf(code, "capture again; capture_list shows the series' history",
		"job %s ended %s with %s before it had a capture id; the rest of its error is no longer in this server's memory", a.jobID, status, code))
	w.Job, w.Status = a.jobID, string(status)
	return errorResult(w)
}

// waitJob waits for j to settle, for at most d or until ctx ends, and
// answers its result, its error, or its state then; asked is the id the
// client named j by. It sends progress at each change of state and every
// progressEvery; a client that asked for none gets none.
func (s *Server) waitJob(ctx context.Context, j *job, d time.Duration, asked string) mcpstdio.Result {
	deadline := time.Now().Add(d)
	timer := time.NewTimer(d)
	defer timer.Stop()
	// The capture's own phases (attaching, profiling, finishing) change
	// inside its Run, which wakes no one: they are read every phasePoll.
	poll := time.NewTicker(phasePoll)
	defer poll.Stop()
	p := progress{ctx: ctx, j: j, last: -1}
	mark, sent := "", time.Time{}
	for {
		v, settled, changed := j.watch()
		if settled {
			return s.settledAnswer(ctx, j, deadline, asked)
		}
		if m := v.Status + " " + v.Phase; m != mark || time.Since(sent) >= progressEvery {
			p.send(v)
			mark, sent = m, time.Now()
		}
		select {
		case <-changed:
		case <-poll.C:
		case <-timer.C:
			return s.stateAnswer(ctx, j, deadline, asked)
		case <-ctx.Done():
			return s.stateAnswer(ctx, j, deadline, asked)
		}
	}
}

// stateAnswer is j's answer when a wait ends first: its state, or, when it
// settled at that very moment, its result or error.
func (s *Server) stateAnswer(ctx context.Context, j *job, deadline time.Time, asked string) mcpstdio.Result {
	v, settled, _ := j.watch()
	if settled {
		return s.settledAnswer(ctx, j, deadline, asked)
	}
	return viewAnswer(v)
}

// settledAnswer is a settled job's answer, given from memory only while
// a done capture's manifest is still there: removed (by another server's
// retention, or a person), the job is forgotten and the capture unknown.
// The re-check holds the call at most recheckFloor past its deadline: when
// it cannot tell in time, the answer is loading, naming the id the client
// asked by.
func (s *Server) settledAnswer(ctx context.Context, j *job, deadline time.Time, asked string) mcpstdio.Result {
	j.mu.Lock()
	status, key := j.status, j.captureID
	j.mu.Unlock()
	if status != jobDone || key == "" {
		return s.jobAnswer(j)
	}
	gone, ready := s.recheck(ctx, key, j.req.Label, time.Until(deadline))
	switch {
	case !ready:
		return loadingAnswer(asked)
	case gone:
		s.registry.forget(key)
		return toolError(unknownCapture(key, "its manifest is gone: another server's retention, or a person, removed it",
			"capture again if you need it; capture_list shows what the series still holds"), nil)
	}
	return s.jobAnswer(j)
}

// progress sends a wait's progress notifications: progress is the
// seconds since the job was admitted, kept increasing, and total what the
// job is expected to take.
type progress struct {
	ctx  context.Context
	j    *job
	last float64
}

func (p *progress) send(v jobView) {
	at := max(time.Since(p.j.began).Seconds(), p.last+0.001)
	if mcpstdio.Progress(p.ctx, at, p.j.expected().Seconds(), strings.TrimSpace(v.Status+" "+v.Phase)) {
		p.last = at
	}
}

// viewAnswer is a job's state as capture_wait answers it before the end.
func viewAnswer(v jobView) mcpstdio.Result {
	r, err := answer(v)
	if err != nil {
		return internalError(err)
	}
	return r
}

// internalError answers a fault of this server's own, which no capture
// code describes; it never happens with the types it encodes.
func internalError(err error) mcpstdio.Result {
	return mcpstdio.Result{Text: "internal error: " + err.Error(), IsError: true}
}

// jobAnswer is a settled job's answer: its error when it failed or was
// cancelled, its result when it is done.
func (s *Server) jobAnswer(j *job) mcpstdio.Result {
	j.mu.Lock()
	status, e, result, derived, recipe := j.status, j.err, j.result, j.derived, j.recipe
	j.mu.Unlock()
	switch status {
	case jobDone:
		return s.resultAnswer(j.id, result, derived, recipe)
	case jobCancelled:
		return toolError(cmp.Or(e, cancelledError()), j)
	}
	if e == nil {
		return internalError(fmt.Errorf("job %s failed and recorded no error", j.id))
	}
	return toolError(e, j)
}

// resultAnswer is a done job's result b with its job id and status, its
// derived state, and, when the store has since evicted one of its
// handles, handles "reopen" and the calls that reopen them; cut to
// MaxResponseBytes.
func (s *Server) resultAnswer(jobID string, b []byte, derived string, recipe *recompute) mcpstdio.Result {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		s.log.Error("capture result", "job", jobID, "err", err)
		return internalError(fmt.Errorf("the result of job %s cannot be read: %v", jobID, err))
	}
	top["job"], top["status"] = rawString(jobID), rawString(string(jobDone))
	if _, ok := top["derived"]; !ok {
		top["derived"] = rawString(derived)
	}
	if derived == derivedReady && !s.handlesOpen(b) {
		top["handles"] = rawString(handlesReopen)
		if recipe != nil {
			if rb, err := json.Marshal(recipe); err == nil {
				top["recompute"] = rb
			}
		}
	}
	out, err := marshal(top)
	if err != nil {
		return internalError(fmt.Errorf("the result of job %s cannot be encoded: %v", jobID, err))
	}
	return s.fitResult(out)
}

func rawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// handlesOpen reports whether every handle result b names is still open
// in the store, read from the map the store publishes for any goroutine.
func (s *Server) handlesOpen(b []byte) bool {
	var r struct {
		Profiles []struct {
			Open *struct {
				Handle string `json:"handle"`
			} `json:"open"`
		} `json:"profiles"`
		Compare *struct {
			Base, New struct {
				Handle string `json:"handle"`
			}
		} `json:"compare"`
	}
	if json.Unmarshal(b, &r) != nil {
		return false
	}
	var hs []string
	for _, p := range r.Profiles {
		if p.Open != nil {
			hs = append(hs, p.Open.Handle)
		}
	}
	if r.Compare != nil {
		hs = append(hs, r.Compare.Base.Handle, r.Compare.New.Handle)
	}
	open := map[string]bool{}
	for _, h := range s.store.openNames() {
		open[string(h)] = true
	}
	for _, h := range hs {
		if h != "" && !open[h] {
			return false
		}
	}
	return true
}

// errNoCapture is a capture id with no manifest.
var errNoCapture = errors.New("no such capture")

// waitHistorical answers the capture key from its manifest: from the
// registry when it was read before, else through the lookup pool, waiting
// at most d. One not read by then, or a full pool, is loading, named by
// asked, the id the client used.
func (s *Server) waitHistorical(ctx context.Context, key, jobID string, d time.Duration, asked string) mcpstdio.Result {
	if h, ok := s.registry.historical(key); ok {
		gone, ready := s.recheck(ctx, key, h.m.Label, d)
		switch {
		case !ready:
			return loadingAnswer(asked)
		case !gone:
			return s.historicalAnswer(key, jobID, h)
		}
		// Another server's retention, or a person, removed it.
		s.registry.forget(key)
	}
	l, ok := s.lookups.get(key, func() (any, error) { return s.loadHistorical(key) })
	if !ok {
		return loadingAnswer(asked)
	}
	v, ready, err := l.wait(ctx, d)
	if ip, ok := errors.AsType[*inProgressError](err); ok {
		if !ip.live {
			s.wantRecovery()
		}
		return ip.answer(asked, key)
	}
	switch {
	case !ready:
		return loadingAnswer(asked)
	case errors.Is(err, errNoCapture) && !s.recovered.Load():
		// The start-up recovery has not run yet: the capture may be one a
		// dead server left, which recovery is yet to record.
		return loadingAnswerNext(asked, loadingRecoveryNext)
	case errors.Is(err, errNoCapture):
		return toolError(unknownCapture(key, "no capture of this server's capture directory", unknownNext), nil)
	case err != nil:
		return toolError(unknownCapture(key, "its manifest cannot be read: "+causeText(err),
			"tell the user: the capture's manifest in the capture directory cannot be read; capture_list shows the series' other captures"), nil)
	}
	return s.historicalAnswer(key, jobID, v.(*historical))
}

// recheckWait bounds the re-check of a manifest before an answer from
// memory: one Lstat, which only a hung filesystem makes slow. recheckFloor
// is what it gets even when the call's own budget is spent (its timer
// fired, or it queued that long), so a settled job is not answered
// loading for want of the time one Lstat takes.
const (
	recheckWait  = time.Second
	recheckFloor = 50 * time.Millisecond
)

// recheck reports whether capture key's manifest (with label) is gone. It
// runs on the lookup pool, shared by the calls that ask at once, and waits
// d, but at least recheckFloor and at most recheckWait: ready is false when
// it could not tell in time, or the pool is full, and the caller answers
// loading.
func (s *Server) recheck(ctx context.Context, key, label string, d time.Duration) (gone, ready bool) {
	l, ok := s.lookups.get("gone:"+key, func() (any, error) { return s.manifestGone(key, label), nil })
	if !ok {
		return false, false
	}
	v, ready, _ := l.wait(ctx, min(max(d, recheckFloor), recheckWait))
	if !ready {
		return false, false
	}
	gone, _ = v.(bool)
	return gone, true
}

// manifestGone reports, with one Lstat, that capture key's manifest (with
// label) is no longer in its series, so an answer kept in memory must not
// be given. A check that cannot tell says it is there.
func (s *Server) manifestGone(key, label string) bool {
	if s.lookupHook != nil {
		s.lookupHook("gone:" + key)
	}
	series, id, ok := splitCaptureKey(key)
	if !ok {
		return false
	}
	d, err := s.readCaptureDir()
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	defer d.close()
	_, err = d.root.Lstat(filepath.Join(series, manifestName(id, label)))
	return errors.Is(err, fs.ErrNotExist)
}

// The next of a loading answer: a read not done in time, and a capture id
// asked before this server's start-up recovery has run.
const (
	loadingNext = "call again after retry_after; a capture that still answers loading after a minute is on a filesystem " +
		"that does not answer: stop waiting and tell the user"
	loadingRecoveryNext = "this server has not recovered the capture directory yet (another process holds its lock, " +
		"at most until that capture's deadline); call again after retry_after, and if it still answers loading after a minute, " +
		"capture_list shows the series' history"
)

// loadingAnswer is an answer that is still being read, for the id the
// client asked by.
func loadingAnswer(id string) mcpstdio.Result { return loadingAnswerNext(id, loadingNext) }

func loadingAnswerNext(id, next string) mcpstdio.Result {
	r, _ := answer(struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		RetryAfter string `json:"retry_after"`
		Next       string `json:"next"`
	}{id, statusLoading, loadingRetry.String(), next})
	return r
}

// statusInProgress is the status of a capture whose intent is on disk and
// whose manifest is not yet.
const statusInProgress = "in_progress"

// inProgressRetry is the retry_after of an in_progress answer when the
// captures lock names no end for it.
const inProgressRetry = 5 * time.Second

// inProgressError is a capture id whose intent is on disk and whose
// manifest is not: its owner, another process, is writing it, or recovery
// has yet to record it. It is never kept: the next wait reads the disk
// again.
type inProgressError struct {
	in    intent
	live  bool
	retry time.Duration
}

func (e *inProgressError) Error() string { return "the capture is in progress" }

// inProgress is capture key's (id's) in-progress state in ser, when its
// intent is there: whether its owner (by its recorded server and reaper)
// is alive, and when to ask again, from the captures lock's record when it
// names this capture.
func (s *Server) inProgress(d *captureDir, ser *os.Root, key string, id int) (*inProgressError, bool) {
	names, err := readDirNames(ser, ".")
	if err != nil {
		return nil, false
	}
	for _, n := range names {
		got, _, kind, ok := parseName(n)
		if !ok || got != id || kind != kindIntent {
			continue
		}
		in, _, err := readIntent(ser, n)
		if err != nil {
			return nil, false
		}
		ip := &inProgressError{in: in, live: s.ownerIsAlive(in), retry: inProgressRetry}
		if h, ok := capture.ReadHolder(d.root); ok && h.CaptureID == key && time.Until(h.Deadline) > 0 {
			ip.retry = capture.BusyRetry(time.Until(h.Deadline))
		}
		return ip, true
	}
	return nil, false
}

// answer is the in_progress answer for key, asked by asked: the server
// (and reaper) writing it, and what to do.
func (e *inProgressError) answer(asked, key string) mcpstdio.Result {
	holder := fmt.Sprintf("perf-agent-mcp pid %d", e.in.ServerPID)
	if e.in.ReaperPID > 0 {
		holder += fmt.Sprintf(" (reaper pid %d)", e.in.ReaperPID)
	}
	next := "another perf-agent-mcp is running this capture; call capture_wait again after retry_after: " +
		"once it is committed or has failed, the answer comes from its manifest"
	if !e.live {
		holder += ", which is gone"
		next = "the server that ran this capture is gone; this server's recovery records it as interrupted once the captures lock is free " +
			"(a reaper of that server holds it at most until the capture's deadline); call capture_wait again after retry_after"
	}
	r, _ := answer(struct {
		ID         string `json:"id"`
		Capture    string `json:"capture_id"`
		Status     string `json:"status"`
		RetryAfter string `json:"retry_after"`
		Next       string `json:"next"`
		Holder     string `json:"holder"`
	}{asked, key, statusInProgress, capture.CeilSecond(e.retry).String(), next, holder})
	return r
}

// historicalAnswer is a capture read back from its manifest: a committed
// one's result, whose derived results are to be recomputed with the calls
// it carries, or a failed one's error. jobID, when known, is the job of
// this server that made it.
func (s *Server) historicalAnswer(key, jobID string, h *historical) mcpstdio.Result {
	m := h.m
	if m.Outcome != captureOK {
		var e *capture.Error
		status := string(jobFailed)
		switch {
		case m.Outcome == captureInterrupted:
			// The server ended under the capture: nothing about the
			// capture itself failed, so it may be run again at once.
			e = capture.Errorf(capture.CodeCancelled, "capture again",
				"the server running capture %s ended before it finished; recovery removed what it had written", key)
			e.Retryable, e.RetryAfter = true, 0
			status = captureInterrupted
		case m.Error == nil:
			e = capture.Errorf(capture.CodeNoOutput, "capture again; capture_list shows the series' history",
				"capture %s: the capture failed without a recorded cause", key)
			e.Retryable, e.RetryAfter = true, 0
		default:
			e = errorFromWire(m.Error)
			if e.Code == capture.CodeCancelled {
				status = string(jobCancelled)
			}
		}
		w := toWireError(e)
		w.Job, w.CaptureID, w.Status = jobID, key, status
		return errorResult(w)
	}
	res := captureResult{
		Capture: s.captureInfo(m), Notes: m.Notes, Cancel: m.Cancel,
		Derived: derivedRecompute, Handles: handlesReopen, Recompute: new(s.recomputeFor(m, h.base, h.baseGone)),
	}
	for _, mode := range modesOf(m) {
		path, execlog := s.profilePaths(m, mode)
		res.Profiles = append(res.Profiles, profileResult{Mode: string(mode), Path: path, Execlog: execlog})
	}
	b, err := json.Marshal(res)
	if err != nil {
		return internalError(fmt.Errorf("the answer for %s cannot be encoded: %v", key, err))
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return internalError(err)
	}
	top["status"] = rawString(string(jobDone))
	if jobID != "" {
		top["job"] = rawString(jobID)
	}
	out, err := marshal(top)
	if err != nil {
		return internalError(err)
	}
	return s.fitResult(out)
}

// readCaptureDir opens the capture directory to read it, creating nothing:
// an error wrapping fs.ErrNotExist means there is none yet.
func (s *Server) readCaptureDir() (*captureDir, error) {
	rel, err := s.rel(s.profiles, s.captureDirName())
	if err != nil {
		return nil, errors.New("the capture directory is not inside the profiles root")
	}
	rel = filepath.Clean(rel)
	if !filepath.IsLocal(rel) || rel == "." {
		return nil, errors.New("the capture directory is not inside the profiles root")
	}
	root, err := s.profiles.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	return &captureDir{root: root, rel: rel, owner: s.ownerKind(), profiles: s.profiles.Name()}, nil
}

// loadHistorical reads capture key's manifest, and its baseline's when it
// was compared with one, on a lookup worker, and keeps the answer in the
// registry for the next wait. A write to the capture directory during the
// read (retention removing this capture, say) makes the answer stale: it
// is read again, and an answer still overtaken after that is returned to
// the waits that asked, but never kept.
func (s *Server) loadHistorical(key string) (any, error) {
	if s.lookupHook != nil {
		s.lookupHook(key)
	}
	for try := 1; ; try++ {
		gen := s.historyGen.Load()
		h, err := s.readHistorical(key)
		if err != nil {
			return nil, err
		}
		if s.readHook != nil {
			s.readHook(key)
		}
		kept, err := s.registry.putIf(key, h, func() bool { return s.historyGen.Load() == gen })
		if err != nil {
			s.log.Error("capture registry", "capture", key, "err", err)
		}
		if kept || err != nil || try == historicalReads {
			return h, nil
		}
	}
}

// historicalReads bounds how often a historical read overtaken by a write
// to the capture directory is read again.
const historicalReads = 3

// readHistorical reads capture key's manifest, and its baseline's.
func (s *Server) readHistorical(key string) (*historical, error) {
	series, id, ok := splitCaptureKey(key)
	if !ok {
		return nil, errNoCapture
	}
	d, err := s.readCaptureDir()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoCapture
	}
	if err != nil {
		return nil, err
	}
	defer d.close()
	if _, err := d.root.Lstat(series); errors.Is(err, fs.ErrNotExist) {
		return nil, errNoCapture
	}
	ser, err := d.openSeries(series)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ser.Close() }()
	m, err := manifestByID(ser, id)
	if errors.Is(err, errNoCapture) {
		if ip, ok := s.inProgress(d, ser, key, id); ok {
			return nil, ip
		}
	}
	if err != nil {
		return nil, err
	}
	h := &historical{m: m}
	if m.Outcome == captureOK && m.CompareWith != "" {
		bid, _ := parseID(m.CompareWith)
		switch bm, err := manifestByID(ser, bid); {
		case errors.Is(err, errNoCapture):
			h.baseGone = fmt.Sprintf("baseline %s is gone: its manifest is no longer in series %s (retention or a person removed it)", m.CompareWith, series)
		case err != nil:
			h.baseGone = fmt.Sprintf("baseline %s cannot be read: %s", m.CompareWith, causeText(err))
		case bm.Outcome != captureOK:
			h.baseGone = fmt.Sprintf("baseline %s is %s and has no profile", m.CompareWith, bm.Outcome)
		default:
			h.base = &bm
		}
	}
	return h, nil
}

// manifestByID reads the manifest of capture id in a series. A capture
// with no manifest, or one whose eviction has left only its marker, is
// errNoCapture.
func manifestByID(ser *os.Root, id int) (manifest, error) {
	names, err := readDirNames(ser, ".")
	if err != nil {
		return manifest{}, err
	}
	slices.Sort(names)
	for _, n := range names {
		if got, _, kind, ok := parseName(n); ok && got == id && kind == kindManifest {
			return readManifest(ser, n)
		}
	}
	return manifest{}, errNoCapture
}

func (s *Server) captureCancelTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name:  "capture_cancel",
		Title: "Cancel a capture",
		Description: s.captureDescription("Cancel a capture job, by its job id or capture id. It is answered at once, ahead of queued calls: " +
			`"cancelling" (the job stops its processes and rolls back; capture_wait reports the end), "cancelling (decision pending)" ` +
			`(its commit could not be read yet; the outcome is reported once it is), "too late: committed", "too late: failed" or "cancelled".`),
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "The job id from capture, or the capture id (series/NNN) of a job of this server."}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
		Annotations: mcpstdio.Annotations{Destructive: true, Idempotent: true},
		Handler:     s.handleCancel,
		Control:     true,
	}
}

// cancelAnswer is capture_cancel's answer, with the job's capture id
// (series/NNN) once it has one.
type cancelAnswer struct {
	Job    string `json:"job"`
	ID     string `json:"capture_id,omitzero"`
	Status string `json:"status"`
}

// handleCancel runs on the reader, beside the request being served: it
// reads the registry and asks the job to stop (a CAS and a channel close),
// and touches nothing else. An id of no job of this server is
// unknown_capture, answered from memory.
func (s *Server) handleCancel(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.captureDisabled != "" {
		return toolError(s.disabledError(), nil), nil
	}
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs("capture_cancel", raw, &a); err != nil {
		return toolError(invalidArgument(argsNext, "%s", err.Error()), nil), nil
	}
	if a.ID == "" {
		return toolError(invalidArgument("pass the job id capture answered", "capture_cancel needs id"), nil), nil
	}
	if e := bareCaptureID("capture_cancel", a.ID); e != nil {
		return toolError(e, nil), nil
	}
	out := cancelAnswer{Job: a.ID}
	if j, ok := s.registry.get(a.ID); ok {
		out.Job, out.ID = j.id, j.captureIDNow()
	} else if al, ok := s.registry.final(a.ID); ok {
		out.Job, out.ID = al.jobID, al.captureID
	}
	status, ok := s.registry.cancel(a.ID)
	if !ok {
		return toolError(unknownCapture(a.ID, "no job of this server: only this server's own running jobs can be cancelled",
			"capture_wait answers a capture another server ran or this server no longer holds; capture_list shows the running job"), nil), nil
	}
	out.Status = status
	return answer(out)
}

// What a cut result's one note says was cut, and what to call for it; and
// the note of a result cut to its paths.
const (
	cutOpenPart     = "each profile's open was cut to its handle, views, default_view and quality"
	cutOpenMinPart  = "each profile's open was cut to its handle and default_view"
	cutRowsPart     = "the compare's rows were left out"
	cutViewsPart    = "each compare view was cut to its totals and next"
	cutTailPart     = "the workload's output tail was left out"
	cutOpenAdvice   = "call open with each profile's path for the full result"
	cutDiffAdvice   = "call diff with each view's next for the rows"
	cutMinimalNote  = "the result is over MaxResponseBytes even cut: call open with each profile's path, and diff, for its contents"
	cutNoteContains = "to fit MaxResponseBytes"
)

// cutState is what cutResult has cut so far.
type cutState struct{ opens, opensMin, rows, views, tail bool }

// note is the one note that says everything cut and what to call for it.
func (c cutState) note() string {
	var parts, advice []string
	switch {
	case c.opensMin:
		parts = append(parts, cutOpenMinPart)
	case c.opens:
		parts = append(parts, cutOpenPart)
	}
	switch {
	case c.views:
		parts = append(parts, cutViewsPart)
	case c.rows:
		parts = append(parts, cutRowsPart)
	}
	if c.tail {
		parts = append(parts, cutTailPart)
	}
	if c.opens || c.opensMin {
		advice = append(advice, cutOpenAdvice)
	}
	if c.rows || c.views {
		advice = append(advice, cutDiffAdvice)
	}
	n := strings.Join(parts, "; ") + ", " + cutNoteContains
	if len(advice) > 0 {
		n += ": " + strings.Join(advice, ", and ")
	}
	return n
}

// fitResult is the result b as an answer, cut to MaxResponseBytes; one
// that cannot be cut that far is a short error saying how to read it.
func (s *Server) fitResult(b []byte) mcpstdio.Result {
	max := s.lim.MaxResponseBytes
	out := s.cutResult(b, max)
	if len(out) <= max {
		return mcpstdio.Result{Text: string(out)}
	}
	return toolError(capture.Errorf(capture.CodeTooLarge,
		"restart perf-agent-mcp with a larger --limit MaxResponseBytes=N; capture_list shows the capture's paths, which open takes",
		"the capture's result is over MaxResponseBytes=%d even cut to its paths", max), nil)
}

// cutResult cuts the result b to max bytes (§4.3), a step at a time, with
// one note saying all that was cut: the opens to their handle, views, default view
// and quality; the compare's rows; the workload's output tail; the opens
// to their handle and default view and the compare views to their totals
// and next; at last everything but what finds the capture and its
// profiles again, and the recompute calls when it has them.
func (s *Server) cutResult(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return b
	}
	var notes []string
	_ = json.Unmarshal(top["notes"], &notes)
	own := len(notes)
	var c cutState
	steps := []func() bool{
		func() bool { c.opens = cutOpens(top, "handle", "views", "default_view", "quality"); return c.opens },
		func() bool { c.rows = cutCompareViews(top, "rows"); return c.rows },
		func() bool { c.tail = cutKey(top, "capture", "workload_output_tail"); return c.tail },
		func() bool {
			c.opensMin = cutOpens(top, "handle", "default_view")
			c.views = cutCompareViews(top, "view", "base_total", "new_total", "next")
			return c.opensMin || c.views
		},
	}
	for _, cut := range steps {
		if !cut() {
			continue
		}
		// One note says all that was cut: each step's replaces the last.
		notes = append(notes[:own], c.note())
		top["notes"], _ = json.Marshal(notes)
		if out, err := marshal(top); err == nil && len(out) <= max {
			return out
		}
	}
	minimal := map[string]json.RawMessage{"notes": mustRaw([]string{cutMinimalNote})}
	for _, k := range []string{"job", "status", "derived", "derive_error", "handles", "cancel", "recompute"} {
		if v, ok := top[k]; ok {
			minimal[k] = v
		}
	}
	var ci map[string]json.RawMessage
	if json.Unmarshal(top["capture"], &ci) == nil {
		kept := map[string]json.RawMessage{}
		for _, k := range []string{"id", "capture", "series", "label", "manifest"} {
			if v, ok := ci[k]; ok {
				kept[k] = v
			}
		}
		minimal["capture"] = mustRaw(kept)
	}
	var ps []map[string]json.RawMessage
	if json.Unmarshal(top["profiles"], &ps) == nil {
		for i, p := range ps {
			ps[i] = keepKeys(p, "mode", "path", "execlog")
		}
		minimal["profiles"] = mustRaw(ps)
	}
	out, err := marshal(minimal)
	if err == nil && len(out) > max {
		// The recipe is the longest part left; the paths it opens are in
		// profiles.
		delete(minimal, "recompute")
		out, err = marshal(minimal)
	}
	if err != nil {
		return b
	}
	return out
}

func mustRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// keepKeys is m with only keys.
func keepKeys(m map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// cutOpens keeps only keys of each profile's open, and reports whether it
// removed anything.
func cutOpens(top map[string]json.RawMessage, keys ...string) bool {
	var ps []map[string]json.RawMessage
	if json.Unmarshal(top["profiles"], &ps) != nil {
		return false
	}
	cut := false
	for _, p := range ps {
		var o map[string]json.RawMessage
		if json.Unmarshal(p["open"], &o) != nil || o == nil {
			continue
		}
		if k := keepKeys(o, keys...); len(k) < len(o) {
			p["open"], cut = mustRaw(k), true
		}
	}
	if cut {
		top["profiles"] = mustRaw(ps)
	}
	return cut
}

// cutCompareViews changes each compare view: with one key it drops that
// key, with several it keeps only them. It reports whether it removed
// anything.
func cutCompareViews(top map[string]json.RawMessage, keys ...string) bool {
	var c map[string]json.RawMessage
	if json.Unmarshal(top["compare"], &c) != nil || c == nil {
		return false
	}
	var vs []map[string]json.RawMessage
	if json.Unmarshal(c["views"], &vs) != nil {
		return false
	}
	cut := false
	for i, v := range vs {
		if len(keys) == 1 {
			if _, ok := v[keys[0]]; ok {
				delete(v, keys[0])
				cut = true
			}
			continue
		}
		if k := keepKeys(v, keys...); len(k) < len(v) {
			vs[i], cut = k, true
		}
	}
	if cut {
		c["views"] = mustRaw(vs)
		top["compare"] = mustRaw(c)
	}
	return cut
}

// cutKey drops key from the object top[in], and reports whether it was
// there.
func cutKey(top map[string]json.RawMessage, in, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(top[in], &m) != nil {
		return false
	}
	if _, ok := m[key]; !ok {
		return false
	}
	delete(m, key)
	top[in] = mustRaw(m)
	return true
}
