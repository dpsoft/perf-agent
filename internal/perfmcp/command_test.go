package perfmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// captureDisabledText is the capture tools' answer, and the start of
// their descriptions, on a stdio server started without a perf-agent.
const captureDisabledText = "DISABLED: start perf-agent-mcp with --perf-agent PATH (the setcap'd perf-agent binary) to enable capturing."

// familyName matches the literals that may name perf-agent-mcp: the
// binary's own default name and format ids (writtenBy, the cgroup
// prefix), the lock holder and the launcher's stderr prefix, which name
// the program family, another server running a capture, and a record no
// server of the family wrote. Every other text names the binary the agent
// talks to, through the server's command.
var familyName = regexp.MustCompile(`^perf-agent-mcp-?$|perf-agent-mcp pid |^perf-agent-mcp launcher: |another perf-agent-mcp is running|that perf-agent-mcp did not write`)

// Every text that tells the user to start or restart the server names the
// binary that runs it, Config.Command, so the hub never sends an agent to
// the stdio binary. The scan keeps a new advice string from naming the
// stdio binary by hand.
func TestCommandNamesTheBinaryInEveryAdvice(t *testing.T) {
	const hub = "perf-agent-hub"
	cfg := testConfig(t)
	cfg.Command = hub
	cfg.Capture.Owner = ownerHub
	s := must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })

	probe := &mcpSession{tb: t, s: s, ss: mcpstdio.NewSession(), nextID: 1}
	var res struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(probe.handle(t.Context(), "initialize", map[string]any{"protocolVersion": "2025-11-25"}), &res); err != nil {
		t.Fatal(err)
	}
	if res.Result.ServerInfo.Name != hub {
		t.Errorf("serverInfo.name = %q", res.Result.ServerInfo.Name)
	}
	if !strings.HasPrefix(res.Result.Instructions, hub+" answers") {
		t.Errorf("the initialize instructions do not name %s: %q", hub, res.Result.Instructions)
	}

	wantDisabled := "DISABLED: start perf-agent-hub with --perf-agent PATH (the setcap'd perf-agent binary) to enable capturing."
	if s.captureDisabled != wantDisabled {
		t.Errorf("captureDisabled = %q", s.captureDisabled)
	}
	if d := s.captureDescription("Capture."); d != wantDisabled+" Capture." {
		t.Errorf("a capture tool's description: %q", d)
	}

	s.ownerSeen.Store(new(ownerStdio))
	if e := s.ownerMismatch(); e == nil || !strings.Contains(e.Next, "start "+hub+" with --captures") {
		t.Errorf("owner_mismatch: %+v", e)
	}
	if e := s.ownerMismatch(); e == nil || !strings.Contains(e.Message, "belongs to a perf-agent-mcp server") || !strings.Contains(e.Message, "this one is "+hub) {
		t.Errorf("owner_mismatch's message: %+v", e)
	}
	stdio := newKindServer(t, t.TempDir(), ownerStdio, "")
	stdio.ownerSeen.Store(new(ownerHub))
	if e := stdio.ownerMismatch(); e == nil || !strings.Contains(e.Message, "belongs to a "+hub+" server") || !strings.Contains(e.Message, "this one is perf-agent-mcp") {
		t.Errorf("stdio's owner_mismatch message: %+v", e)
	}
	s.ownerSeen.Store(nil)
	d, e := s.openCaptureDir()
	if e != nil {
		t.Fatal(e)
	}
	defer d.close()
	if e := d.writeError(syscall.ENOSPC); !strings.Contains(e.Next, "start "+hub+" with --captures") {
		t.Errorf("a write error's next: %q", e.Next)
	}
	for _, code := range []capture.Code{capture.CodeDisabled, capture.CodePerfAgentUnstartable, capture.CodeBinaryChanged, capture.CodeClosing, capture.CodeDisk} {
		w := s.wire(capture.Errorf(code, "", "x"))
		if !strings.Contains(w.Next, hub) || strings.Contains(w.Next, "perf-agent-mcp") || strings.Contains(w.Next, capture.CommandMark) {
			t.Errorf("%s's default next: %q", code, w.Next)
		}
		if w := toWireError(capture.Errorf(code, "", "x")); !strings.Contains(w.Next, "perf-agent-mcp") || strings.Contains(w.Next, capture.CommandMark) {
			t.Errorf("%s's default next with the default command: %q", code, w.Next)
		}
	}

	// A frame over MaxPageBytes, on the hub's page.
	const depth, nameLen, budget = 300, 1000, 96 << 10
	frames := make([]pmfixture.Frame, depth)
	for i := range frames {
		p := fmt.Sprintf("f%04d_", i)
		frames[i] = pmfixture.Fn(p + strings.Repeat("n", nameLen-len(p)))
	}
	deep := newLiveServer(t, func(c *Config) { c.Limits.MaxPageBytes, c.Command = budget, hub },
		map[string][]byte{"deep.pb.gz": pmfixture.Build("cpu", "nanoseconds", nil, pmfixture.Sample{Frames: frames, Value: 10})})
	do := openLive(t, deep, "deep.pb.gz")
	do.facts(t, deep)
	bottom := fmt.Sprintf("f%04d_", depth-1)
	_, err := deep.PageSnapshot().Frame("p1", string(do.node(t, bottom+strings.Repeat("n", nameLen-len(bottom)))))
	if le, ok := errors.AsType[*PageLimitError](err); !ok || !strings.Contains(le.Msg, "restart "+hub+" with a larger --limit MaxPageBytes=N") {
		t.Errorf("a frame over MaxPageBytes: %v", err)
	}

	// No string literal of perfmcp or capture names the stdio binary
	// outside its family uses.
	var files []string
	for _, pattern := range []string{"*.go", "../capture/*.go"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
			}
			if strings.Contains(v, "perf-agent-mcp") && !familyName.MatchString(v) {
				t.Errorf("%s: %q names the stdio binary; name the server's command (capture.CommandMark in internal/capture)", fset.Position(lit.Pos()), v)
			}
			return true
		})
	}
	if scanned < 60 {
		t.Errorf("scanned %d files", scanned)
	}
}

// The hub writes its captures as owner kind hub, by default in
// captures-hub. No released build ever wrote kind serve, so an .owner
// naming it is one this server did not write.
func TestTheHubOwnerKind(t *testing.T) {
	profiles := t.TempDir()
	s := newKindServer(t, profiles, ownerHub, "")
	if got := s.captureDirName(); got != "captures-hub" {
		t.Errorf("the hub's default capture directory is %q, want captures-hub", got)
	}
	d, e := s.openCaptureDir()
	if e != nil {
		t.Fatal(e)
	}
	d.close()
	b, err := os.ReadFile(filepath.Join(profiles, "captures-hub", ownerFile))
	if err != nil || string(b) != `{"kind":"hub","format":1}`+"\n" {
		t.Errorf(".owner = %q (%v)", b, err)
	}

	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "captures-hub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(other, "captures-hub"), ownerFile, []byte(`{"kind":"serve","format":1}`+"\n"))
	_, e = newKindServer(t, other, ownerHub, "").openCaptureDir()
	if e == nil || e.Code != capture.CodeOwnerMismatch || !strings.Contains(e.Message, "did not write") {
		t.Errorf("an .owner of kind serve: %v", e)
	}
}

// Capture's own advice reaches the agent naming the hub too: the reasons
// a self-check disables capture, and a capture that fails because
// perf-agent changed.
func TestCaptureAdviceNamesTheHub(t *testing.T) {
	if !capture.Supported {
		t.Skip("capture needs Linux")
	}
	const hub = "perf-agent-hub"
	named := func(t *testing.T, what, text string) {
		t.Helper()
		if !strings.Contains(text, hub) || strings.Contains(text, "perf-agent-mcp") || strings.Contains(text, capture.CommandMark) {
			t.Errorf("%s does not name only %s: %q", what, hub, text)
		}
	}

	t.Run("self-check", func(t *testing.T) {
		perfCaps := capture.CapSet(1)<<capture.CapPerfmon | capture.CapSet(1)<<capture.CapBPF
		withSelfStatus(t, strings.Replace(cleanStatus, "Uid:\t1000\t1000", "Uid:\t1000\t0", 1), nil)
		cfg := testConfig(t)
		cfg.Command = hub
		cfg.Capture.PerfAgent = "/opt/perf-agent"
		cfg.Capture.Verify = fakeVerify(perfCaps)
		s := must(New(cfg))(t)
		defer func() { _ = s.Close() }()
		named(t, "the disabled reason", s.captureDisabled)
		if !strings.Contains(s.captureDisabled, "run perf-agent-hub as your own user") {
			t.Errorf("the disabled reason: %q", s.captureDisabled)
		}
		named(t, "the capture tool's description", s.captureDescription("Capture."))
		named(t, "a disabled answer", s.wire(s.disabledError()).Message)
	})

	t.Run("binary_changed", func(t *testing.T) {
		text := failedCapture(t, jobOpts{setup: func(s *Server) { s.cmd = hub }}, map[string]any{"duration": "1s"}, func(e *jobEnv) {
			b, err := os.ReadFile(e.fake)
			if err != nil {
				t.Fatal(err)
			}
			next := e.fake + ".new"
			if err := os.WriteFile(next, b, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(next, e.fake); err != nil {
				t.Fatal(err)
			}
		})
		w := toolErrorOf(t, text)
		if w.Code != string(capture.CodeBinaryChanged) {
			t.Fatalf("not binary_changed: %s", text)
		}
		named(t, "binary_changed's next", w.Next)
	})
}

// toWireError is e as the wire of a server running as perf-agent-mcp shows
// it, nil for nil.
func toWireError(e *capture.Error) *wireError { return wireErrorFor(e, defaultCommand) }
