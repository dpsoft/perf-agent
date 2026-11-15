//go:build linux

package capturetest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// stream collects what a child writes to one pipe. A single goroutine owns
// the read side, so no test reads the pipe itself.
type stream struct {
	mu   sync.Mutex
	buf  []byte
	pos  int
	done chan struct{}
}

func newStream(r *os.File) *stream {
	s := &stream{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		chunk := make([]byte, 32<<10)
		for {
			n, err := r.Read(chunk)
			s.mu.Lock()
			s.buf = append(s.buf, chunk[:n]...)
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *stream) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.buf)
}

func (s *stream) String() string { return string(s.bytes()) }
func (s *stream) Len() int       { return len(s.bytes()) }

// line returns the next unread line, waiting up to d for it.
func (s *stream) line(d time.Duration) (string, bool) {
	deadline := time.Now().Add(d)
	for {
		s.mu.Lock()
		if i := bytes.IndexByte(s.buf[s.pos:], '\n'); i >= 0 {
			l := string(s.buf[s.pos : s.pos+i+1])
			s.pos += i + 1
			s.mu.Unlock()
			return l, true
		}
		s.mu.Unlock()
		select {
		case <-s.done:
			return "", false
		default:
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// proc is a started helper. Its stdout and stderr are pipes owned by
// stream goroutines, and Wait never waits on a copier.
type proc struct {
	cmd    *exec.Cmd
	stdout *stream
	stderr *stream
}

func start(t *testing.T, cmd *exec.Cmd) *proc {
	t.Helper()
	pipe := func() (*stream, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
		s := newStream(r)
		return s, w
	}
	var wout, werr *os.File
	p := &proc{cmd: cmd}
	p.stdout, wout = pipe()
	p.stderr, werr = pipe()
	cmd.Stdout, cmd.Stderr = wout, werr
	err := cmd.Start()
	_ = wout.Close()
	_ = werr.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return p
}

// wait reaps the process within 10 s, then lets the output streams drain
// for up to 2 s (a grandchild holding the pipe would keep them open), and
// returns the exit code.
func (p *proc) wait(t *testing.T) int {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- p.cmd.Wait() }()
	var code int
	select {
	case e := <-res:
		code = exitCode(t, e)
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-res
		t.Fatal("process did not exit")
	}
	for _, s := range []*stream{p.stdout, p.stderr} {
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
	}
	return code
}

func (p *proc) alive() bool { return syscall.Kill(p.cmd.Process.Pid, 0) == nil }

// startTime is field 22 of /proc/PID/stat, which with the pid identifies a
// process.
func startTime(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	if len(f) < 20 {
		return "", errors.New("short /proc stat")
	}
	return f[19], nil
}

// killPID registers a verified kill of pid, a process a helper reported,
// and returns it for tests that kill earlier. It signals only a pid above 1
// whose start time is still the one seen here, and at cleanup it checks the
// process is gone.
func killPID(t *testing.T, pid int) (kill func()) {
	t.Helper()
	if pid <= 1 {
		t.Fatalf("refusing to track pid %d", pid)
	}
	st, err := startTime(pid)
	if err != nil {
		t.Fatalf("pid %d is not running: %v", pid, err)
	}
	kill = func() {
		if now, err := startTime(pid); err == nil && now == st {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	t.Cleanup(func() {
		kill()
		for range 80 {
			if now, err := startTime(pid); err != nil || now != st {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("leftover process %d", pid)
	})
	return kill
}

func settings(modes ...capture.Mode) capture.Settings {
	return capture.Settings{Modes: modes, Duration: 150 * time.Millisecond, SampleRate: 99, Unwind: "fp"}
}

func argvFor(t *testing.T, fake, staging string, s capture.Settings) []string {
	t.Helper()
	argv, err := capture.BuildArgv(capture.ArgvSpec{Binary: fake, PID: 1, Staging: staging, GPUShim: "/shim.so", CaptureID: "c1", Settings: s})
	if err != nil {
		t.Fatal(err)
	}
	return argv
}

func readyLine(t *testing.T, p *proc) string {
	t.Helper()
	line, ok := p.stdout.line(10 * time.Second)
	if !ok {
		t.Fatalf("no readiness line (stderr %q)", p.stderr)
	}
	return line
}

func TestFakeModes(t *testing.T) {
	long := settings(capture.ModeCPU)
	long.Duration = time.Minute
	short := settings(capture.ModeCPU)
	cpuFile := func(dir string) string { return filepath.Join(dir, capture.OutputFile(capture.ModeCPU)) }

	t.Run("mode-flags-gate-outputs", func(t *testing.T) {
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{})
		for _, flag := range []string{"profile-output", "offcpu-output", "gpu-output", "gpu-executions-output"} {
			dst := filepath.Join(t.TempDir(), "out")
			cmd := exec.Command(fake, "--pid", "1", "--duration", "1ms", "--"+flag, dst)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if got := exitCode(t, cmd.Run()); got != 1 || !strings.Contains(stderr.String(), "At least one of --profile") {
				t.Errorf("--%s alone: exit %d stderr %q", flag, got, &stderr)
			}
			if _, err := os.Stat(dst); err == nil {
				t.Errorf("--%s alone wrote its output", flag)
			}
		}
		// An enabled mode writes only its own outputs.
		dir := t.TempDir()
		out := func(n string) string { return filepath.Join(dir, n) }
		if err := exec.Command(fake, "--pid", "1", "--duration", "1ms", "--profile", "--profile-output", out("cpu"),
			"--offcpu-output", out("off"), "--gpu-output", out("gpu")).Run(); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 || entries[0].Name() != "cpu" {
			t.Fatalf("wrote %v", entries)
		}
		for _, args := range [][]string{{"--pid", "x", "--profile"}, {"--pid", "1", "--sample-rate", "fast", "--profile"}, {"--pid", "1", "--gpu"}, {"--pid", "1", "--gpu-shim", "/x", "--profile"}, {"--profile"}} {
			if got := exitCode(t, exec.Command(fake, args...).Run()); got == 0 {
				t.Errorf("%q succeeded", args)
			}
		}
	})

	t.Run("unknown-fake-mode", func(t *testing.T) {
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: "bogus"})
		if got := exitCode(t, exec.Command(fake, "--pid", "1", "--profile").Run()); got != 1 {
			t.Fatalf("exit %d", got)
		}
	})

	t.Run("sample-types", func(t *testing.T) {
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{})
		for _, tc := range []struct {
			mode capture.Mode
			typ  string
		}{{capture.ModeCPU, "cpu"}, {capture.ModeOffCPU, "offcpu"}, {capture.ModeGPU, "gpu"}} {
			dir := t.TempDir()
			if err := fakeCmd(argvFor(t, fake, dir, settings(tc.mode))).Run(); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(dir, capture.OutputFile(tc.mode)))
			if err != nil {
				t.Fatal(err)
			}
			prof, err := profile.ParseData(b)
			if err != nil || len(prof.SampleType) != 1 || prof.SampleType[0].Type != tc.typ || prof.SampleType[0].Unit != "nanoseconds" {
				t.Errorf("%s: %v %v", tc.mode, prof.SampleType, err)
			}
		}
	})

	t.Run("never-ready", func(t *testing.T) {
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNeverReady})
		p := start(t, fakeCmd(argvFor(t, fake, dir, long)))
		if l, ok := p.stdout.line(300 * time.Millisecond); ok {
			t.Fatalf("printed %q", l)
		}
		_ = p.cmd.Process.Signal(syscall.SIGINT)
		if got := p.wait(t); got != 1 {
			t.Fatalf("exit %d", got)
		}
		if _, err := os.Stat(cpuFile(dir)); err == nil {
			t.Fatal("wrote a file")
		}
	})

	t.Run("ready-after", func(t *testing.T) {
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{ReadyAfter: 300 * time.Millisecond})
		begin := time.Now()
		p := start(t, fakeCmd(argvFor(t, fake, t.TempDir(), short)))
		readyLine(t, p)
		if time.Since(begin) < 250*time.Millisecond {
			t.Fatal("ready too early")
		}
		p.wait(t)
	})

	t.Run("ignore-sigint", func(t *testing.T) {
		dir := t.TempDir()
		s := settings(capture.ModeCPU)
		s.Duration = 800 * time.Millisecond
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeIgnoreSIGINT})
		p := start(t, fakeCmd(argvFor(t, fake, dir, s)))
		readyLine(t, p)
		_ = p.cmd.Process.Signal(syscall.SIGINT)
		time.Sleep(300 * time.Millisecond)
		if !p.alive() {
			t.Fatal("exited on SIGINT")
		}
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if _, err := os.Stat(cpuFile(dir)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("hang-after-sigint", func(t *testing.T) {
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeHangAfterSIGINT})
		p := start(t, fakeCmd(argvFor(t, fake, dir, long)))
		readyLine(t, p)
		_ = p.cmd.Process.Signal(syscall.SIGINT)
		time.Sleep(300 * time.Millisecond)
		if !p.alive() {
			t.Fatal("exited after SIGINT")
		}
		_ = p.cmd.Process.Kill()
		_ = p.wait(t)
		if _, err := os.Stat(cpuFile(dir)); err == nil {
			t.Fatal("wrote a file")
		}
	})

	for mode, want := range map[capturetest.FakeMode]struct {
		code   int
		stderr string
	}{
		capturetest.FakeExit1Stderr:   {1, "failed to set capabilities"},
		capturetest.FakeExit2:         {2, "usage: fakeperfagent"},
		capturetest.FakeExit127Loader: {127, "error while loading shared libraries: libblazesym_c.so"},
	} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: mode})
			p := start(t, fakeCmd(argvFor(t, fake, dir, short)))
			if got := p.wait(t); got != want.code {
				t.Fatalf("exit %d", got)
			}
			out := p.stdout.bytes()
			if len(out) != 0 || !strings.Contains(p.stderr.String(), want.stderr) {
				t.Fatalf("stdout %q stderr %q", out, p.stderr)
			}
			if _, err := os.Stat(cpuFile(dir)); err == nil {
				t.Fatal("wrote a file")
			}
		})
	}

	t.Run("no-file", func(t *testing.T) {
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNoFile})
		p := start(t, fakeCmd(argvFor(t, fake, dir, short)))
		readyLine(t, p)
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("wrote %d files", len(entries))
		}
	})

	t.Run("huge-file", func(t *testing.T) {
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeHugeFile})
		p := start(t, fakeCmd(argvFor(t, fake, dir, short)))
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if fi, err := os.Stat(cpuFile(dir)); err != nil || fi.Size() < 64<<20 {
			t.Fatalf("size %v err %v", fi, err)
		}
	})

	t.Run("symlink-output", func(t *testing.T) {
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeSymlinkOutput})
		p := start(t, fakeCmd(argvFor(t, fake, dir, short)))
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if fi, err := os.Lstat(cpuFile(dir)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("not a symlink: %v %v", fi, err)
		}
	})

	t.Run("hardlink-output", func(t *testing.T) {
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeHardlinkOutput})
		p := start(t, fakeCmd(argvFor(t, fake, dir, short)))
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		fi, err := os.Lstat(cpuFile(dir))
		if err != nil || !fi.Mode().IsRegular() || fi.Sys().(*syscall.Stat_t).Nlink < 2 {
			t.Fatalf("not a hard link: %v %v", fi, err)
		}
	})

	t.Run("spam-stdout", func(t *testing.T) {
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeSpamStdout})
		p := start(t, fakeCmd(argvFor(t, fake, t.TempDir(), short)))
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		out := p.stdout.bytes()
		if len(out) < 4<<20 || p.stderr.Len() < 4<<20 {
			t.Fatalf("stdout %d stderr %d bytes", len(out), p.stderr.Len())
		}
	})

	t.Run("fork-grandchild", func(t *testing.T) {
		record := filepath.Join(t.TempDir(), "rec.json")
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeForkGrandchild, Record: record})
		cmd := fakeCmd(argvFor(t, fake, t.TempDir(), short))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		p := start(t, cmd)
		readyLine(t, p)
		var rec struct {
			Grandchild int `json:"grandchild"`
		}
		readJSON(t, record, &rec)
		kill := killPID(t, rec.Grandchild)
		if pg, _ := syscall.Getpgid(rec.Grandchild); pg != cmd.Process.Pid {
			t.Fatalf("grandchild pgid %d, want %d", pg, cmd.Process.Pid)
		}
		p.wait(t)
		kill()
	})

	t.Run("no-gpu-samples", func(t *testing.T) {
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNoGPUSamples})
		p := start(t, fakeCmd(argvFor(t, fake, dir, settings(capture.ModeGPU))))
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		f, err := os.Open(filepath.Join(dir, capture.OutputFile(capture.ModeGPU)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		prof, err := profile.Parse(f)
		if err != nil {
			t.Fatalf("not a valid profile: %v", err)
		}
		if len(prof.Sample) != 0 || len(prof.SampleType) != 1 || prof.SampleType[0].Type != "gpu" {
			t.Fatalf("samples %d types %v", len(prof.Sample), prof.SampleType)
		}
		lg, err := os.Open(filepath.Join(dir, capture.ExeclogFile))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lg.Close() }()
		rows := 0
		if _, _, err := execlog.Read(lg, func(execlog.Row) error { rows++; return nil }); err != nil || rows != 0 {
			t.Fatalf("execlog rows %d err %v", rows, err)
		}
	})

	t.Run("stray-positional-and-bad-duration", func(t *testing.T) {
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{})
		for _, args := range [][]string{{"--duration", "5s", "stray"}, {"--duration", "soon"}} {
			if got := exitCode(t, exec.Command(fake, args...).Run()); got != 2 {
				t.Errorf("%q: exit %d, want 2", args, got)
			}
		}
	})
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestWorkloadModes(t *testing.T) {
	w := capturetest.Workload(t)
	run := func(t *testing.T, args ...string) *proc {
		t.Helper()
		cmd := exec.Command(w, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return start(t, cmd)
	}
	pidKills := map[int]func(){}
	pidLine := func(t *testing.T, p *proc) int {
		t.Helper()
		n, err := strconv.Atoi(strings.TrimSpace(readyLine(t, p)))
		if err != nil {
			t.Fatal(err)
		}
		pidKills[n] = killPID(t, n)
		return n
	}

	t.Run("busy-sleep-exit-after", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			code int
		}{{[]string{"busy", "50ms"}, 0}, {[]string{"sleep", "50ms"}, 0}, {[]string{"exit-after", "20ms", "3"}, 3}} {
			if got := run(t, tc.args...).wait(t); got != tc.code {
				t.Errorf("%q: exit %d, want %d", tc.args, got, tc.code)
			}
		}
	})

	t.Run("fork-and-exit", func(t *testing.T) {
		p := run(t, "fork-and-exit", "5s")
		child := pidLine(t, p)
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if pg, err := syscall.Getpgid(child); err != nil || pg != p.cmd.Process.Pid {
			t.Fatalf("child pgid %d (%v), want %d", pg, err, p.cmd.Process.Pid)
		}
		pidKills[child]()
	})

	t.Run("double-fork-setsid", func(t *testing.T) {
		p := run(t, "double-fork-setsid", "5s")
		gc := pidLine(t, p)
		pg, err := syscall.Getpgid(gc)
		if err != nil {
			t.Fatal(err)
		}
		sid := sessionOf(t, gc)
		if pg == p.cmd.Process.Pid || sid != gc {
			t.Fatalf("grandchild pgid %d sid %d, workload pgid %d, pid %d", pg, sid, p.cmd.Process.Pid, gc)
		}
		pidKills[gc]()
		_ = p.cmd.Process.Kill()
		p.wait(t)
	})

	t.Run("drop-dumpable-after", func(t *testing.T) {
		capturetest.SkipIfPrivileged(t)
		p := run(t, "drop-dumpable-after", "50ms", "5s")
		time.Sleep(400 * time.Millisecond)
		fi, err := os.Stat("/proc/" + strconv.Itoa(p.cmd.Process.Pid) + "/status")
		if err != nil {
			t.Fatal(err)
		}
		if uid := fi.Sys().(*syscall.Stat_t).Uid; uid != 0 {
			t.Fatalf("/proc status file owned by %d, want 0 (not dumpable)", uid)
		}
		_ = p.cmd.Process.Kill()
		p.wait(t)
	})

	t.Run("setresgid-after", func(t *testing.T) {
		p := run(t, "setresgid-after", "10ms", "10ms")
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		out := p.stdout.bytes()
		if s := string(out); s != "" && s != "skip\n" {
			t.Fatalf("output %q", s)
		}
	})

	t.Run("ignore-sigterm", func(t *testing.T) {
		p := run(t, "ignore-sigterm", "700ms")
		time.Sleep(150 * time.Millisecond)
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		if !p.alive() {
			t.Fatal("exited on SIGTERM")
		}
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
	})

	t.Run("flood-output", func(t *testing.T) {
		p := run(t, "flood-output", "1")
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		out := p.stdout.bytes()
		if len(out) < 1<<20 || p.stderr.Len() < 1<<20 || utf8.Valid(out) {
			t.Fatalf("stdout %d (valid utf8 %v) stderr %d", len(out), utf8.Valid(out), p.stderr.Len())
		}
	})

	t.Run("bad-invocations", func(t *testing.T) {
		for _, args := range [][]string{{}, {"nope"}, {"sleep"}, {"sleep", "soon"}, {"exit-after", "1ms", "x"}, {"flood-output", "many"}, {"exec"}, {"exec", "/nonexistent/x"}} {
			if got := run(t, args...).wait(t); got != 2 {
				t.Errorf("%q: exit %d, want 2", args, got)
			}
		}
	})
}

// sessionOf reads the session id from /proc/<pid>/stat.
func sessionOf(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	n, err := strconv.Atoi(f[3])
	if err != nil {
		t.Fatal(err)
	}
	return n
}
