//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
)

// stopGrace is the Capture.StopGrace the process tests run the server with.
const stopGrace = time.Second

// killBound is how soon after a signal the capture's fake perf-agent must
// be gone: far less than the 800 ms the server spends draining its output.
const killBound = 300 * time.Millisecond

func TestRunRefusesABadPerfAgent(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", t.TempDir(), "--perf-agent", "bin/perf-agent"}, strings.NewReader(""), &out, &errb); code != 2 ||
		!strings.Contains(errb.String(), "--perf-agent") || !strings.Contains(errb.String(), "absolute") {
		t.Errorf("relative --perf-agent: exit %d, stderr %q; want 2 asking for an absolute path", code, errb.String())
	}

	capturetest.SkipIfPrivileged(t)
	plain := plainBinary(t)
	errb.Reset()
	code := run([]string{"--profiles", t.TempDir(), "--perf-agent", plain}, strings.NewReader(""), &out, &errb)
	if code != 1 {
		t.Errorf("a perf-agent without file capabilities: exit %d, want 1; stderr %q", code, errb.String())
	}
	for _, want := range []string{plain, "no file capabilities", "setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr %q does not name %q", errb.String(), want)
		}
	}
}

// plainBinary is a 0755 file in a directory where VerifyBinary gets as
// far as the capability check. /tmp is usually nosuid, so it tries a
// dot-directory in the user's cache directory first, then in the temporary
// directory and beside the package, each removed at the end of the test.
func plainBinary(t *testing.T) string {
	t.Helper()
	var tried []string
	try := func(dir string) string {
		p := filepath.Join(dir, "perf-agent")
		if err := os.WriteFile(p, []byte("not perf-agent\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := capture.VerifyBinary(p)
		if err != nil && strings.Contains(err.Error(), "no file capabilities") {
			return p
		}
		tried = append(tried, fmt.Sprint(err))
		return ""
	}
	var parents []string
	if cache, err := os.UserCacheDir(); err == nil {
		parents = append(parents, cache)
	}
	parents = append(parents, os.TempDir(), ".")
	for _, parent := range parents {
		d, err := os.MkdirTemp(parent, ".verify-test-")
		if err != nil {
			tried = append(tried, err.Error())
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		if d, err = filepath.Abs(d); err != nil {
			t.Fatal(err)
		}
		if p := try(d); p != "" {
			return p
		}
	}
	t.Skipf("no directory where a plain file reaches the capability check: %s", strings.Join(tried, "; "))
	return ""
}

// Every stdout line stays JSON-RPC while a fake perf-agent spams its own
// stdout and the workload floods both of its streams.
func TestStdoutStaysJSONRPCWhileCapturing(t *testing.T) {
	p := startCaptureServer(t, capturetest.FakeSpamStdout, nil)
	job := p.startCapture(map[string]any{"command": []any{capturetest.Workload(t), "flood-then-sleep", "1", "2m"}, "duration": "1s"})
	text, isErr := p.waitEnd(job)
	if isErr {
		t.Fatalf("the capture failed: %s", text)
	}
	p.call("tools/list", map[string]any{})
	if code := p.end(); code != 0 {
		t.Errorf("exit %d", code)
	}
	if len(p.lines) == 0 {
		t.Fatal("no stdout lines")
	}
	for _, line := range p.lines {
		var m struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC != "2.0" {
			t.Errorf("stdout line is not JSON-RPC 2.0: %.200q (%v)", line, err)
		}
	}
}

// A signal kills the running capture on every path: with stdout read, the
// response in flight is still written; with stdout blocked, the capture is
// gone within killBound of the signal, long before the server gives up on
// its output. Either way the server exits 0 within shutdownGrace + 300 ms,
// and the fake perf-agent, which ignores SIGINT, and the setsid grandchild
// of the workload are gone within StopGrace + 500 ms.
func TestSignalKillsCapturesOnEveryPath(t *testing.T) {
	for _, c := range []struct {
		name    string
		sig     syscall.Signal
		blocked bool
	}{
		{"SIGTERM drained", syscall.SIGTERM, false},
		{"SIGINT drained", syscall.SIGINT, false},
		{"SIGTERM blocked", syscall.SIGTERM, true},
		{"SIGINT blocked", syscall.SIGINT, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := startCaptureServer(t, capturetest.FakeIgnoreSIGINT, nil)
			job := p.startCapture(map[string]any{"command": []any{capturetest.Workload(t), "double-fork-setsid", "2m"}, "duration": "1m"})
			p.waitProfiling(job)
			gc := p.awaitGrandchild()
			rec := p.fakeRecord()
			if c.blocked {
				// Answers larger than the 4 KiB pipe, never read.
				p.send("tools/list", map[string]any{})
				p.send("tools/list", map[string]any{})
			}
			inflight := p.send("tools/call", map[string]any{"name": "capture_wait", "arguments": map[string]any{"id": job, "timeout": "45s"}})
			time.Sleep(300 * time.Millisecond)
			if err := p.cmd.Process.Signal(c.sig); err != nil {
				t.Fatal(err)
			}
			sent := time.Now()
			rest := make(chan [][]byte, 1)
			if !c.blocked {
				go func() { rest <- p.readAll() }()
			}
			// The capture is killed before the server waits on its output:
			// with stdout blocked, the fake's group is gone long before the
			// drain of the answers nobody reads gives up.
			p.goneBy(sent.Add(killBound), "the fake perf-agent's group, right after the signal,", func() bool { return groupGone(rec.PGID) })
			select {
			case <-p.exited:
				t.Logf("exited %v after the signal", time.Since(sent).Round(time.Millisecond))
			case <-time.After(shutdownGrace + 300*time.Millisecond):
				t.Fatalf("the server still runs %v after the signal", shutdownGrace+300*time.Millisecond)
			}
			if p.waitErr != nil {
				t.Errorf("exit: %v", p.waitErr)
			}
			if !c.blocked {
				if !slices.ContainsFunc(<-rest, func(l []byte) bool { return responseID(l) == inflight }) {
					t.Error("the capture_wait in flight at the signal was not answered")
				}
			}
			deadline := sent.Add(stopGrace + 500*time.Millisecond)
			p.goneBy(deadline, "the fake perf-agent's group", func() bool { return groupGone(rec.PGID) })
			p.goneBy(deadline, "the workload's setsid grandchild", func() bool { return !gc.alive() })
		})
	}
}

// A client that stops reading its output is taken as gone: the server
// kills the capture and exits 0 without waiting on stdout or stderr.
func TestADeadClientEndsCaptures(t *testing.T) {
	p := startCaptureServer(t, capturetest.FakeIgnoreSIGINT, []string{outqEnv + "=2"})
	job := p.startCapture(map[string]any{"command": []any{capturetest.Workload(t), "double-fork-setsid", "2m"}, "duration": "1m"})
	p.waitProfiling(job)
	gc := p.awaitGrandchild()
	rec := p.fakeRecord()

	// Far more answers than the 4 KiB pipe and two queued messages hold.
	var flood bytes.Buffer
	for i := range 200 {
		fmt.Fprintf(&flood, `{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", 100000+i)
	}
	go func() { _, _ = p.in.Write(flood.Bytes()) }()
	overall := time.Now().Add(20 * time.Second)
	for !strings.Contains(p.stderr.String(), "client is not reading") {
		if time.Now().After(overall) {
			t.Fatalf("no saturation line on stderr:\n%s", p.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-p.exited:
	case <-time.After(time.Until(overall)):
		t.Fatal("the server still runs after it found the client not reading")
	}
	if p.waitErr != nil {
		t.Errorf("exit: %v", p.waitErr)
	}
	deadline := time.Now().Add(stopGrace)
	p.goneBy(deadline, "the fake perf-agent's group", func() bool { return groupGone(rec.PGID) })
	p.goneBy(deadline, "the workload's setsid grandchild", func() bool { return !gc.alive() })
}

// EOF on stdin cancels the running capture: the server exits 0 once it
// has rolled back, and leaves no profile behind.
func TestEOFCancelsTheCapture(t *testing.T) {
	p := startCaptureServer(t, capturetest.FakeIgnoreSIGINT, nil)
	job := p.startCapture(map[string]any{"command": []any{capturetest.Workload(t), "double-fork-setsid", "2m"}, "duration": "1m"})
	p.waitProfiling(job)
	gc := p.awaitGrandchild()
	rec := p.fakeRecord()
	if code := p.end(); code != 0 {
		t.Errorf("exit %d", code)
	}
	deadline := time.Now().Add(stopGrace + 500*time.Millisecond)
	p.goneBy(deadline, "the fake perf-agent's group", func() bool { return groupGone(rec.PGID) })
	p.goneBy(deadline, "the workload's setsid grandchild", func() bool { return !gc.alive() })
	var kept []string
	_ = filepath.WalkDir(p.profiles, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".pb.gz") {
			kept = append(kept, path)
		}
		return nil
	})
	if len(kept) != 0 {
		t.Errorf("the cancelled capture left profiles: %v", kept)
	}
}

// A client that stopped reading while stderr is full too: no log write
// keeps the server alive, and the capture is killed.
func TestADeadClientWithStderrBlockedEndsCaptures(t *testing.T) {
	p := startCaptureServer(t, capturetest.FakeIgnoreSIGINT, []string{outqEnv + "=2"})
	job := p.startCapture(map[string]any{"command": []any{capturetest.Workload(t), "double-fork-setsid", "2m"}, "duration": "1m"})
	p.waitProfiling(job)
	gc := p.awaitGrandchild()
	rec := p.fakeRecord()
	p.blockStderr()
	var flood bytes.Buffer
	for i := range 200 {
		fmt.Fprintf(&flood, `{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", 100000+i)
	}
	go func() { _, _ = p.in.Write(flood.Bytes()) }()
	select {
	case <-p.exited:
	case <-time.After(20 * time.Second):
		t.Fatal("the server still runs 20 s after the flood, with stdout and stderr blocked")
	}
	if p.waitErr != nil {
		t.Errorf("exit: %v", p.waitErr)
	}
	deadline := time.Now().Add(stopGrace)
	p.goneBy(deadline, "the fake perf-agent's group", func() bool { return groupGone(rec.PGID) })
	p.goneBy(deadline, "the workload's setsid grandchild", func() bool { return !gc.alive() })
}

// With stderr full and never read, no log line keeps the server from
// ending: not on a signal, not at EOF, where Close waits for the killed
// job to roll back, and not on a signal while it closes.
func TestABlockedStderrNeverHoldsTheExit(t *testing.T) {
	for _, how := range []string{"SIGTERM", "EOF", "EOF then SIGTERM"} {
		t.Run(how, func(t *testing.T) {
			p := startCaptureServer(t, capturetest.FakeIgnoreSIGINT, nil)
			job := p.startCapture(map[string]any{"command": []any{capturetest.Workload(t), "double-fork-setsid", "2m"}, "duration": "1m"})
			p.waitProfiling(job)
			gc := p.awaitGrandchild()
			rec := p.fakeRecord()
			p.blockStderr()
			go func() { _ = p.readAll() }()
			bound := shutdownGrace + 300*time.Millisecond
			switch how {
			case "EOF":
				// The killed job ends long before Close's limit on its
				// wait, StopGrace + 1 s: past StopGrace, its last log line
				// held it.
				bound = stopGrace
				_ = p.in.Close()
			case "EOF then SIGTERM":
				_ = p.in.Close()
				// Close is under way: the job is killed, and its end is
				// logged to the full stderr.
				time.Sleep(100 * time.Millisecond)
			}
			if how != "EOF" {
				if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			sent := time.Now()
			select {
			case <-p.exited:
				t.Logf("exited %v after the %s", time.Since(sent).Round(time.Millisecond), how)
			case <-time.After(bound):
				// Let the server write again, so the test can end.
				go func() { _, _ = io.Copy(io.Discard, p.errR) }()
				_ = p.errR.SetReadDeadline(time.Time{})
				t.Fatalf("the server still runs %v after the %s with stderr blocked", bound, how)
			}
			if p.waitErr != nil {
				t.Errorf("exit: %v", p.waitErr)
			}
			deadline := sent.Add(stopGrace + 500*time.Millisecond)
			p.goneBy(deadline, "the fake perf-agent's group", func() bool { return groupGone(rec.PGID) })
			p.goneBy(deadline, "the workload's setsid grandchild", func() bool { return !gc.alive() })
		})
	}
}

// serverProc is the server run as a subprocess over pipes, its stdout a
// 4 KiB pipe so a test can block it by not reading.
type serverProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     *os.File
	outR   *os.File
	out    *bufio.Reader
	stderr *lockedBuffer
	// errR and errW are the ends of the server's stderr pipe; copied is
	// closed once the copy of errR into stderr stops.
	errR, errW *os.File
	copied     chan struct{}
	profiles   string
	record     string
	nextID     int
	// lines are the stdout lines read so far.
	lines [][]byte
	// exited is closed once the process has been waited for; waitErr is
	// then its exit error.
	exited  chan struct{}
	waitErr error

	mu      sync.Mutex
	watched []procRef
}

// startCaptureServer runs the server over a fake perf-agent in mode,
// accepted without capabilities, and initializes it. At cleanup it fails
// the test for any process the server started that is still alive.
func startCaptureServer(t *testing.T, mode capturetest.FakeMode, env []string) *serverProc {
	t.Helper()
	capturetest.SkipIfPrivileged(t)
	record := filepath.Join(t.TempDir(), "record.json")
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: mode, Record: record})
	profiles := t.TempDir()
	cmd := fakeCommand(t, append([]string{fakeVerifyEnv + "=1"}, env...),
		"--profiles", profiles, "--perf-agent", fake,
		"--limit", "Capture.StopGrace="+stopGrace.String(),
		"--limit", "Capture.StartTimeout=30s",
		"--limit", "Capture.MaxDuration=2m",
		"--limit", "Capture.MinFreeBytes=1048576")
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(outW.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
		t.Skipf("cannot shrink the pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(errW.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
		t.Skipf("cannot shrink the pipe: %v", err)
	}
	p := &serverProc{t: t, cmd: cmd, in: inW, outR: outR, out: bufio.NewReaderSize(outR, 1<<20), stderr: &lockedBuffer{},
		errR: errR, errW: errW, copied: make(chan struct{}),
		profiles: profiles, record: record, nextID: 1, exited: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = inR.Close()
	_ = outW.Close()
	// errW stays open here, so blockStderr can fill the pipe.
	go func() {
		_, _ = io.Copy(p.stderr, errR)
		close(p.copied)
	}()
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = p.in.Close()
		select {
		case <-p.exited:
		case <-time.After(20 * time.Second):
			t.Error("the server did not exit at the end of the test")
			_ = cmd.Process.Kill()
			<-p.exited
		}
		_ = outR.Close()
		_ = errR.Close()
		_ = errW.Close()
		p.noLeftovers()
		if t.Failed() {
			t.Logf("server stderr:\n%s", p.stderr.String())
		}
	})
	p.call("initialize", map[string]any{"protocolVersion": "2025-11-25"})
	return p
}

// send writes one request and returns its id.
func (p *serverProc) send(method string, params any) int {
	p.t.Helper()
	id := p.nextID
	p.nextID++
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		p.t.Fatal(err)
	}
	return id
}

// call sends a request and returns its result, skipping notifications.
func (p *serverProc) call(method string, params any) json.RawMessage {
	p.t.Helper()
	id := p.send(method, params)
	_ = p.outR.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		line, err := p.out.ReadBytes('\n')
		if err != nil {
			p.t.Fatalf("%s: %v", method, err)
		}
		p.lines = append(p.lines, line)
		if responseID(line) != id {
			continue
		}
		var res struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(line, &res); err != nil || len(res.Result) == 0 {
			p.t.Fatalf("%s: %s (%v)", method, line, err)
		}
		return res.Result
	}
}

// tool calls a tool and returns its text and whether it is an error.
func (p *serverProc) tool(name string, args any) (string, bool) {
	p.t.Helper()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	raw := p.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Content) != 1 {
		p.t.Fatalf("%s: %s (%v)", name, raw, err)
	}
	return res.Content[0].Text, res.IsError
}

// startCapture calls capture, which must accept, and returns the job id.
func (p *serverProc) startCapture(args map[string]any) string {
	p.t.Helper()
	text, isErr := p.tool("capture", args)
	var a struct{ Job, Status string }
	if isErr || json.Unmarshal([]byte(text), &a) != nil || a.Status != "accepted" {
		p.t.Fatalf("capture: %s", text)
	}
	return a.Job
}

// waitProfiling waits until the job's perf-agent is ready and profiling.
func (p *serverProc) waitProfiling(job string) {
	p.t.Helper()
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); {
		text, isErr := p.tool("capture_wait", map[string]any{"id": job, "timeout": "500ms"})
		var v struct{ Status, Phase string }
		if isErr || json.Unmarshal([]byte(text), &v) != nil {
			p.t.Fatalf("capture_wait: %s", text)
		}
		if v.Status == "running" && v.Phase == capture.PhaseProfiling {
			return
		}
		if v.Status != "accepted" && v.Status != "preparing" && v.Status != "running" {
			p.t.Fatalf("the capture is %s before it profiled: %s", v.Status, text)
		}
	}
	p.t.Fatal("the capture never started profiling")
}

// waitEnd calls capture_wait until the job ends and returns its result or
// its error.
func (p *serverProc) waitEnd(job string) (string, bool) {
	p.t.Helper()
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); {
		text, isErr := p.tool("capture_wait", map[string]any{"id": job, "timeout": "10s"})
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &m) != nil {
			p.t.Fatalf("capture_wait: %s", text)
		}
		if _, done := m["capture"]; done || isErr {
			return text, isErr
		}
	}
	p.t.Fatalf("%s never ended", job)
	return "", false
}

// end closes stdin, reads stdout to its end and returns the exit status.
func (p *serverProc) end() int {
	p.t.Helper()
	_ = p.in.Close()
	p.lines = append(p.lines, p.readAll()...)
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		p.t.Fatal("the server did not exit at EOF")
	}
	if ee, ok := errors.AsType[*exec.ExitError](p.waitErr); ok {
		return ee.ExitCode()
	}
	if p.waitErr != nil {
		p.t.Fatal(p.waitErr)
	}
	return 0
}

// readAll reads the stdout lines left until stdout ends.
func (p *serverProc) readAll() [][]byte {
	_ = p.outR.SetReadDeadline(time.Now().Add(30 * time.Second))
	var lines [][]byte
	for {
		line, err := p.out.ReadBytes('\n')
		if len(line) > 0 {
			lines = append(lines, line)
		}
		if err != nil {
			return lines
		}
	}
}

// fakeRecord is what the capture's fake perf-agent recorded.
func (p *serverProc) fakeRecord() fakemode.Record {
	p.t.Helper()
	var r fakemode.Record
	b, err := os.ReadFile(p.record)
	if err != nil || json.Unmarshal(b, &r) != nil || r.PGID <= 0 {
		p.t.Fatalf("the fake's record: %s (%v)", b, err)
	}
	return r
}

// awaitGrandchild waits until the workload's setsid grandchild runs under
// the server, watches every process the server has started by then, and
// returns the grandchild.
func (p *serverProc) awaitGrandchild() procRef {
	p.t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		procs := descendants(p.cmd.Process.Pid)
		i := slices.IndexFunc(procs, func(r procRef) bool { return r.comm == "capworkload" && r.sid == r.pid })
		if i < 0 {
			continue
		}
		p.mu.Lock()
		p.watched = append(p.watched, procs...)
		p.mu.Unlock()
		return procs[i]
	}
	p.t.Fatalf("no setsid grandchild of the workload under the server: %+v", descendants(p.cmd.Process.Pid))
	return procRef{}
}

// goneBy fails the test unless gone holds by deadline.
func (p *serverProc) goneBy(deadline time.Time, what string, gone func() bool) {
	p.t.Helper()
	for !gone() {
		if time.Now().After(deadline) {
			p.t.Errorf("%s is still alive %v after the deadline", what, time.Since(deadline).Round(time.Millisecond))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockStderr stops reading the server's stderr and fills the pipe, so
// the server's next log write blocks.
func (p *serverProc) blockStderr() {
	p.t.Helper()
	_ = p.errR.SetReadDeadline(time.Now())
	<-p.copied
	go func() {
		for {
			if _, err := p.errW.Write([]byte{'x'}); err != nil {
				return
			}
		}
	}()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if n, err := unix.IoctlGetInt(int(p.errR.Fd()), unix.TIOCINQ); err == nil && n >= 4096 {
			return
		}
	}
	p.t.Fatal("stderr never filled")
}

// noLeftovers fails the test for any watched process still alive after a
// grace, and kills it by its PID once its start time still matches.
func (p *serverProc) noLeftovers() {
	p.mu.Lock()
	watched := slices.Clone(p.watched)
	p.mu.Unlock()
	for _, r := range watched {
		end := time.Now().Add(10 * time.Second)
		for r.alive() && time.Now().Before(end) {
			time.Sleep(20 * time.Millisecond)
		}
		if r.alive() {
			p.t.Errorf("%s (pid %d) is still alive after the server exited", r.comm, r.pid)
			_ = syscall.Kill(r.pid, syscall.SIGKILL)
		}
	}
}

// procRef is a process by its PID and start time, with what /proc/PID/stat
// said of it.
type procRef struct {
	pid, ppid, pgrp, sid int
	start                uint64
	comm                 string
}

func (r procRef) alive() bool { return capture.Alive(capture.DefaultProc(), r.pid, r.start) }

// readProc reads /proc/PID/stat.
func readProc(pid int) (procRef, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procRef{}, false
	}
	open, closing := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || closing < open {
		return procRef{}, false
	}
	f := strings.Fields(string(b[closing+1:]))
	if len(f) < 20 {
		return procRef{}, false
	}
	r := procRef{pid: pid, comm: string(b[open+1 : closing])}
	r.ppid, _ = strconv.Atoi(f[1])
	r.pgrp, _ = strconv.Atoi(f[2])
	r.sid, _ = strconv.Atoi(f[3])
	r.start, _ = strconv.ParseUint(f[19], 10, 64)
	return r, true
}

// descendants are the live processes under root.
func descendants(root int) []procRef {
	entries, _ := os.ReadDir("/proc")
	children := map[int][]procRef{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if r, ok := readProc(pid); ok {
			children[r.ppid] = append(children[r.ppid], r)
		}
	}
	var out []procRef
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}
	return out
}

// groupGone reports that no process is left in pgid.
func groupGone(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

// responseID is a response line's integer id, or -1.
func responseID(line []byte) int {
	var m struct {
		ID     *int            `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil || m.ID == nil || m.Method != "" {
		return -1
	}
	return *m.ID
}
