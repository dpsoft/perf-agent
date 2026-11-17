package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/mcpflags/binarytest"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// The environment of a re-executed test binary: testMainEnv makes it run
// main(); the others set test seams through configHook.
const (
	testMainEnv = "PERF_AGENT_HUB_TEST_MAIN"
	// holdEnv names a path P: the tool named in P.tool, once it holds the
	// engine, creates P.held and waits until P.release exists.
	holdEnv = "PERF_AGENT_HUB_TEST_HOLD"
	// fakeVerifyEnv accepts any --perf-agent, such as the fake perf-agent,
	// which cannot be setcap'd.
	fakeVerifyEnv = "PERF_AGENT_HUB_TEST_FAKE_VERIFY"
	// nofileEnv sets the hub's RLIMIT_NOFILE, soft and hard, to that many
	// descriptors, so a test can run it out of them.
	nofileEnv = "PERF_AGENT_HUB_TEST_NOFILE"
)

// TestMain lets this test binary serve as a capture's reaper and launcher,
// as the command's own binary does, and as the command itself when a test
// runs it with testMainEnv.
func TestMain(m *testing.M) {
	if code, handled := capture.RunInternal(); handled {
		os.Exit(code)
	}
	if os.Getenv(testMainEnv) == "1" {
		if v := os.Getenv(nofileEnv); v != "" {
			n, err := strconv.ParseUint(v, 10, 64)
			if err == nil {
				err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: n, Max: n})
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s=%q: %v\n", nofileEnv, v, err)
				os.Exit(2)
			}
		}
		configHook = envConfigHook()
		main()
		return
	}
	code := capturetest.Main(m)
	if hubBinDir != "" {
		_ = os.RemoveAll(hubBinDir)
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
	if p := os.Getenv(holdEnv); p != "" {
		steps = append(steps, func(c *perfmcp.Config) { c.HoldEngine = holdFiles(p) })
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

// holdFiles is a HoldEngine that holds the engine across processes: the
// tool named in path.tool creates path.held once it holds the engine, and
// waits until path.release exists.
func holdFiles(path string) func(string) {
	return func(tool string) {
		want, err := os.ReadFile(path + ".tool")
		if err != nil || strings.TrimSpace(string(want)) != tool {
			return
		}
		_ = os.WriteFile(path+".held", nil, 0o600)
		for {
			if _, err := os.Stat(path + ".release"); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// fakeVerify accepts any file as a perf-agent without capabilities.
func fakeVerify(p string) (capture.Binary, error) {
	id, err := capture.IdentityOf(p)
	return capture.Binary{Path: p, ID: id, Version: "v0.0.0-test"}, err
}

// hubBinDir holds the test binary hubCommand runs.
var hubBinDir string

// hubTestBinary builds this package's test binary without the race
// detector, whose overhead would slow the shutdowns the tests time.
var hubTestBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "perf-agent-hub-test")
	if err != nil {
		return "", err
	}
	hubBinDir = dir
	bin := filepath.Join(dir, "hub.test")
	if out, err := exec.Command("go", "test", "-c", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("go test -c: %v\n%s", err, out)
	}
	return bin, nil
})

// hubCommand re-executes the test binary as perf-agent-hub with args,
// with env added to the test's environment. env or args must name the
// runtime directory, so no test ever touches the user's own.
func hubCommand(t testing.TB, env []string, args ...string) *exec.Cmd {
	t.Helper()
	if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "XDG_RUNTIME_DIR=") }) && !slices.Contains(args, "--runtime-dir") {
		t.Fatal("hubCommand: set XDG_RUNTIME_DIR or pass --runtime-dir")
	}
	exe, err := hubTestBinary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(append(os.Environ(), testMainEnv+"=1"), env...)
	return cmd
}

// runHub runs a short command of the hub, such as token or url, and
// returns its exit status, stdout and stderr.
func runHub(t testing.TB, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := hubCommand(t, env, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), out.String(), errb.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, out.String(), errb.String()
}

// freePort is a loopback port free a moment ago: a probe listener's,
// closed before it returns.
func freePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
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

// profilesDir is a profiles root holding cpu.pb.gz (CPUBasic).
func profilesDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// hubOptions configure startHub; every zero field has a default.
type hubOptions struct {
	env  []string
	args []string
	// profiles defaults to profilesDir; xdg to a new temporary directory;
	// port to freePort.
	profiles, xdg string
	port          int
	// stdout and stderr, when set, are the hub's; otherwise the test reads
	// both into buffers.
	stdout, stderr *os.File
	// started, when set, replaces the wait for the runtime files.
	started func(*hubProc)
}

// hubProc is a running perf-agent-hub.
type hubProc struct {
	t        testing.TB
	cmd      *exec.Cmd
	xdg, dir string
	profiles string
	port     int
	stdout   *lockedBuffer
	stderr   *lockedBuffer
	exited   chan struct{}
	waitErr  error
}

// startHub starts perf-agent-hub and waits until its runtime files are
// written, when it serves. The test's cleanup ends it with SIGTERM, if it
// still runs, and fails when it does not exit.
func startHub(t testing.TB, o hubOptions) *hubProc {
	t.Helper()
	if o.profiles == "" {
		o.profiles = profilesDir(t)
	}
	if o.xdg == "" {
		o.xdg = t.TempDir()
	}
	if o.port == 0 {
		o.port = freePort(t)
	}
	p := &hubProc{t: t, xdg: o.xdg, dir: filepath.Join(o.xdg, "perf-agent-mcp"), profiles: o.profiles, port: o.port,
		stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, exited: make(chan struct{})}
	args := append([]string{"--profiles", o.profiles, "--port", strconv.Itoa(o.port)}, o.args...)
	p.cmd = hubCommand(t, append([]string{"XDG_RUNTIME_DIR=" + o.xdg}, o.env...), args...)
	// The pipes the test reads are closed in this process once the hub
	// has its own copies.
	var mine []*os.File
	pipe := func(buf *lockedBuffer) *os.File {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		mine = append(mine, w)
		go func() {
			_, _ = io.Copy(buf, r)
			_ = r.Close()
		}()
		return w
	}
	stdout, stderr := o.stdout, o.stderr
	if stdout == nil {
		stdout = pipe(p.stdout)
	}
	if stderr == nil {
		stderr = pipe(p.stderr)
	}
	p.cmd.Stdout, p.cmd.Stderr = stdout, stderr
	err := p.cmd.Start()
	for _, w := range mine {
		_ = w.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-p.exited:
		case <-time.After(10 * time.Second):
			t.Error("perf-agent-hub did not exit at the end of the test")
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
		if t.Failed() {
			t.Logf("perf-agent-hub stderr:\n%s", p.stderr.String())
		}
	})
	if o.started != nil {
		o.started(p)
		return p
	}
	p.waitStarted()
	return p
}

// waitStarted waits until this hub's hub.json, the last runtime file, is
// written.
func (p *hubProc) waitStarted() {
	p.t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		// A dead server's hub.json may be there until this one replaces it.
		var info struct{ PID int }
		if b, err := os.ReadFile(filepath.Join(p.dir, "hub.json")); err == nil && json.Unmarshal(b, &info) == nil && info.PID == p.cmd.Process.Pid {
			return
		}
		select {
		case <-p.exited:
			p.t.Fatalf("perf-agent-hub exited (%v) before it served:\n%s", p.waitErr, p.stderr.String())
		default:
		}
	}
	p.t.Fatalf("perf-agent-hub wrote no hub.json in 30 s:\n%s", p.stderr.String())
}

// file is a runtime file's content.
func (p *hubProc) file(name string) string {
	p.t.Helper()
	b, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		p.t.Fatal(err)
	}
	return string(b)
}

func (p *hubProc) pageURL() string  { return strings.TrimSpace(p.file("page-url")) }
func (p *hubProc) mcpToken() string { return strings.TrimSpace(p.file("mcp-token")) }

// env is the environment that names p's runtime directory, for runHub.
func (p *hubProc) env() []string { return []string{"XDG_RUNTIME_DIR=" + p.xdg} }

// stop signals p and waits up to d for it to exit, failing otherwise. It
// returns how long the exit took.
func (p *hubProc) stop(sig syscall.Signal, d time.Duration) time.Duration {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.t.Fatal(err)
	}
	start := time.Now()
	select {
	case <-p.exited:
	case <-time.After(d):
		p.t.Fatalf("perf-agent-hub still runs %v after %v", d, sig)
	}
	took := time.Since(start)
	if p.waitErr != nil {
		p.t.Errorf("perf-agent-hub exit after %v: %v\n%s", sig, p.waitErr, p.stderr.String())
	}
	return took
}

// client is an MCP client of p, initialized with label.
func (p *hubProc) client(label string) *hubClient {
	p.t.Helper()
	c := newHubClient(p.t, p.port, p.mcpToken())
	c.initialize(label)
	return c
}

// runtimeFiles are the names in p's runtime directory.
func (p *hubProc) runtimeFiles() []string {
	p.t.Helper()
	ents, err := os.ReadDir(p.dir)
	if err != nil {
		p.t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestASecondInstanceIsRefused(t *testing.T) {
	first := startHub(t, hubOptions{})
	code, stdout, stderr := runHub(t, first.env(), "--profiles", first.profiles, "--port", strconv.Itoa(freePort(t)))
	want := fmt.Sprintf("perf-agent-hub is already running (pid %d, port %d)", first.cmd.Process.Pid, first.port)
	if code != 1 || !strings.Contains(stderr, want) || stdout != "" {
		t.Errorf("a second instance: exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	// The second instance changed nothing of the first's.
	if got := first.runtimeFiles(); !slices.Equal(got, []string{"hub.json", "hub.lock", "mcp-token", "page-url"}) {
		t.Errorf("runtime files after the refused start: %v", got)
	}
	if c := first.client("after"); c == nil {
		t.Error("the first instance stopped serving")
	}
}

func TestFilesAreWrittenAndRemoved(t *testing.T) {
	p := startHub(t, hubOptions{})
	if fi, err := os.Stat(p.dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("runtime directory: %v %v", fi, err)
	}
	for _, name := range []string{"hub.json", "mcp-token", "page-url", "hub.lock"} {
		fi, err := os.Stat(filepath.Join(p.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", name, fi.Mode().Perm())
		}
	}
	raw := p.file("hub.json")
	var info map[string]any
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("hub.json: %v\n%s", err, raw)
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range info {
			if !yield(k) {
				return
			}
		}
	})
	if !slices.Equal(keys, []string{"mcp_url", "pid", "port", "started", "version"}) {
		t.Errorf("hub.json keys %v", keys)
	}
	if info["pid"] != float64(p.cmd.Process.Pid) || info["port"] != float64(p.port) || info["mcp_url"] != fmt.Sprintf("http://127.0.0.1:%d/mcp", p.port) {
		t.Errorf("hub.json %s", raw)
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(info["started"])); err != nil {
		t.Errorf("hub.json started: %v", err)
	}
	pageTok := strings.TrimSuffix(strings.TrimPrefix(p.pageURL(), fmt.Sprintf("http://127.0.0.1:%d/s/", p.port)), "/")
	for _, tok := range []string{p.mcpToken(), pageTok} {
		if len(tok) != 43 || strings.Contains(raw, tok) {
			t.Errorf("token %q: hub.json %s", tok, raw)
		}
	}
	p.stop(syscall.SIGTERM, 3*time.Second)
	if got := p.runtimeFiles(); !slices.Equal(got, []string{"hub.lock"}) {
		t.Errorf("runtime files after SIGTERM: %v, want only hub.lock", got)
	}
}

// TestNoTokenEverReachesStderr runs a full session: initialize, tools,
// page reads, an inbox report and SIGTERM. Neither token is logged.
func TestNoTokenEverReachesStderr(t *testing.T) {
	p := startHub(t, hubOptions{})
	c := p.client("logger")
	o := c.open("cpu.pb.gz")
	pub := c.publish(o)
	page := p.pageURL()
	for _, route := range []string{"", "state", "assets/live.js", "pub/" + pub + "/model"} {
		if code, _ := httpGet(t, page+route); code != 200 {
			t.Errorf("GET %s: %d", route, code)
		}
	}
	if code := postReport(t, page, map[string]string{"kind": "ask", "publication": pub, "node": c.nodeID(o, "b")}); code != 204 {
		t.Errorf("inbox: %d", code)
	}
	c.tool("page_state", map[string]any{})
	// A wrong token in a request is not echoed either.
	wrong := newHubClient(t, p.port, strings.Repeat("A", 43))
	if status, _, _ := wrong.post("1", "ping", nil); status != 401 {
		t.Errorf("a wrong bearer: %d", status)
	}
	mcpTok := p.mcpToken()
	pageTok := strings.TrimSuffix(page[strings.LastIndex(page[:len(page)-1], "/")+1:], "/")
	p.stop(syscall.SIGTERM, 3*time.Second)
	out := p.stderr.String()
	if !strings.Contains(out, "listening") {
		t.Fatalf("stderr has no listening line:\n%s", out)
	}
	for _, tok := range []string{mcpTok, pageTok} {
		if strings.Contains(out, tok) {
			t.Errorf("token %s… reached stderr", tok[:6])
		}
	}
	if strings.Contains(p.stdout.String(), mcpTok) {
		t.Error("the MCP token reached stdout")
	}
}

func TestThePageURLGoesToStdoutOnly(t *testing.T) {
	p := startHub(t, hubOptions{})
	want := p.pageURL()
	for end := time.Now().Add(5 * time.Second); !strings.HasSuffix(p.stdout.String(), "\n") && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	p.client("x").tool("open", map[string]any{"path": "cpu.pb.gz"})
	p.stop(syscall.SIGTERM, 3*time.Second)
	if got := p.stdout.String(); got != want+"\n" {
		t.Errorf("stdout %q, want the page URL %q alone", got, want+"\n")
	}
	if strings.Contains(p.stderr.String(), want) {
		t.Error("the page URL is on stderr")
	}
}

// TestThePageURLIsLoopbackWithAToken: the page is served on 127.0.0.1 at
// the port given, under a 43-character token, and only there.
func TestThePageURLIsLoopbackWithAToken(t *testing.T) {
	p := startHub(t, hubOptions{})
	u := p.pageURL()
	re := regexp.MustCompile(`^http://127\.0\.0\.1:(\d+)/s/[A-Za-z0-9_-]{43}/$`)
	m := re.FindStringSubmatch(u)
	if m == nil || m[1] != strconv.Itoa(p.port) {
		t.Fatalf("page URL %q", u)
	}
	code, body := httpGet(t, u)
	if code != 200 || !bytes.HasPrefix(body, []byte("<!doctype html>")) {
		t.Errorf("GET the shell: %d %.80q", code, body)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	other := "http://" + parsed.Host + "/s/" + strings.Repeat("A", 43) + "/"
	if code, _ := httpGet(t, other); code != 404 {
		t.Errorf("another token: %d", code)
	}
	// The hub listens on 127.0.0.1 only, whatever other loopback address.
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.2:%d", p.port), time.Second); err == nil {
		_ = conn.Close()
		t.Error("the hub accepts connections on 127.0.0.2")
	}
	if !strings.Contains(p.stderr.String(), fmt.Sprintf("mcp_url=http://127.0.0.1:%d/mcp", p.port)) {
		t.Errorf("the listening line does not name the MCP URL:\n%s", p.stderr.String())
	}
}

func TestTokenAndURLSubcommands(t *testing.T) {
	p := startHub(t, hubOptions{})
	want := `{"Authorization":"Bearer ` + p.mcpToken() + `"}` + "\n"
	for _, args := range [][]string{
		{"token", "--port", strconv.Itoa(p.port)},
		{"token", "--url", fmt.Sprintf("http://127.0.0.1:%d/mcp", p.port)},
	} {
		code, stdout, stderr := runHub(t, p.env(), args...)
		if code != 0 || stdout != want || stderr != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	// The flag names the runtime directory as well as the environment.
	code, stdout, _ := runHub(t, []string{"XDG_RUNTIME_DIR="}, "token", "--port", strconv.Itoa(p.port), "--runtime-dir", p.dir)
	if code != 0 || stdout != want {
		t.Errorf("token --runtime-dir: exit %d, stdout %q", code, stdout)
	}
	code, stdout, stderr := runHub(t, p.env(), "url")
	if code != 0 || stdout != p.pageURL()+"\n" || stderr != "" {
		t.Errorf("url: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, args := range [][]string{
		{"token"},
		{"token", "--port", "0"},
		{"token", "--port", "65536"},
		{"token", "--port", "1", "--url", "http://127.0.0.1:1/mcp"},
		{"token", "--url", "127.0.0.1:7362"},
		{"token", "--url", fmt.Sprintf("http://127.0.0.1:%d/", p.port)},
		{"token", "--url", fmt.Sprintf("http://127.0.0.1:%d/other", p.port)},
		{"token", "--url", fmt.Sprintf("http://user:pw@127.0.0.1:%d/mcp", p.port)},
		{"token", "--url", fmt.Sprintf("http://127.0.0.1:0%d/mcp", p.port)},
		{"token", "--url", fmt.Sprintf("http://127.0.0.1:%d/mcp?x=1", p.port)},
		{"token", "--url", fmt.Sprintf("https://127.0.0.1:%d/mcp", p.port)},
		{"token", "extra"},
		{"url", "extra"},
	} {
		code, stdout, stderr := runHub(t, p.env(), args...)
		if code != 2 || stdout != "" || stderr == "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want a usage error", args, code, stdout, stderr)
		}
	}
}

// TestTokenAcrossTimeZones: hub.json is compared as written, so a hub
// started in a zone whose offset is not a whole hour is read alike from
// any zone.
func TestTokenAcrossTimeZones(t *testing.T) {
	p := startHub(t, hubOptions{env: []string{"TZ=Asia/Kolkata"}})
	if !strings.Contains(p.file("hub.json"), "+05:30") {
		t.Fatalf("hub.json was not written in Asia/Kolkata: %s", p.file("hub.json"))
	}
	want := `{"Authorization":"Bearer ` + p.mcpToken() + `"}` + "\n"
	for _, tz := range []string{"UTC", "Asia/Kolkata", "America/St_Johns"} {
		code, stdout, stderr := runHub(t, append(p.env(), "TZ="+tz), "token", "--port", strconv.Itoa(p.port))
		if code != 0 || stdout != want {
			t.Errorf("token with TZ=%s: exit %d, stdout %q, stderr %q", tz, code, stdout, stderr)
		}
	}
}

// TestTokenOnlyForTheServersPort: the client's config names a port the
// hub does not listen on, where another program may: the token is never
// printed for it, nor for a URL whose host is not 127.0.0.1.
func TestTokenOnlyForTheServersPort(t *testing.T) {
	p := startHub(t, hubOptions{})
	other := strconv.Itoa(p.port + 1)
	for _, args := range [][]string{
		{"token", "--port", other},
		{"token", "--url", "http://127.0.0.1:" + other + "/mcp"},
	} {
		code, stdout, stderr := runHub(t, p.env(), args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, strconv.Itoa(p.port)) || !strings.Contains(stderr, other) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 1, nothing on stdout, both ports named", args, code, stdout, stderr)
		}
	}
	for _, host := range []string{"localhost", "[::1]", "127.0.0.2", "example.com"} {
		u := fmt.Sprintf("http://%s:%d/mcp", host, p.port)
		code, stdout, stderr := runHub(t, p.env(), "token", "--url", u)
		if code == 0 || stdout != "" || !strings.Contains(stderr, "127.0.0.1") {
			t.Errorf("token --url %s: exit %d, stdout %q, stderr %q", u, code, stdout, stderr)
		}
	}
}

// TestTokenWithoutARunningServerPrintsNothing: a hub killed with SIGKILL
// left its files; with the lock free, the helper prints nothing, so a
// client sends no stale header.
func TestTokenWithoutARunningServerPrintsNothing(t *testing.T) {
	xdg := t.TempDir()
	dir := filepath.Join(xdg, "perf-agent-mcp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	stale := map[string]string{
		"hub.json":  fmt.Sprintf(`{"pid":999999,"port":%d,"mcp_url":"http://127.0.0.1:%d/mcp","started":"2026-11-16T00:00:00Z","version":"dev"}`, port, port),
		"mcp-token": strings.Repeat("A", 43) + "\n",
		"page-url":  fmt.Sprintf("http://127.0.0.1:%d/s/%s/\n", port, strings.Repeat("B", 43)),
		"hub.lock":  "",
	}
	for name, content := range stale {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"XDG_RUNTIME_DIR=" + xdg}
	for _, args := range [][]string{{"token", "--port", strconv.Itoa(port)}, {"url"}} {
		code, stdout, stderr := runHub(t, env, args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "not running") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	// No runtime directory at all reads the same.
	code, stdout, stderr := runHub(t, []string{"XDG_RUNTIME_DIR=" + t.TempDir()}, "token", "--port", strconv.Itoa(port))
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not running") {
		t.Errorf("no runtime directory: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestABusyPortNamesItAndTheFlag(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	xdg := t.TempDir()
	code, stdout, stderr := runHub(t, []string{"XDG_RUNTIME_DIR=" + xdg}, "--profiles", t.TempDir(), "--port", strconv.Itoa(port))
	want := fmt.Sprintf("port %d is in use; start with --port N", port)
	if code != 1 || stdout != "" || !strings.Contains(stderr, want) {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	// It left nothing but the lock.
	ents, err := os.ReadDir(filepath.Join(xdg, "perf-agent-mcp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "hub.lock" {
		t.Errorf("runtime files after a busy port: %v", ents)
	}
}

// TestAStaleHubJSONDoesNotBlockStart: the files of a server killed with
// SIGKILL, naming a dead pid, with the lock free, are replaced.
func TestAStaleHubJSONDoesNotBlockStart(t *testing.T) {
	xdg := t.TempDir()
	dir := filepath.Join(xdg, "perf-agent-mcp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stalePort := freePort(t)
	staleJSON := fmt.Sprintf(`{"pid":999999,"port":%d,"mcp_url":"http://127.0.0.1:%d/mcp","started":"2026-11-16T00:00:00Z","version":"dev"}`, stalePort, stalePort)
	for name, content := range map[string]string{"hub.json": staleJSON, "mcp-token": strings.Repeat("A", 43), "hub.lock": "999999"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := startHub(t, hubOptions{xdg: xdg})
	var info struct{ PID, Port int }
	if err := json.Unmarshal([]byte(p.file("hub.json")), &info); err != nil {
		t.Fatal(err)
	}
	if info.PID != p.cmd.Process.Pid || info.Port != p.port || p.mcpToken() == strings.Repeat("A", 43) {
		t.Errorf("hub.json %+v and the token were not replaced", info)
	}
	if c := p.client("after-stale"); c == nil {
		t.Error("no client")
	}
}

func TestHubHasNoAddressFlag(t *testing.T) {
	env := []string{"XDG_RUNTIME_DIR=" + t.TempDir()}
	if code, _, stderr := runHub(t, env, "--help"); code != 0 || !strings.Contains(stderr, "(default 7362)") {
		t.Errorf("--help: exit %d, the default port is not 7362:\n%s", code, stderr)
	}
	for _, flag := range []string{"--addr", "--host", "--listen", "--live"} {
		code, stdout, stderr := runHub(t, env, "--profiles", t.TempDir(), flag, "127.0.0.1:0")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "flag provided but not defined") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", flag, code, stdout, stderr)
		}
	}
	for _, port := range []string{"0", "65536", "-1", "x"} {
		code, _, stderr := runHub(t, env, "--profiles", t.TempDir(), "--port", port)
		if code != 2 || !strings.Contains(stderr, "-port") {
			t.Errorf("--port %s: exit %d, stderr %q", port, code, stderr)
		}
	}
}

func TestTheHubBinaryIsNotShipped(t *testing.T) {
	binarytest.NotShipped(t, "perf-agent-hub")
}

func TestTheHubBinaryLinksNoProfiler(t *testing.T) {
	binarytest.LinksNoProfiler(t, ".")
}

// httpGet GETs u as a page's script does and returns the status and body.
func httpGet(t testing.TB, u string) (int, []byte) {
	t.Helper()
	res, err := pageHTTP.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	return res.StatusCode, body
}

// postReport posts a report to the page's inbox as the page does, and
// returns the status.
func postReport(t testing.TB, page string, report map[string]string) int {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(page)
	if err != nil {
		t.Fatal(err)
	}
	req, err := newPageReport(u, b)
	if err != nil {
		t.Fatal(err)
	}
	res, err := pageHTTP.Do(req)
	if err != nil {
		t.Fatalf("POST inbox: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}
