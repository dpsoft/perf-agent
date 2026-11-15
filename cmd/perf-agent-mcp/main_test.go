package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

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

func TestStdoutCarriesOnlyJSONRPC(t *testing.T) {
	stdout, _, _, _ := session(t)
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		var m struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.JSONRPC != "2.0" {
			t.Errorf("stdout line is not JSON-RPC 2.0: %q (%v)", line, err)
		}
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
var networkingPackages = []string{"net", "net/http", "crypto/tls"}

// handlerImports are the networking imports each package besides
// internal/livehttp is allowed.
var handlerImports = map[string][]string{
	"github.com/dpsoft/perf-agent/internal/perfmcp": {"net/http"},
}

// clientSymbols are functions every outgoing connection or name lookup
// through the standard library runs.
var clientSymbols = []string{
	"net/http.(*Client).do",
	"net/http.(*Transport).roundTrip",
	"net/http.(*Transport).dialConn",
	"net.Dial",
	"net.DialTimeout",
	"net.(*Resolver).LookupHost",
	"net.(*Resolver).LookupIPAddr",
	"crypto/tls.(*Conn).clientHandshake",
}

// TestTheServerOpensNoConnection is a stopgap for the network scan that
// replaces it: only internal/livehttp imports a networking package (and
// internal/perfmcp net/http, for its handlers), and
// the linked binary has none of the client symbols. A call of
// (*net.Dialer).DialContext is linked by the server side too, so nm cannot
// tell it apart; the source scan and the socket test are still required.
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
	w      *io.PipeWriter
	r      *bufio.Reader
	nextID int
	done   chan int
}

func startRun(t *testing.T, args ...string) *stdioClient {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	c := &stdioClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), nextID: 1, done: make(chan int, 1)}
	go func() {
		code := run(args, sr, sw, io.Discard)
		_ = sw.Close()
		c.done <- code
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		if code := <-c.done; code != 0 {
			t.Errorf("exit %d", code)
		}
	})
	c.call("initialize", map[string]any{"protocolVersion": "2025-11-25"})
	return c
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
// reading its output and the server is blocked writing a response.
func TestSignalEndsAServerBlockedOnOutput(t *testing.T) {
	bin := buildServer(t)
	for _, c := range []struct {
		name    string
		sig     syscall.Signal
		blocked bool
		// errBlocked also leaves stderr full and unread.
		errBlocked bool
	}{
		{"SIGTERM blocked", syscall.SIGTERM, true, false},
		{"SIGINT blocked", syscall.SIGINT, true, false},
		{"SIGTERM drained", syscall.SIGTERM, false, false},
		{"SIGTERM stdout and stderr blocked", syscall.SIGTERM, true, true},
		{"SIGINT stdout and stderr blocked", syscall.SIGINT, true, true},
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
			cmd := exec.Command(bin, "--profiles", t.TempDir())
			cmd.Stdin, cmd.Stdout = inR, outW
			if c.errBlocked {
				errR, errW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = errR.Close() }()
				if _, err := unix.FcntlInt(errW.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
					t.Skipf("cannot shrink the pipe: %v", err)
				}
				if _, err := errW.Write(make([]byte, 4096)); err != nil {
					t.Fatal(err)
				}
				cmd.Stderr = errW
				defer func() { _ = errW.Close() }()
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = inR.Close()
			_ = outW.Close()
			t.Cleanup(func() { _ = cmd.Process.Kill() })

			r := bufio.NewReader(outR)
			_, _ = inW.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n")
			if _, err := r.ReadBytes('\n'); err != nil {
				t.Fatal(err)
			}
			_, _ = inW.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
			if !c.blocked {
				go func() { _, _ = io.Copy(io.Discard, r) }()
			}
			time.Sleep(500 * time.Millisecond)
			if err := cmd.Process.Signal(c.sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("exit: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the server is still running 3s after the signal")
			}
		})
	}
}
