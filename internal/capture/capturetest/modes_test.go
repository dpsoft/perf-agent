//go:build linux

package capturetest_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// pgrepX lists the pids whose comm is exactly name; nil when pgrep is
// missing.
func pgrepX(name string) []int {
	out, _ := exec.Command("pgrep", "-x", name).Output()
	var pids []int
	for f := range strings.FieldsSeq(string(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// noLeftovers fails tb when a fakeperfagent or capworkload process appears
// that was not running before.
func noLeftovers(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		return
	}
	names := []string{"fakeperfagent", "capworkload"}
	before := map[string][]int{}
	for _, n := range names {
		before[n] = pgrepX(n)
	}
	t.Cleanup(func() {
		for _, n := range names {
			var extra []int
			for range 40 {
				extra = slices.DeleteFunc(pgrepX(n), func(p int) bool { return slices.Contains(before[n], p) })
				if len(extra) == 0 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if len(extra) > 0 {
				t.Errorf("leftover %s processes %v", n, extra)
			}
		}
	})
}

// proc is a started helper whose stdout is read line by line.
type proc struct {
	cmd    *exec.Cmd
	out    *bufio.Reader
	stderr *bytes.Buffer
	done   chan error
}

func start(t *testing.T, cmd *exec.Cmd) *proc {
	t.Helper()
	p := &proc{cmd: cmd, stderr: new(bytes.Buffer), done: make(chan error, 1)}
	cmd.Stderr = p.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.out = bufio.NewReader(stdout)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return p
}

// wait reaps the process (draining stdout first) and returns its exit code.
func (p *proc) wait(t *testing.T) int {
	t.Helper()
	_, _ = io.Copy(io.Discard, p.out)
	err := make(chan error, 1)
	go func() { err <- p.cmd.Wait() }()
	select {
	case e := <-err:
		return exitCode(t, e)
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit")
		return -1
	}
}

func (p *proc) alive() bool { return syscall.Kill(p.cmd.Process.Pid, 0) == nil }

func killPID(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
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
	line, err := p.out.ReadString('\n')
	if err != nil {
		t.Fatalf("no readiness line: %v (stderr %q)", err, p.stderr)
	}
	return line
}

func TestFakeModes(t *testing.T) {
	long := settings(capture.ModeCPU)
	long.Duration = time.Minute
	short := settings(capture.ModeCPU)
	cpuFile := func(dir string) string { return filepath.Join(dir, capture.OutputFile(capture.ModeCPU)) }

	t.Run("never-ready", func(t *testing.T) {
		noLeftovers(t)
		dir := t.TempDir()
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNeverReady})
		p := start(t, fakeCmd(argvFor(t, fake, dir, long)))
		lines := make(chan string, 1)
		go func() { l, _ := p.out.ReadString('\n'); lines <- l }()
		select {
		case l := <-lines:
			t.Fatalf("printed %q", l)
		case <-time.After(300 * time.Millisecond):
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
		noLeftovers(t)
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
		noLeftovers(t)
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
		noLeftovers(t)
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
			noLeftovers(t)
			dir := t.TempDir()
			fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: mode})
			p := start(t, fakeCmd(argvFor(t, fake, dir, short)))
			out, _ := io.ReadAll(p.out)
			if got := p.wait(t); got != want.code {
				t.Fatalf("exit %d", got)
			}
			if len(out) != 0 || !strings.Contains(p.stderr.String(), want.stderr) {
				t.Fatalf("stdout %q stderr %q", out, p.stderr)
			}
			if _, err := os.Stat(cpuFile(dir)); err == nil {
				t.Fatal("wrote a file")
			}
		})
	}

	t.Run("no-file", func(t *testing.T) {
		noLeftovers(t)
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
		noLeftovers(t)
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
		noLeftovers(t)
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
		noLeftovers(t)
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
		noLeftovers(t)
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeSpamStdout})
		p := start(t, fakeCmd(argvFor(t, fake, t.TempDir(), short)))
		out, _ := io.ReadAll(p.out)
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if len(out) < 4<<20 || p.stderr.Len() < 4<<20 {
			t.Fatalf("stdout %d stderr %d bytes", len(out), p.stderr.Len())
		}
	})

	t.Run("fork-grandchild", func(t *testing.T) {
		noLeftovers(t)
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
		killPID(t, rec.Grandchild)
		if rec.Grandchild == 0 || syscall.Kill(rec.Grandchild, 0) != nil {
			t.Fatalf("grandchild %d is not running", rec.Grandchild)
		}
		if pg, _ := syscall.Getpgid(rec.Grandchild); pg != cmd.Process.Pid {
			t.Fatalf("grandchild pgid %d, want %d", pg, cmd.Process.Pid)
		}
		p.wait(t)
		if err := syscall.Kill(rec.Grandchild, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no-gpu-samples", func(t *testing.T) {
		noLeftovers(t)
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
	pidLine := func(t *testing.T, p *proc) int {
		t.Helper()
		n, err := strconv.Atoi(strings.TrimSpace(readyLine(t, p)))
		if err != nil {
			t.Fatal(err)
		}
		killPID(t, n)
		return n
	}

	t.Run("busy-sleep-exit-after", func(t *testing.T) {
		noLeftovers(t)
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
		noLeftovers(t)
		p := run(t, "fork-and-exit", "5s")
		child := pidLine(t, p)
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if pg, err := syscall.Getpgid(child); err != nil || pg != p.cmd.Process.Pid {
			t.Fatalf("child pgid %d (%v), want %d", pg, err, p.cmd.Process.Pid)
		}
		_ = syscall.Kill(child, syscall.SIGKILL)
	})

	t.Run("double-fork-setsid", func(t *testing.T) {
		noLeftovers(t)
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
		_ = syscall.Kill(gc, syscall.SIGKILL)
		_ = p.cmd.Process.Kill()
		p.wait(t)
	})

	t.Run("drop-dumpable-after", func(t *testing.T) {
		noLeftovers(t)
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
		noLeftovers(t)
		p := run(t, "setresgid-after", "10ms", "10ms")
		out, _ := io.ReadAll(p.out)
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if s := string(out); s != "" && s != "skip\n" {
			t.Fatalf("output %q", s)
		}
	})

	t.Run("ignore-sigterm", func(t *testing.T) {
		noLeftovers(t)
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
		noLeftovers(t)
		p := run(t, "flood-output", "1")
		out, _ := io.ReadAll(p.out)
		if got := p.wait(t); got != 0 {
			t.Fatalf("exit %d", got)
		}
		if len(out) < 1<<20 || p.stderr.Len() < 1<<20 || utf8.Valid(out) {
			t.Fatalf("stdout %d (valid utf8 %v) stderr %d", len(out), utf8.Valid(out), p.stderr.Len())
		}
	})

	t.Run("bad-invocations", func(t *testing.T) {
		noLeftovers(t)
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
