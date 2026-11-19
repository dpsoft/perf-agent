package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
)

// The capture tests run the hub as this test binary, which accepts the
// fake perf-agent (fakeVerifyEnv), with these limits: a short stop grace,
// room for a slow start on a loaded machine, and a free-space floor any
// test machine meets.
const stopGrace = time.Second

var captureLimits = []string{
	"--limit", "Capture.StopGrace=" + stopGrace.String(),
	"--limit", "Capture.StartTimeout=30s",
	"--limit", "Capture.MaxDuration=2m",
	"--limit", "Capture.MinFreeBytes=1048576",
}

// hubCaptures is the captures directory a hub given no --captures uses,
// inside its profiles root.
const hubCaptures = "captures-hub"

// stdioFakeVerifyEnv is perf-agent-mcp's own fakeVerifyEnv: its test
// binary then runs as the stdio server and accepts the fake perf-agent.
const stdioFakeVerifyEnv = "PERF_AGENT_MCP_TEST_FAKE_VERIFY"

// procRef is a process by its PID and start time, with what
// /proc/PID/stat said of it.
type procRef struct {
	pid, ppid, pgid, sid int
	start                uint64
	state                byte
	comm                 string
}

// gone reports that the process no longer runs: its PID is free, holds
// another process, or is a zombie.
func (r procRef) gone() bool { return !capture.Alive(capture.DefaultProc(), r.pid, r.start) }

// procOf reads /proc/PID/stat.
func procOf(pid int) (procRef, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procRef{}, false
	}
	open, closing := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || closing < open {
		return procRef{}, false
	}
	f := strings.Fields(string(b[closing+1:]))
	if len(f) < 20 || len(f[0]) != 1 {
		return procRef{}, false
	}
	r := procRef{pid: pid, comm: string(b[open+1 : closing]), state: f[0][0]}
	r.ppid, _ = strconv.Atoi(f[1])
	r.pgid, _ = strconv.Atoi(f[2])
	r.sid, _ = strconv.Atoi(f[3])
	r.start, _ = strconv.ParseUint(f[19], 10, 64)
	return r, true
}

// descendantsOf are the processes under root now.
func descendantsOf(root int) []procRef {
	ents, _ := os.ReadDir("/proc")
	children := map[int][]procRef{}
	for _, d := range ents {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		if r, ok := procOf(pid); ok {
			children[r.ppid] = append(children[r.ppid], r)
		}
	}
	var out []procRef
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, c := range children[queue[0]] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}
	return out
}

// captureCgroupsOf lists the capture cgroups the server pid made that are
// still there: they are made in the cgroup the server was started in,
// this test's own, and their names carry the server's pid.
func captureCgroupsOf(pid int) []string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil
	}
	parent, _ := capture.DelegatedCgroup(b, "/sys/fs/cgroup")
	if parent == "" {
		return nil
	}
	mark := "-" + strconv.Itoa(pid) + "-"
	var left []string
	_ = filepath.WalkDir(parent, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && strings.HasPrefix(d.Name(), capture.CgroupPrefix) && strings.Contains(d.Name(), mark) {
			left = append(left, path)
		}
		return nil
	})
	return left
}

// eventually polls cond every 10 ms until it holds or d passes.
func eventually(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); ; time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// procWatch records the processes under the servers a test starts, so
// that at the end of the test it can check each is gone, and that none of
// those servers left a capture cgroup. A process still there is killed
// by its PID, once its start time says it is the same process.
type procWatch struct {
	t       *testing.T
	mu      sync.Mutex
	roots   []int
	watched []procRef
}

// newProcWatch registers the check first, so it runs after the cleanups
// that stop the servers.
func newProcWatch(t *testing.T) *procWatch {
	w := &procWatch{t: t}
	t.Cleanup(w.check)
	return w
}

// add watches what runs under the server pid from now on.
func (w *procWatch) add(pid int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.roots = append(w.roots, pid)
}

// watch records every process under the servers now and returns them.
func (w *procWatch) watch() []procRef {
	w.mu.Lock()
	defer w.mu.Unlock()
	var now []procRef
	for _, root := range w.roots {
		now = append(now, descendantsOf(root)...)
	}
	for _, r := range now {
		if !slices.Contains(w.watched, r) {
			w.watched = append(w.watched, r)
		}
	}
	return now
}

func (w *procWatch) check() {
	w.mu.Lock()
	watched, roots := slices.Clone(w.watched), slices.Clone(w.roots)
	w.mu.Unlock()
	for _, r := range watched {
		if !eventually(30*time.Second, r.gone) {
			w.t.Errorf("%s (pid %d) is still alive after the test", r.comm, r.pid)
			if now, ok := procOf(r.pid); ok && now.start == r.start {
				_ = syscall.Kill(r.pid, syscall.SIGKILL)
			}
		}
	}
	for _, root := range roots {
		var left []string
		if !eventually(15*time.Second, func() bool { left = captureCgroupsOf(root); return len(left) == 0 }) {
			w.t.Errorf("capture cgroups of server pid %d are left: %v", root, left)
			for _, p := range left {
				_ = os.Remove(p) // only an empty one goes
			}
		}
	}
}

// capHub is a hub that captures with the fake perf-agent, and the watch
// on what runs under it.
type capHub struct {
	*hubProc
	procs *procWatch
}

// startCaptureHub starts a hub that captures with fake, with the capture
// limits before o's own arguments, which may override them. At the end
// of the test, every process seen under it must be gone and its capture
// cgroups removed.
func startCaptureHub(t *testing.T, fake string, o hubOptions) *capHub {
	t.Helper()
	capturetest.SkipIfPrivileged(t)
	w := newProcWatch(t)
	o.env = append(slices.Clone(o.env), fakeVerifyEnv+"=1")
	o.args = append(append([]string{"--perf-agent", fake}, captureLimits...), o.args...)
	p := startHub(t, o)
	w.add(p.cmd.Process.Pid)
	// Registered after startHub's own cleanup, so it runs before the hub
	// is stopped: what runs under it then is watched too.
	t.Cleanup(func() { w.watch() })
	return &capHub{hubProc: p, procs: w}
}

// kill9 SIGKILLs the hub, after watching what runs under it, and reaps
// it.
func (h *capHub) kill9() {
	h.t.Helper()
	h.procs.watch()
	if err := h.cmd.Process.Kill(); err != nil {
		h.t.Fatal(err)
	}
	select {
	case <-h.exited:
	case <-time.After(10 * time.Second):
		h.t.Fatal("perf-agent-hub still runs 10s after SIGKILL")
	}
	if ee, ok := errors.AsType[*exec.ExitError](h.waitErr); !ok || ee.ExitCode() != -1 {
		h.t.Fatalf("perf-agent-hub ended %v, not by the kill", h.waitErr)
	}
}

// sigterm sends SIGTERM and waits up to d for an exit 0, returning how
// long the exit took.
func (h *capHub) sigterm(d time.Duration) time.Duration {
	h.t.Helper()
	return h.stop(syscall.SIGTERM, d)
}

// freeze stops the hub with SIGSTOP until the returned function, which
// the test's cleanup also calls, continues it.
func (h *capHub) freeze() (thaw func()) {
	h.t.Helper()
	pid := h.cmd.Process.Pid
	thaw = sync.OnceFunc(func() {
		select {
		case <-h.exited:
		default:
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	})
	h.t.Cleanup(thaw)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		h.t.Fatal(err)
	}
	if !eventually(10*time.Second, func() bool { r, _ := procOf(pid); return r.state == 'T' }) {
		h.t.Fatal("perf-agent-hub never stopped")
	}
	return thaw
}

// The capture tools' answers, as an agent reads them.
type (
	// jobState is capture_wait's answer for a job that has not settled.
	jobState struct {
		Job     string `json:"job"`
		Capture string `json:"capture_id"`
		Status  string `json:"status"`
		Phase   string `json:"phase"`
		Derived string `json:"derived"`
	}
	// capResult is a done capture's result.
	capResult struct {
		Capture struct {
			ID       string `json:"id"`
			Capture  string `json:"capture"`
			Manifest string `json:"manifest"`
		} `json:"capture"`
		Profiles []struct {
			Mode string `json:"mode"`
			Path string `json:"path"`
		} `json:"profiles"`
		Derived         string `json:"derived"`
		DeriveError     string `json:"derive_error"`
		Publication     string `json:"publication"`
		DiffPublication string `json:"diff_publication"`
	}
	// capError is the error object of a capture tool's isError answer.
	capError struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		Retryable  bool   `json:"retryable"`
		RetryAfter string `json:"retry_after"`
		Next       string `json:"next"`
		Capture    string `json:"capture_id"`
		Status     string `json:"status"`
	}
	// inProgress is capture_wait's answer for another server's capture
	// whose intent is on disk and whose manifest is not.
	inProgress struct {
		Capture    string `json:"capture_id"`
		Status     string `json:"status"`
		RetryAfter string `json:"retry_after"`
		Next       string `json:"next"`
		Holder     string `json:"holder"`
	}
)

// toolCaller calls a tool and returns its text and isError, over HTTP or
// stdio.
type toolCaller func(tool string, args any) (string, bool)

// capErrorOf decodes a capture tool's error text.
func capErrorOf(t testing.TB, text string) capError {
	t.Helper()
	var out struct {
		Error *capError `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Error == nil {
		t.Fatalf("not a capture error: %s (%v)", text, err)
	}
	return *out.Error
}

// isCaptureResult reports whether a capture_wait answer is a done
// capture's result rather than a state.
func isCaptureResult(t testing.TB, text string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("capture_wait answered %s: %v", text, err)
	}
	_, ok := m["capture"]
	return ok
}

// startCapture calls capture, which must accept, and returns the job.
func startCapture(t testing.TB, call toolCaller, args map[string]any) string {
	t.Helper()
	text, isErr := call("capture", args)
	var a struct{ Job, Status string }
	if isErr || json.Unmarshal([]byte(text), &a) != nil || a.Status != "accepted" || a.Job == "" {
		t.Fatalf("capture: %s", text)
	}
	return a.Job
}

// waitCaptureEnd calls capture_wait until the job ends: its result, or
// its error.
func waitCaptureEnd(t testing.TB, call toolCaller, job string) (string, bool) {
	t.Helper()
	var text string
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); {
		var isErr bool
		text, isErr = call("capture_wait", map[string]any{"id": job, "timeout": "10s"})
		if isErr || isCaptureResult(t, text) {
			return text, isErr
		}
	}
	t.Fatalf("%s never ended: %s", job, text)
	return "", false
}

// captureResultOf is the result of a job that must end done.
func captureResultOf(t testing.TB, call toolCaller, job string) capResult {
	t.Helper()
	text, isErr := waitCaptureEnd(t, call, job)
	var r capResult
	if isErr || json.Unmarshal([]byte(text), &r) != nil {
		t.Fatalf("%s did not end done: %s", job, text)
	}
	return r
}

func (c *httpClient) capture(args map[string]any) string {
	c.t.Helper()
	return startCapture(c.t, c.try, args)
}

func (c *httpClient) captureEnd(job string) (string, bool) {
	c.t.Helper()
	return waitCaptureEnd(c.t, c.try, job)
}

func (c *httpClient) captureResult(job string) capResult {
	c.t.Helper()
	return captureResultOf(c.t, c.try, job)
}

// waitCapture calls capture_wait with short timeouts until the job's
// state passes ok; the job must not end first.
func (c *httpClient) waitCapture(job string, ok func(jobState) bool) jobState {
	c.t.Helper()
	var v jobState
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); {
		text, isErr := c.try("capture_wait", map[string]any{"id": job, "timeout": "200ms"})
		if isErr || isCaptureResult(c.t, text) {
			c.t.Fatalf("%s ended: %s", job, text)
		}
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			c.t.Fatal(err)
		}
		if ok(v) {
			return v
		}
	}
	c.t.Fatalf("%s never reached the state: %+v", job, v)
	return v
}

// cancelCapture calls capture_cancel and returns its status.
func (c *httpClient) cancelCapture(id string) string {
	c.t.Helper()
	var a struct{ Status string }
	text := c.call("capture_cancel", map[string]any{"id": id})
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		c.t.Fatalf("capture_cancel: %s (%v)", text, err)
	}
	return a.Status
}

// tryAsync is try without failing the test, for any goroutine.
func (c *httpClient) tryAsync(tool string, args any) (string, bool, error) {
	id := strconv.FormatInt(c.next.Add(1), 10)
	a, err := c.send(id, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", false, err
	}
	r, err := decode(a, id)
	if err != nil {
		return "", false, err
	}
	return r.tool()
}

// workloadArgs are capture's arguments for the workload sleeping, profiled
// for d.
func workloadArgs(t testing.TB, d string) map[string]any {
	return map[string]any{"command": []any{capturetest.Workload(t), "sleep", "120s"}, "duration": d}
}

func profiling(v jobState) bool { return v.Status == "running" && v.Phase == capture.PhaseProfiling }

// capturesTree lists every entry under dir with its type, size and
// modification time, in lexical order.
func capturesTree(t testing.TB, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, fmt.Sprintf("%s %v %d %d", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// dirNames are the names in dir, sorted.
func dirNames(t testing.TB, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// TestHubCapturesOverHTTP: a capture over HTTP is accepted, ends done
// with its derivations ready and a publication, which the page serves.
func TestHubCapturesOverHTTP(t *testing.T) {
	h := startCaptureHub(t, capturetest.FakePerfAgent(t, capturetest.FakeOptions{}), hubOptions{})
	// A hub left behind is searched for by this name.
	if r, _ := procOf(h.cmd.Process.Pid); !strings.HasPrefix(r.comm, "perf-agent-hub") {
		t.Errorf("the hub's comm is %q, not perf-agent-hub…", r.comm)
	}
	c := h.client("claude-code")
	job := c.capture(workloadArgs(t, "1s"))
	r := c.captureResult(job)
	if r.Capture.Capture != "adhoc/001" || r.Derived != "ready" || r.Publication == "" || r.DiffPublication != "" {
		t.Fatalf("result: capture %q, derived %q (%s), publication %q, diff %q",
			r.Capture.Capture, r.Derived, r.DeriveError, r.Publication, r.DiffPublication)
	}
	for _, p := range r.Profiles {
		if _, err := os.Stat(filepath.Join(h.profiles, p.Path)); err != nil {
			t.Errorf("profile %s: %v", p.Path, err)
		}
	}
	code, model := httpGet(t, h.pageURL()+"pub/"+r.Publication+"/model")
	if code != http.StatusOK || len(model) == 0 {
		t.Errorf("GET pub/%s/model: %d, %d bytes", r.Publication, code, len(model))
	}
	var state struct {
		Engine struct {
			Current string `json:"current"`
		} `json:"engine"`
	}
	if code, body := httpGet(t, h.pageURL()+"state"); code != http.StatusOK || json.Unmarshal(body, &state) != nil || state.Engine.Current != r.Publication {
		t.Errorf("GET state: %d %.200s, want %s current", code, body, r.Publication)
	}
}

// TestDeleteAndEvictionNeverCancelAJob: a job outlives the session that
// started it, whether the client ends its session or the hub evicts it
// for another (MaxSessions 2). Two other sessions wait for it at once,
// each to its own deadline; one cancels it, and the other sees it
// cancelled. The capture is rolled back to a cancelled manifest.
func TestDeleteAndEvictionNeverCancelAJob(t *testing.T) {
	for _, end := range []string{"deleted", "evicted"} {
		t.Run(end, func(t *testing.T) {
			h := startCaptureHub(t, capturetest.FakePerfAgent(t, capturetest.FakeOptions{}),
				hubOptions{args: []string{"--limit", "Hub.MaxSessions=2"}})
			a := h.client("A")
			job := a.capture(workloadArgs(t, "1m"))
			a.waitCapture(job, profiling)
			if end == "deleted" {
				a.delete()
			}
			b, c := h.client("B"), h.client("C")
			if end == "evicted" {
				if ans, err := a.send("1", "tools/call", map[string]any{"name": "capture_list", "arguments": map[string]any{}}); err != nil || ans.status != http.StatusNotFound {
					t.Fatalf("A's session after B and C started: %d %s (%v), want 404", ans.status, ans.body, err)
				}
			}

			type waited struct {
				text  string
				isErr bool
				err   error
				took  time.Duration
			}
			wait := func(cl *httpClient, timeout string, out *waited) {
				start := time.Now()
				out.text, out.isErr, out.err = cl.tryAsync("capture_wait", map[string]any{"id": job, "timeout": timeout})
				out.took = time.Since(start)
			}
			var bw, cw waited
			var wg sync.WaitGroup
			wg.Go(func() { wait(b, "1s", &bw) })
			wg.Go(func() { wait(c, "3s", &cw) })
			wg.Wait()
			for _, w := range []struct {
				name     string
				got      waited
				min, max time.Duration
			}{{"B", bw, time.Second, 2500 * time.Millisecond}, {"C", cw, 3 * time.Second, 4500 * time.Millisecond}} {
				var v jobState
				if w.got.err != nil || w.got.isErr || json.Unmarshal([]byte(w.got.text), &v) != nil || v.Status != "running" {
					t.Errorf("%s's wait: %s (%v)", w.name, w.got.text, w.got.err)
				}
				if w.got.took < w.min || w.got.took > w.max {
					t.Errorf("%s's wait took %v, want [%v, %v]", w.name, w.got.took, w.min, w.max)
				}
			}
			if s := b.cancelCapture(job); s != "cancelling" {
				t.Errorf("B's cancel: %q", s)
			}
			text, isErr := c.captureEnd(job)
			if e := capErrorOf(t, text); !isErr || e.Code != string(capture.CodeCancelled) || e.Status != "cancelled" {
				t.Errorf("C's wait after B's cancel: %s", text)
			}

			series := filepath.Join(h.profiles, hubCaptures, "adhoc")
			if names := dirNames(t, series); !slices.Equal(names, []string{".seq", "001-run.capture.json"}) {
				t.Errorf("the series after the cancel holds %v, want only .seq and the manifest", names)
			}
			var m struct {
				Outcome string `json:"outcome"`
				Error   *struct {
					Code string `json:"code"`
				} `json:"error"`
				Files []json.RawMessage `json:"files"`
			}
			if b, err := os.ReadFile(filepath.Join(series, "001-run.capture.json")); err != nil || json.Unmarshal(b, &m) != nil ||
				m.Outcome != "failed" || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) || len(m.Files) != 0 {
				t.Errorf("manifest: %s (%v)", b, err)
			}
		})
	}
}

// heldCalls fills the hub's calls while the engine is held: one call of
// open holds the engine through the hub's HoldEngine files, and calls of
// top wait for the engine behind it.
type heldCalls struct {
	t    *testing.T
	hold string
	wg   sync.WaitGroup
	mu   sync.Mutex
	errs []error
}

// newHeldCalls arms the hold files for a new hold of open.
func newHeldCalls(t *testing.T, hold string) *heldCalls {
	t.Helper()
	for _, f := range []string{".held", ".release"} {
		if err := os.Remove(hold + f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(hold+".tool", []byte("open"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &heldCalls{t: t, hold: hold}
}

// call sends one tool call from c and records a failure to answer it. A
// call refused because the calls are full is sent again until it is
// admitted: the probe of full may have taken its place for a moment.
func (h *heldCalls) call(c *httpClient, tool string, args any) {
	h.wg.Go(func() {
		params := map[string]any{"name": tool, "arguments": args}
		for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			id := strconv.FormatInt(c.next.Add(1), 10)
			a, err := c.send(id, "tools/call", params)
			if err == nil && a.status == http.StatusServiceUnavailable && time.Now().Before(end) {
				continue
			}
			if err == nil {
				_, err = decode(a, id)
			}
			if err != nil {
				h.mu.Lock()
				h.errs = append(h.errs, fmt.Errorf("%s: %w", tool, err))
				h.mu.Unlock()
			}
			return
		}
	})
}

// holdEngine starts the call that holds the engine, and returns once it
// does.
func (h *heldCalls) holdEngine(c *httpClient) {
	h.t.Helper()
	h.call(c, "open", map[string]any{"path": "cpu.pb.gz"})
	if !eventually(10*time.Second, func() bool { _, err := os.Stat(h.hold + ".held"); return err == nil }) {
		h.t.Fatal("open never held the engine")
	}
}

// fill starts n calls that wait for the engine.
func (h *heldCalls) fill(c *httpClient, n int) {
	for range n {
		h.call(c, "top", map[string]any{"handle": "h1", "view": "cpu"})
	}
}

// full waits until a call from probe is refused for MaxCalls=n.
func (h *heldCalls) full(probe *httpClient, n int) {
	h.t.Helper()
	want := fmt.Sprintf("MaxCalls=%d reached", n)
	var last answer
	if !eventually(10*time.Second, func() bool {
		a, err := probe.send("1", "tools/call", map[string]any{"name": "capture_list", "arguments": map[string]any{}})
		last = a
		return err == nil && a.status == http.StatusServiceUnavailable && bytes.Contains(a.body, []byte(want))
	}) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.t.Fatalf("the calls never filled: %d %s; held calls failed: %v", last.status, last.body, h.errs)
	}
}

// release lets the engine go and waits until every call is answered.
func (h *heldCalls) release() {
	h.t.Helper()
	if err := os.WriteFile(h.hold+".release", nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.wg.Wait()
	for _, err := range h.errs {
		h.t.Errorf("a held call: %v", err)
	}
}

// TestCaptureCancelUnderFullCallsWithTheEngineHeld: with every call taken
// by tools held behind the engine, capture_cancel, a control call, still
// answers at once. A job waiting for the engine before its capture has
// an id ends cancelled while the engine is still held, having written
// nothing; a job already committed, waiting for the engine to derive, is
// "too late: committed", and its files stay.
func TestCaptureCancelUnderFullCallsWithTheEngineHeld(t *testing.T) {
	const maxCalls = 4
	hold := filepath.Join(t.TempDir(), "hold")
	t.Cleanup(func() { _ = os.WriteFile(hold+".release", nil, 0o600) })
	h := startCaptureHub(t, capturetest.FakePerfAgent(t, capturetest.FakeOptions{}), hubOptions{
		env:  []string{holdEnv + "=" + hold},
		args: []string{"--limit", "Hub.MaxCalls=" + strconv.Itoa(maxCalls), "--limit", "Hub.MaxCallsPerSession=" + strconv.Itoa(maxCalls)},
	})
	a, w := h.client("A"), h.client("W")
	captures := filepath.Join(h.profiles, hubCaptures)
	timedCancel := func(id, want string) {
		t.Helper()
		start := time.Now()
		got := a.cancelCapture(id)
		took := time.Since(start)
		if got != want || took >= 100*time.Millisecond {
			t.Errorf("capture_cancel %s under full calls: %q after %v, want %q under 100ms", id, got, took, want)
		}
	}

	// A job waiting for the engine, before its capture has an id.
	held := newHeldCalls(t, hold)
	held.holdEngine(w)
	job := a.capture(workloadArgs(t, "1m"))
	a.waitCapture(job, func(v jobState) bool { return v.Phase == "retention" })
	held.fill(w, maxCalls-1)
	held.full(a, maxCalls)
	timedCancel(job, "cancelling")
	var last string
	if !eventually(10*time.Second, func() bool { last = a.cancelCapture(job); return last == "cancelled" }) {
		t.Fatalf("the job never ended cancelled while the engine was held: %q", last)
	}
	if _, err := os.Lstat(filepath.Join(captures, "adhoc")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cancelled job made its series (%v)", err)
	}
	held.full(a, maxCalls)
	held.release()
	text, isErr := a.captureEnd(job)
	if e := capErrorOf(t, text); !isErr || e.Code != string(capture.CodeCancelled) || e.Status != "cancelled" || e.Capture != "" {
		t.Errorf("the cancelled job: %s", text)
	}

	// A committed job waiting for the engine to derive.
	held = newHeldCalls(t, hold)
	job = a.capture(workloadArgs(t, "3s"))
	a.waitCapture(job, func(v jobState) bool { return v.Status == "running" })
	held.holdEngine(w)
	st := a.waitCapture(job, func(v jobState) bool { return v.Status == "done" })
	if st.Derived != "pending" || st.Capture != "adhoc/001" {
		t.Fatalf("the committed job: %+v, want adhoc/001 deriving", st)
	}
	held.fill(w, maxCalls-1)
	held.full(a, maxCalls)
	series := filepath.Join(captures, "adhoc")
	before := capturesTree(t, series)
	if !slices.ContainsFunc(before, func(e string) bool { return strings.HasPrefix(e, "001-run.capture.json ") }) {
		t.Fatalf("no manifest in %v", before)
	}
	timedCancel("adhoc/001", "too late: committed")
	held.release()
	r := a.captureResult(job)
	if r.Capture.Capture != "adhoc/001" || r.Derived != "ready" || r.Publication == "" {
		t.Errorf("the result after a late cancel: capture %q, derived %q (%s), publication %q", r.Capture.Capture, r.Derived, r.DeriveError, r.Publication)
	}
	if after := capturesTree(t, series); !slices.Equal(after, before) {
		t.Errorf("the late cancel changed the series:\n%v\nthen\n%v", before, after)
	}
}

// TestAnotherHubHoldsTheLease: two hubs, with their own runtime
// directories and ports, share one profiles root and so one captures
// directory. While hub 1 captures, hub 2 accepts a capture, whose job
// fails busy naming hub 1's capture and pid. Hub 2's attempt changes
// nothing in the captures directory: hub 1 is stopped while it runs, so
// its heartbeat does not move either, and the tree, every name, size and
// modification time, is the same before and after.
func TestAnotherHubHoldsTheLease(t *testing.T) {
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{})
	profiles := profilesDir(t)
	h1 := startCaptureHub(t, fake, hubOptions{profiles: profiles})
	c1 := h1.client("one")
	job := c1.capture(workloadArgs(t, "1m"))
	c1.waitCapture(job, profiling)
	h1.procs.watch()
	h2 := startCaptureHub(t, fake, hubOptions{profiles: profiles})
	c2 := h2.client("two")

	captures := filepath.Join(profiles, hubCaptures)
	thaw := h1.freeze()
	before := capturesTree(t, captures)
	text, isErr := c2.captureEnd(c2.capture(workloadArgs(t, "1s")))
	after := capturesTree(t, captures)
	thaw()
	e := capErrorOf(t, text)
	if !isErr || e.Code != string(capture.CodeBusy) || !e.Retryable ||
		!strings.Contains(e.Message, "capture adhoc/001") || !strings.Contains(e.Message, fmt.Sprintf("pid %d", h1.cmd.Process.Pid)) {
		t.Errorf("hub 2's capture: %s, want busy naming capture adhoc/001 of pid %d", text, h1.cmd.Process.Pid)
	}
	if d, err := time.ParseDuration(e.RetryAfter); err != nil || d <= 0 || d > 5*time.Second {
		t.Errorf("busy retry_after %q, want at most 5s", e.RetryAfter)
	}
	if !slices.Equal(before, after) {
		t.Errorf("hub 2's attempt changed the captures directory:\n%s\nthen\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}

	if s := c1.cancelCapture(job); s != "cancelling" {
		t.Errorf("hub 1's cancel: %q", s)
	}
	if text, isErr := c1.captureEnd(job); !isErr || capErrorOf(t, text).Code != string(capture.CodeCancelled) {
		t.Errorf("hub 1's capture: %s", text)
	}
}

// TestAKilledHubIsRecoveredByTheNext: a hub SIGKILLed while it profiles,
// and another started at once on the same runtime directory, port and
// profiles root. Its reaper may still hold the captures lock for a while:
// a wait by the capture id answers in_progress naming the dead hub, or
// interrupted, and within the stop grace and 15 s, interrupted and
// retryable. A capture started meanwhile is accepted, and ends done or
// fails busy. The case where the reaper is held for certain is
// perfmcp's TestADeadHubsReaperHoldsTheLease.
func TestAKilledHubIsRecoveredByTheNext(t *testing.T) {
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{})
	profiles, xdg := profilesDir(t), t.TempDir()
	h1 := startCaptureHub(t, fake, hubOptions{profiles: profiles, xdg: xdg})
	c1 := h1.client("one")
	job := c1.capture(workloadArgs(t, "1m"))
	if st := c1.waitCapture(job, profiling); st.Capture != "adhoc/001" {
		t.Fatalf("the capture is %+v", st)
	}
	dead := h1.cmd.Process.Pid
	h1.kill9()

	h2 := startCaptureHub(t, fake, hubOptions{profiles: profiles, xdg: xdg, port: h1.port})
	// The bound counts from when the new hub serves: its own start-up time
	// on a loaded machine is no part of recovery.
	restart := time.Now()
	c2 := h2.client("two")
	meanwhile := c2.capture(workloadArgs(t, "1s"))
	deadline := restart.Add(stopGrace + 15*time.Second)
	var answers []string
	for {
		text, isErr := c2.try("capture_wait", map[string]any{"id": "adhoc/001", "timeout": "1s"})
		answers = append(answers, text)
		if isErr {
			e := capErrorOf(t, text)
			if e.Status != "interrupted" || !e.Retryable || e.Capture != "adhoc/001" {
				t.Fatalf("capture_wait adhoc/001: %s, want in_progress or interrupted", text)
			}
			break
		}
		var ip inProgress
		if json.Unmarshal([]byte(text), &ip) != nil || ip.Status != "in_progress" || ip.Capture != "adhoc/001" ||
			!strings.Contains(ip.Holder, fmt.Sprintf("pid %d", dead)) {
			t.Fatalf("capture_wait adhoc/001: %s, want in_progress naming the dead hub pid %d, or interrupted", text, dead)
		}
		if time.Now().After(deadline) {
			t.Fatalf("not interrupted %v after the restart: %s", stopGrace+15*time.Second, strings.Join(answers, "\n"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("interrupted %v after the restart, after %d answers", time.Since(restart).Round(time.Millisecond), len(answers))

	text, isErr := c2.captureEnd(meanwhile)
	switch {
	case !isErr:
		var r capResult
		if json.Unmarshal([]byte(text), &r) != nil || r.Capture.Capture == "" {
			t.Errorf("the capture started meanwhile: %s", text)
		}
	case capErrorOf(t, text).Code != string(capture.CodeBusy):
		t.Errorf("the capture started meanwhile: %s, want done or busy", text)
	}
}

// stdioProc is perf-agent-mcp, the stdio server, run as its own test
// binary over pipes.
type stdioProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	lines  chan []byte
	stderr *lockedBuffer
	exited chan struct{}
	next   int
}

// stdioTestBinary builds perf-agent-mcp's test binary, beside the hub's.
var stdioTestBinary = sync.OnceValues(func() (string, error) {
	dir, err := binDir()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "perf-agent-mcp.test")
	if out, err := exec.Command("go", "test", "-c", "-buildvcs=false", "-o", bin, "../perf-agent-mcp").CombinedOutput(); err != nil {
		return "", fmt.Errorf("go test -c ../perf-agent-mcp: %v\n%s", err, out)
	}
	return bin, nil
})

// startStdio starts the stdio server capturing with fake, initialized,
// and watched by w. The test's cleanup ends its input, which ends it.
func startStdio(t *testing.T, w *procWatch, profiles, fake string, args ...string) *stdioProc {
	t.Helper()
	bin, err := stdioTestBinary()
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]string{"--profiles", profiles, "--perf-agent", fake}, captureLimits...), args...)
	s := &stdioProc{t: t, cmd: exec.Command(bin, all...), lines: make(chan []byte, 16), stderr: &lockedBuffer{}, exited: make(chan struct{})}
	s.cmd.Env = append(os.Environ(), stdioFakeVerifyEnv+"=1")
	s.cmd.Stderr = s.stderr
	killWithTest(s.cmd)
	if s.in, err = s.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := s.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		r := bufio.NewReader(out)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				s.lines <- line
			}
			if err != nil {
				break
			}
		}
		close(s.lines)
		_ = s.cmd.Wait()
		close(s.exited)
	}()
	w.add(s.cmd.Process.Pid)
	t.Cleanup(func() {
		_ = s.in.Close()
		go func() {
			for range s.lines {
			}
		}()
		select {
		case <-s.exited:
		case <-time.After(30 * time.Second):
			t.Error("perf-agent-mcp did not exit at the end of its input")
			_ = s.cmd.Process.Kill()
			<-s.exited
		}
		if t.Failed() {
			t.Logf("perf-agent-mcp stderr:\n%s", s.stderr.String())
		}
	})
	t.Cleanup(func() { w.watch() })
	s.rpc("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "stdio", "version": "1"},
	})
	s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

func (s *stdioProc) write(msg map[string]any) {
	s.t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// rpc sends a request and returns its answer, skipping anything else.
func (s *stdioProc) rpc(method string, params any) rpcAnswer {
	s.t.Helper()
	s.next++
	id := strconv.Itoa(s.next)
	s.write(map[string]any{"jsonrpc": "2.0", "id": s.next, "method": method, "params": params})
	timeout := time.After(60 * time.Second)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				s.t.Fatalf("perf-agent-mcp ended before answering %s:\n%s", method, s.stderr.String())
			}
			var r rpcAnswer
			if json.Unmarshal(line, &r) == nil && string(r.ID) == id {
				return r
			}
		case <-timeout:
			s.t.Fatalf("perf-agent-mcp did not answer %s in 60s", method)
		}
	}
}

// try calls a tool and returns its text and isError.
func (s *stdioProc) try(tool string, args any) (string, bool) {
	s.t.Helper()
	text, isErr, err := s.rpc("tools/call", map[string]any{"name": tool, "arguments": args}).tool()
	if err != nil {
		s.t.Fatalf("%s: %v", tool, err)
	}
	return text, isErr
}

// refusesTheOthersDir checks that call's server refuses captures in a
// directory the other kind of server made: each capture, refused at once
// or accepted and then failed, is owner_mismatch naming both kinds, and
// tells the agent to give the server its own directory with --captures.
func refusesTheOthersDir(t *testing.T, call toolCaller, args map[string]any, theirs, theirBinary, mine string) {
	t.Helper()
	for range 2 {
		text, isErr := call("capture", args)
		if !isErr {
			var a struct{ Job string }
			if err := json.Unmarshal([]byte(text), &a); err != nil || a.Job == "" {
				t.Fatalf("capture: %s", text)
			}
			if text, isErr = waitCaptureEnd(t, call, a.Job); !isErr {
				t.Fatalf("a %s server captured in a %s directory: %s", mine, theirs, text)
			}
		}
		e := capErrorOf(t, text)
		if e.Code != string(capture.CodeOwnerMismatch) ||
			!strings.Contains(e.Message, "belongs to a "+theirBinary+" server (owner kind "+theirs+")") ||
			!strings.Contains(e.Message, "(owner kind "+mine+")") || !strings.Contains(e.Next, "--captures") {
			t.Errorf("the %s server's capture: %s", mine, text)
		}
	}
}

// TestStdioAndHubRefuseEachOthersDirAtTheBinary: the two binaries, given
// one captures directory with --captures, never share it: whichever made
// it first, the other's captures are owner_mismatch, pointing at
// --captures, and it writes nothing there.
func TestStdioAndHubRefuseEachOthersDirAtTheBinary(t *testing.T) {
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{})
	shared := []string{"--captures", "shared"}

	t.Run("stdio refuses the hub's", func(t *testing.T) {
		profiles := profilesDir(t)
		h := startCaptureHub(t, fake, hubOptions{profiles: profiles, args: shared})
		c := h.client("hub")
		c.captureResult(c.capture(workloadArgs(t, "1s")))
		before := capturesTree(t, filepath.Join(profiles, "shared"))
		s := startStdio(t, newProcWatch(t), profiles, fake, shared...)
		refusesTheOthersDir(t, s.try, workloadArgs(t, "1s"), "hub", "perf-agent-hub", "stdio")
		if after := capturesTree(t, filepath.Join(profiles, "shared")); !slices.Equal(before, after) {
			t.Errorf("the stdio server changed the hub's directory:\n%s\nthen\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
		}
	})

	t.Run("the hub refuses stdio's", func(t *testing.T) {
		profiles := profilesDir(t)
		s := startStdio(t, newProcWatch(t), profiles, fake, shared...)
		captureResultOf(t, s.try, startCapture(t, s.try, workloadArgs(t, "1s")))
		before := capturesTree(t, filepath.Join(profiles, "shared"))
		h := startCaptureHub(t, fake, hubOptions{profiles: profiles, args: shared})
		refusesTheOthersDir(t, h.client("hub").try, workloadArgs(t, "1s"), "stdio", "perf-agent-mcp", "hub")
		if after := capturesTree(t, filepath.Join(profiles, "shared")); !slices.Equal(before, after) {
			t.Errorf("the hub changed the stdio server's directory:\n%s\nthen\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
		}
	})
}

// socketsOf are the inodes of the sockets pid holds, nil once it is gone.
func socketsOf(pid int) []string {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if link, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && strings.HasPrefix(link, "socket:") {
			out = append(out, link)
		}
	}
	return out
}

// TestEverySocketIsLoopbackWhileCapturing is TestEverySocketIsLoopback
// while the hub captures with the fake perf-agent: the hub's sockets are
// the loopback listener and connections it accepted, and the processes the
// capture runs under it, its reaper among them, hold no socket at all.
func TestEverySocketIsLoopbackWhileCapturing(t *testing.T) {
	h := startCaptureHub(t, capturetest.FakePerfAgent(t, capturetest.FakeOptions{}), hubOptions{})
	c := h.client("claude")
	job := c.capture(workloadArgs(t, "1m"))
	c.waitCapture(job, profiling)

	o := c.open("cpu.pb.gz")
	pub := c.publish(o)
	c.call("page_state", map[string]any{})
	pageURL := h.pageURL()
	page := strings.TrimPrefix(pageURL, c.origin)
	for _, route := range []string{"", "state", "pub/" + pub + "/model"} {
		if status, body := c.get(page + route); status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", redact(page+route), status, body)
		}
	}
	stream, err := (&http.Client{}).Get(pageURL + "events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	if line, err := bufio.NewReader(stream.Body).ReadString('\n'); err != nil || line != "event: state\n" {
		t.Fatalf("stream: %q (%v)", line, err)
	}
	if status := postReport(t, pageURL, map[string]string{"kind": "select", "publication": pub, "node": c.nodeID(o, "a"), "focus": ""}); status != http.StatusNoContent {
		t.Fatalf("POST inbox: %d, want 204", status)
	}

	listening, connected := checkHubSockets(t, h.cmd.Process.Pid, h.port)
	if listening != 1 || connected < 1 {
		t.Errorf("%d listening and %d connected sockets, want 1 and at least 1", listening, connected)
	}
	under := h.procs.watch()
	if !slices.ContainsFunc(under, func(r procRef) bool { return strings.HasPrefix(r.comm, "perf-agent-hub") }) ||
		!slices.ContainsFunc(under, func(r procRef) bool { return r.comm == "fakeperfagent" }) {
		t.Fatalf("no reaper and perf-agent under the hub while it captures: %+v", under)
	}
	for _, r := range under {
		if s := socketsOf(r.pid); len(s) > 0 {
			t.Errorf("%s (pid %d), under the hub, holds sockets %v", r.comm, r.pid, s)
		}
	}

	if s := c.cancelCapture(job); s != "cancelling" {
		t.Errorf("cancel: %q", s)
	}
	if text, isErr := c.captureEnd(job); !isErr || capErrorOf(t, text).Code != string(capture.CodeCancelled) {
		t.Errorf("the cancelled capture: %s", text)
	}
}

// TestSIGTERMKillsTheCaptureFirst: SIGTERM while a capture runs a
// workload that has left a grandchild in a session of its own, and while
// a call holding the engine, which ignores its context, makes the HTTP
// server use its whole 1 s grace: the fake perf-agent's process group and
// the grandchild are gone within the stop grace and 500 ms, well before
// that grace ends, since the captures are killed first; and the hub exits
// 0 within its grace and 300 ms.
func TestSIGTERMKillsTheCaptureFirst(t *testing.T) {
	const grace = 200 * time.Millisecond
	record := filepath.Join(t.TempDir(), "record.json")
	hold := filepath.Join(t.TempDir(), "hold")
	t.Cleanup(func() { _ = os.WriteFile(hold+".release", nil, 0o600) })
	h := startCaptureHub(t, capturetest.FakePerfAgent(t, capturetest.FakeOptions{Record: record}), hubOptions{
		env:  []string{holdEnv + "=" + hold},
		args: []string{"--limit", "Capture.StopGrace=" + grace.String()},
	})
	c := h.client("A")
	job := c.capture(map[string]any{"command": []any{capturetest.Workload(t), "double-fork-setsid", "2m"}, "duration": "1m"})
	c.waitCapture(job, profiling)
	var rec fakemode.Record
	if b, err := os.ReadFile(record); err != nil || json.Unmarshal(b, &rec) != nil || rec.PGID <= 0 {
		t.Fatalf("the fake's record: %s (%v)", b, err)
	}
	var grandchild procRef
	if !eventually(30*time.Second, func() bool {
		under := h.procs.watch()
		i := slices.IndexFunc(under, func(r procRef) bool { return r.comm == "capworkload" && r.sid == r.pid })
		if i >= 0 {
			grandchild = under[i]
		}
		return i >= 0
	}) {
		t.Fatalf("no setsid grandchild of the workload under the hub: %+v", h.procs.watch())
	}
	newHeldCalls(t, hold).holdEngine(h.client("holder"))

	sent := time.Now()
	gone := make(chan time.Duration, 1)
	go func() {
		for time.Since(sent) < 10*time.Second {
			if groupGone(rec.PGID) && grandchild.gone() {
				gone <- time.Since(sent)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		gone <- -1
	}()
	took := h.sigterm(time.Second + 300*time.Millisecond)
	killed := <-gone
	t.Logf("exited %v and the capture's processes gone %v after SIGTERM", took.Round(time.Millisecond), killed.Round(time.Millisecond))
	if killed < 0 || killed > grace+500*time.Millisecond {
		t.Errorf("the fake's group %d and the grandchild %d were gone %v after SIGTERM, want within %v",
			rec.PGID, grandchild.pid, killed, grace+500*time.Millisecond)
	}
	// The held call kept the HTTP server's grace in use: a kill after it
	// would have come too late.
	if took < 800*time.Millisecond {
		t.Errorf("exited %v after SIGTERM: the held call did not hold the HTTP grace", took)
	}
}
