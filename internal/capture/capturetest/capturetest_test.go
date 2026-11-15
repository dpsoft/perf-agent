//go:build linux

package capturetest_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func cpuArgv(t *testing.T, fake, staging, duration string) []string {
	t.Helper()
	d, err := time.ParseDuration(duration)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := capture.BuildArgv(capture.ArgvSpec{
		Binary: fake, PID: 1, Staging: staging, CaptureID: "c1",
		Settings: capture.Settings{Modes: []capture.Mode{capture.ModeCPU}, Duration: d, SampleRate: 99, Unwind: "fp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return argv
}

func fakeCmd(argv []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = slices.Clone(capture.PerfAgentEnv)
	return cmd
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func TestFakeRefusesUnknownFlags(t *testing.T) {
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNormal})
	err := exec.Command(fake, "--bogus").Run()
	if got := exitCode(t, err); got != 2 {
		t.Fatalf("exit %d, want 2", got)
	}
}

func TestFakeWritesTheRequestedOutputs(t *testing.T) {
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNormal})
	staging := t.TempDir()
	cmd := fakeCmd(cpuArgv(t, fake, staging, "200ms"))
	out, err := cmd.Output()
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("exit %d", got)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	if !strings.HasPrefix(first, "Collecting data for 200ms") {
		t.Fatalf("first line %q", first)
	}
	got, err := os.ReadFile(filepath.Join(staging, capture.OutputFile(capture.ModeCPU)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pmfixture.CPUBasic()) {
		t.Fatal("cpu.pb.gz is not CPUBasic()")
	}
}

func TestFakeStopsOnSIGINT(t *testing.T) {
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNormal})
	staging := t.TempDir()
	cmd := fakeCmd(cpuArgv(t, fake, staging, "1m"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if got := exitCode(t, cmd.Wait()); got != 0 {
		t.Fatalf("exit %d", got)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	if _, err := os.Stat(filepath.Join(staging, capture.OutputFile(capture.ModeCPU))); err != nil {
		t.Fatal(err)
	}
}

func TestFakeRecordsArgvEnvAndPgid(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNormal, Record: record})
	argv := cpuArgv(t, fake, t.TempDir(), "100ms")
	cmd := fakeCmd(argv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Argv []string `json:"argv"`
		Env  []string `json:"env"`
		PID  int      `json:"pid"`
		PGID int      `json:"pgid"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.Argv, argv) {
		t.Fatalf("argv %q, want %q", rec.Argv, argv)
	}
	if !slices.Equal(slices.Sorted(slices.Values(rec.Env)), slices.Sorted(slices.Values(capture.PerfAgentEnv))) {
		t.Fatalf("env %q", rec.Env)
	}
	if rec.PID != cmd.ProcessState.Pid() || rec.PGID != rec.PID {
		t.Fatalf("pid %d pgid %d, want both %d", rec.PID, rec.PGID, cmd.ProcessState.Pid())
	}
}

func TestFakeKeepsItsComm(t *testing.T) {
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNormal})
	cmd := fakeCmd(cpuArgv(t, fake, t.TempDir(), "1m"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/comm")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(comm)); got != "fakeperfagent" {
		t.Fatalf("comm %q", got)
	}
}

func TestWorkloadReportAndExecChain(t *testing.T) {
	w := capturetest.Workload(t)
	first, err := exec.Command(w, "report").Output()
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(first, &keys); err != nil {
		t.Fatalf("%v: %q", err, first)
	}
	for _, k := range []string{"nonewprivs", "tty_nr", "env"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("report lacks %q: %s", k, first)
		}
	}
	chained, err := exec.Command(w, "exec", w, "report").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, chained) {
		t.Fatalf("exec chain differs:\n%s\n%s", first, chained)
	}
}

func TestFakeFailureModes(t *testing.T) {
	for mode, want := range map[capturetest.FakeMode]struct {
		code int
		err  string
	}{
		capturetest.FakeExit1Stderr:   {1, "failed to set capabilities"},
		capturetest.FakeExit2:         {2, "usage: fakeperfagent"},
		capturetest.FakeExit127Loader: {127, "error while loading shared libraries: libblazesym_c.so"},
	} {
		fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: mode})
		var stderr bytes.Buffer
		cmd := fakeCmd(cpuArgv(t, fake, t.TempDir(), "1m"))
		cmd.Stderr = &stderr
		if got := exitCode(t, cmd.Run()); got != want.code || !strings.Contains(stderr.String(), want.err) {
			t.Errorf("%s: exit %d stderr %q", mode, got, stderr.String())
		}
	}
}
