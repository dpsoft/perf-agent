package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/mcpflags/binarytest"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// fakeVerifyEnv makes a test binary run as the server and accept any
// --perf-agent, such as the fake perf-agent, which cannot be setcap'd.
const fakeVerifyEnv = "PERF_AGENT_MCP_TEST_FAKE_VERIFY"

// outqEnv makes a test binary run as the server with an output queue of
// that many messages, so a test can fill it with a pipe of a few KiB.
const outqEnv = "PERF_AGENT_MCP_TEST_OUTQ"

// TestMain lets this test binary serve as a capture's reaper and launcher,
// as the command's own binary does, and as the server itself when a test
// runs it with one of the environment variables above.
func TestMain(m *testing.M) {
	if code, handled := capture.RunInternal(); handled {
		os.Exit(code)
	}
	if hook := envConfigHook(); hook != nil {
		configHook = hook
		main()
		return
	}
	code := capturetest.Main(m)
	if fakeBinDir != "" {
		_ = os.RemoveAll(fakeBinDir)
	}
	os.Exit(code)
}

// envConfigHook is the configHook the environment asks for, composed of
// every test variable set, or nil when none is.
func envConfigHook() func(*perfmcp.Config) {
	var steps []func(*perfmcp.Config)
	if os.Getenv(fakeVerifyEnv) == "1" {
		steps = append(steps, func(c *perfmcp.Config) { c.Capture.Verify = fakeVerify })
	}
	if v := os.Getenv(outqEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "%s=%q is not a positive integer\n", outqEnv, v)
			os.Exit(2)
		}
		steps = append(steps, func(c *perfmcp.Config) { c.OutputQueueMessages = n })
	}
	if len(steps) == 0 {
		return nil
	}
	return func(c *perfmcp.Config) {
		for _, step := range steps {
			step(c)
		}
	}
}

// fakeVerify accepts any file as a perf-agent without capabilities; its
// identity is still read, so the check before each run holds.
func fakeVerify(p string) (capture.Binary, error) {
	id, err := capture.IdentityOf(p)
	return capture.Binary{Path: p, ID: id, Version: "v0.0.0-test"}, err
}

// fakeCommand is a subprocess running the server, this package's test
// binary built without -race, with env added to the test's environment.
func fakeCommand(t *testing.T, env []string, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := fakeTestBinary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// fakeBinDir holds the test binary fakeCommand builds.
var fakeBinDir string

// fakeTestBinary builds this package's test binary without the race
// detector, whose overhead slows the shutdown the signal tests time.
var fakeTestBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "perf-agent-mcp-fake")
	if err != nil {
		return "", err
	}
	fakeBinDir = dir
	bin := filepath.Join(dir, "server.test")
	if out, err := exec.Command("go", "test", "-c", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("go test -c: %v\n%s", err, out)
	}
	return bin, nil
})

func TestRunNeedsAProfilesRoot(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "--profiles") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunRefusesUnknownLimits(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"--profiles", t.TempDir(), "--limit", "Bogus=1"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Model.MaxRawBytes") || !strings.Contains(errb.String(), "Bogus") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunAppliesLimits(t *testing.T) {
	var out, errb bytes.Buffer
	args := []string{"--profiles", t.TempDir(), "--limit", "Model.MaxRawBytes=1073741824", "--limit", "Source.GitTimeout=2s", "--limit", "MaxRows=7"}
	if code := run(args, strings.NewReader(""), &out, &errb); code != 0 {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if code := run([]string{"--profiles", t.TempDir(), "--limit", "MaxRows=x"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("non-integer limit: exit %d, want 2", code)
	}
}

// The capture flags reach the server's configuration as given, and
// --capture-ttl wins over --limit Capture.TTL wherever it stands.
func TestRunCaptureFlagsReachTheConfig(t *testing.T) {
	var got perfmcp.Config
	// The hook keeps what the flags made and turns capture off, so the
	// made-up paths are never verified.
	configHook = func(c *perfmcp.Config) {
		got = *c
		c.Capture = perfmcp.CaptureConfig{}
	}
	t.Cleanup(func() { configHook = nil })
	runOK := func(args ...string) perfmcp.Config {
		t.Helper()
		got = perfmcp.Config{}
		var out, errb bytes.Buffer
		if code := run(append([]string{"--profiles", t.TempDir()}, args...), strings.NewReader(""), &out, &errb); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errb.String())
		}
		return got
	}

	c := runOK("--perf-agent", "/opt/pa/perf-agent", "--captures", "runs",
		"--capture-command", "/usr/bin/true", "--capture-command", "/usr/bin/false",
		"--gpu-shim", "/opt/pa/libshim.so", "--capture-ttl", "48h", "--limit", "Capture.TTL=1h")
	want := perfmcp.CaptureConfig{PerfAgent: "/opt/pa/perf-agent", Dir: "runs", GPUShim: "/opt/pa/libshim.so", Owner: "stdio",
		Commands: []string{"/usr/bin/true", "/usr/bin/false"}}
	if c.Capture.Verify != nil {
		t.Error("the command set a Verify seam")
	}
	c.Capture.Verify = nil
	if !reflect.DeepEqual(c.Capture, want) {
		t.Errorf("Capture = %+v, want %+v", c.Capture, want)
	}
	if c.Limits.Capture.TTL != 48*time.Hour {
		t.Errorf("--capture-ttl 48h after --limit: TTL %v", c.Limits.Capture.TTL)
	}
	if c := runOK("--capture-ttl", "48h", "--limit", "Capture.TTL=1h"); c.Limits.Capture.TTL != 48*time.Hour {
		t.Errorf("--capture-ttl 48h before --limit: TTL %v", c.Limits.Capture.TTL)
	}
	if c := runOK("--limit", "Capture.TTL=1h"); c.Limits.Capture.TTL != time.Hour {
		t.Errorf("--limit Capture.TTL=1h alone: TTL %v", c.Limits.Capture.TTL)
	}
	if c := runOK("--capture-ttl", "0s"); c.Limits.Capture.TTL != 0 {
		t.Errorf("--capture-ttl 0s: TTL %v, want 0 (no age limit)", c.Limits.Capture.TTL)
	}
	c = runOK()
	if c.Limits.Capture.TTL != perfmcp.DefaultLimits().Capture.TTL || c.Capture.PerfAgent != "" || c.Capture.Dir != "" {
		t.Errorf("defaults: TTL %v, Capture %+v", c.Limits.Capture.TTL, c.Capture)
	}

	for _, args := range [][]string{
		{"--capture-ttl", "-1h"},
		{"--capture-ttl", "soon"},
		{"--gpu-shim", "lib/shim.so"},
		{"--capture-command", "bin/bench"},
	} {
		var out, errb bytes.Buffer
		if code := run(append([]string{"--profiles", t.TempDir()}, args...), strings.NewReader(""), &out, &errb); code != 2 || !strings.Contains(errb.String(), strings.TrimLeft(args[0], "-")) {
			t.Errorf("%v: exit %d, stderr %q; want 2 naming %s", args, code, errb.String(), args[0])
		}
	}
}

// --captures names a directory inside the profiles root; one outside it,
// through .. or a symlink, or a file, is a usage error. An absolute one
// inside reaches the server relative to the root.
func TestRunValidatesTheCapturesDir(t *testing.T) {
	profiles, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(profiles, "away")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(profiles, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside", filepath.Join(profiles, "near")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Links that lead back to the profiles root itself.
	if err := os.Symlink(".", filepath.Join(profiles, "self")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(profiles, "inside", "up")); err != nil {
		t.Fatal(err)
	}
	var got perfmcp.Config
	configHook = func(c *perfmcp.Config) {
		got = *c
		c.Capture = perfmcp.CaptureConfig{}
	}
	t.Cleanup(func() { configHook = nil })
	for _, c := range []struct {
		dir, want string
		code      int
	}{
		{"runs", "runs", 0},
		{"inside/runs", "inside/runs", 0},
		{filepath.Join(profiles, "inside", "runs"), "inside/runs", 0},
		{"near/runs", "near/runs", 0},
		{"../runs", "outside the profiles root", 2},
		{filepath.Join(outside, "runs"), "outside the profiles root", 2},
		{"away/runs", "outside the profiles root", 2},
		{"away", "outside the profiles root", 2},
		{"file", "not a directory", 2},
		{".", "the profiles root itself", 2},
		{"inside/..", "the profiles root itself", 2},
		{profiles, "the profiles root itself", 2},
		{profiles + "/", "the profiles root itself", 2},
		{"self", "the profiles root itself", 2},
		{"inside/up", "the profiles root itself", 2},
		{filepath.Join(profiles, "self"), "the profiles root itself", 2},
	} {
		got = perfmcp.Config{}
		var out, errb bytes.Buffer
		code := run([]string{"--profiles", profiles, "--perf-agent", "/opt/perf-agent/perf-agent", "--captures", c.dir}, strings.NewReader(""), &out, &errb)
		if code != c.code {
			t.Errorf("--captures %s: exit %d, want %d; stderr %q", c.dir, code, c.code, errb.String())
			continue
		}
		if code == 0 && got.Capture.Dir != c.want {
			t.Errorf("--captures %s reached the server as %q, want %q", c.dir, got.Capture.Dir, c.want)
		}
		if code != 0 && (!strings.Contains(errb.String(), "--captures") || !strings.Contains(errb.String(), c.want)) {
			t.Errorf("--captures %s: stderr %q, want it to name --captures and %q", c.dir, errb.String(), c.want)
		}
	}
}

// Without --perf-agent, --captures is not checked: it has no effect, and
// the one line saying so is all the server says of it.
func TestRunLeavesCapturesUncheckedWithoutPerfAgent(t *testing.T) {
	for _, dir := range []string{"../runs", "."} {
		var out, errb bytes.Buffer
		if code := run([]string{"--profiles", t.TempDir(), "--captures", dir}, strings.NewReader(""), &out, &errb); code != 0 {
			t.Errorf("--captures %s without --perf-agent: exit %d, stderr %q", dir, code, errb.String())
		}
		if !strings.Contains(errb.String(), "without --perf-agent") || strings.Contains(errb.String(), "profiles root") {
			t.Errorf("--captures %s without --perf-agent: stderr %q", dir, errb.String())
		}
	}
}

// The capture flags do nothing without --perf-agent; one line on stderr
// says so, naming the flags given.
func TestRunWarnsOfCaptureFlagsWithoutPerfAgent(t *testing.T) {
	var out, errb bytes.Buffer
	args := []string{"--profiles", t.TempDir(), "--gpu-shim", "/opt/libshim.so", "--captures", "runs", "--capture-command", "/usr/bin/true", "--capture-ttl", "1h"}
	if code := run(args, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	var warned []string
	for line := range strings.Lines(errb.String()) {
		if strings.Contains(line, "without --perf-agent") {
			warned = append(warned, line)
		}
	}
	if len(warned) != 1 {
		t.Fatalf("stderr %q: want one warning", errb.String())
	}
	for _, f := range []string{"--gpu-shim", "--captures", "--capture-command", "--capture-ttl"} {
		if !strings.Contains(warned[0], f) {
			t.Errorf("the warning %q does not name %s", warned[0], f)
		}
	}
	errb.Reset()
	if code := run([]string{"--profiles", t.TempDir()}, strings.NewReader(""), &out, &errb); code != 0 || strings.Contains(errb.String(), "without --perf-agent") {
		t.Errorf("no capture flag: exit %d, stderr %q", code, errb.String())
	}
}

// On the paths that do not wait for Close, a signal and a client that
// stopped reading, the command kills the captures itself before it
// starts closing the server.
func TestTheCapturesAreKilledBeforeClose(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	note := func(c string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, c)
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
	kill, closeSrv := killCaptures, closeServer
	killCaptures = func(s *perfmcp.Server) { note("kill"); kill(s) }
	closeServer = func(s *perfmcp.Server) error { note("close"); return closeSrv(s) }
	t.Cleanup(func() { killCaptures, closeServer = kill, closeSrv })
	// order waits for Close to start and returns the calls until then.
	order := func(t *testing.T) []string {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if c := seen(); slices.Contains(c, "close") {
				return c[:slices.Index(c, "close")+1]
			}
		}
		t.Fatalf("Close never ran: %v", seen())
		return nil
	}

	t.Run("signal", func(t *testing.T) {
		mu.Lock()
		calls = nil
		mu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		in, inW := io.Pipe()
		defer func() { _ = inW.Close() }()
		var out, errb bytes.Buffer
		if code := runContext(ctx, []string{"--profiles", t.TempDir()}, in, &out, &errb); code != 0 {
			t.Errorf("exit %d, stderr %q", code, errb.String())
		}
		if c := order(t); c[0] != "kill" {
			t.Errorf("calls %v: Close ran before the captures were killed", c)
		}
	})

	// At EOF Close may wait for a job to roll back; a signal then ends the
	// wait, and the captures are killed.
	t.Run("signal while closing", func(t *testing.T) {
		mu.Lock()
		calls = nil
		mu.Unlock()
		release := make(chan struct{})
		closeServer = func(s *perfmcp.Server) error {
			note("close")
			<-release
			return closeSrv(s)
		}
		t.Cleanup(func() {
			close(release)
			closeServer = func(s *perfmcp.Server) error { note("close"); return closeSrv(s) }
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan int, 1)
		go func() {
			done <- runContext(ctx, []string{"--profiles", t.TempDir()}, strings.NewReader(""), io.Discard, &lockedBuffer{})
		}()
		order(t)
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("exit %d", code)
			}
		case <-time.After(shutdownGrace):
			t.Fatal("a signal while Close waits did not end the run")
		}
		if c := seen(); len(c) < 2 || !slices.Equal(c[:2], []string{"close", "kill"}) {
			t.Errorf("calls %v: want Close under way, then the kill", c)
		}
	})

	t.Run("client gone", func(t *testing.T) {
		mu.Lock()
		calls = nil
		mu.Unlock()
		configHook = func(c *perfmcp.Config) { c.OutputQueueMessages = 2 }
		t.Cleanup(func() { configHook = nil })
		in, inW := io.Pipe()
		// Nothing ever reads the answers.
		outR, outW := io.Pipe()
		t.Cleanup(func() { _ = inW.Close(); _ = outR.Close() })
		go func() {
			_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`+"\n")
			for i := range 200 {
				if _, err := fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", i+1); err != nil {
					return
				}
			}
		}()
		errb := &lockedBuffer{}
		done := make(chan int, 1)
		go func() { done <- run([]string{"--profiles", t.TempDir()}, in, outW, errb) }()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("exit %d, stderr %q", code, errb.String())
			}
		case <-time.After(20 * time.Second):
			t.Fatal("run did not end with the client not reading")
		}
		if !strings.Contains(errb.String(), "client is not reading") {
			t.Errorf("stderr %q does not say the client is not reading", errb.String())
		}
		if c := order(t); c[0] != "kill" {
			t.Errorf("calls %v: Close ran before the captures were killed", c)
		}
	})
}

// session runs initialize, tools/list, open and render against a profiles
// root and an output root and returns stdout and stderr.
func session(t *testing.T) (stdout, stderr string, profiles, output string) {
	t.Helper()
	profiles, output = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(profiles, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := func(id int, method string, params any) string {
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	in := msg(1, "initialize", map[string]any{"protocolVersion": "2025-11-25"}) +
		msg(2, "tools/list", map[string]any{}) +
		msg(3, "tools/call", map[string]any{"name": "open", "arguments": map[string]any{"path": "cpu.pb.gz"}})
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", profiles, "--output", output}, strings.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	return out.String(), errb.String(), profiles, output
}

func TestRunServesOverStdio(t *testing.T) {
	stdout, _, profiles, output := session(t)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d responses, want 3:\n%s", len(lines), stdout)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	want := []string{"open", "top", "path", "sandwich", "gpu_kernels_for", "facts", "diff", "source", "render_flamegraph",
		"capture", "capture_wait", "capture_cancel", "capture_list"}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}

	var opened struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &opened); err != nil || opened.Result.IsError {
		t.Fatalf("open: %s (%v)", lines[2], err)
	}
	var o struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(opened.Result.Content[0].Text), &o); err != nil || o.Handle == "" {
		t.Fatalf("open result: %v", err)
	}

	// Render over a second run against the same roots: the page lands in
	// --output and nowhere else.
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "render_flamegraph",
		"arguments": map[string]any{
			"handle": o.Handle, "view": o.DefaultView, "out": "page.html", "explanations": []any{},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	in := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"open","arguments":{"path":"cpu.pb.gz"}}}` + "\n" + string(req) + "\n"
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", profiles, "--output", output}, strings.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(output, "page.html")); err != nil {
		t.Errorf("page not written: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(profiles, "page.html")); err == nil {
		t.Error("page written outside --output")
	}
}

func TestStdoutCarriesOnlyJSONRPC(t *testing.T) {
	stdout, _, _, _ := session(t)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, line := range lines {
		var m struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.JSONRPC != "2.0" {
			t.Errorf("stdout line is not JSON-RPC 2.0: %q (%v)", line, err)
		}
	}
}

// The command has no live page flags: the page is not served over stdio,
// so there is no address to choose and no way to switch it on.
func TestRunHasNoLiveFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--live"},
		{"--live", "--live-addr", "127.0.0.1:8080"},
		{"--live-addr", "0.0.0.0:0"},
	} {
		var out, errb bytes.Buffer
		code := run(append([]string{"--profiles", t.TempDir()}, args...), strings.NewReader(""), &out, &errb)
		if code != 2 || !strings.Contains(errb.String(), "live") {
			t.Errorf("%v: exit %d, stderr %q; want 2 and an unknown flag", args, code, errb.String())
		}
	}
}

// The stdio server registers no page tool: tools/list is the nine tools
// of the profile and render surface, and a page tool call is unknown.
func TestRunRegistersTheProfileAndCaptureTools(t *testing.T) {
	c := startRun(t, "--profiles", t.TempDir())
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.call("tools/list", map[string]any{}), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	want := []string{"open", "top", "path", "facts", "gpu_kernels_for", "sandwich", "diff", "source", "render_flamegraph",
		"capture", "capture_wait", "capture_cancel", "capture_list"}
	if !slices.Equal(names, want) {
		t.Errorf("tools/list has %v, want %v", names, want)
	}
	if raw := c.call("tools/call", map[string]any{"name": "page_open", "arguments": map[string]any{}}); len(raw) != 0 {
		t.Errorf("page_open answered with a result: %s", raw)
	}
}

// TestTheServerLinksNoProfiler keeps the profiler out of the binary.
func TestTheServerLinksNoProfiler(t *testing.T) {
	binarytest.LinksNoProfiler(t, ".")
}

// networkingPackages are the packages that open or accept connections.
// net/url and net/netip only parse addresses, and stay allowed.
var networkingPackages = []string{"net", "net/http", "crypto/tls", "net/textproto", "log/syslog"}

// pageServers are this repository's packages that serve the live page or
// MCP over HTTP: only perf-agent-hub links them.
var pageServers = []string{
	"github.com/dpsoft/perf-agent/internal/livehttp",
	"github.com/dpsoft/perf-agent/internal/mcphttp",
	"github.com/dpsoft/perf-agent/internal/perfpage",
}

// linksNetworking fails t for every networking package and page server
// that pkg links, through any import, on this platform.
func linksNetworking(t *testing.T, pkg string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", "{{.ImportPath}}", pkg).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "net/url") || !slices.Contains(deps, "github.com/dpsoft/perf-agent/internal/mcpstdio") {
		t.Fatalf("go list %s lists %d packages without net/url or mcpstdio: not the server's dependencies", pkg, len(deps))
	}
	for _, d := range deps {
		if slices.Contains(networkingPackages, d) || strings.HasPrefix(d, "net/http/") || slices.Contains(pageServers, d) {
			t.Errorf("%s links %s", pkg, d)
		}
	}
}

// The stdio server opens no socket: its binary links no networking
// package and none of the packages that serve the page or MCP over HTTP.
func TestTheServerLinksNoNetworking(t *testing.T) {
	linksNetworking(t, ".")
}

// perfmcp, which both binaries embed, links no networking: the page's
// routes live in internal/perfpage, which reads it through PageView.
func TestPerfmcpImportsNoNetworking(t *testing.T) {
	linksNetworking(t, "../../internal/perfmcp")
}

func TestTheServerIsNotShipped(t *testing.T) {
	binarytest.NotShipped(t, "perf-agent-mcp")
}

func TestRunValidatesResolvedLimits(t *testing.T) {
	for _, c := range []struct{ limit, field string }{
		{"MaxRows=0", "MaxRows"},
		{"MaxOpenHandles=-1", "MaxOpenHandles"},
		{"Model.MaxSamples=0", "Model.MaxSamples"},
		{"Model.MaxCommentBytes=-5", "Model.MaxCommentBytes"},
		{"Explain.MaxClaimsTotal=0", "Explain.MaxClaimsTotal"},
		{"Explain.MaxTitleBytes=0", "Explain.MaxTitleBytes"},
		{"Source.GitTimeout=0s", "Source.GitTimeout"},
	} {
		var out, errb bytes.Buffer
		code := run([]string{"--profiles", t.TempDir(), "--limit", c.limit}, strings.NewReader(""), &out, &errb)
		if code != 2 || !strings.Contains(errb.String(), c.field) {
			t.Errorf("%s: exit %d, stderr %q", c.limit, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", t.TempDir(), "--limit", "Source.MaxContext=0"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Errorf("Source.MaxContext=0: exit %d, stderr %q", code, errb.String())
	}
}

// Capture.TTL 0 means no age limit, so --limit takes it; the other
// capture limits must be positive, and a ceiling is the server's to refuse.
func TestRunAcceptsAZeroCaptureTTL(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", t.TempDir(), "--limit", "Capture.TTL=0s"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Errorf("Capture.TTL=0s: exit %d, stderr %q", code, errb.String())
	}
	for _, c := range []struct {
		limit, want string
		code        int
	}{
		{"Capture.TTL=-1s", "must not be negative", 2},
		{"Capture.StopGrace=0s", "Capture.StopGrace", 2},
		{"Capture.MaxStoredBytes=0", "Capture.MaxStoredBytes", 2},
		{"Capture.MaxDuration=6m", "5m0s", 1},
	} {
		errb.Reset()
		if code := run([]string{"--profiles", t.TempDir(), "--limit", c.limit}, strings.NewReader(""), &out, &errb); code != c.code || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%s: exit %d, stderr %q", c.limit, code, errb.String())
		}
	}
}

// Lowering an M2 limit alone still starts the server: the capture limits
// left at their defaults follow it.
func TestRunAcceptsLoweredM2LimitsAlone(t *testing.T) {
	for _, limit := range []string{"MaxDiffRows=10", "Model.MaxRawBytes=1048576", "Model.MaxSidecarBytes=1048576", "MaxResponseBytes=4096"} {
		var out, errb bytes.Buffer
		if code := run([]string{"--profiles", t.TempDir(), "--limit", limit}, strings.NewReader(""), &out, &errb); code != 0 {
			t.Errorf("%s: exit %d, stderr %q", limit, code, errb.String())
		}
	}
}

// A tiny MaxResponseBytes alone still starts the server, and the tail it
// lowered is named on stderr.
func TestRunAcceptsATinyResponseLimit(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", t.TempDir(), "--limit", "MaxResponseBytes=8"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "limit=Capture.MaxOutputTail default=2048 now=1") {
		t.Errorf("stderr %q does not name the lowered tail", errb.String())
	}
}

func TestRunSaysOutOfRangeLimits(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"--profiles", t.TempDir(), "--limit", "MaxRows=99999999999999999999"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "out of range") || strings.Contains(errb.String(), "not an integer") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

func TestLogsReachStderr(t *testing.T) {
	_, stderr, _, _ := session(t)
	if !strings.Contains(stderr, "msg=opened") {
		t.Errorf("stderr = %q", stderr)
	}
}

// stdioClient drives run over pipes.
type stdioClient struct {
	t      *testing.T
	w      io.WriteCloser
	r      *bufio.Reader
	nextID int
	done   chan int
	// lines are the stdout lines call read, in order; rest is what stdout
	// carried after them, once end has run.
	lines [][]byte
	rest  []byte
	exit  *int
}

func startRun(t *testing.T, args ...string) *stdioClient {
	t.Helper()
	return startRunLogging(t, io.Discard, args...)
}

// startRunLogging is startRun with run's stderr written to stderr.
func startRunLogging(t *testing.T, stderr io.Writer, args ...string) *stdioClient {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	c := &stdioClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), nextID: 1, done: make(chan int, 1)}
	go func() {
		code := run(args, sr, sw, stderr)
		// A run that ended early fails the next call instead of blocking it.
		_ = sr.Close()
		_ = sw.Close()
		c.done <- code
	}()
	t.Cleanup(func() {
		if code := c.end(); code != 0 {
			t.Errorf("exit %d", code)
		}
	})
	c.call("initialize", map[string]any{"protocolVersion": "2025-11-25"})
	return c
}

// end closes run's input, keeps what it still writes to stdout in rest,
// and returns its exit status. Later calls return the same status.
func (c *stdioClient) end() int {
	if c.exit != nil {
		return *c.exit
	}
	rest := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(c.r)
		rest <- b
	}()
	_ = c.w.Close()
	c.exit = new(<-c.done)
	c.rest = <-rest
	return *c.exit
}

// lockedBuffer is a bytes.Buffer that many goroutines may write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (c *stdioClient) call(method string, params any) json.RawMessage {
	c.t.Helper()
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	c.nextID++
	if _, err := c.w.Write(append(req, '\n')); err != nil {
		c.t.Fatal(err)
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		c.t.Fatal(err)
	}
	c.lines = append(c.lines, line)
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &res); err != nil {
		c.t.Fatalf("%s: %v", line, err)
	}
	return res.Result
}

// tool calls a tool and returns its text.
func (c *stdioClient) tool(name string, args any) string {
	c.t.Helper()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	raw := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Content) != 1 || res.IsError {
		c.t.Fatalf("%s: %s (%v)", name, raw, err)
	}
	return res.Content[0].Text
}

// sourceStatus runs the source tool on CPUBasic's node b against a git
// checkout holding src/app.c and returns the row's status and reason.
func sourceStatus(t *testing.T, extra ...string) (status, reason string) {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(src, "src", "app.c"), []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main", "-C", src}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	profiles := t.TempDir()
	if err := os.WriteFile(filepath.Join(profiles, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startRun(t, append([]string{"--profiles", profiles, "--source", src}, extra...)...)
	var o struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(c.tool("open", map[string]any{"path": "cpu.pb.gz"})), &o); err != nil {
		t.Fatal(err)
	}
	var top struct {
		Rows []struct{ Node, Name string } `json:"rows"`
	}
	if err := json.Unmarshal([]byte(c.tool("top", map[string]any{"handle": o.Handle, "view": o.DefaultView, "rows": "nodes", "limit": 20})), &top); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(top.Rows, func(r struct{ Node, Name string }) bool { return r.Name == "b" })
	if i < 0 {
		t.Fatalf("no node b in %+v", top.Rows)
	}
	var out struct {
		Rows []struct {
			Resolution struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"resolution"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(c.tool("source", map[string]any{"handle": o.Handle, "view": o.DefaultView, "node": top.Rows[i].Node})), &out); err != nil || len(out.Rows) != 1 {
		t.Fatalf("source: %+v (%v)", out, err)
	}
	return out.Rows[0].Resolution.Status, out.Rows[0].Resolution.Reason
}

func TestSourceGitIsOnByDefault(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if status, reason := sourceStatus(t); status != "user_selected" {
		t.Errorf("default: status %q (%s), want user_selected", status, reason)
	}
	status, reason := sourceStatus(t, "--source-git=false")
	if status != "source_unverified" || reason != "git probe disabled" {
		t.Errorf("--source-git=false: status %q reason %q", status, reason)
	}
}
