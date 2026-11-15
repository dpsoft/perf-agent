package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// liveEnv makes a test binary run as the server with the live page engine,
// which the stdio command does not expose: the engine is exercised through
// the same run function, and a subprocess is this package's test binary, built without -race.
const liveEnv = "PERF_AGENT_MCP_TEST_LIVE"

func TestMain(m *testing.M) {
	if os.Getenv(liveEnv) == "1" {
		configHook = func(c *perfmcp.Config) { c.Live = true }
		main()
		return
	}
	code := m.Run()
	if liveBinDir != "" {
		_ = os.RemoveAll(liveBinDir)
	}
	os.Exit(code)
}

// liveBinDir holds the test binary liveCommand builds.
var liveBinDir string

// liveTestBinary builds this package's test binary without the race
// detector, whose overhead slows the shutdown the signal tests time.
var liveTestBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "perf-agent-mcp-live")
	if err != nil {
		return "", err
	}
	liveBinDir = dir
	bin := filepath.Join(dir, "live.test")
	if out, err := exec.Command("go", "test", "-c", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("go test -c: %v\n%s", err, out)
	}
	return bin, nil
})

// withLive runs the server of this test with the live page engine.
func withLive(t *testing.T) {
	t.Helper()
	configHook = func(c *perfmcp.Config) { c.Live = true }
	t.Cleanup(func() { configHook = nil })
}

// liveCommand is a subprocess running the server with the live page engine.
func liveCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := liveTestBinary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), liveEnv+"=1")
	return cmd
}

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
	want := []string{"open", "top", "path", "sandwich", "gpu_kernels_for", "facts", "diff", "source", "render_flamegraph"}
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

// liveSession runs a server with the live page over pipes: it opens
// cpu.pb.gz, starts the page, publishes the facts of its default view and
// steers the page to them, while a browser reads the shell, the model and
// the stream. It returns the ended client, its stderr and the page's URL.
func liveSession(t *testing.T) (c *stdioClient, stderr, pageURL string) {
	t.Helper()
	profiles, output := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(profiles, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		t.Fatal(err)
	}
	var errb lockedBuffer
	withLive(t)
	c = startRunLogging(t, &errb, "--profiles", profiles, "--output", output)
	c.call("tools/list", map[string]any{})
	pageURL, pub := c.openAndPublish()
	c.tool("page_show", map[string]any{"publication": pub})

	browser := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	defer browser.CloseIdleConnections()
	stream := browserGet(t, browser, pageURL+"events")
	if line, err := bufio.NewReader(stream.Body).ReadString('\n'); err != nil || line != "event: state\n" {
		t.Errorf("stream: %q (%v)", line, err)
	}
	_ = stream.Body.Close()
	for _, route := range []string{"", "pub/" + pub + "/model"} {
		res := browserGet(t, browser, pageURL+route)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	if code := c.end(); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	return c, errb.String(), pageURL
}

// pageAddr is the host:port of a page URL.
func pageAddr(t *testing.T, pageURL string) netip.AddrPort {
	t.Helper()
	u, err := url.Parse(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	ap, err := netip.ParseAddrPort(u.Host)
	if err != nil {
		t.Fatalf("page URL host %q: %v", u.Host, err)
	}
	return ap
}

// pageToken is the token segment of a page URL.
func pageToken(t *testing.T, pageURL string) string {
	t.Helper()
	_, after, ok := strings.Cut(pageURL, "/s/")
	tok, _, _ := strings.Cut(after, "/")
	if !ok || len(tok) < 32 {
		t.Fatalf("page URL %s has no token", redact(pageURL))
	}
	return tok
}

func TestStdoutCarriesOnlyJSONRPC(t *testing.T) {
	stdout, _, _, _ := session(t)
	live, stderr, pageURL := liveSession(t)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, l := range live.lines {
		lines = append(lines, strings.TrimSuffix(string(l), "\n"))
	}
	if len(live.rest) != 0 {
		t.Errorf("stdout carried %q after the last response", live.rest)
	}
	for _, line := range lines {
		var m struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.JSONRPC != "2.0" {
			t.Errorf("stdout line is not JSON-RPC 2.0: %q (%v)", line, err)
		}
	}
	if want := "addr=" + pageAddr(t, pageURL).String(); !strings.Contains(stderr, "live page listening") || !strings.Contains(stderr, want) {
		t.Errorf("stderr does not log the listening address %s:\n%s", want, stderr)
	}
	if strings.Contains(stderr, pageToken(t, pageURL)) {
		t.Error("stderr carries the page token")
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
func TestRunRegistersNoPageTools(t *testing.T) {
	c := startRun(t, "--profiles", t.TempDir())
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.call("tools/list", map[string]any{}), &list); err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Tools {
		if strings.HasPrefix(tl.Name, "page_") {
			t.Errorf("tools/list has %s", tl.Name)
		}
	}
	if raw := c.call("tools/call", map[string]any{"name": "page_open", "arguments": map[string]any{}}); len(raw) != 0 {
		t.Errorf("page_open answered with a result: %s", raw)
	}
}

// The live page engine, run through the same entry point with the page
// switched on by a test, listens on 127.0.0.1 and a port the kernel picks.
func TestLivePageListensOnLoopback(t *testing.T) {
	withLive(t)
	c := startRun(t, "--profiles", t.TempDir())
	var opened struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(c.tool("page_open", map[string]any{})), &opened); err != nil {
		t.Fatal(err)
	}
	if ap := pageAddr(t, opened.URL); ap.Addr() != netip.MustParseAddr("127.0.0.1") || ap.Port() == 0 {
		t.Errorf("the page listens on %v, want 127.0.0.1 and a port", ap)
	}
}

func TestRunServesTheLivePage(t *testing.T) {
	profiles := t.TempDir()
	if err := os.WriteFile(filepath.Join(profiles, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		t.Fatal(err)
	}
	withLive(t)
	c := startRun(t, "--profiles", profiles)
	pageURL, pub := c.openAndPublish()
	browser := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 10 * time.Second}
	for _, route := range []string{"", "pub/" + pub + "/model"} {
		res := browserGet(t, browser, pageURL+route)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	if code := c.end(); code != 0 {
		t.Fatalf("exit %d at the end of input", code)
	}
	addr := pageAddr(t, pageURL).String()
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("%s still accepts connections after run returned", addr)
	}
}

// TestTheServerLinksNoProfiler keeps the profiler out of the binary. net,
// net/http and crypto/tls are linked for the live page's loopback listener.
func TestTheServerLinksNoProfiler(t *testing.T) {
	out, err := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	forbidden := []string{"github.com/dpsoft/perf-agent/perfagent", "github.com/cilium/ebpf", "github.com/libbpf/blazesym/go"}
	allowed := []string{"github.com/dpsoft/perf-agent", "github.com/google/pprof"}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		path, std, _ := strings.Cut(line, " ")
		if slices.Contains(forbidden, path) {
			t.Errorf("the server links %s", path)
		}
		if std == "true" {
			continue
		}
		if !slices.ContainsFunc(allowed, func(m string) bool { return path == m || strings.HasPrefix(path, m+"/") }) {
			t.Errorf("the server links %s, outside %v", path, allowed)
		}
	}
}

// networkingPackages are the packages that open connections. Among the
// binary's own packages only internal/livehttp, the loopback listener, may
// import them, and internal/perfmcp may import net/http for the page's
// handlers and the server they run in.
var networkingPackages = []string{"net", "net/http", "crypto/tls", "net/textproto", "log/syslog"}

// handlerImports are the networking imports each package besides
// internal/livehttp is allowed.
var handlerImports = map[string][]string{
	"github.com/dpsoft/perf-agent/internal/perfmcp": {"net/http"},
}

// clientSymbols are functions every outgoing connection or name lookup
// through the standard library runs. crypto/tls's client handshake is not
// one of them: net/http's server links it too, since it wraps an accepted
// unencrypted HTTP/2 connection with tls.Client, so nm cannot tell it
// apart. Nor is (*net.Resolver).LookupIPAddr: the compiler always inlines
// it into its callers, so it never has a symbol, and the lookupIPAddr it
// calls is linked by the listener's address parsing too. The source scan
// (TestNoClientNetworkingInTheServer) refuses calls of it.
var clientSymbols = []string{
	"net/http.(*Client).do",
	"net/http.(*Transport).roundTrip",
	"net/http.(*Transport).dialConn",
	"net.Dial",
	"net.DialTimeout",
	"net.(*Resolver).LookupHost",
}

// liveSymbols are functions of the live page's server and handlers. The
// linker keeps only code a page tool can reach, so the client symbols'
// absence says something about the handlers only while these are linked.
// Each is called only through a func value or an interface, so the
// compiler cannot inline it away: an inlined function could be missing
// from the symbol table and still be reachable.
var liveSymbols = []string{
	"github.com/dpsoft/perf-agent/internal/perfmcp.(*Server).liveMux",
	"github.com/dpsoft/perf-agent/internal/perfmcp.(*Server).liveMux.func1",
	"github.com/dpsoft/perf-agent/internal/livehttp.(*Hub).ServeHTTP",
}

// TestTheServerOpensNoConnection is a layer under the source scan
// (TestNoClientNetworkingInTheServer) and the socket test
// (TestEverySocketIsLoopback): only internal/livehttp imports a networking
// package (and internal/perfmcp net/http, for its handlers), and the linked
// binary has none of the client symbols while it links the live page's
// handlers. It cannot replace either: a call of (*net.Dialer).DialContext,
// which tls.Dial makes too, is linked by the server side as well, so nm
// cannot tell it apart.
func TestTheServerOpensNoConnection(t *testing.T) {
	out, err := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", `{{if not .Standard}}{{.ImportPath}}: {{join .Imports " "}}{{end}}`, ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		path, imports, _ := strings.Cut(line, ": ")
		if path == "github.com/dpsoft/perf-agent/internal/livehttp" {
			continue
		}
		for imp := range strings.FieldsSeq(imports) {
			if slices.Contains(handlerImports[path], imp) {
				continue
			}
			if slices.Contains(networkingPackages, imp) || strings.HasPrefix(imp, "net/http/") {
				t.Errorf("%s imports %s; only internal/livehttp may", path, imp)
			}
		}
	}

	bin := filepath.Join(t.TempDir(), "perf-agent-mcp")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	syms, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatalf("go tool nm: %v", err)
	}
	linked := map[string]bool{}
	for line := range strings.SplitSeq(string(syms), "\n") {
		if f := strings.Fields(line); len(f) >= 3 {
			linked[f[len(f)-1]] = true
		}
	}
	for _, sym := range liveSymbols {
		if !linked[sym] {
			t.Errorf("the binary does not link %s: the live page's handlers are unreachable, so finding no client symbol proves nothing about them", sym)
		}
	}
	for _, sym := range clientSymbols {
		if linked[sym] {
			t.Errorf("the binary links %s, which opens a connection or looks a name up", sym)
		}
	}
}

func TestTheServerIsNotShipped(t *testing.T) {
	for _, f := range []string{"Makefile", "Dockerfile.agent", "Dockerfile.shim", ".github/workflows/release.yml", "scripts/third-party-notices.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if bytes.Contains(b, []byte("perf-agent-mcp")) {
			t.Errorf("%s mentions perf-agent-mcp: the server is source-only", f)
		}
	}
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

func TestEveryLimitFieldCanBeSetByFlag(t *testing.T) {
	l := perfmcp.DefaultLimits()
	fields := limitFields(&l)
	if len(fields) < 30 {
		t.Errorf("only %d settable limits", len(fields))
	}
	for name, f := range fields {
		switch k := f.Kind(); {
		case f.Type() == reflect.TypeFor[time.Duration](), k >= reflect.Int && k <= reflect.Int64:
		default:
			t.Errorf("%s has kind %v", name, k)
		}
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

// buildServer builds the binary into a temporary directory.
func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "perf-agent-mcp")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// A signal ends the server with status 0 even when the client has stopped
// reading its output and the server is blocked writing a response. With
// the live page listening, a browser holding a stream open and another
// not reading a model delay it by no more than the page's bounded close.
func TestSignalEndsAServerBlockedOnOutput(t *testing.T) {
	bin := buildServer(t)
	for _, c := range []struct {
		name    string
		sig     syscall.Signal
		blocked bool
		// errBlocked also leaves stderr full and unread.
		errBlocked bool
		// live starts the page first, with a stream and a stuck download.
		live bool
	}{
		{"SIGTERM blocked", syscall.SIGTERM, true, false, false},
		{"SIGINT blocked", syscall.SIGINT, true, false, false},
		{"SIGTERM drained", syscall.SIGTERM, false, false, false},
		{"SIGTERM stdout and stderr blocked", syscall.SIGTERM, true, true, false},
		{"SIGINT stdout and stderr blocked", syscall.SIGINT, true, true, false},
		{"live SIGTERM drained", syscall.SIGTERM, false, false, true},
		{"live SIGTERM stdout and stderr blocked", syscall.SIGTERM, true, true, true},
		{"live SIGINT stdout and stderr blocked", syscall.SIGINT, true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			outR, outW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = outR.Close() }()
			if _, err := unix.FcntlInt(outW.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
				t.Skipf("cannot shrink the pipe: %v", err)
			}
			inR, inW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = inW.Close() }()
			profiles := t.TempDir()
			args := []string{"--profiles", profiles}
			if c.live {
				if err := os.WriteFile(filepath.Join(profiles, "long.pb.gz"), longNames(8000, 1000), 0o644); err != nil {
					t.Fatal(err)
				}
				// Pages large enough for the 8 MiB model of long.pb.gz.
				args = append(args, "--limit", "MaxPageBytes=67108864", "--limit", "MaxPageNodes=1048576")
			}
			cmd := exec.Command(bin, args...)
			if c.live {
				cmd = liveCommand(t, args...)
			}
			cmd.Stdin, cmd.Stdout = inR, outW
			// blockStderr fills stderr and stops reading it.
			blockStderr := func() {}
			if c.errBlocked {
				errR, errW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = errR.Close() }()
				if _, err := unix.FcntlInt(errW.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
					t.Skipf("cannot shrink the pipe: %v", err)
				}
				cmd.Stderr = errW
				defer func() { _ = errW.Close() }()
				// Until the page is set up, stderr is read: the server logs
				// while it opens the profile and starts the page.
				drained := make(chan struct{})
				go func() {
					defer close(drained)
					_, _ = io.Copy(io.Discard, errR)
				}()
				// errW is in blocking mode once exec has taken its descriptor,
				// so the write that fills the pipe runs on its own: it returns
				// when the pipe has room for all of it, or when errR closes.
				blockStderr = func() {
					_ = errR.SetReadDeadline(time.Now())
					<-drained
					go func() { _, _ = errW.Write(make([]byte, 4096)) }()
				}
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = inR.Close()
			_ = outW.Close()
			t.Cleanup(func() { _ = cmd.Process.Kill() })

			r := bufio.NewReader(outR)
			id := 0
			send := func(method string, params any) {
				t.Helper()
				id++
				b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := inW.Write(append(b, '\n')); err != nil {
					t.Fatal(err)
				}
			}
			// tool calls a tool and returns its text.
			tool := func(name string, args any) string {
				t.Helper()
				send("tools/call", map[string]any{"name": name, "arguments": args})
				line, err := r.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				var res struct {
					Result struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
						IsError bool `json:"isError"`
					} `json:"result"`
				}
				if err := json.Unmarshal(line, &res); err != nil || res.Result.IsError || len(res.Result.Content) != 1 {
					t.Fatalf("%s: %s (%v)", name, line, err)
				}
				return res.Result.Content[0].Text
			}
			send("initialize", map[string]any{"protocolVersion": "2025-11-25"})
			if _, err := r.ReadBytes('\n'); err != nil {
				t.Fatal(err)
			}
			if c.live {
				startPage(t, tool)
			}
			blockStderr()
			send("tools/list", map[string]any{})
			if !c.blocked {
				go func() { _, _ = io.Copy(io.Discard, r) }()
			}
			time.Sleep(500 * time.Millisecond)
			if err := cmd.Process.Signal(c.sig); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			budget := 3 * time.Second
			if c.live {
				budget = 2 * time.Second
			}
			select {
			case err := <-done:
				took := time.Since(start)
				if err != nil {
					t.Errorf("exit: %v", err)
				}
				// Drained, the server closes the page in full, and its
				// bounded wait for the stuck download shows the download
				// really was stuck.
				if c.live && !c.blocked && took < 400*time.Millisecond {
					t.Errorf("exited %v after the signal: the download was not stuck", took)
				}
				t.Logf("exited %v after the signal", took.Round(time.Millisecond))
			case <-time.After(budget):
				t.Fatalf("the server is still running %v after the signal", budget)
			}
		})
	}
}

// longNames is cpu/nanoseconds with one stack of depth frames, each a
// function with its own name of about nameLen bytes: every frame is as
// wide as the view, so a page draws them all.
func longNames(depth, nameLen int) []byte {
	frames := make([]pmfixture.Frame, depth)
	for i := range frames {
		frames[i] = pmfixture.Fn(fmt.Sprintf("f%05d_", i) + strings.Repeat("x", nameLen))
	}
	return pmfixture.Build("cpu", "nanoseconds", nil, pmfixture.Sample{Frames: frames, Value: 1})
}

// startPage opens long.pb.gz through tool, publishes its default view's
// facts on the live page, and leaves two browser connections open: a
// stream read up to its first state, and a download of the model, far
// larger than the socket buffers, that is never read.
func startPage(t *testing.T, tool func(name string, args any) string) {
	t.Helper()
	var o struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(tool("open", map[string]any{"path": "long.pb.gz"})), &o); err != nil {
		t.Fatal(err)
	}
	var pub struct {
		Publication string `json:"publication"`
		URL         string `json:"url"`
	}
	text := tool("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}})
	if err := json.Unmarshal([]byte(text), &pub); err != nil || pub.URL == "" {
		t.Fatalf("page_explain: %s (%v)", text, err)
	}
	u, err := url.Parse(pub.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A small receive buffer keeps the unread download from ending inside
	// the kernel.
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
		}); err != nil {
			return err
		}
		return serr
	}}
	request := func(route string) net.Conn {
		t.Helper()
		conn, err := d.Dial("tcp", u.Host)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := fmt.Fprintf(conn, "GET %s%s HTTP/1.1\r\nHost: %s\r\n\r\n", u.Path, route, u.Host); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	stream := request("events")
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(stream)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if line == "event: state\n" {
			break
		}
	}
	request("pub/" + pub.Publication + "/model")
}
