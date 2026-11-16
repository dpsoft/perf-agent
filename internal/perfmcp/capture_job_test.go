package perfmcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// faultGates is a captureFault a test arms while jobs run: a point can
// block its first occurrence until released, or fail with an error.
type faultGates struct {
	mu    sync.Mutex
	gates map[faultPoint]*gate
	errs  map[faultPoint]error
}

// gate blocks the first time its point is reached: entered is closed
// then, and the point waits for release.
type gate struct {
	entered, release chan struct{}
	used             bool
}

func newFaultGates() *faultGates {
	return &faultGates{gates: map[faultPoint]*gate{}, errs: map[faultPoint]error{}}
}

func (f *faultGates) fault(p faultPoint) error {
	f.mu.Lock()
	g := f.gates[p]
	if g != nil && !g.used {
		g.used = true
	} else {
		g = nil
	}
	f.mu.Unlock()
	if g != nil {
		close(g.entered)
		<-g.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errs[p]
}

// block arms p: its next occurrence blocks until the gate is opened.
func (f *faultGates) block(t *testing.T, p faultPoint) *gate {
	t.Helper()
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.gates[p] = g
	f.mu.Unlock()
	t.Cleanup(g.open)
	return g
}

// fail makes every occurrence of p return err (nil disarms it).
func (f *faultGates) fail(p faultPoint, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[p] = err
}

func (g *gate) open() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

// reached waits until the gate's point is blocked on it.
func (g *gate) reached(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the fault point was never reached")
	}
}

// jobOpts configures a capture-enabled test server.
type jobOpts struct {
	fake     capturetest.FakeOptions
	limits   func(*Limits)
	caps     capture.CapSet
	live     bool
	profiles string
	// setup runs on the new server before its maintenance starts.
	setup func(*Server)
}

// jobEnv is a server whose perf-agent is the fake, and what it ran.
type jobEnv struct {
	t        *testing.T
	s        *Server
	profiles string
	fake     string
	record   string
	faults   *faultGates
	mu       sync.Mutex
	groups   []int
}

// newJobEnv starts a capture-enabled server over the fake perf-agent. At
// cleanup it closes the server and fails the test for any process group
// of a fake it ran, or any launched workload, still alive.
func newJobEnv(t *testing.T, o jobOpts) *jobEnv {
	t.Helper()
	capturetest.SkipIfPrivileged(t)
	o.fake.Record = filepath.Join(t.TempDir(), "record.json")
	fake := capturetest.FakePerfAgent(t, o.fake)
	cfg := testConfig(t)
	if o.profiles != "" {
		cfg.ProfilesDir = o.profiles
	}
	cfg.Live = o.live
	cfg.Limits = jobLimits()
	if o.limits != nil {
		o.limits(&cfg.Limits)
	}
	cfg.Capture = CaptureConfig{PerfAgent: fake, Verify: func(p string) (capture.Binary, error) {
		id, err := capture.IdentityOf(p)
		return capture.Binary{Path: p, ID: id, Version: "v0.0.0-test", Caps: o.caps}, err
	}}
	env := &jobEnv{t: t, profiles: cfg.ProfilesDir, fake: fake, record: o.fake.Record, faults: newFaultGates()}
	beforeMaintenance = func(s *Server) {
		s.captureFault = env.faults.fault
		if o.setup != nil {
			o.setup(s)
		}
	}
	s, err := New(cfg)
	beforeMaintenance = nil
	if err != nil {
		t.Fatal(err)
	}
	if s.captureDisabled != "" {
		t.Fatalf("capture is disabled: %s", s.captureDisabled)
	}
	env.s = s
	t.Cleanup(env.noLeftovers)
	t.Cleanup(func() { _ = s.Close() })
	return env
}

// jobLimits are the test's capture limits: short bounds for a loaded
// -race machine, and no free-space floor to speak of.
func jobLimits() Limits {
	l := DefaultLimits()
	l.Capture.StopGrace = 500 * time.Millisecond
	l.Capture.StartTimeout = 20 * time.Second
	l.Capture.FinishTimeout = 20 * time.Second
	l.Capture.MinFreeBytes = 1 << 20
	l.Capture.MaxDuration = 2 * time.Minute
	l.Capture.WindowDuration = 30 * time.Minute
	return l
}

// noLeftovers fails the test for a fake's process group or a launched
// workload (by its PID and start time) still alive.
func (e *jobEnv) noLeftovers() {
	e.noteRecord()
	e.mu.Lock()
	groups := slices.Clone(e.groups)
	e.mu.Unlock()
	for _, g := range groups {
		if !eventuallyTrue(10*time.Second, func() bool { return syscall.Kill(-g, 0) == syscall.ESRCH }) {
			e.t.Errorf("the fake perf-agent's group %d is still alive", g)
			_ = syscall.Kill(-g, syscall.SIGKILL)
		}
	}
	d, ok := e.dir()
	if !ok {
		return
	}
	defer d.close()
	series, _ := seriesDirs(d)
	for _, name := range series {
		ser, err := d.openSeries(name)
		if err != nil {
			continue
		}
		ms, _, _ := seriesManifests(ser)
		_ = ser.Close()
		for _, m := range ms {
			if !m.Target.Launched {
				continue
			}
			pid, start := atoi(m.Target.PID), uint64(atoi(m.Target.StartTime))
			if pid > 0 && !eventuallyTrue(10*time.Second, func() bool { return !capture.Alive(capture.DefaultProc(), pid, start) }) {
				e.t.Errorf("the workload %d of %s/%s is still alive", pid, name, m.ID)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// noteRecord remembers the process group of the fake the last capture
// ran, from its record.
func (e *jobEnv) noteRecord() {
	b, err := os.ReadFile(e.record)
	if err != nil {
		return
	}
	var r fakemode.Record
	if json.Unmarshal(b, &r) != nil || r.PGID <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !slices.Contains(e.groups, r.PGID) {
		e.groups = append(e.groups, r.PGID)
	}
}

// fakeRecord is what the fake of the last capture recorded, and whether it
// ran at all.
func (e *jobEnv) fakeRecord() (fakemode.Record, bool) {
	var r fakemode.Record
	b, err := os.ReadFile(e.record)
	if err != nil {
		return r, false
	}
	return r, json.Unmarshal(b, &r) == nil
}

func eventuallyTrue(d time.Duration, f func() bool) bool {
	end := time.Now().Add(d)
	for {
		if f() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// request is a capture of the workload sleeping, with every setting
// passed explicitly.
func (e *jobEnv) request(d time.Duration) captureRequest {
	w := capturetest.Workload(e.t)
	return captureRequest{
		Command:  []string{w, "sleep", "120s"},
		Settings: capture.Settings{Modes: []capture.Mode{capture.ModeCPU}, Duration: d, SampleRate: 99, Unwind: "auto"},
		Explicit: map[string]bool{"modes": true, "duration": true, "sample_rate": true, "unwind": true, "kernel_stacks": true},
		Series:   "adhoc", Label: "run",
	}
}

// compareRequest is a capture compared with id, inheriting every setting.
func (e *jobEnv) compareRequest(id string) captureRequest {
	r := e.request(0)
	r.Settings = capture.Settings{}
	r.Explicit = map[string]bool{}
	r.CompareWith = id
	r.Label = "cand"
	return r
}

func (e *jobEnv) start(req captureRequest) *job {
	e.t.Helper()
	j, ce := e.s.startJob(req)
	if ce != nil {
		e.t.Fatalf("startJob: %v", ce)
	}
	return j
}

// finish waits for j to end, deriving included.
func (e *jobEnv) finish(j *job) jobView {
	e.t.Helper()
	end := time.Now().Add(60 * time.Second)
	for time.Now().Before(end) {
		v := j.wait(e.t.Context(), 5*time.Second)
		if ended(v) {
			e.noteRecord()
			return v
		}
	}
	e.t.Fatalf("job %s did not end: %+v", j.id, j.view())
	return jobView{}
}

// run is a job of req, waited for.
func (e *jobEnv) run(req captureRequest) (*job, jobView) {
	e.t.Helper()
	j := e.start(req)
	return j, e.finish(j)
}

func ended(v jobView) bool {
	switch jobStatus(v.Status) {
	case jobFailed, jobCancelled:
		return true
	case jobDone:
		return v.Derived != derivedPending
	}
	return false
}

// result is j's decoded result.
func (e *jobEnv) result(j *job) captureResult {
	e.t.Helper()
	j.mu.Lock()
	b := slices.Clone(j.result)
	j.mu.Unlock()
	var r captureResult
	if err := json.Unmarshal(b, &r); err != nil {
		e.t.Fatalf("result of %s: %v (%s)", j.id, err, b)
	}
	return r
}

func jobErr(j *job) *capture.Error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// manifestOf reads the manifest of capture id in series.
func (e *jobEnv) manifestOf(series, id string) (manifest, bool) {
	e.t.Helper()
	d, ok := e.dir()
	if !ok {
		return manifest{}, false
	}
	defer d.close()
	ser, err := d.openSeries(series)
	if err != nil {
		return manifest{}, false
	}
	defer func() { _ = ser.Close() }()
	ms, _, _ := seriesManifests(ser)
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return manifest{}, false
}

// seriesNames are the names in series, sorted.
func (e *jobEnv) seriesNames(series string) []string {
	e.t.Helper()
	d, ok := e.dir()
	if !ok {
		return nil
	}
	defer d.close()
	return dirNames(e.t, d.root, series)
}

// dir is the capture directory, opened on its own: the server's roots may
// be closed already.
func (e *jobEnv) dir() (*captureDir, bool) {
	r, err := os.OpenRoot(filepath.Join(e.profiles, defaultStdioCaptures))
	if err != nil {
		return nil, false
	}
	return &captureDir{root: r, rel: defaultStdioCaptures, owner: ownerStdio, profiles: e.profiles}, true
}

// attachClient serves s over pipes and completes the initialize handshake;
// the caller closes s.
func attachClient(t *testing.T, s *Server) *testClient {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.Serve(t.Context(), sr, sw)
		_ = sw.Close()
		done <- err
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	c := &testClient{t: t, srv: s, w: cw, r: bufio.NewReaderSize(cr, 1<<20), nextID: 1}
	if res := c.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25"}); res.Error != nil {
		t.Fatalf("initialize: %+v", res.Error)
	}
	return c
}

// commitSynthetic commits a capture of st in series with fixture files and
// no process, as a job would, under the directory slot, and returns its
// id. Its profiles differ from the fake perf-agent's, so a compare of the
// two has two handles per mode.
func (e *jobEnv) commitSynthetic(series, label string, st capture.Settings, meta manifest) int {
	e.t.Helper()
	// The directory slot, as a job holds it: the server's start-up
	// maintenance, which finds the directory this creates, must not
	// recover the transaction while it is written.
	if !e.s.lockDir(nil) {
		e.t.Fatal("the server closed")
	}
	defer e.s.unlockDir()
	d, ce := e.s.openCaptureDir()
	if ce != nil {
		e.t.Fatal(ce)
	}
	defer d.close()
	ser, err := d.series(series)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	id, err := e.s.nextID(ser)
	if err != nil {
		e.t.Fatal(err)
	}
	outputs := capture.ExpectedFiles(st.Modes)
	txn, err := e.s.beginTxn(d, series, id, label, toWire(st), outputs)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := txn.makeStaging(); err != nil {
		e.t.Fatal(err)
	}
	for _, out := range outputs {
		b := fileContent(out, label)
		switch out {
		case capture.OutputFile(capture.ModeCPU):
			b = pmfixture.CPUBasicNew()
		case capture.OutputFile(capture.ModeOffCPU):
			b = pmfixture.Build("offcpu", "nanoseconds", nil,
				pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("poll")}, Value: 2000})
		case capture.OutputFile(capture.ModeGPU):
			b = pmfixture.GPUBasic()
		case capture.ExeclogFile:
			b = pmfixture.GPUBasicExeclog(e.t)
		}
		if err := os.WriteFile(filepath.Join(ser.Name(), txn.in.Staging, out), b, 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	if meta.StoppedBy == "" {
		meta.StoppedBy, meta.ProfiledDuration = "duration", st.Duration.String()
	}
	if committed, err := txn.finish(meta, &decisionWord{}); !committed || err != nil {
		e.t.Fatalf("commit: %v %v", committed, err)
	}
	return id
}

var cpuOnly = []capture.Mode{capture.ModeCPU}

// A job goes accepted → preparing → running → committing → done, derives
// once committed, and its result's handle is open in the store.
func TestJobRunsThroughEveryState(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	j, v := e.run(e.request(500 * time.Millisecond))
	if v.Status != string(jobDone) || v.Derived != derivedReady {
		t.Fatalf("view %+v, err %v", v, jobErr(j))
	}
	j.mu.Lock()
	seen := slices.Clone(j.trail)
	j.mu.Unlock()
	want := []string{"accepted", "preparing", "running", "committing", "done/pending", "done/ready"}
	if !slices.Equal(seen, want) {
		t.Errorf("states %q, want %q", seen, want)
	}
	if v.ID != "adhoc/001" || v.Job != j.id {
		t.Errorf("view ids %+v", v)
	}
	m, ok := e.manifestOf("adhoc", "001")
	if !ok || m.Outcome != captureOK || len(m.Files) != 1 || !m.Target.Launched {
		t.Fatalf("manifest %+v", m)
	}
	r := e.result(j)
	if len(r.Profiles) != 1 || r.Profiles[0].Open == nil {
		t.Fatalf("profiles %+v", r.Profiles)
	}
	if r.Profiles[0].Path != "captures/adhoc/001-run-cpu.pb.gz" || r.Capture.ID != "001" || r.Capture.Series != "adhoc" {
		t.Errorf("result %+v", r.Capture)
	}
	if err := e.s.engine.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, open := e.s.store.entries[r.Profiles[0].Open.Handle]
	e.s.engine.release()
	if !open {
		t.Errorf("handle %s is not open in the store", r.Profiles[0].Open.Handle)
	}
	if r.Capture.WorkloadOutputTail == nil || !r.Capture.WorkloadOutputTail.Untrusted {
		t.Errorf("no untrusted workload tail: %+v", r.Capture.WorkloadOutputTail)
	}
	if e.s.registry.active.Load() != nil {
		t.Error("the slot is still held")
	}
	if got, ok := e.s.registry.get("adhoc/001"); !ok || got != j {
		t.Error("the finished job is not in the registry by its capture id")
	}
}

// startJob only admits: with the profiles root unwritable it still returns
// a job at once, and the failure arrives as the job's state.
func TestStartJobDoesNoIO(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	if err := os.Chmod(e.profiles, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.profiles, 0o700) })
	begin := time.Now()
	j, ce := e.s.startJob(e.request(500 * time.Millisecond))
	took := time.Since(begin)
	if ce != nil {
		t.Fatalf("startJob refused: %v", ce)
	}
	if took > 20*time.Millisecond {
		t.Errorf("startJob took %v", took)
	}
	v := e.finish(j)
	if v.Status != string(jobFailed) || jobErr(j) == nil || jobErr(j).Code != capture.CodeDisk {
		t.Fatalf("view %+v, err %v", v, jobErr(j))
	}
	if _, ok := e.fakeRecord(); ok {
		t.Error("perf-agent ran")
	}
}

// Each blocked seam blocks only its job: admission returns at once, a wait
// returns the current state at its deadline, and the job finishes once
// the seam opens.
func TestBlockedSeamsNeverBlockAdmissionOrWait(t *testing.T) {
	for _, p := range []faultPoint{faultHash, faultRetentionHash, faultFsync, faultInEngine, faultBeforeOpen, faultBeforeDiff} {
		t.Run(p.String(), func(t *testing.T) {
			e := newJobEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.KeepPerSeries = 1 }})
			for range 2 {
				e.commitSynthetic("adhoc", "run", capture.Settings{Modes: cpuOnly, Duration: 500 * time.Millisecond, SampleRate: 99, Unwind: "auto"}, manifest{})
			}
			g := e.faults.block(t, p)
			req := e.compareRequest("002")
			begin := time.Now()
			j := e.start(req)
			if took := time.Since(begin); took > 20*time.Millisecond {
				t.Errorf("startJob took %v", took)
			}
			g.reached(t)
			begin = time.Now()
			v := j.wait(t.Context(), 100*time.Millisecond)
			if took := time.Since(begin); took < 90*time.Millisecond || took > time.Second {
				t.Errorf("wait took %v", took)
			}
			if ended(v) {
				t.Errorf("the job ended while %s blocked: %+v", p, v)
			}
			g.open()
			if v := e.finish(j); v.Status != string(jobDone) || v.Derived != derivedReady {
				t.Fatalf("view %+v, err %v", v, jobErr(j))
			}
		})
	}
	t.Run("the engine", func(t *testing.T) {
		e := newJobEnv(t, jobOpts{})
		if err := e.s.engine.acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		held := true
		defer func() {
			if held {
				e.s.engine.release()
			}
		}()
		j := e.start(e.request(300 * time.Millisecond))
		v := j.wait(t.Context(), 100*time.Millisecond)
		if v.Status != string(jobPreparing) {
			t.Errorf("view %+v while the engine is held", v)
		}
		e.s.engine.release()
		held = false
		if v := e.finish(j); v.Status != string(jobDone) {
			t.Fatalf("view %+v, err %v", v, jobErr(j))
		}
	})
}

// A wait that ends while the job commits says committing; the next one
// gets done.
func TestACommitThatOutlivesAWait(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	g := e.faults.block(t, faultAfterRecord)
	j := e.start(e.request(300 * time.Millisecond))
	g.reached(t)
	if v := j.wait(t.Context(), 100*time.Millisecond); v.Status != string(jobCommitting) || v.Phase != "linking" {
		t.Errorf("view %+v", v)
	}
	g.open()
	if v := e.finish(j); v.Status != string(jobDone) {
		t.Fatalf("view %+v, err %v", v, jobErr(j))
	}
}

// An open that fails after the commit leaves the capture committed: done,
// derived failed with the reason, the files and the manifest kept.
func TestDerivationFailureNeverRollsBack(t *testing.T) {
	e := newJobEnv(t, jobOpts{limits: func(l *Limits) { l.Model.MaxSamples = 1 }})
	j, v := e.run(e.request(300 * time.Millisecond))
	if v.Status != string(jobDone) || v.Derived != derivedFailed {
		t.Fatalf("view %+v, err %v", v, jobErr(j))
	}
	r := e.result(j)
	if r.DeriveError == "" || len(r.Profiles) != 1 || r.Profiles[0].OpenError == "" || r.Recompute == nil {
		t.Errorf("result %+v", r)
	}
	names := e.seriesNames("adhoc")
	for _, n := range []string{"001-run-cpu.pb.gz", "001-run.capture.json"} {
		if !slices.Contains(names, n) {
			t.Errorf("%s is gone: %q", n, names)
		}
	}
	if m, _ := e.manifestOf("adhoc", "001"); m.Outcome != captureOK {
		t.Errorf("manifest %+v", m)
	}
}

// compare_with inherits every setting not passed, and refuses one passed
// with another value, naming it and both values.
func TestCompareWithInheritsAndRefuses(t *testing.T) {
	e := newJobEnv(t, jobOpts{caps: capture.CapSet(1) << capture.CapSyslog})
	baseReq := e.request(400 * time.Millisecond)
	baseReq.Settings.SampleRate, baseReq.Settings.Unwind = 49, "fp"
	baseReq.Keep, baseReq.Label = true, "base"
	if _, v := e.run(baseReq); v.Status != string(jobDone) {
		t.Fatalf("base: %+v", v)
	}
	j, v := e.run(e.compareRequest("001"))
	if v.Status != string(jobDone) || v.Derived != derivedReady {
		t.Fatalf("cand: %+v, err %v", v, jobErr(j))
	}
	base, _ := e.manifestOf("adhoc", "001")
	cand, _ := e.manifestOf("adhoc", "002")
	if cand.SettingsDigest != base.SettingsDigest || cand.CompareWith != "001" {
		t.Errorf("cand %+v, base %+v", cand.Settings, base.Settings)
	}
	r := e.result(j)
	if r.Compare == nil || len(r.Compare.Views) != 1 || r.Compare.Comparability["settings"] != "equal" {
		t.Fatalf("compare %+v", r.Compare)
	}
	if next := r.Compare.Views[0].Next; next.Tool != "diff" || next.Arguments["limit"] != nil {
		t.Errorf("next %+v", next)
	}
	cases := []struct {
		name string
		set  func(*captureRequest)
		want []string
	}{
		{"sample_rate", func(r *captureRequest) { r.Settings.SampleRate = 199 }, []string{"sample_rate", "49", "199"}},
		{"duration", func(r *captureRequest) { r.Settings.Duration = time.Second }, []string{"duration", "400ms", "1s"}},
		{"unwind", func(r *captureRequest) { r.Settings.Unwind = "dwarf" }, []string{"unwind", "fp", "dwarf"}},
		{"modes", func(r *captureRequest) { r.Settings.Modes = []capture.Mode{capture.ModeCPU, capture.ModeOffCPU} }, []string{"modes", "cpu", "offcpu"}},
		{"kernel_stacks", func(r *captureRequest) { r.Settings.KernelStacks = true }, []string{"kernel_stacks", "false", "true"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := e.compareRequest("001")
			req.Explicit = map[string]bool{c.name: true}
			c.set(&req)
			j, v := e.run(req)
			ce := jobErr(j)
			if v.Status != string(jobFailed) || ce == nil || ce.Code != capture.CodeIncomparable {
				t.Fatalf("view %+v, err %v", v, ce)
			}
			for _, w := range c.want {
				if !strings.Contains(ce.Message, w) {
					t.Errorf("message %q lacks %q", ce.Message, w)
				}
			}
		})
	}
}

// A baseline that failed is refused, naming its outcome and listing the
// series' ok captures; so is a missing one.
func TestCompareWithAFailedCaptureIsRefusedNamingItsOutcome(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	st := capture.Settings{Modes: cpuOnly, Duration: 500 * time.Millisecond, SampleRate: 99, Unwind: "auto"}
	e.commitSynthetic("adhoc", "base", st, manifest{})
	d, ce := e.s.openCaptureDir()
	if ce != nil {
		t.Fatal(ce)
	}
	ser, err := d.series("adhoc")
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.s.nextID(ser)
	_ = ser.Close()
	if err != nil {
		t.Fatal(err)
	}
	cause := capture.Errorf(capture.CodeTargetExited, "", "pid 4 exited before profiling began")
	if err := e.s.writeFailure(d, "adhoc", id, "cand", toWire(st), cause, manifest{}); err != nil {
		t.Fatal(err)
	}
	d.close()
	for _, c := range []struct{ id, want string }{{"002", "target_exited"}, {"009", "no capture 009"}} {
		j, v := e.run(e.compareRequest(c.id))
		ce := jobErr(j)
		if v.Status != string(jobFailed) || ce == nil || ce.Code != capture.CodeUnknownBaseline {
			t.Fatalf("%s: view %+v, err %v", c.id, v, ce)
		}
		for _, w := range []string{c.want, "001"} {
			if !strings.Contains(ce.Message, w) {
				t.Errorf("%s: message %q lacks %q", c.id, ce.Message, w)
			}
		}
		if c.want == "target_exited" && !strings.Contains(ce.Message, "failed") {
			t.Errorf("message %q does not say it failed", ce.Message)
		}
		if _, ok := e.fakeRecord(); ok {
			t.Errorf("%s: perf-agent ran", c.id)
		}
	}
}

// Inherited settings are checked against this server and charged to the
// windows: the reservation (MaxDuration, as the duration was not passed)
// becomes the baseline's duration once the job knows it.
func TestInheritedSettingsAreValidatedAndBudgeted(t *testing.T) {
	e := newJobEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.MaxDuration = 30 * time.Second }})
	long := capture.Settings{Modes: cpuOnly, Duration: 20 * time.Second, SampleRate: 99, Unwind: "auto"}
	e.commitSynthetic("adhoc", "base", long, manifest{})
	j := e.start(e.compareRequest("001"))
	if j.reserve != 30*time.Second {
		t.Errorf("admission reserved %v, want MaxDuration", j.reserve)
	}
	if !eventuallyTrue(30*time.Second, func() bool { return j.view().Phase == capture.PhaseProfiling }) {
		t.Fatalf("never profiling: %+v", j.view())
	}
	e.s.registry.mu.Lock()
	res := e.s.registry.res[j]
	e.s.registry.mu.Unlock()
	if res == nil || res.d != 20*time.Second {
		t.Errorf("reservation %+v, want 20s", res)
	}
	rec, ok := e.fakeRecord()
	if !ok || !slices.Contains(rec.Argv, "20s") {
		t.Errorf("perf-agent argv %q does not run 20s", rec.Argv)
	}
	j.requestCancel()
	if v := e.finish(j); v.Status != string(jobCancelled) {
		t.Fatalf("view %+v", v)
	}

	gpu := capture.Settings{Modes: []capture.Mode{capture.ModeGPU}, Duration: 500 * time.Millisecond, SampleRate: 99, Unwind: "auto"}
	e.commitSynthetic("gpu", "base", gpu, manifest{})
	kernel := capture.Settings{Modes: cpuOnly, Duration: 500 * time.Millisecond, SampleRate: 99, Unwind: "auto", KernelStacks: true}
	e.commitSynthetic("kernel", "base", kernel, manifest{})

	short := newJobEnv(t, jobOpts{profiles: e.profiles, limits: func(l *Limits) { l.Capture.MaxDuration = 10 * time.Second }})
	cases := []struct {
		series string
		code   capture.Code
		want   string
	}{
		{"adhoc", capture.CodeInvalidArgument, "20s"},
		{"gpu", capture.CodeDisabled, "--gpu-shim"},
		{"kernel", capture.CodeInvalidArgument, "cap_syslog"},
	}
	for _, c := range cases {
		req := short.compareRequest("001")
		req.Series = c.series
		j, v := short.run(req)
		ce := jobErr(j)
		if v.Status != string(jobFailed) || ce == nil || ce.Code != c.code || !strings.Contains(ce.Message+ce.Next, c.want) {
			t.Errorf("%s: view %+v, err %v", c.series, v, ce)
		}
	}
	if _, ok := short.fakeRecord(); ok {
		t.Error("perf-agent ran for a refused setting")
	}
}

// The baseline is pinned before retention runs: compare_with the oldest
// capture while KeepPerSeries would evict it, and it survives and is
// diffed.
func TestTheBaselineIsPinnedBeforeRetention(t *testing.T) {
	e := newJobEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.KeepPerSeries = 1 }})
	st := capture.Settings{Modes: cpuOnly, Duration: 400 * time.Millisecond, SampleRate: 99, Unwind: "auto"}
	e.commitSynthetic("adhoc", "base", st, manifest{})
	e.commitSynthetic("adhoc", "two", st, manifest{})
	e.commitSynthetic("adhoc", "three", st, manifest{})
	j, v := e.run(e.compareRequest("001"))
	if v.Status != string(jobDone) || v.Derived != derivedReady {
		t.Fatalf("view %+v, err %v", v, jobErr(j))
	}
	r := e.result(j)
	if !slices.Contains(e.seriesNames("adhoc"), "001-base-cpu.pb.gz") {
		t.Errorf("the baseline was evicted: %q", e.seriesNames("adhoc"))
	}
	if !slices.Contains(r.Evicted, "adhoc/002-two") {
		t.Errorf("evicted %q", r.Evicted)
	}
	if r.Compare == nil || len(r.Compare.Views) != 1 || len(r.Compare.Views[0].Rows) == 0 {
		t.Fatalf("compare %+v", r.Compare)
	}
}

// A cancel while the capture runs stops its children within the stop
// grace, and the capture is rolled back to a cancelled manifest.
func TestCancelStopsTheChildPromptly(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	j := e.start(e.request(time.Minute))
	if !eventuallyTrue(30*time.Second, func() bool { return j.view().Phase == capture.PhaseProfiling }) {
		t.Fatalf("never profiling: %+v", j.view())
	}
	rec, ok := e.fakeRecord()
	if !ok {
		t.Fatal("no fake record")
	}
	e.noteRecord()
	if a := j.requestCancel(); a != cancelAnswerCancelling {
		t.Fatalf("cancel answered %q", a)
	}
	grace := e.s.lim.Capture.StopGrace + time.Second
	if !eventuallyTrue(grace, func() bool { return syscall.Kill(-rec.PGID, 0) == syscall.ESRCH }) {
		t.Errorf("the fake's group %d outlived %v", rec.PGID, grace)
	}
	if v := e.finish(j); v.Status != string(jobCancelled) {
		t.Fatalf("view %+v", v)
	}
	m, ok := e.manifestOf("adhoc", "001")
	if !ok || m.Outcome != captureFailed || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) || len(m.Files) != 0 {
		t.Fatalf("manifest %+v", m)
	}
	if slices.Contains(e.seriesNames("adhoc"), "001-run-cpu.pb.gz") {
		t.Error("the profile was published")
	}
}

// A cancel that comes before the capture has started is honoured when it
// would start: perf-agent never runs.
func TestCancelBeforeRunExistsIsHonoured(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	g := e.faults.block(t, faultAfterStaging)
	j := e.start(e.request(time.Minute))
	g.reached(t)
	if a := j.requestCancel(); a != cancelAnswerCancelling {
		t.Fatalf("cancel answered %q", a)
	}
	g.open()
	if v := e.finish(j); v.Status != string(jobCancelled) {
		t.Fatalf("view %+v", v)
	}
	if _, ok := e.fakeRecord(); ok {
		t.Error("perf-agent ran")
	}
	if m, ok := e.manifestOf("adhoc", "001"); !ok || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) {
		t.Fatalf("manifest %+v", m)
	}
}

// A cancel in each state does what §7.4 says.
func TestCancelAtEachState(t *testing.T) {
	cancelled := func(t *testing.T, e *jobEnv, j *job) {
		t.Helper()
		if v := e.finish(j); v.Status != string(jobCancelled) {
			t.Fatalf("view %+v, err %v", v, jobErr(j))
		}
		m, ok := e.manifestOf("adhoc", "001")
		if !ok || m.Outcome != captureFailed || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) || m.Cancel != cancelAnswerCancelled {
			t.Fatalf("manifest %+v", m)
		}
		for _, n := range e.seriesNames("adhoc") {
			if strings.HasSuffix(n, ".pb.gz") {
				t.Errorf("%s was left", n)
			}
		}
		if a := j.requestCancel(); a != cancelAnswerCancelled {
			t.Errorf("a cancel after the end answered %q", a)
		}
	}
	for _, c := range []struct {
		name string
		p    faultPoint
	}{{"preparing", faultAfterSeq}, {"committing", faultAfterRecord}} {
		t.Run(c.name, func(t *testing.T) {
			e := newJobEnv(t, jobOpts{})
			g := e.faults.block(t, c.p)
			j := e.start(e.request(300 * time.Millisecond))
			g.reached(t)
			if a := j.requestCancel(); a != cancelAnswerCancelling {
				t.Fatalf("cancel answered %q", a)
			}
			g.open()
			cancelled(t, e, j)
		})
	}
	t.Run("running", func(t *testing.T) {
		e := newJobEnv(t, jobOpts{})
		j := e.start(e.request(time.Minute))
		if !eventuallyTrue(30*time.Second, func() bool { return j.view().Status == string(jobRunning) }) {
			t.Fatalf("never running: %+v", j.view())
		}
		j.requestCancel()
		cancelled(t, e, j)
	})
	for _, linked := range []bool{true, false} {
		t.Run("in the manifest link, linked "+map[bool]string{true: "yes", false: "no"}[linked], func(t *testing.T) {
			e := newJobEnv(t, jobOpts{})
			g := e.faults.block(t, faultInManifestLink)
			if !linked {
				e.faults.fail(faultInManifestLink, errInjected)
			}
			j := e.start(e.request(300 * time.Millisecond))
			g.reached(t)
			if a := j.requestCancel(); a != cancelAnswerCancelling {
				t.Fatalf("cancel answered %q", a)
			}
			g.open()
			if !linked {
				cancelled(t, e, j)
				return
			}
			if v := e.finish(j); v.Status != string(jobDone) {
				t.Fatalf("view %+v, err %v", v, jobErr(j))
			}
			if r := e.result(j); r.Cancel != cancelAnswerTooLate {
				t.Errorf("cancel %q", r.Cancel)
			}
			if a := j.requestCancel(); a != cancelAnswerTooLate {
				t.Errorf("a cancel after the commit answered %q", a)
			}
		})
	}
}

// Close cancels the running job and kills its capture: a perf-agent that
// ignores the stop is gone long before the stop grace would have ended it,
// Close returns, and the capture is cancelled.
func TestCloseCancelsAndKills(t *testing.T) {
	const grace = 4 * time.Second
	e := newJobEnv(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeHangAfterSIGINT},
		limits: func(l *Limits) { l.Capture.StopGrace = grace }})
	j := e.start(e.request(time.Minute))
	if !eventuallyTrue(30*time.Second, func() bool { return j.view().Phase == capture.PhaseProfiling }) {
		t.Fatalf("never profiling: %+v", j.view())
	}
	rec, _ := e.fakeRecord()
	e.noteRecord()
	begin := time.Now()
	if err := e.s.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > grace/2 {
		t.Errorf("Close took %v: the capture was not killed", took)
	}
	if syscall.Kill(-rec.PGID, 0) != syscall.ESRCH {
		t.Errorf("the fake's group %d is alive", rec.PGID)
	}
	if v := j.view(); v.Status != string(jobCancelled) {
		t.Errorf("view %+v", v)
	}
	if m, ok := e.manifestOf("adhoc", "001"); !ok || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) {
		t.Errorf("manifest %+v", m)
	}
}

// KillCaptures takes no lock a job holds: it returns at once while the job
// is blocked in an fsync. A job it finds before its capture started never
// starts it.
func TestKillCapturesNeverBlocks(t *testing.T) {
	t.Run("in an fsync", func(t *testing.T) {
		e := newJobEnv(t, jobOpts{})
		g := e.faults.block(t, faultFsync)
		j := e.start(e.request(300 * time.Millisecond))
		g.reached(t)
		begin := time.Now()
		e.s.KillCaptures()
		if took := time.Since(begin); took > 10*time.Millisecond {
			t.Errorf("KillCaptures took %v", took)
		}
		if !e.s.killed.Load() {
			t.Error("killed is not set")
		}
		g.open()
		e.finish(j)
	})
	t.Run("before the capture starts", func(t *testing.T) {
		e := newJobEnv(t, jobOpts{})
		g := e.faults.block(t, faultAfterStaging)
		j := e.start(e.request(time.Minute))
		g.reached(t)
		e.s.KillCaptures()
		g.open()
		if v := e.finish(j); v.Status != string(jobCancelled) {
			t.Fatalf("view %+v, err %v", v, jobErr(j))
		}
		if _, ok := e.fakeRecord(); ok {
			t.Error("perf-agent ran after KillCaptures")
		}
	})
}

// Start-up maintenance blocked in retention leaves the control path
// answering, and the engine too: retention snapshots what it must keep
// under the engine and removes files without it.
func TestMaintenanceKeepsControlResponsive(t *testing.T) {
	first := newJobEnv(t, jobOpts{})
	st := capture.Settings{Modes: cpuOnly, Duration: 400 * time.Millisecond, SampleRate: 99, Unwind: "auto"}
	first.commitSynthetic("adhoc", "one", st, manifest{})
	first.commitSynthetic("adhoc", "two", st, manifest{})
	writeFixture(t, first.profiles, "cpu.pb.gz", pmfixture.CPUBasic())

	faults := newFaultGates()
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	faults.gates[faultRetentionHash] = g
	t.Cleanup(g.open)
	e := newJobEnv(t, jobOpts{profiles: first.profiles, limits: func(l *Limits) { l.Capture.KeepPerSeries = 1 },
		setup: func(s *Server) { s.captureFault = faults.fault }})
	g.reached(t)

	c := e.serve()
	if res := c.rpc("tools/list", map[string]any{}); res.Error != nil {
		t.Fatalf("tools/list: %+v", res.Error)
	}
	if _, ok := e.s.registry.get("adhoc/001"); ok {
		t.Error("a job that never ran was found")
	}
	if _, ok := e.s.registry.cancel("j1-x"); ok {
		t.Error("a cancel of an unknown job was answered")
	}
	j, ce := e.s.startJob(e.request(300 * time.Millisecond))
	if ce != nil {
		t.Fatal(ce)
	}
	if v := j.wait(t.Context(), 50*time.Millisecond); v.Status != string(jobPreparing) && v.Status != string(jobAccepted) {
		t.Errorf("view %+v", v)
	}
	// The capture tools answer too: the history is read without the
	// directory slot or the engine, and a wait and a cancel need neither.
	var l listing
	c.callOK("capture_list", map[string]any{"series": "adhoc"}, &l)
	if l.Active != j.id || l.Loading || len(l.Captures) != 2 {
		t.Errorf("capture_list %+v", l)
	}
	var v jobView
	c.callOK("capture_wait", map[string]any{"id": j.id, "timeout": "50ms"}, &v)
	if v.Status != string(jobPreparing) && v.Status != string(jobAccepted) {
		t.Errorf("capture_wait %+v", v)
	}
	if text, isErr := c.call("capture_cancel", map[string]any{"id": "j99-0000"}); !isErr || toolErrorOf(t, text).Code != string(capture.CodeUnknownCapture) {
		t.Errorf("an unknown cancel: %s", text)
	}
	var a map[string]string
	c.callOK("capture_cancel", map[string]any{"id": j.id}, &a)
	if a["status"] != cancelAnswerCancelling {
		t.Errorf("cancel %v", a)
	}
	openDone := make(chan string, 1)
	go func() {
		text, _ := c.call("open", map[string]any{"path": "cpu.pb.gz"})
		openDone <- text
	}()
	select {
	case text := <-openDone:
		if !strings.Contains(text, `"handle"`) {
			t.Errorf("open: %s", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("an engine tool waited for retention's hashing")
	}
	g.open()
	e.finish(j)
	if !eventuallyTrue(10*time.Second, func() bool { return !slices.Contains(e.seriesNames("adhoc"), "001-one-cpu.pb.gz") }) {
		t.Error("maintenance did not evict the old capture")
	}
}

// serve runs the env's server over pipes, initialized.
func (e *jobEnv) serve() *testClient {
	e.t.Helper()
	return attachClient(e.t, e.s)
}

// A live server derives a job's results while page tools run: both hold
// the engine, so the race detector sees no race.
func TestALiveServerDerivesWhilePageToolsRun(t *testing.T) {
	e := newJobEnv(t, jobOpts{live: true})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	c := e.serve()
	var o openedAs
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	c.callOK("page_open", map[string]any{}, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			var pub struct {
				Publication string `json:"publication"`
			}
			c.callOK("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}}, &pub)
		}
	})
	for i := range 3 {
		req := e.request(300 * time.Millisecond)
		if i > 0 {
			req = e.compareRequest("001")
		}
		if j, v := e.run(req); v.Status != string(jobDone) || v.Derived != derivedReady {
			t.Errorf("job %d: %+v, err %v", i, v, jobErr(j))
		}
	}
	close(stop)
	wg.Wait()
}

// The recompute recipe opens the baseline first, binds each open, and
// diffs through the bindings; without its baseline it only reopens the
// capture and says why.
func TestRecomputeRecipeShape(t *testing.T) {
	s, _, _ := newCaptureServer(t)
	st := toWire(capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"})
	base := &manifest{ID: "001", Series: "s", Label: "base", Outcome: captureOK, Settings: st}
	m := manifest{ID: "002", Series: "s", Label: "cand", Outcome: captureOK, Settings: st, CompareWith: "001"}
	r := s.recomputeFor(m, base, "")
	if r.Baseline != "present" || len(r.Steps) != 3 {
		t.Fatalf("recipe %+v", r)
	}
	want := []toolCall{
		{Tool: "open", Arguments: map[string]any{"path": "captures/s/001-base-cpu.pb.gz"}, Bind: "base_cpu"},
		{Tool: "open", Arguments: map[string]any{"path": "captures/s/002-cand-cpu.pb.gz"}, Bind: "new_cpu"},
		{Tool: "diff", Arguments: map[string]any{
			"base_handle": "${base_cpu.handle}", "base_view": "${new_cpu.default_view}",
			"new_handle": "${new_cpu.handle}", "new_view": "${new_cpu.default_view}", "limit": s.lim.Capture.CompareRows,
		}},
	}
	if got, w := mustJSON(t, r.Steps), mustJSON(t, want); got != w {
		t.Errorf("steps\n got %s\nwant %s", got, w)
	}
	gone := s.recomputeFor(m, nil, "001 was removed by retention")
	if gone.Baseline != "001 was removed by retention" || len(gone.Steps) != 1 || gone.Steps[0].Bind != "new_cpu" {
		t.Errorf("recipe %+v", gone)
	}
	gpu := m
	gpu.CompareWith = ""
	gpu.Settings = toWire(capture.Settings{Modes: []capture.Mode{capture.ModeGPU}, Duration: time.Second, SampleRate: 99, Unwind: "auto"})
	none := s.recomputeFor(gpu, nil, "")
	if none.Baseline != "none" || len(none.Steps) != 1 || none.Steps[0].Arguments["execlog"] != "captures/s/002-cand-gpu.execlog" {
		t.Errorf("recipe %s", mustJSON(t, none))
	}
}

// A GPU capture with no samples is kept, with a note saying why it is
// empty.
func TestTheGPUNoteForAnEmptyProfile(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "libshim.so")
	if err := os.WriteFile(shim, []byte("shim"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newJobEnv(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeNoGPUSamples},
		setup: func(s *Server) { s.shim = capture.Shim{Path: shim} }})
	req := e.request(300 * time.Millisecond)
	req.Settings.Modes = []capture.Mode{capture.ModeGPU}
	j, v := e.run(req)
	if v.Status != string(jobDone) || v.Derived != derivedReady {
		t.Fatalf("view %+v, err %v", v, jobErr(j))
	}
	r := e.result(j)
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.HasPrefix(n, "the GPU profile has no samples") }) {
		t.Errorf("notes %q", r.Notes)
	}
	if len(r.Profiles) != 1 || r.Profiles[0].Execlog == "" {
		t.Errorf("profiles %+v", r.Profiles)
	}
	rec, _ := e.fakeRecord()
	if !slices.Contains(rec.Argv, shim) {
		t.Errorf("argv %q lacks the shim", rec.Argv)
	}
}

// The space check refuses a capture the filesystem has no room for, with
// a failure manifest and nothing started.
func TestTheSpaceCheckRefusesWithDisk(t *testing.T) {
	e := newJobEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.MinFreeBytes = 1 << 62 }})
	j, v := e.run(e.request(300 * time.Millisecond))
	ce := jobErr(j)
	if v.Status != string(jobFailed) || ce == nil || ce.Code != capture.CodeDisk || !strings.Contains(ce.Message, "MinFreeBytes") {
		t.Fatalf("view %+v, err %v", v, ce)
	}
	if m, ok := e.manifestOf("adhoc", "001"); !ok || m.Error == nil || m.Error.Code != string(capture.CodeDisk) {
		t.Errorf("manifest %+v", m)
	}
	if _, ok := e.fakeRecord(); ok {
		t.Error("perf-agent ran")
	}
}

// A commit whose manifest link has an unknown outcome is shown as
// undecided, not as a failure, until a later pass settles it from the
// disk; the registry entry is then corrected.
func TestAnUndecidedCommitIsSettledLater(t *testing.T) {
	var mu sync.Mutex
	broken := true
	isBroken := func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		return broken && strings.HasSuffix(name, manifestSuffix)
	}
	e := newJobEnv(t, jobOpts{setup: func(s *Server) {
		s.link = func(r *os.Root, oldname, newname string) error {
			if isBroken(newname) {
				return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EIO}
			}
			return r.Link(oldname, newname)
		}
	}})
	landedLstat = func(r *os.Root, name string) (os.FileInfo, error) {
		if isBroken(name) {
			return nil, syscall.EIO
		}
		return r.Lstat(name)
	}
	t.Cleanup(func() { landedLstat = (*os.Root).Lstat })
	j := e.start(e.request(300 * time.Millisecond))
	if !eventuallyTrue(30*time.Second, func() bool { return j.view().Phase == phaseUndecided }) {
		t.Fatalf("view %+v, err %v", j.view(), jobErr(j))
	}
	if v := j.view(); v.Status != string(jobCommitting) {
		t.Errorf("view %+v", v)
	}
	if !eventuallyTrue(5*time.Second, func() bool { return e.s.registry.active.Load() == nil }) {
		t.Error("an undecided job holds the slot")
	}
	mu.Lock()
	broken = false
	mu.Unlock()
	next, v := e.run(e.request(300 * time.Millisecond))
	if v.Status != string(jobDone) {
		t.Fatalf("next: %+v, err %v", v, jobErr(next))
	}
	if v := j.view(); v.Status != string(jobFailed) {
		t.Errorf("the undecided job is now %+v", v)
	}
	if ce := jobErr(j); ce == nil || ce.Code != capture.CodeDisk {
		t.Errorf("err %v", ce)
	}
	if a, _ := e.s.registry.cancel(j.id); a != cancelAnswerEnded {
		t.Errorf("cancel of the settled job answered %q", a)
	}
	if m, ok := e.manifestOf("adhoc", "001"); !ok || m.Outcome != captureFailed {
		t.Errorf("manifest %+v", m)
	}
}

// A rollback that can never finish (a foreign file at its manifest name)
// is noted and never stops the next capture.
func TestAStrandedRollbackNeverBlocksTheNextCapture(t *testing.T) {
	e := newJobEnv(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeExit2}})
	e.s.writeErr = failOpAt("manifest link", 1, syscall.ENOSPC)
	j, v := e.run(e.request(300 * time.Millisecond))
	if v.Status != string(jobFailed) {
		t.Fatalf("view %+v", v)
	}
	if ce := jobErr(j); ce == nil || ce.Code != capture.CodePerfAgentRefusedArgs {
		t.Errorf("err %v", ce)
	}
	if len(e.s.stranded) != 1 {
		t.Fatalf("stranded %d", len(e.s.stranded))
	}
	foreign := filepath.Join(e.profiles, "captures", "adhoc", "001-run.capture.json")
	if err := os.WriteFile(foreign, []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, v := e.run(e.request(300 * time.Millisecond))
	if v.Status != string(jobFailed) || jobErr(next).Code != capture.CodePerfAgentRefusedArgs {
		t.Fatalf("next: %+v, err %v", v, jobErr(next))
	}
	if b, _ := os.ReadFile(foreign); !bytes.Equal(b, []byte("not ours\n")) {
		t.Errorf("the foreign file changed: %q", b)
	}
	if _, ok := e.manifestOf("adhoc", "002"); !ok {
		t.Error("the next capture has no manifest")
	}
}

var _ = errors.New

// The slot is free before a job's end can be seen: with the registry held,
// a job that has run cannot look ended, and once the registry is free a
// wait that sees the end finds no active job, so the next capture is not
// busy.
func TestTheSlotIsFreeWhenAJobLooksEnded(t *testing.T) {
	for _, mode := range []capturetest.FakeMode{capturetest.FakeNormal, capturetest.FakeExit2} {
		t.Run(string(mode), func(t *testing.T) {
			e := newJobEnv(t, jobOpts{fake: capturetest.FakeOptions{Mode: mode}})
			j := e.start(e.request(300 * time.Millisecond))
			if !eventuallyTrue(30*time.Second, func() bool { return j.view().Status == string(jobRunning) }) {
				t.Fatalf("never running: %+v", j.view())
			}
			e.s.registry.mu.Lock()
			v := j.wait(t.Context(), 2*time.Second)
			e.s.registry.mu.Unlock()
			if ended(v) {
				t.Fatalf("the job looked ended while the registry still held its slot: %+v", v)
			}
			v = e.finish(j)
			if a := e.s.registry.active.Load(); a != nil {
				t.Errorf("the slot holds %s after %+v", a.id, v)
			}
			if _, ce := e.s.startJob(e.request(300 * time.Millisecond)); ce != nil {
				t.Errorf("the next capture: %v", ce)
			}
		})
	}
}

// What a capture's programs print is shown without the server's paths:
// the profiles root and the staging directory are named by what they
// are, in perf-agent's stderr tail and in the workload's tails.
func TestTailsNameNoServerPath(t *testing.T) {
	t.Run("perf-agent stderr", func(t *testing.T) {
		e := newJobEnv(t, jobOpts{})
		g := e.faults.block(t, faultAfterStaging)
		j := e.start(e.request(300 * time.Millisecond))
		g.reached(t)
		ser := filepath.Join(e.profiles, defaultStdioCaptures, "adhoc")
		ents, err := os.ReadDir(ser)
		if err != nil {
			t.Fatal(err)
		}
		staged := ""
		for _, de := range ents {
			if strings.HasPrefix(de.Name(), stagingPrefix) {
				staged = filepath.Join(ser, de.Name())
			}
		}
		// perf-agent cannot write its output there, and says where.
		if err := os.Chmod(staged, 0o500); err != nil {
			t.Fatal(err)
		}
		g.open()
		v := e.finish(j)
		ce := jobErr(j)
		if v.Status != string(jobFailed) || ce == nil || ce.StderrTail == "" {
			t.Fatalf("view %+v, err %+v", v, ce)
		}
		if strings.Contains(ce.StderrTail, e.profiles) || !strings.Contains(ce.StderrTail, "staging") {
			t.Errorf("stderr tail %q", ce.StderrTail)
		}
	})
	t.Run("workload stdout", func(t *testing.T) {
		py, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("no python3")
		}
		e := newJobEnv(t, jobOpts{})
		req := e.request(500 * time.Millisecond)
		req.Command = []string{py, "-c", "import os, sys, time; print(os.getcwd(), flush=True); print(os.getcwd(), file=sys.stderr, flush=True); time.sleep(60)"}
		j, v := e.run(req)
		if v.Status != string(jobDone) {
			t.Fatalf("view %+v, err %v", v, jobErr(j))
		}
		tail := e.result(j).Capture.WorkloadOutputTail
		if tail == nil || !strings.Contains(tail.Stdout, "the profiles root") || strings.Contains(tail.Stdout+tail.Stderr, e.profiles) {
			t.Errorf("workload tail %+v", tail)
		}
	})
	t.Run("a path across the tail's cut", func(t *testing.T) {
		py, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("no python3")
		}
		e := newJobEnv(t, jobOpts{})
		n := e.s.lim.Capture.MaxOutputTail
		req := e.request(500 * time.Millisecond)
		// The last n bytes hold only the path's last 12 bytes.
		req.Command = []string{py, "-c", fmt.Sprintf("import os, time; print(os.getcwd() + 'Z' * %d, end='', flush=True); time.sleep(60)", n-12)}
		j, v := e.run(req)
		if v.Status != string(jobDone) {
			t.Fatalf("view %+v, err %v", v, jobErr(j))
		}
		tail := e.result(j).Capture.WorkloadOutputTail
		if tail == nil || len(tail.Stdout) > n || strings.Contains(tail.Stdout, e.profiles[len(e.profiles)-12:]) {
			t.Errorf("workload tail %q", tail.Stdout)
		}
	})
}
