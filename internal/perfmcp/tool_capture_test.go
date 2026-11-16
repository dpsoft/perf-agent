package perfmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// newCapEnv is a capture-enabled server over the fake perf-agent, served
// over pipes and initialized.
func newCapEnv(t *testing.T, o jobOpts) (*jobEnv, *testClient) {
	t.Helper()
	e := newJobEnv(t, o)
	return e, e.serve()
}

// workloadCmd is the test workload sleeping, as a command argument.
func workloadCmd(t *testing.T) []any {
	return []any{capturetest.Workload(t), "sleep", "120s"}
}

// startCapture calls capture with args, which must be accepted, and
// returns the job id.
func startCapture(t *testing.T, c *testClient, args map[string]any) string {
	t.Helper()
	var a struct {
		Job    string `json:"job"`
		Status string `json:"status"`
		ETA    string `json:"eta"`
	}
	c.callOK("capture", args, &a)
	if a.Status != "accepted" || a.Job == "" {
		t.Fatalf("capture answered %+v", a)
	}
	return a.Job
}

// toolErrorOf decodes a capture tool's error text, which must be exactly
// one {"error": {...}} object.
func toolErrorOf(t *testing.T, text string) wireError {
	t.Helper()
	var out struct {
		Error *wireError `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil || out.Error == nil || dec.More() {
		t.Fatalf("not one {\"error\": …} object: %s (%v)", text, err)
	}
	return *out.Error
}

// isResult reports whether a capture_wait answer is a done capture's
// result, not a state.
func isResult(t *testing.T, text string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("capture_wait answered %s: %v", text, err)
	}
	_, ok := m["capture"]
	return ok
}

// waitEnd calls capture_wait until the job ends: its result, or its
// error.
func waitEnd(t *testing.T, c *testClient, id string) (text string, isErr bool) {
	t.Helper()
	end := time.Now().Add(90 * time.Second)
	for time.Now().Before(end) {
		text, isErr = c.call("capture_wait", map[string]any{"id": id, "timeout": "10s"})
		if isErr || isResult(t, text) {
			return text, isErr
		}
	}
	t.Fatalf("%s never ended: %s", id, text)
	return "", false
}

// waitResult is the result of a job that must end done.
func waitResult(t *testing.T, c *testClient, id string) (captureResult, string) {
	t.Helper()
	text, isErr := waitEnd(t, c, id)
	if isErr {
		t.Fatalf("%s failed: %s", id, text)
	}
	var r captureResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("result %s: %v", text, err)
	}
	return r, text
}

// waitStatus calls capture_wait with short timeouts until the job's state
// passes ok.
func waitStatus(t *testing.T, c *testClient, id string, ok func(jobView) bool) jobView {
	t.Helper()
	end := time.Now().Add(30 * time.Second)
	var v jobView
	for time.Now().Before(end) {
		text, isErr := c.call("capture_wait", map[string]any{"id": id, "timeout": "200ms"})
		if isErr || isResult(t, text) {
			t.Fatalf("%s ended: %s", id, text)
		}
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatal(err)
		}
		if ok(v) {
			return v
		}
	}
	t.Fatalf("%s never reached the state: %+v", id, v)
	return v
}

func profiling(v jobView) bool { return v.Phase == capture.PhaseProfiling }

// Without --perf-agent the four tools are listed, each description starts
// with the DISABLED text, and every call is a disabled error with it.
func TestCaptureIsDisabledWithoutPerfAgent(t *testing.T) {
	c := newTestServer(t, testConfig(t))
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.rpc("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tl := range list.Tools {
		if !slices.Contains(captureToolNames, tl.Name) {
			continue
		}
		seen++
		if !strings.HasPrefix(tl.Description, captureDisabledText) {
			t.Errorf("%s: description %q", tl.Name, tl.Description)
		}
	}
	if seen != len(captureToolNames) {
		t.Fatalf("listed %d capture tools", seen)
	}
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"capture", map[string]any{"pid": 1}},
		{"capture_wait", map[string]any{"id": "j1-0000"}},
		{"capture_cancel", map[string]any{"id": "j1-0000"}},
		{"capture_list", map[string]any{}},
	} {
		text, isErr := c.call(call.tool, call.args)
		if !isErr {
			t.Fatalf("%s answered %s", call.tool, text)
		}
		if w := toolErrorOf(t, text); w.Code != string(capture.CodeDisabled) || w.Message != captureDisabledText || w.Retryable {
			t.Errorf("%s: %+v", call.tool, w)
		}
	}
}

// capture only admits the job: it answers accepted in microseconds,
// whatever the job then waits for.
func TestCaptureAcceptsInMicroseconds(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	g := e.faults.block(t, faultAfterLease)
	raw := json.RawMessage(mustJSON(t, map[string]any{"command": workloadCmd(t), "duration": "1m"}))
	begin := time.Now()
	res, err := e.s.handleCapture(t.Context(), raw)
	took := time.Since(begin)
	if err != nil || res.IsError {
		t.Fatalf("capture: %v %s", err, res.Text)
	}
	if took > 20*time.Millisecond {
		t.Errorf("capture took %v", took)
	}
	t.Logf("capture answered in %v", took)
	var a map[string]string
	if err := json.Unmarshal([]byte(res.Text), &a); err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 || a["status"] != "accepted" || a["eta"] != "1m5s" {
		t.Fatalf("answer %s", res.Text)
	}
	j, ok := e.s.registry.get(a["job"])
	if !ok || j != e.s.registry.active.Load() {
		t.Fatalf("job %s is not the active one", a["job"])
	}
	g.reached(t)
	j.requestCancel()
	g.open()
	e.finish(j)
}

// Every argument is checked before a job is admitted: a refusal admits
// nothing and touches no file.
func TestCaptureArgumentRefusals(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	cmd := workloadCmd(t)
	with := func(kv ...any) map[string]any {
		m := map[string]any{"command": cmd}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for _, c0 := range []struct {
		name string
		args map[string]any
		code capture.Code
		says string
	}{
		{"neither pid nor command", map[string]any{}, capture.CodeInvalidArgument, "pid or command"},
		{"pid and command", with("pid", 1), capture.CodeInvalidArgument, "pid or command"},
		{"pid zero", map[string]any{"pid": 0}, capture.CodeInvalidArgument, "pid"},
		{"pid negative", map[string]any{"pid": -3}, capture.CodeInvalidArgument, "pid"},
		{"pid as a string", map[string]any{"pid": "12"}, capture.CodeInvalidArgument, "pid"},
		{"pid over pid_max", map[string]any{"pid": 1 << 40}, capture.CodeInvalidArgument, "pid"},
		{"empty command", map[string]any{"command": []any{}}, capture.CodeInvalidArgument, "command"},
		{"empty program", map[string]any{"command": []any{""}}, capture.CodeInvalidArgument, "command"},
		{"a NUL byte", map[string]any{"command": []any{"/bin/true", "a\x00b"}}, capture.CodeInvalidArgument, "NUL"},
		{"cwd with pid", map[string]any{"pid": 1, "cwd": "/tmp"}, capture.CodeInvalidArgument, "cwd"},
		{"a mode twice", with("modes", []any{"cpu", "cpu"}), capture.CodeInvalidArgument, "modes"},
		{"an unknown mode", with("modes", []any{"lbr"}), capture.CodeInvalidArgument, "modes"},
		{"no mode", with("modes", []any{}), capture.CodeInvalidArgument, "modes"},
		{"gpu without a shim", with("modes", []any{"cpu", "gpu"}), capture.CodeDisabled, "--gpu-shim"},
		{"a duration that is not one", with("duration", "ten"), capture.CodeInvalidArgument, "duration"},
		{"a duration under 1s", with("duration", "500ms"), capture.CodeInvalidArgument, "duration"},
		{"a duration over MaxDuration", with("duration", "10m"), capture.CodeInvalidArgument, "MaxDuration"},
		{"sample_rate zero", with("sample_rate", 0), capture.CodeInvalidArgument, "sample_rate"},
		{"sample_rate over the limit", with("sample_rate", 100000), capture.CodeInvalidArgument, "MaxSampleRate"},
		{"an unknown unwind", with("unwind", "lbr"), capture.CodeInvalidArgument, "unwind"},
		{"kernel stacks without cap_syslog", with("kernel_stacks", true), capture.CodeInvalidArgument, "cap_syslog"},
		{"a series starting with a dot", with("series", ".x"), capture.CodeInvalidArgument, "series"},
		{"a series with a slash", with("series", "a/b"), capture.CodeInvalidArgument, "series"},
		{"a series too long", with("series", strings.Repeat("s", 65)), capture.CodeInvalidArgument, "series"},
		{"a label too long", with("label", strings.Repeat("l", 33)), capture.CodeInvalidArgument, "label"},
		{"compare_with not canonical", with("compare_with", "2"), capture.CodeInvalidArgument, "compare_with"},
		{"compare_with not an id", with("compare_with", "base"), capture.CodeInvalidArgument, "compare_with"},
		{"compare_with with no series", with("compare_with", "/001"), capture.CodeInvalidArgument, "not a capture id"},
		{"compare_with with a path after", with("compare_with", "adhoc/001/x"), capture.CodeInvalidArgument, "not a capture id"},
		{"compare_with in another series", with("series", "a", "compare_with", "b/001"), capture.CodeInvalidArgument, "series"},
		{"an unknown argument", with("env", []any{"A=1"}), capture.CodeInvalidArgument, "env"},
	} {
		t.Run(c0.name, func(t *testing.T) {
			text, isErr := c.call("capture", c0.args)
			if !isErr {
				t.Fatalf("accepted: %s", text)
			}
			w := toolErrorOf(t, text)
			if w.Code != string(c0.code) || w.Retryable || !strings.Contains(w.Message+" "+w.Next, c0.says) {
				t.Errorf("%+v", w)
			}
			if strings.ContainsRune(w.Message, '\n') {
				t.Errorf("a message of more than one line: %q", w.Message)
			}
			if e.s.registry.active.Load() != nil {
				t.Fatal("a job was admitted")
			}
		})
	}
	if _, ok := e.dir(); ok {
		t.Error("a refused capture created the capture directory")
	}

	// A capture directory known to belong to the other kind is refused
	// before anything else is looked at.
	e.s.ownerSeen.Store(new(ownerServe))
	text, _ := c.call("capture", map[string]any{"pid": 0})
	if w := toolErrorOf(t, text); w.Code != string(capture.CodeOwnerMismatch) {
		t.Errorf("%+v", w)
	}
}

// command ["./bench"] with cwd is refused at once, and next gives the
// absolute path joined from cwd, with no I/O.
func TestCaptureRelativeCommandSuggestsTheAbsolutePath(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	for _, tc := range []struct {
		cmd, cwd, want string
	}{
		{"./bench", "/home/u/proj", `"/home/u/proj/bench"`},
		{"bin/bench", "/home/u/proj", `"/home/u/proj/bin/bench"`},
		{"../tools/bench", "/home/u/proj", `"/home/u/tools/bench"`},
		{"./bench", "/home/u/a\nb", `"/home/u/a\nb/bench"`},
	} {
		text, isErr := c.call("capture", map[string]any{"command": []any{tc.cmd, "-n", "3"}, "cwd": tc.cwd})
		if !isErr {
			t.Fatalf("%s accepted", tc.cmd)
		}
		w := toolErrorOf(t, text)
		if w.Code != string(capture.CodeInvalidArgument) || !strings.Contains(w.Message, tc.cmd) || !strings.Contains(w.Next, "pass "+tc.want) {
			t.Errorf("%s: %+v", tc.cmd, w)
		}
		if strings.ContainsAny(w.Message+w.Next, "\n\r") {
			t.Errorf("a control character is echoed raw: %q", w.Next)
		}
	}
	// ~ is the shell's: it is never joined with cwd.
	text, _ := c.call("capture", map[string]any{"command": []any{"~/bin/b"}, "cwd": "/home/u"})
	if w := toolErrorOf(t, text); w.Code != string(capture.CodeInvalidArgument) || strings.Contains(w.Next, "/home/u/~") || !strings.Contains(w.Next, "absolute path") {
		t.Errorf("~: %+v", w)
	}
	// Without an absolute cwd there is nothing to join that is the agent's
	// own: the advice names no path of the server's.
	for _, cwd := range []string{"", "proj"} {
		text, _ := c.call("capture", map[string]any{"command": []any{"./bench"}, "cwd": cwd})
		w := toolErrorOf(t, text)
		if w.Code != string(capture.CodeInvalidArgument) || !strings.Contains(w.Next, "absolute path") || strings.Contains(text, e.profiles) {
			t.Errorf("cwd %q: %+v", cwd, w)
		}
	}
	if e.s.registry.active.Load() != nil {
		t.Fatal("a job was admitted")
	}
	if _, ok := e.dir(); ok {
		t.Error("the capture directory was created")
	}
}

// capture_wait's timeout is a Go duration in (0, 45s], 30s by default; a
// wait returns the current state when it ends first.
func TestCaptureWaitTimeouts(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 30 * time.Second, "45s": 45 * time.Second, "250ms": 250 * time.Millisecond} {
		if d, e := waitTimeout(raw); e != nil || d != want {
			t.Errorf("waitTimeout(%q) = %v, %v", raw, d, e)
		}
	}
	_, c := newCapEnv(t, jobOpts{})
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
	for _, bad := range []string{"0s", "-1s", "46s", "1h", "ten", "30"} {
		text, isErr := c.call("capture_wait", map[string]any{"id": id, "timeout": bad})
		if !isErr {
			t.Fatalf("timeout %q accepted: %s", bad, text)
		}
		if w := toolErrorOf(t, text); w.Code != string(capture.CodeInvalidArgument) || !strings.Contains(w.Message, "45s") {
			t.Errorf("timeout %q: %+v", bad, w)
		}
	}
	begin := time.Now()
	text, isErr := c.call("capture_wait", map[string]any{"id": id, "timeout": "300ms"})
	took := time.Since(begin)
	if isErr || isResult(t, text) {
		t.Fatalf("a 1m capture ended: %s", text)
	}
	var v jobView
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	if v.Job != id || (v.Status != string(jobPreparing) && v.Status != string(jobRunning)) || v.ETA == "" || v.Elapsed == "" {
		t.Errorf("view %s", text)
	}
	if took < 300*time.Millisecond || took > 5*time.Second {
		t.Errorf("a 300ms wait took %v", took)
	}
	for _, unknown := range []string{"j9-ffff", "nope/001", "001"} {
		text, isErr := c.call("capture_wait", map[string]any{"id": unknown, "timeout": "1s"})
		if !isErr {
			t.Fatalf("%s answered %s", unknown, text)
		}
		if w := toolErrorOf(t, text); w.Code != string(capture.CodeUnknownCapture) || !strings.Contains(w.Next, "capture_list") {
			t.Errorf("%s: %+v", unknown, w)
		}
	}
	c.callOK("capture_cancel", map[string]any{"id": id}, nil)
	begin = time.Now()
	text, isErr = c.call("capture_wait", map[string]any{"id": id, "timeout": "45s"})
	if !isErr || toolErrorOf(t, text).Code != string(capture.CodeCancelled) || time.Since(begin) > 20*time.Second {
		t.Errorf("after the cancel the wait answered %s after %v", text, time.Since(begin))
	}
}

// While capture_wait blocks the dispatcher on a long capture, the requests
// behind it queue and are answered in order once it returns its state.
func TestCaptureWaitDuringALongCaptureWithLoad(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	writeFixture(t, e.profiles, "cpu.pb.gz", pmfixture.CPUBasic())
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
	waitStatus(t, c, id, profiling)
	begin := time.Now()
	wait := c.sendTool("capture_wait", map[string]any{"id": id, "timeout": "3s"}, nil)
	var queued []int
	for i := range 6 {
		if i%2 == 0 {
			queued = append(queued, c.send("tools/list", map[string]any{}))
		} else {
			queued = append(queued, c.sendTool("open", map[string]any{"path": "cpu.pb.gz"}, nil))
		}
	}
	list := c.sendTool("capture_list", map[string]any{}, nil)
	res, _ := c.recv(wait)
	text, isErr := c.toolText(res)
	if took := time.Since(begin); took < 3*time.Second || took > 5*time.Second {
		t.Errorf("the 3s wait took %v", took)
	}
	var v jobView
	if isErr || json.Unmarshal([]byte(text), &v) != nil || v.Status != string(jobRunning) || v.Phase != capture.PhaseProfiling {
		t.Errorf("the wait answered %s", text)
	}
	for i, q := range queued {
		res, _ := c.recv(q)
		if res.Error != nil {
			t.Errorf("request %d: %+v", q, res.Error)
		}
		if i%2 == 1 {
			if text, isErr := c.toolText(res); isErr || !strings.Contains(text, `"handle"`) {
				t.Errorf("open: %s", text)
			}
		}
	}
	res, _ = c.recv(list)
	if text, isErr := c.toolText(res); isErr || !strings.Contains(text, id) {
		t.Errorf("capture_list: %s", text)
	}
	c.callOK("capture_cancel", map[string]any{"id": id}, nil)
	if text, isErr := waitEnd(t, c, id); !isErr || toolErrorOf(t, text).Code != string(capture.CodeCancelled) {
		t.Errorf("end: %s", text)
	}
}

// capture_cancel is answered on the reader while a 45s capture_wait holds
// the dispatcher, and the wait then reports the cancellation.
func TestCaptureCancelIsAControlMessage(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
	waitStatus(t, c, id, func(v jobView) bool { return v.Status == string(jobRunning) })
	wait := c.sendTool("capture_wait", map[string]any{"id": id, "timeout": "45s"}, nil)
	time.Sleep(200 * time.Millisecond)
	begin := time.Now()
	res, _ := c.recv(c.sendTool("capture_cancel", map[string]any{"id": id}, nil))
	if took := time.Since(begin); took > time.Second {
		t.Errorf("the cancel took %v", took)
	}
	text, isErr := c.toolText(res)
	var a map[string]string
	if isErr || json.Unmarshal([]byte(text), &a) != nil || a["status"] != cancelAnswerCancelling || a["job"] != id {
		t.Fatalf("cancel: %s", text)
	}
	if _, early := c.held[wait]; early {
		t.Error("the wait was answered before the cancel")
	}
	res, _ = c.recv(wait)
	text, isErr = c.toolText(res)
	if !isErr {
		t.Fatalf("the wait answered %s", text)
	}
	if w := toolErrorOf(t, text); w.Code != string(capture.CodeCancelled) || w.Status != string(jobCancelled) || w.Job != id || w.CaptureID != "adhoc/001" {
		t.Errorf("%+v", w)
	}
	if took := time.Since(begin); took > 20*time.Second {
		t.Errorf("the wait ended %v after the cancel", took)
	}
}

// A cancel through the tool during a 1m capture stops the fake's process
// group within the stop grace, and the capture is rolled back.
func TestCaptureCancelStopsTheChildPromptly(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
	waitStatus(t, c, id, profiling)
	rec, ok := e.fakeRecord()
	if !ok {
		t.Fatal("no fake record")
	}
	e.noteRecord()
	var a map[string]string
	c.callOK("capture_cancel", map[string]any{"id": "adhoc/001"}, &a)
	if a["status"] != cancelAnswerCancelling || a["job"] != id || a["id"] != "adhoc/001" {
		t.Fatalf("cancel %v", a)
	}
	grace := e.s.lim.Capture.StopGrace + time.Second
	if !eventuallyTrue(grace, func() bool { return syscall.Kill(-rec.PGID, 0) == syscall.ESRCH }) {
		t.Errorf("the fake's group %d outlived %v", rec.PGID, grace)
	}
	if text, isErr := waitEnd(t, c, id); !isErr || toolErrorOf(t, text).Code != string(capture.CodeCancelled) {
		t.Errorf("end: %s", text)
	}
	if m, ok := e.manifestOf("adhoc", "001"); !ok || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) {
		t.Errorf("manifest %+v", m)
	}
	var again map[string]string
	c.callOK("capture_cancel", map[string]any{"id": id}, &again)
	if again["status"] != cancelAnswerCancelled {
		t.Errorf("a cancel after the end answered %v", again)
	}
}

// The end of the client's input stops the capture: nobody would collect
// it. The fake's group is gone within the stop grace and the capture is
// cancelled.
func TestEOFStopsTheChildPromptly(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
	waitStatus(t, c, id, profiling)
	j, ok := e.s.registry.get(id)
	if !ok {
		t.Fatal("no job")
	}
	rec, _ := e.fakeRecord()
	e.noteRecord()
	if err := c.w.Close(); err != nil {
		t.Fatal(err)
	}
	grace := e.s.lim.Capture.StopGrace + time.Second
	if !eventuallyTrue(grace, func() bool { return syscall.Kill(-rec.PGID, 0) == syscall.ESRCH }) {
		t.Errorf("the fake's group %d outlived %v", rec.PGID, grace)
	}
	if v := e.finish(j); v.Status != string(jobCancelled) {
		t.Errorf("view %+v", v)
	}
	if m, ok := e.manifestOf("adhoc", "001"); !ok || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) {
		t.Errorf("manifest %+v", m)
	}
}

// A cancel of an id this server never started is answered from memory:
// no lookup, and no file touched.
func TestCaptureCancelUnknownIDDoesNoIO(t *testing.T) {
	var lookups atomic.Int32
	e, c := newCapEnv(t, jobOpts{setup: func(s *Server) { s.lookupHook = func(string) { lookups.Add(1) } }})
	for _, id := range []string{"j9-ffff", "adhoc/001", "001", "../x"} {
		begin := time.Now()
		text, isErr := c.call("capture_cancel", map[string]any{"id": id})
		if !isErr {
			t.Fatalf("%s answered %s", id, text)
		}
		if w := toolErrorOf(t, text); w.Code != string(capture.CodeUnknownCapture) {
			t.Errorf("%s: %+v", id, w)
		}
		if took := time.Since(begin); took > time.Second {
			t.Errorf("%s took %v", id, took)
		}
	}
	text, _ := c.call("capture_cancel", map[string]any{"id": ""})
	if w := toolErrorOf(t, text); w.Code != string(capture.CodeInvalidArgument) {
		t.Errorf("empty id: %+v", w)
	}
	if n := lookups.Load(); n != 0 {
		t.Errorf("%d lookups", n)
	}
	if _, ok := e.dir(); ok {
		t.Error("the capture directory was created")
	}
}

// listing is capture_list's answer as the tests read it.
type listing struct {
	Active   string `json:"active"`
	Jobs     []listJob
	Series   []seriesRow `json:"series"`
	Captures []listRow   `json:"captures"`
	Stranded []strandedRow
	Loading  bool   `json:"loading"`
	Retry    string `json:"retry_after"`
	Warnings []string
	Left     string `json:"leftovers"`
	Next     string `json:"next_cursor"`
}

// The running capture is listed first, ahead of the history read from
// the manifests.
func TestCaptureListShowsTheRunningCaptureFirst(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	first := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s"})
	waitResult(t, c, first)
	second := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
	waitStatus(t, c, second, func(v jobView) bool { return v.Status == string(jobRunning) })
	var l listing
	c.callOK("capture_list", map[string]any{"series": "adhoc"}, &l)
	if l.Active != second || len(l.Jobs) != 2 || l.Jobs[0].Job != second || l.Jobs[0].Status != string(jobRunning) || l.Jobs[1].Job != first {
		t.Fatalf("jobs %+v active %q", l.Jobs, l.Active)
	}
	if len(l.Captures) != 1 || l.Captures[0].ID != "001" || l.Captures[0].Capture != "adhoc/001" || l.Captures[0].Outcome != captureOK {
		t.Fatalf("captures %+v", l.Captures)
	}
	if ps := l.Captures[0].Profiles; len(ps) != 1 || ps[0].Path != "captures/adhoc/001-run-cpu.pb.gz" || ps[0].Handle == "" {
		t.Errorf("profiles %+v", ps)
	}
	var all listing
	c.callOK("capture_list", map[string]any{}, &all)
	if len(all.Series) != 1 || all.Series[0].Series != "adhoc" || all.Series[0].Captures != "1" || all.Series[0].InProgress != "1" {
		t.Errorf("series %+v", all.Series)
	}
	c.callOK("capture_cancel", map[string]any{"id": second}, nil)
	waitEnd(t, c, second)
}

// The history pages like top, never shows the token or a path of the
// server's, hides a capture whose eviction left only its marker, counts
// what recovery would note, warns of a missing .seq, shows a stranded
// rollback, and is marked loading when it is not read in time.
func TestCaptureListPagesAndLoads(t *testing.T) {
	var block atomic.Pointer[chan struct{}]
	e := newJobEnv(t, jobOpts{setup: func(s *Server) {
		s.listWait = 300 * time.Millisecond
		s.lookupHook = func(string) {
			if ch := block.Load(); ch != nil {
				<-*ch
			}
		}
	}})
	st := capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"}
	for i := range 5 {
		e.commitSynthetic("s", fmt.Sprintf("l%d", i+1), st, manifest{})
	}
	c := e.serve()
	var ids []string
	cursor := ""
	for page := 0; ; page++ {
		args := map[string]any{"series": "s", "limit": 2}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var l listing
		text := c.callOK("capture_list", args, &l)
		if strings.Contains(text, e.profiles) {
			t.Errorf("the listing names the profiles root: %s", text)
		}
		for _, r := range l.Captures {
			ids = append(ids, r.ID)
			m, _ := e.manifestOf("s", r.ID)
			if m.Token == "" || strings.Contains(text, m.Token) || strings.Contains(text, "token") {
				t.Errorf("the listing shows %s's token", r.ID)
			}
		}
		if cursor = l.Next; cursor == "" {
			break
		}
		if page > 5 {
			t.Fatal("too many pages")
		}
	}
	if !slices.Equal(ids, []string{"005", "004", "003", "002", "001"}) {
		t.Fatalf("ids %v", ids)
	}

	ser := filepath.Join(e.profiles, defaultStdioCaptures, "s")
	rows := func() listing {
		t.Helper()
		var l listing
		c.callOK("capture_list", map[string]any{"series": "s"}, &l)
		return l
	}
	// A marker beside its manifest: a manifest with two links is listed.
	if err := os.Link(filepath.Join(ser, "005-l5.capture.json"), filepath.Join(ser, "005-l5.evicting")); err != nil {
		t.Fatal(err)
	}
	if l := rows(); len(l.Captures) != 5 || l.Left != "0" || len(l.Warnings) != 0 {
		t.Errorf("with a marker: %+v", l)
	}
	// The marker alone: the capture is being evicted and is not listed.
	if err := os.Remove(filepath.Join(ser, "005-l5.capture.json")); err != nil {
		t.Fatal(err)
	}
	if l := rows(); len(l.Captures) != 4 || l.Captures[0].ID != "004" || l.Left != "0" {
		t.Errorf("marker only: %+v", l)
	}
	// What recovery would note, a stale staging directory and a heartbeat
	// no intent names, is counted; a missing .seq is warned of.
	if err := os.Mkdir(filepath.Join(ser, ".staging-0123456789abcdef"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, ser, "009-gone.intent.hb", []byte("x"))
	if err := os.Remove(filepath.Join(ser, seqFile)); err != nil {
		t.Fatal(err)
	}
	l := rows()
	if l.Left != "2" || len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], ".seq") {
		t.Errorf("leftovers %q warnings %q", l.Left, l.Warnings)
	}
	var all listing
	c.callOK("capture_list", map[string]any{}, &all)
	if len(all.Series) != 1 || all.Series[0].Captures != "4" || all.Series[0].Leftovers != "2" || all.Series[0].Bytes == "0" {
		t.Errorf("series %+v", all.Series)
	}

	// A stranded rollback is shown.
	e.s.capMu.Lock()
	stranded := &captureTxn{series: "s", id: 7, in: intent{Label: "zz"}}
	e.s.stranded = append(e.s.stranded, stranded)
	e.s.capMu.Unlock()
	l = rows()
	e.s.capMu.Lock()
	e.s.stranded = slices.DeleteFunc(e.s.stranded, func(u *captureTxn) bool { return u == stranded })
	e.s.capMu.Unlock()
	if len(l.Stranded) != 1 || l.Stranded[0].Capture != "s/007" || l.Stranded[0].Label != "zz" {
		t.Errorf("stranded %+v", l.Stranded)
	}

	// A history read that does not finish in time answers the last read
	// that did, saying when it was read; with none that is still current,
	// it is loading. Once the read is released the call answers.
	ch := make(chan struct{})
	block.Store(&ch)
	var kept listing
	text := c.callOK("capture_list", map[string]any{"series": "s"}, &kept)
	if kept.Loading || len(kept.Captures) != 4 || !strings.Contains(text, `"read_at"`) {
		t.Errorf("blocked, with an earlier read: %s", text)
	}
	e.s.historyChanged()
	var loading listing
	c.callOK("capture_list", map[string]any{"series": "s"}, &loading)
	if !loading.Loading || loading.Retry != "1s" || len(loading.Captures) != 0 {
		t.Errorf("blocked: %+v", loading)
	}
	text, isErr := c.call("capture_wait", map[string]any{"id": "s/002", "timeout": "200ms"})
	if isErr || !strings.Contains(text, `"loading"`) || !strings.Contains(text, `"retry_after":"1s"`) {
		t.Errorf("a blocked wait: %s", text)
	}
	// Two loads hold the workers; sixteen more fill the queue; the next is
	// refused at once.
	for i := 3; i <= 18; i++ {
		c.call("capture_wait", map[string]any{"id": fmt.Sprintf("s/%03d", i), "timeout": "10ms"})
	}
	begin := time.Now()
	text, _ = c.call("capture_wait", map[string]any{"id": "s/099", "timeout": "5s"})
	if !strings.Contains(text, `"loading"`) || time.Since(begin) > 2*time.Second {
		t.Errorf("a full queue answered %s after %v", text, time.Since(begin))
	}
	block.Store(nil)
	close(ch)
	if !eventuallyTrue(10*time.Second, func() bool {
		var l listing
		c.callOK("capture_list", map[string]any{"series": "s"}, &l)
		return !l.Loading && len(l.Captures) == 4
	}) {
		t.Error("the listing never came")
	}
	r, _ := waitResult(t, c, "s/002")
	if r.Derived != derivedRecompute || r.Handles != "reopen" || r.Recompute == nil {
		t.Errorf("historical %+v", r)
	}
}

// No call takes longer than a client allows: capture_wait's 45s cap
// leaves 15s of Codex's 60s.
func TestEveryCallStaysUnderTheClientBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 45s")
	}
	e, c := newCapEnv(t, jobOpts{})
	const budget = 50 * time.Second
	timed := func(tool string, args map[string]any) (string, bool) {
		t.Helper()
		begin := time.Now()
		text, isErr := c.call(tool, args)
		if took := time.Since(begin); took > budget {
			t.Errorf("%s took %v", tool, took)
		}
		return text, isErr
	}
	text, _ := timed("capture", map[string]any{"command": workloadCmd(t), "duration": "1m"})
	var a map[string]string
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		t.Fatal(text)
	}
	id := a["job"]
	begin := time.Now()
	text, isErr := timed("capture_wait", map[string]any{"id": id, "timeout": "45s"})
	if isErr || isResult(t, text) || !strings.Contains(text, `"running"`) || time.Since(begin) < 45*time.Second {
		t.Errorf("the 45s wait answered %s after %v", text, time.Since(begin))
	}
	timed("capture_list", map[string]any{})
	timed("capture_cancel", map[string]any{"id": id})
	text, isErr = timed("capture_wait", map[string]any{"id": id, "timeout": "45s"})
	if !isErr || toolErrorOf(t, text).Code != string(capture.CodeCancelled) {
		t.Errorf("end: %s", text)
	}
	e.noteRecord()
}

// Progress notifications go out in the state machine's order, each with
// an increasing progress, and every line on stdout is one JSON-RPC
// message.
func TestProgressNotificationsAreInPhaseOrder(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1500ms"})
	res, notes := c.recv(c.sendTool("capture_wait", map[string]any{"id": id, "timeout": "45s"}, map[string]any{"progressToken": "p1"}))
	text, isErr := c.toolText(res)
	if isErr || !isResult(t, text) {
		t.Fatalf("the wait answered %s", text)
	}
	if len(notes) < 2 {
		t.Fatalf("%d notifications", len(notes))
	}
	rank := map[string]int{}
	for i, s := range []jobStatus{jobAccepted, jobPreparing, jobRunning, jobCommitting, jobDone} {
		rank[string(s)] = i
	}
	last, lastProgress := -1, -1.0
	var seen []string
	lastMessage, lastAt := "", -100.0
	for _, n := range notes {
		var p struct {
			Token    string  `json:"progressToken"`
			Progress float64 `json:"progress"`
			Total    float64 `json:"total"`
			Message  string  `json:"message"`
		}
		if n.Method != "notifications/progress" || json.Unmarshal(n.Params, &p) != nil || p.Token != "p1" {
			t.Fatalf("notification %s %s", n.Method, n.Params)
		}
		status, _, _ := strings.Cut(p.Message, " ")
		r, ok := rank[status]
		if !ok || r < last {
			t.Errorf("status %q after rank %d", p.Message, last)
		}
		if p.Progress <= lastProgress || p.Total <= 0 {
			t.Errorf("progress %v after %v, total %v", p.Progress, lastProgress, p.Total)
		}
		// The same state is repeated only every 5s.
		if p.Message == lastMessage && p.Progress-lastAt < 4.5 {
			t.Errorf("%q repeated after %.2fs", p.Message, p.Progress-lastAt)
		}
		if p.Message != lastMessage {
			lastMessage, lastAt = p.Message, p.Progress
		}
		last, lastProgress = r, p.Progress
		seen = append(seen, p.Message)
	}
	// The capture's own phase change reaches the client without waiting
	// for the 5s tick: this capture is over before it.
	if !slices.Contains(seen, "running "+capture.PhaseProfiling) {
		t.Errorf("no profiling notification in %q", seen)
	}
}

// A result over MaxResponseBytes is cut: first each open to its handle,
// views, default view and quality, then the compare rows, the output
// tail, the views and quality, and at last everything but what finds the
// capture again.
func TestTheResultIsCutToMaxResponseBytes(t *testing.T) {
	s := newTestServer(t, testConfig(t)).srv
	open := map[string]any{
		"handle": "h1", "profile_digest": "sha256:" + strings.Repeat("a", 64), "profile_name": "captures/s/001-a-cpu.pb.gz",
		"capture_id": "", "views": []any{map[string]any{"view": "cpu/nanoseconds#0"}}, "default_view": "cpu/nanoseconds#0",
		"capabilities": map[string]any{}, "quality": map[string]any{"q": "ok"}, "notes": []any{strings.Repeat("n", 4000)},
	}
	rows := make([]any, 40)
	for i := range rows {
		rows[i] = map[string]any{"function": strings.Repeat("f", 100), "i": i}
	}
	full := map[string]any{
		"capture": map[string]any{"id": "001", "series": "s", "label": "a", "manifest": "captures/s/001-a.capture.json",
			"workload_output_tail": map[string]any{"stdout": strings.Repeat("o", 2000), "stderr": "", "untrusted": true}},
		"profiles": []any{map[string]any{"mode": "cpu", "path": "captures/s/001-a-cpu.pb.gz", "open": open}},
		"compare": map[string]any{"base": map[string]any{"id": "000"}, "views": []any{map[string]any{"view": "cpu/nanoseconds#0", "rows": rows,
			"next": map[string]any{"tool": "diff"}}}},
		"recompute": map[string]any{"steps": []any{}, "baseline": "none"},
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(t *testing.T, text string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		return m
	}
	openOf := func(m map[string]any) map[string]any {
		return m["profiles"].([]any)[0].(map[string]any)["open"].(map[string]any)
	}
	for _, tc := range []struct {
		max   int
		check func(t *testing.T, m map[string]any)
	}{
		{len(b), func(t *testing.T, m map[string]any) {
			if len(openOf(m)) != len(open) || m["notes"] != nil {
				t.Errorf("a result that fits was cut: %v", m["notes"])
			}
		}},
		{len(b) - 1000, func(t *testing.T, m map[string]any) {
			o := openOf(m)
			if len(o) != 4 || o["handle"] != "h1" || o["quality"] == nil || o["views"] == nil || o["default_view"] == nil {
				t.Errorf("open %v", o)
			}
			if !strings.Contains(fmt.Sprint(m["notes"]), "call open with path for the full result") {
				t.Errorf("notes %v", m["notes"])
			}
			if v := m["compare"].(map[string]any)["views"].([]any)[0].(map[string]any); v["rows"] == nil {
				t.Error("the rows were cut too early")
			}
		}},
		{2500, func(t *testing.T, m map[string]any) {
			v := m["compare"].(map[string]any)["views"].([]any)[0].(map[string]any)
			if v["rows"] != nil || v["next"] == nil {
				t.Errorf("view %v", v)
			}
		}},
		{700, func(t *testing.T, m map[string]any) {
			if m["recompute"] == nil || m["capture"].(map[string]any)["manifest"] == nil || m["profiles"] == nil {
				t.Errorf("the last cut %v", m)
			}
		}},
	} {
		text := s.cutResult(b, tc.max)
		if len(text) > tc.max {
			t.Errorf("max %d: %d bytes", tc.max, len(text))
		}
		tc.check(t, decode(t, string(text)))
	}

	// Through the tools: a compare's result stays under a small
	// MaxResponseBytes.
	_, c := newCapEnv(t, jobOpts{limits: func(l *Limits) {
		l.MaxResponseBytes = 6 << 10
		l.Capture.MaxOutputTail = 256
	}})
	waitResult(t, c, startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s"}))
	_, text := waitResult(t, c, startCapture(t, c, map[string]any{"command": workloadCmd(t), "compare_with": "001"}))
	if len(text) > 6<<10 {
		t.Errorf("%d bytes", len(text))
	}
}

// tools/list shows the nine profile tools, the four capture tools, then
// the page tools of a live server.
func TestToolsListOrder(t *testing.T) {
	cfg := testConfig(t)
	cfg.Live = true
	c := newTestServer(t, cfg)
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.rpc("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	want := []string{"open", "top", "path", "facts", "gpu_kernels_for", "sandwich", "diff", "source", "render_flamegraph",
		"capture", "capture_wait", "capture_cancel", "capture_list",
		"page_open", "page_explain", "page_show", "page_diff"}
	if !slices.Equal(names, want) {
		t.Errorf("tools %v", names)
	}
}

// A command that ends during the capture stops it, and the result says
// so: stopped_by and the target's exit. The result's derived state is
// ready and its handles are open.
func TestTheResultSaysHowTheCommandEnded(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	w := capturetest.Workload(t)
	r, _ := waitResult(t, c, startCapture(t, c, map[string]any{"command": []any{w, "exit-after", "1500ms", "3"}, "duration": "20s"}))
	if r.Capture.StoppedBy != capture.StoppedByCommandExit || !strings.HasPrefix(r.Capture.Target.Exit, "exited 3 after ") ||
		r.Capture.Target.Exit != "exited 3 after "+r.Capture.ProfiledDuration {
		t.Errorf("stopped_by %q, target %+v", r.Capture.StoppedBy, r.Capture.Target)
	}
	if r.Derived != derivedReady || r.Handles != "" || r.Recompute != nil || len(r.Profiles) != 1 || r.Profiles[0].Open == nil {
		t.Errorf("result %+v", r)
	}
}

// A done job whose handles the store has since evicted says so, with the
// calls that reopen them.
func TestAResultWhoseHandlesWereEvictedSaysReopen(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{limits: func(l *Limits) { l.MaxOpenHandles = 1 }})
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s"})
	if r, _ := waitResult(t, c, id); r.Handles != "" || r.Recompute != nil {
		t.Fatalf("fresh result %+v", r)
	}
	writeFixture(t, e.profiles, "other.pb.gz", pmfixture.CPUBasicNew())
	c.callOK("open", map[string]any{"path": "other.pb.gz"}, nil)
	r, _ := waitResult(t, c, id)
	if r.Handles != handlesReopen || r.Recompute == nil || len(r.Recompute.Steps) != 1 || r.Recompute.Steps[0].Tool != "open" {
		t.Errorf("result %+v", r)
	}
}

// The capture tools never wait for the engine: while a job's derivation
// holds it, a wait answers the job's state, the list answers, and a new
// capture is refused at once while the slot is still taken.
func TestCaptureToolsNeverWaitForTheEngine(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	g := e.faults.block(t, faultBeforeOpen)
	id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s"})
	g.reached(t)
	begin := time.Now()
	var v jobView
	c.callOK("capture_wait", map[string]any{"id": id, "timeout": "200ms"}, &v)
	if v.Status != string(jobDone) || v.Derived != derivedPending {
		t.Errorf("view %+v", v)
	}
	var l listing
	c.callOK("capture_list", map[string]any{"series": "adhoc"}, &l)
	if l.Active != id || len(l.Jobs) != 1 || l.Jobs[0].Derived != derivedPending || len(l.Captures) != 1 {
		t.Errorf("list %+v", l)
	}
	text, isErr := c.call("capture", map[string]any{"command": workloadCmd(t)})
	if !isErr || toolErrorOf(t, text).Code != string(capture.CodeBusy) {
		t.Errorf("a capture while the slot is taken: %s", text)
	}
	if took := time.Since(begin); took > 10*time.Second {
		t.Errorf("the calls took %v", took)
	}
	g.open()
	if r, _ := waitResult(t, c, id); r.Derived != derivedReady {
		t.Errorf("result %+v", r)
	}
}

// How a target ended reads the same in a live and a historical result:
// a launched command with its status or signal, a PID with its time.
func TestTargetExitTexts(t *testing.T) {
	if got := commandExit(capture.Outcome{WorkloadExited: true, WorkloadStatus: 3, Profiled: 1500 * time.Millisecond}); got != "exited 3 after 1.5s" {
		t.Errorf("status: %q", got)
	}
	if got := commandExit(capture.Outcome{WorkloadExited: true, WorkloadStatus: -1, WorkloadSignal: "killed", Profiled: 2 * time.Second}); got != `killed by signal "killed" after 2s` {
		t.Errorf("signal: %q", got)
	}
	s := newTestServer(t, testConfig(t)).srv
	info := s.captureInfo(manifest{ID: "001", Series: "s", Label: "a", StoppedBy: capture.StoppedByTargetExit, ProfiledDuration: "1.2s"})
	if info.Target.Exit != "exited after 1.2s" {
		t.Errorf("pid: %q", info.Target.Exit)
	}
}

// A result that cannot be cut under MaxResponseBytes, even to its paths,
// is a short error saying how to read the capture.
func TestAResultTooLargeEvenCutIsAShortError(t *testing.T) {
	s := newTestServer(t, testConfig(t)).srv
	s.lim.MaxResponseBytes = 120
	b := mustJSON(t, map[string]any{"capture": map[string]any{"id": "001", "series": "s", "label": "a", "manifest": "captures/s/001-a.capture.json"},
		"profiles":  []any{map[string]any{"mode": "cpu", "path": "captures/s/001-a-cpu.pb.gz", "open": map[string]any{"handle": strings.Repeat("h", 64)}}},
		"recompute": map[string]any{"steps": []any{strings.Repeat("s", 300)}}})
	res := s.fitResult([]byte(b))
	if !res.IsError {
		t.Fatalf("answered %d bytes: %s", len(res.Text), res.Text)
	}
	if w := toolErrorOf(t, res.Text); w.Code != string(capture.CodeTooLarge) || !strings.Contains(w.Next, "MaxResponseBytes") {
		t.Errorf("%+v", w)
	}
	s.lim.MaxResponseBytes = 400
	if res := s.fitResult([]byte(b)); res.IsError || len(res.Text) > 400 {
		t.Errorf("a minimal cut that fits: %v %d bytes", res.IsError, len(res.Text))
	}
}

// A history read slower than capture_list waits still reaches the agent:
// the next call answers with it.
func TestCaptureListConvergesWhenTheReadIsSlow(t *testing.T) {
	e := newJobEnv(t, jobOpts{setup: func(s *Server) {
		s.listWait = 100 * time.Millisecond
		s.lookupHook = func(k string) {
			if strings.HasPrefix(k, "list:") {
				time.Sleep(300 * time.Millisecond)
			}
		}
	}})
	st := capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"}
	e.commitSynthetic("s", "a", st, manifest{})
	c := e.serve()
	var first listing
	c.callOK("capture_list", map[string]any{"series": "s"}, &first)
	if !first.Loading {
		t.Fatalf("a 300ms read answered within 100ms: %+v", first)
	}
	time.Sleep(400 * time.Millisecond)
	// Every later call answers, each with the last read that finished,
	// however often the agent calls.
	for range 5 {
		var next listing
		text := c.callOK("capture_list", map[string]any{"series": "s"}, &next)
		if next.Loading || len(next.Captures) != 1 || !strings.Contains(text, `"read_at"`) {
			t.Errorf("the finished read was not answered: %s", text)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A capture of this server makes the kept read stale.
	e.s.historyChanged()
	var stale listing
	c.callOK("capture_list", map[string]any{"series": "s"}, &stale)
	if !stale.Loading {
		t.Errorf("a stale read was answered: %+v", stale)
	}
}

// A capture whose server ended under it can be captured again at once.
func TestAnInterruptedCaptureIsRetryable(t *testing.T) {
	s, d, clk := newCaptureServer(t)
	s.captureFault = inject(shot{faultAfterStaging, 1, errCrash})
	r := drive(t, s, d, capCase{})
	crashRecover(t, s, d, clk)
	if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
		t.Fatalf("outcome %q", m.Outcome)
	}
	profiles := filepath.Dir(d.root.Name())
	d.close()
	_, c := newCapEnv(t, jobOpts{profiles: profiles})
	text, isErr := c.call("capture_wait", map[string]any{"id": "adhoc/001", "timeout": "5s"})
	if !isErr {
		t.Fatalf("answered %s", text)
	}
	if w := toolErrorOf(t, text); w.Code != string(capture.CodeCancelled) || w.Status != captureInterrupted || !w.Retryable || w.RetryAfter != "0s" {
		t.Errorf("%+v", w)
	}
	var l listing
	c.callOK("capture_list", map[string]any{"series": "adhoc"}, &l)
	if len(l.Captures) != 1 || l.Captures[0].Outcome != captureInterrupted {
		t.Errorf("list %+v", l.Captures)
	}
}

// Once retention removes a capture, no answer kept in memory says it is
// done: a wait reads the disk and finds it gone.
func TestRetentionForgetsWhatItRemoved(t *testing.T) {
	// A series name may hold '-', as the capture's own name does.
	for _, series := range []string{"s", "parse-opt"} {
		t.Run(series, func(t *testing.T) {
			e := newJobEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.KeepPerSeries = 1 }})
			st := capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"}
			e.commitSynthetic(series, "a", st, manifest{})
			e.commitSynthetic(series, "b", st, manifest{})
			c := e.serve()
			if r, _ := waitResult(t, c, series+"/001"); r.Derived != derivedRecompute {
				t.Fatalf("first answer %+v", r)
			}
			text, _ := waitEnd(t, c, startCapture(t, c, map[string]any{"command": workloadCmd(t), "series": series, "duration": "1s"}))
			if !isResult(t, text) {
				t.Fatalf("the capture: %s", text)
			}
			if _, ok := e.manifestOf(series, "001"); ok {
				t.Fatal("retention kept 001")
			}
			text, isErr := c.call("capture_wait", map[string]any{"id": series + "/001", "timeout": "5s"})
			if !isErr || toolErrorOf(t, text).Code != string(capture.CodeUnknownCapture) {
				t.Errorf("a removed capture answered %s", text)
			}
		})
	}
}

// A historical read that retention overtakes, removing the capture
// between the read and the keeping of its answer, keeps nothing: a later
// wait reads the disk and finds the capture gone.
func TestARemovalDuringAHistoricalReadKeepsNoAnswer(t *testing.T) {
	read := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e := newJobEnv(t, jobOpts{
		limits: func(l *Limits) { l.Capture.KeepPerSeries = 1 },
		setup: func(s *Server) {
			s.readHook = func(k string) {
				if k == "s/001" {
					once.Do(func() {
						close(read)
						<-release
					})
				}
			}
		},
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	st := capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"}
	e.commitSynthetic("s", "a", st, manifest{})
	e.commitSynthetic("s", "b", st, manifest{})
	c := e.serve()
	if text, isErr := c.call("capture_wait", map[string]any{"id": "s/001", "timeout": "1s"}); isErr || isResult(t, text) {
		t.Fatalf("the paused read answered %s", text)
	}
	select {
	case <-read:
	case <-time.After(30 * time.Second):
		t.Fatal("the read never reached its answer")
	}
	text, _ := waitEnd(t, c, startCapture(t, c, map[string]any{"command": workloadCmd(t), "series": "s", "duration": "1s"}))
	if !isResult(t, text) {
		t.Fatalf("the capture: %s", text)
	}
	if _, ok := e.manifestOf("s", "001"); ok {
		t.Fatal("retention kept 001")
	}
	close(release)
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		text, isErr := c.call("capture_wait", map[string]any{"id": "s/001", "timeout": "5s"})
		if !isErr && !isResult(t, text) {
			if time.Now().After(end) {
				t.Fatalf("s/001 is still loading: %s", text)
			}
			continue
		}
		if !isErr || toolErrorOf(t, text).Code != string(capture.CodeUnknownCapture) {
			t.Fatalf("a removed capture answered %s", text)
		}
		return
	}
}

// A capture_wait queued behind another counts its time in the queue
// against its own timeout: it answers when the one ahead does, its budget
// spent, not a whole timeout later.
func TestAQueuedWaitKeepsItsBudgetFromReceipt(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	job := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "60s"})
	waitStatus(t, c, job, profiling)
	first := c.sendTool("capture_wait", map[string]any{"id": job, "timeout": "4s"}, nil)
	second := c.sendTool("capture_wait", map[string]any{"id": job, "timeout": "3s"}, nil)
	sent := time.Now()
	if res, _ := c.recv(first); res.Error != nil {
		t.Fatalf("first wait: %+v", res.Error)
	}
	firstAt := time.Since(sent)
	res, _ := c.recv(second)
	text, isErr := c.toolText(res)
	secondAt := time.Since(sent)
	if isErr || isResult(t, text) {
		t.Fatalf("second wait: %s", text)
	}
	// Without the receipt time the second would wait its 3 s after the
	// first's 4 s.
	if secondAt > firstAt+1500*time.Millisecond {
		t.Errorf("the queued wait answered %v after it was sent, %v after the one ahead of it; its budget was 3s from receipt", secondAt, secondAt-firstAt)
	}
	c.callOK("capture_cancel", map[string]any{"id": job}, nil)
	waitEnd(t, c, job)
}

// A capture_list queued behind a wait answers once its own wait for the
// history is spent from receipt.
func TestAQueuedListKeepsItsBudgetFromReceipt(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{setup: func(s *Server) {
		s.listWait = 2 * time.Second
		s.lookupHook = func(k string) {
			if strings.HasPrefix(k, "list:") {
				time.Sleep(5 * time.Second)
			}
		}
	}})
	job := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "60s"})
	waitStatus(t, c, job, profiling)
	first := c.sendTool("capture_wait", map[string]any{"id": job, "timeout": "3s"}, nil)
	list := c.sendTool("capture_list", map[string]any{"series": "adhoc"}, nil)
	sent := time.Now()
	c.recv(first)
	firstAt := time.Since(sent)
	c.recv(list)
	if listAt := time.Since(sent); listAt > firstAt+1500*time.Millisecond {
		t.Errorf("the queued list answered %v after it was sent, %v after the wait ahead of it; its wait was 2s from receipt", listAt, listAt-firstAt)
	}
	c.callOK("capture_cancel", map[string]any{"id": job}, nil)
	waitEnd(t, c, job)
}

// A capture read back from its manifest is answered by its outcome: an
// interrupted one is the retryable cancelled answer with status
// interrupted, whatever error it records; a failed one is its own code,
// or a fallback when it records none, never interrupted.
func TestAHistoricalFailureIsAnsweredByItsOutcome(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	disk := &wireError{Code: string(capture.CodeDisk), Message: "no space", Retryable: true, RetryAfter: "0s"}
	for _, tc := range []struct {
		name      string
		m         manifest
		code      capture.Code
		status    string
		retryable bool
	}{
		{"interrupted", manifest{Outcome: captureInterrupted}, capture.CodeCancelled, captureInterrupted, true},
		{"interrupted with an error", manifest{Outcome: captureInterrupted, Error: disk}, capture.CodeCancelled, captureInterrupted, true},
		{"failed", manifest{Outcome: captureFailed, Error: disk}, capture.CodeDisk, string(jobFailed), true},
		{"failed without an error", manifest{Outcome: captureFailed}, capture.CodePerfAgentFailed, string(jobFailed), true},
		{"cancelled", manifest{Outcome: captureFailed, Error: &wireError{Code: string(capture.CodeCancelled), Message: "cancelled"}}, capture.CodeCancelled, string(jobCancelled), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := e.s.historicalAnswer("s/001", "", &historical{m: tc.m})
			if !res.IsError {
				t.Fatalf("answered %s", res.Text)
			}
			w := toolErrorOf(t, res.Text)
			if w.Code != string(tc.code) || w.Status != tc.status || w.Retryable != tc.retryable || w.CaptureID != "s/001" {
				t.Errorf("%+v", w)
			}
		})
	}
}

// The histories kept for capture_list stay bounded: one that can no
// longer be answered (older than listKeep, or of an earlier history
// generation) is dropped when another is kept, and at most maxListKept
// are kept, the oldest going first.
func TestKeptHistoriesStayBounded(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	s := e.s
	now := time.Now()
	gen := s.historyGen.Load()
	s.keepHistory("list:old", keptHistory{h: &captureHistory{}, gen: gen, at: now.Add(-2 * listKeep)})
	s.keepHistory("list:stale", keptHistory{h: &captureHistory{}, gen: gen, at: now})
	s.historyChanged()
	gen = s.historyGen.Load()
	for i := range 3 * maxListKept {
		s.keepHistory(fmt.Sprintf("list:s%d", i), keptHistory{h: &captureHistory{}, gen: gen, at: now.Add(time.Duration(i) * time.Millisecond)})
	}
	s.listMu.Lock()
	defer s.listMu.Unlock()
	if len(s.listKept) > maxListKept {
		t.Errorf("%d histories kept, more than %d", len(s.listKept), maxListKept)
	}
	for _, k := range []string{"list:old", "list:stale", "list:s0"} {
		if _, ok := s.listKept[k]; ok {
			t.Errorf("%s is still kept", k)
		}
	}
	if _, ok := s.listKept[fmt.Sprintf("list:s%d", 3*maxListKept-1)]; !ok {
		t.Error("the newest history was not kept")
	}
}

// Ids whose jobs have left the registry still answer, by job id and by
// capture id: a cancel with the final state and both ids, a wait with
// the job id, and a job that failed before it had a capture id with its
// code.
func TestEvictedIDsStillAnswer(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.MaxJobs = 1 }})
	done := startCapture(t, c, map[string]any{"command": workloadCmd(t), "series": "x", "duration": "1s"})
	waitResult(t, c, done)
	early := startCapture(t, c, map[string]any{"command": workloadCmd(t), "cwd": "/nonexistent-dir-for-a-test"})
	if text, isErr := waitEnd(t, c, early); !isErr || toolErrorOf(t, text).Code != string(capture.CodeInvalidArgument) {
		t.Fatalf("early failure: %s", text)
	}
	waitResult(t, c, startCapture(t, c, map[string]any{"command": workloadCmd(t), "series": "y", "duration": "1s"}))

	for _, id := range []string{done, "x/001"} {
		var a map[string]string
		c.callOK("capture_cancel", map[string]any{"id": id}, &a)
		if a["status"] != cancelAnswerTooLate || a["job"] != done || a["id"] != "x/001" {
			t.Errorf("cancel %s: %v", id, a)
		}
	}
	r := map[string]json.RawMessage{}
	text, _ := waitEnd(t, c, done)
	if err := json.Unmarshal([]byte(text), &r); err != nil || string(r["job"]) != `"`+done+`"` || string(r["derived"]) != `"recompute"` {
		t.Errorf("wait by the evicted job id: %s", text)
	}
	text, isErr := c.call("capture_wait", map[string]any{"id": early, "timeout": "1s"})
	if w := toolErrorOf(t, text); !isErr || w.Code != string(capture.CodeInvalidArgument) || w.Job != early || w.Status != string(jobFailed) {
		t.Errorf("wait by an evicted job that failed early: %s", text)
	}
}

// An undecided commit is listed as one, not as a rollback.
func TestAnUndecidedCommitIsListedAsUndecided(t *testing.T) {
	u := newUndecidedEnv(t, false)
	j := u.undecided(t)
	c := u.serve()
	var l listing
	c.callOK("capture_list", map[string]any{}, &l)
	if len(l.Stranded) != 1 || l.Stranded[0].Capture != j.captureIDNow() || l.Stranded[0].State != stateUndecided {
		t.Errorf("stranded %+v", l.Stranded)
	}
	u.setBroken(false)
}
