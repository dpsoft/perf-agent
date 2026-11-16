//go:build linux

package perfmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
)

func init() { serverMain = runTestServer }

// procConfig is a test server process's configuration, passed as JSON in
// testConfigEnv. The perf-agent is a fake, accepted without capabilities.
type procConfig struct {
	Profiles  string        `json:"profiles"`
	Captures  string        `json:"captures,omitzero"`
	Owner     string        `json:"owner,omitzero"`
	PerfAgent string        `json:"perf_agent"`
	Capture   CaptureLimits `json:"capture"`
	// Crash names a fault point at whose first occurrence the server exits
	// with crashExit at once, as a crash would: nothing is cleaned up.
	Crash string `json:"crash,omitzero"`
	// Pause names a fault point whose first occurrence blocks for good,
	// after pausedLine on stderr.
	Pause string `json:"pause,omitzero"`
	// Fail names a fault point whose first occurrence fails its step.
	Fail string `json:"fail,omitzero"`
	// ReaperPause is every capture's RunSpec.FaultPause.
	ReaperPause string `json:"reaper_pause,omitzero"`
}

// What a test server says on stderr at its fault points, and its exit
// status at a crash.
const (
	crashExit  = 3
	crashLine  = "perfmcp-test-server: crashing at "
	pausedLine = "perfmcp-test-server: paused at "
)

// serverFaults makes each kind of fault act at its first occurrence only.
var serverFaults struct{ crash, pause, fail atomic.Bool }

// fault is the test server's captureFault.
func (pc procConfig) fault(p faultPoint) error {
	name := p.String()
	if name == pc.Crash && serverFaults.crash.CompareAndSwap(false, true) {
		fmt.Fprintln(os.Stderr, crashLine+name)
		os.Exit(crashExit)
	}
	if name == pc.Pause && serverFaults.pause.CompareAndSwap(false, true) {
		fmt.Fprintln(os.Stderr, pausedLine+name)
		for {
			time.Sleep(time.Hour)
		}
	}
	if name == pc.Fail && serverFaults.fail.CompareAndSwap(false, true) {
		return fmt.Errorf("injected fault at %s", name)
	}
	return nil
}

// runTestServer is the test binary as a stdio server on its stdin and
// stdout, logging to stderr, until stdin ends.
func runTestServer() int {
	var pc procConfig
	if err := json.Unmarshal([]byte(os.Getenv(testConfigEnv)), &pc); err != nil {
		fmt.Fprintln(os.Stderr, "perfmcp-test-server: config:", err)
		return 2
	}
	lim := DefaultLimits()
	lim.Capture = pc.Capture
	cfg := Config{
		ProfilesDir: pc.Profiles,
		Limits:      lim,
		Version:     "test",
		Log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Capture: CaptureConfig{PerfAgent: pc.PerfAgent, Dir: pc.Captures, Owner: pc.Owner,
			Verify: func(p string) (capture.Binary, error) {
				id, err := capture.IdentityOf(p)
				return capture.Binary{Path: p, ID: id, Version: "v0.0.0-test"}, err
			}},
	}
	beforeMaintenance = func(s *Server) {
		s.captureFault = pc.fault
		s.reaperPause = pc.ReaperPause
	}
	s, err := New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "perfmcp-test-server:", err)
		return 1
	}
	if err := s.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "perfmcp-test-server: serve:", err)
	}
	_ = s.Close()
	return 0
}

// procEnv is what the server processes of one test share: a profiles root
// and a fake perf-agent.
type procEnv struct {
	t        *testing.T
	profiles string
	fake     string
	record   string
	// plain is a fake in the normal mode, for the servers that capture
	// after the one under test.
	plain func() string
}

func newProcEnv(t *testing.T, fo capturetest.FakeOptions) *procEnv {
	t.Helper()
	capturetest.SkipIfPrivileged(t)
	fo.Record = filepath.Join(t.TempDir(), "record.json")
	e := &procEnv{t: t, profiles: t.TempDir(), fake: ownFake(t, fo), record: fo.Record}
	e.plain = sync.OnceValue(func() string {
		return ownFake(t, capturetest.FakeOptions{Record: filepath.Join(t.TempDir(), "record.json")})
	})
	return e
}

// ownFake is a fake perf-agent on an inode of its own. FakePerfAgent
// hard-links the one built binary, so staging another fake changes the
// inode's ctime under every server that verified it, and a parallel test
// would make their captures fail binary_changed.
func ownFake(t *testing.T, fo capturetest.FakeOptions) string {
	t.Helper()
	path := capturetest.FakePerfAgent(t, fo)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// config is a server over the env with the job tests' limits.
func (e *procEnv) config() procConfig {
	return procConfig{Profiles: e.profiles, PerfAgent: e.fake, Capture: jobLimits().Capture}
}

// plainConfig is config with a fake perf-agent in the normal mode.
func (e *procEnv) plainConfig() procConfig {
	c := e.config()
	c.PerfAgent = e.plain()
	return c
}

// args are a capture of the workload, which leaves a setsid grandchild,
// for d, in series adhoc with label run.
func (e *procEnv) args(d string) map[string]any {
	return map[string]any{
		"command":  []any{capturetest.Workload(e.t), "double-fork-setsid", "120s"},
		"duration": d,
		"modes":    []any{"cpu"},
		"label":    "run",
	}
}

func (e *procEnv) captures() string { return filepath.Join(e.profiles, defaultStdioCaptures) }

func (e *procEnv) series() string { return filepath.Join(e.captures(), defaultSeries) }

// listCaptures is the names in the captures directory, sorted.
func (e *procEnv) listCaptures() []string {
	e.t.Helper()
	ents, err := os.ReadDir(e.captures())
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, d := range ents {
		names = append(names, d.Name())
	}
	return names
}

// exists reports whether name is in the series directory.
func (e *procEnv) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(e.series(), name))
	return err == nil
}

// holder waits for the captures lock's record to pass ok, and returns it.
func (e *procEnv) holder(ok func(capture.Holder) bool) capture.Holder {
	e.t.Helper()
	var h capture.Holder
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile(filepath.Join(e.captures(), lockFile))
		if err == nil && json.NewDecoder(bytes.NewReader(b)).Decode(&h) == nil && ok(h) {
			return h
		}
	}
	e.t.Fatalf("the lock's holder never passed the check: %+v", h)
	return h
}

// lockFree reports whether the captures lock can be taken now; it is
// released at once.
func (e *procEnv) lockFree() bool {
	root, err := os.OpenRoot(e.captures())
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	defer func() { _ = root.Close() }()
	l, ce := capture.TryLock(root, capture.Holder{ServerPID: os.Getpid()})
	if ce != nil {
		return false
	}
	_ = l.Close()
	return true
}

func (e *procEnv) openSeries() *os.Root {
	e.t.Helper()
	ser, err := os.OpenRoot(e.series())
	if err != nil {
		e.t.Fatal(err)
	}
	return ser
}

// intent is the series' intent of capture id.
func (e *procEnv) intent(id int, label string) intent {
	e.t.Helper()
	ser := e.openSeries()
	defer func() { _ = ser.Close() }()
	in, _, err := readIntent(ser, intentName(id, label))
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

// manifest is the series' manifest of capture id, and whether there is one.
func (e *procEnv) manifest(id int, label string) (manifest, bool) {
	e.t.Helper()
	ser := e.openSeries()
	defer func() { _ = ser.Close() }()
	m, err := readManifest(ser, manifestName(id, label))
	if errors.Is(err, fs.ErrNotExist) {
		return m, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return m, true
}

// invariant checks the captures directory as the protocol leaves it: only
// committed captures with verified files, failure or interrupted manifests
// without files, and .seq.
func (e *procEnv) invariant() {
	e.t.Helper()
	root, err := os.OpenRoot(e.captures())
	if err != nil {
		e.t.Fatal(err)
	}
	d := &captureDir{root: root}
	defer d.close()
	invariant(e.t, d, nil)
}

// waitStale waits until every heartbeat in the series is older than
// recovery's bound, as a dead owner's heartbeat becomes.
func (e *procEnv) waitStale() {
	e.t.Helper()
	for end := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		hbs, _ := filepath.Glob(filepath.Join(e.series(), "*"+heartbeatSuffix))
		fresh := slices.ContainsFunc(hbs, func(p string) bool {
			fi, err := os.Lstat(p)
			return err == nil && time.Since(fi.ModTime()) <= staleAfter+time.Second
		})
		if !fresh {
			return
		}
		if time.Now().After(end) {
			e.t.Fatalf("a heartbeat stayed fresh: %v", hbs)
		}
	}
}

// fakeRecord is what the fake perf-agent recorded of its run.
func (e *procEnv) fakeRecord() (fakemode.Record, bool) {
	var r fakemode.Record
	b, err := os.ReadFile(e.record)
	return r, err == nil && json.Unmarshal(b, &r) == nil && r.PGID > 0
}

// procClient is a test server process and an MCP client of it.
type procClient struct {
	*testClient
	t    *testing.T
	cmd  *exec.Cmd
	in   *os.File
	out  *os.File
	self procRef
	log  *lineLog

	exited  chan struct{}
	waitErr error

	mu      sync.Mutex
	watched []procRef
}

// startServerProcess starts the test binary as a server of cfg and
// initializes it. At cleanup it ends the server, then fails the test for
// any process it started that is still alive, by PID and start time, and
// for any cgroup of its captures left under the delegated cgroup.
func startServerProcess(t *testing.T, cfg procConfig) *procClient {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), testServerEnv+"=1", testConfigEnv+"="+string(b))
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{inR, outW, errW} {
		_ = f.Close()
	}
	p := &procClient{t: t, cmd: cmd, in: inW, out: outR, log: newLineLog(errR), exited: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	p.self, _ = procOf(cmd.Process.Pid)
	t.Cleanup(p.cleanup)
	p.testClient = &testClient{t: t, w: inW, r: bufio.NewReaderSize(deadlineReader{outR}, 1<<20), nextID: 1}
	if res := p.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25"}); res.Error != nil {
		t.Fatalf("initialize: %+v", res.Error)
	}
	return p
}

// deadlineReader bounds each read of a server's stdout, so a server that
// stops answering fails the test instead of hanging it.
type deadlineReader struct{ f *os.File }

func (r deadlineReader) Read(b []byte) (int, error) {
	_ = r.f.SetReadDeadline(time.Now().Add(3 * time.Minute))
	return r.f.Read(b)
}

func (p *procClient) pid() int { return p.cmd.Process.Pid }

func (p *procClient) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *procClient) cleanup() {
	t := p.t
	if p.running() {
		// A stopped server must run to see its stdin end.
		_ = syscall.Kill(p.pid(), syscall.SIGCONT)
		p.watch()
	}
	_ = p.in.Close()
	select {
	case <-p.exited:
	case <-time.After(time.Minute):
		t.Error("the server did not exit at the end of the test")
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
	_ = p.out.Close()
	p.mu.Lock()
	watched := slices.Clone(p.watched)
	p.mu.Unlock()
	for _, r := range watched {
		if !eventuallyTrue(30*time.Second, r.gone) {
			t.Errorf("%s (pid %d) is still alive after the test", r.comm, r.pid)
			if now, ok := procOf(r.pid); ok && now.start == r.start {
				_ = syscall.Kill(r.pid, syscall.SIGKILL)
			}
		}
	}
	var left []string
	if !eventuallyTrue(10*time.Second, func() bool { left = cgroupsOf(p.pid()); return len(left) == 0 }) {
		t.Errorf("cgroups of the server's captures are left: %v", left)
	}
	if t.Failed() {
		t.Logf("server pid %d stderr:\n%s", p.pid(), p.log.String())
	}
}

// watch records every process under the server now, so cleanup checks it
// is gone, and returns them.
func (p *procClient) watch() []procRef {
	now := descendantsOf(p.pid())
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range now {
		if !slices.Contains(p.watched, r) {
			p.watched = append(p.watched, r)
		}
	}
	return now
}

// waitWatchedGone waits for every watched process to be gone.
func (p *procClient) waitWatchedGone() {
	p.t.Helper()
	p.mu.Lock()
	watched := slices.Clone(p.watched)
	p.mu.Unlock()
	for _, r := range watched {
		waitGone(p.t, r.comm, r.gone)
	}
}

// kill9 SIGKILLs the server, after watching what runs under it, and reaps
// it.
func (p *procClient) kill9() {
	p.t.Helper()
	p.watch()
	if err := p.cmd.Process.Kill(); err != nil {
		p.t.Fatal(err)
	}
	if code := p.exitCode(); code != -1 {
		p.t.Fatalf("the server exited %d, not by the kill", code)
	}
}

// stop and cont send the server SIGSTOP and SIGCONT; stop returns once it
// is stopped.
func (p *procClient) stop() {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		p.t.Fatal(err)
	}
	if !eventuallyTrue(10*time.Second, func() bool { return p.state() == 'T' }) {
		p.t.Fatalf("the server never stopped (state %c)", p.state())
	}
}

func (p *procClient) cont() {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		p.t.Fatal(err)
	}
}

// state is the server's state letter in /proc/PID/stat.
func (p *procClient) state() byte {
	r, _ := procOf(p.pid())
	return r.state
}

// exitCode waits for the server to exit and returns its status, -1 when a
// signal ended it.
func (p *procClient) exitCode() int {
	p.t.Helper()
	select {
	case <-p.exited:
	case <-time.After(time.Minute):
		p.t.Fatal("the server did not exit")
	}
	if ee, ok := errors.AsType[*exec.ExitError](p.waitErr); ok {
		return ee.ExitCode()
	}
	if p.waitErr != nil {
		p.t.Fatal(p.waitErr)
	}
	return 0
}

// waitLine waits for a stderr line containing s.
func (p *procClient) waitLine(s string) {
	p.t.Helper()
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if p.log.has(s) {
			return
		}
		if !p.running() && p.log.ended() && !p.log.has(s) {
			p.t.Fatalf("the server exited without %q", s)
		}
	}
	p.t.Fatalf("the server never wrote %q", s)
}

// startCapture calls capture, which must accept, and returns the job.
func (p *procClient) startCapture(args map[string]any) string {
	p.t.Helper()
	return startCapture(p.t, p.testClient, args)
}

func (p *procClient) waitEnd(job string) (string, bool) {
	p.t.Helper()
	return waitEnd(p.t, p.testClient, job)
}

func (p *procClient) waitResult(job string) captureResult {
	p.t.Helper()
	r, _ := waitResult(p.t, p.testClient, job)
	return r
}

// status is the job's state now.
func (p *procClient) status(job string) jobView {
	p.t.Helper()
	text, isErr := p.call("capture_wait", map[string]any{"id": job, "timeout": "100ms"})
	var v jobView
	if isErr || json.Unmarshal([]byte(text), &v) != nil {
		p.t.Fatalf("capture_wait %s: %s", job, text)
	}
	return v
}

// waitPhase waits until the job is in phase.
func (p *procClient) waitPhase(job, phase string) {
	p.t.Helper()
	waitStatus(p.t, p.testClient, job, func(v jobView) bool { return v.Phase == phase })
}

func (p *procClient) waitProfiling(job string) { p.waitPhase(job, capture.PhaseProfiling) }

func (p *procClient) cancel(job string) {
	p.t.Helper()
	if text, isErr := p.call("capture_cancel", map[string]any{"id": job}); isErr {
		p.t.Fatalf("capture_cancel: %s", text)
	}
}

// awaitGrandchild waits until the workload's setsid grandchild runs under
// the server, and watches everything under it then.
func (p *procClient) awaitGrandchild() {
	p.t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if slices.ContainsFunc(descendantsOf(p.pid()), func(r procRef) bool { return r.comm == "capworkload" && r.sid == r.pid }) {
			p.watch()
			return
		}
	}
	p.t.Fatalf("no setsid grandchild of the workload under the server: %+v", descendantsOf(p.pid()))
}

// lineLog collects a server's stderr.
type lineLog struct {
	mu   sync.Mutex
	buf  strings.Builder
	done bool
}

func newLineLog(r *os.File) *lineLog {
	l := &lineLog{}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			l.mu.Lock()
			l.buf.WriteString(sc.Text() + "\n")
			l.mu.Unlock()
		}
		_ = r.Close()
		l.mu.Lock()
		l.done = true
		l.mu.Unlock()
	}()
	return l
}

func (l *lineLog) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), s)
}

func (l *lineLog) ended() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done
}

func (l *lineLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// procRef is a process by its PID and start time, with what
// /proc/PID/stat said of it.
type procRef struct {
	pid, ppid, sid int
	start          uint64
	state          byte
	comm           string
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

// cgroupsOf lists the cgroups the server pid made for its captures that
// are still under the delegated cgroup, at any depth.
func cgroupsOf(pid int) []string {
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

// waitGone fails the test unless gone holds within a bound generous
// enough for a loaded -race machine.
func waitGone(t *testing.T, what string, gone func() bool) {
	t.Helper()
	waitGoneWithin(t, what, 30*time.Second, gone)
}

func waitGoneWithin(t *testing.T, what string, d time.Duration, gone func() bool) {
	t.Helper()
	if !eventuallyTrue(d, gone) {
		t.Fatalf("%s is still there after %v", what, d)
	}
}
