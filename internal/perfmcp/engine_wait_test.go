package perfmcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// mcpSession is one client session answered through the per-message API,
// as another transport answers it: several may share one server.
type mcpSession struct {
	tb     testing.TB
	s      *Server
	ss     *mcpstdio.Session
	nextID int
}

// newMCPSession initializes a session of s whose clientInfo.name is name.
func newMCPSession(tb testing.TB, s *Server, name string) *mcpSession {
	tb.Helper()
	c := &mcpSession{tb: tb, s: s, ss: mcpstdio.NewSession(), nextID: 1}
	params := map[string]any{"protocolVersion": "2025-11-25", "clientInfo": map[string]any{"name": name, "version": "1"}}
	var res struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(c.handle(tb.Context(), "initialize", params), &res); err != nil || res.Error != nil {
		tb.Fatalf("initialize %s: %s (%v)", name, res.Error, err)
	}
	return c
}

// handle answers one request of method with params.
func (c *mcpSession) handle(ctx context.Context, method string, params any) []byte {
	c.tb.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	if err != nil {
		c.tb.Fatal(err)
	}
	c.nextID++
	r, bad := c.s.mcp.Parse(b)
	if r == nil {
		c.tb.Fatalf("parse %s: %s", b, bad)
	}
	return c.s.mcp.Handle(ctx, c.ss, r)
}

// call runs tool with args and returns its text and isError. It may run
// on any goroutine: a failure is returned as the text.
func (c *mcpSession) call(ctx context.Context, tool string, args any) (string, bool) {
	var res struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	out := c.handle(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err := json.Unmarshal(out, &res); err != nil || res.Error != nil || len(res.Result.Content) != 1 {
		return fmt.Sprintf("not one tool result: %s (%v)", out, err), true
	}
	return res.Result.Content[0].Text, res.Result.IsError
}

// callOK runs tool, which must succeed, and decodes its text into v.
func (c *mcpSession) callOK(tool string, args, v any) string {
	c.tb.Helper()
	text, isErr := c.call(c.tb.Context(), tool, args)
	if isErr {
		c.tb.Fatalf("%s %v: %s", tool, args, text)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(text), v); err != nil {
			c.tb.Fatalf("%s: decode %s: %v", tool, text, err)
		}
	}
	return text
}

// BenchmarkTopThroughTheEngine is one top call through the per-message
// API, holding the engine: informative only. The allocation gate is
// TestTheUncontendedEngineAllocatesNothing.
func BenchmarkTopThroughTheEngine(b *testing.B) {
	cfg := Config{ProfilesDir: b.TempDir(), Limits: DefaultLimits(), Version: "test"}
	if err := os.WriteFile(filepath.Join(cfg.ProfilesDir, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		b.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	c := newMCPSession(b, s, "bench")
	var o openedAs
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	args := map[string]any{"handle": o.Handle, "view": o.DefaultView, "rows": "functions"}
	b.ReportAllocs()
	for b.Loop() {
		if text, isErr := c.call(b.Context(), "top", args); isErr {
			b.Fatal(text)
		}
	}
}

// engineHold is a HoldEngine that holds the engine inside the first top
// call until release is closed; held is closed once it does. ran lists
// every tool it was called for.
type engineHold struct {
	held, release chan struct{}
	once          sync.Once
	mu            sync.Mutex
	ran           []string
}

func newEngineHold() *engineHold {
	return &engineHold{held: make(chan struct{}), release: make(chan struct{})}
}

func (h *engineHold) hold(tool string) {
	h.mu.Lock()
	h.ran = append(h.ran, tool)
	h.mu.Unlock()
	if tool == "top" {
		h.once.Do(func() {
			close(h.held)
			<-h.release
		})
	}
}

func (h *engineHold) tools() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.ran)
}

// engineServer is a server over cpu.pb.gz whose engine h holds, with
// MaxEngineWait wait, named command; and one session of it named A with
// the profile open.
func engineServer(t *testing.T, h *engineHold, wait time.Duration, command string) (*Server, *mcpSession, openedAs) {
	t.Helper()
	cfg := testConfig(t)
	writeCPUFixtures(t, cfg)
	cfg.Limits.MaxEngineWait = wait
	cfg.HoldEngine = h.hold
	cfg.Command = command
	s := must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	a := newMCPSession(t, s, "A")
	var o openedAs
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	return s, a, o
}

// holdTop starts session a's top call, which holds the engine until h is
// released, and returns its answer's channel.
func holdTop(t *testing.T, h *engineHold, a *mcpSession, o openedAs) <-chan string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		text, isErr := a.call(context.Background(), "top", map[string]any{"handle": o.Handle, "view": o.DefaultView, "rows": "functions"})
		if isErr {
			text = "isError: " + text
		}
		done <- text
	}()
	select {
	case <-h.held:
	case <-time.After(10 * time.Second):
		t.Fatal("top never held the engine")
	}
	return done
}

// §D2.4: a tool call waits at most MaxEngineWait, and the answer names
// the holder: its tool, its session's label and how long it has run.
func TestABusyEngineAnswersWhoHoldsIt(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"", "perf-agent-mcp is busy with top from A (running 0s); call again"},
		{"perf-agent-hub", "perf-agent-hub is busy with top from A (running 0s); call again"},
	} {
		t.Run(cmp.Or(tc.command, "default"), func(t *testing.T) {
			h := newEngineHold()
			s, a, o := engineServer(t, h, 200*time.Millisecond, tc.command)
			done := holdTop(t, h, a, o)
			if got, ok := s.engine.holding(); !ok || got.tool != "top" || got.by != "A" || got.since.IsZero() {
				t.Errorf("holding() = %+v, %v", got, ok)
			}
			b := newMCPSession(t, s, "B")
			start := time.Now()
			text, isErr := b.call(t.Context(), "facts", map[string]any{"handle": o.Handle, "view": o.DefaultView, "node": "1"})
			took := time.Since(start)
			if !isErr || text != tc.want {
				t.Errorf("facts while top holds the engine: %q (isError %v), want %q", text, isErr, tc.want)
			}
			if took < 200*time.Millisecond || took > 400*time.Millisecond {
				t.Errorf("the busy answer took %v, want 200..400 ms", took)
			}
			close(h.release)
			if got := <-done; strings.HasPrefix(got, "isError") {
				t.Errorf("top: %s", got)
			}
			if got := h.tools(); slices.Contains(got, "facts") {
				t.Errorf("facts ran its handler: %v", got)
			}
			if _, ok := s.engine.holding(); ok {
				t.Error("the engine is held after every call answered")
			}
			// The engine is free again.
			if text, isErr := b.call(t.Context(), "top", map[string]any{"handle": o.Handle, "view": o.DefaultView, "rows": "functions"}); isErr {
				t.Errorf("top after release: %s", text)
			}
		})
	}
}

// A call whose context ends while it waits for the engine runs nothing,
// and leaves the engine as it found it.
func TestAWaiterWhoseContextEndsRunsNothing(t *testing.T) {
	h := newEngineHold()
	s, a, o := engineServer(t, h, 0, "")
	done := holdTop(t, h, a, o)
	b := newMCPSession(t, s, "B")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	text, isErr := b.call(ctx, "facts", map[string]any{"handle": o.Handle, "view": o.DefaultView, "node": "1"})
	if !isErr || !strings.Contains(text, "ended while it waited for the server's engine") {
		t.Errorf("a cancelled waiter: %q (isError %v)", text, isErr)
	}
	close(h.release)
	<-done
	if got := h.tools(); !slices.Equal(got, []string{"open", "top"}) {
		t.Errorf("HoldEngine ran for %v, want open and top only", got)
	}
	if _, ok := s.engine.holding(); ok {
		t.Error("the engine is held after every call answered")
	}
	if err := s.engine.acquire(t.Context(), holder{tool: "check"}); err != nil {
		t.Fatalf("the engine is not free: %v", err)
	}
	s.engine.release()
}

// The engine records its holder without allocating: the uncontended path
// of every tool call stays as cheap as it was.
func TestTheUncontendedEngineAllocatesNothing(t *testing.T) {
	e := newEngine()
	ctx := t.Context()
	h := holder{tool: "top", by: "claude-code"}
	if n := testing.AllocsPerRun(1000, func() {
		if err := e.acquire(ctx, h); err != nil {
			t.Fatal(err)
		}
		if got, ok := e.holding(); !ok || got.tool != "top" {
			t.Fatalf("holding() = %+v, %v", got, ok)
		}
		e.release()
	}); n != 0 {
		t.Errorf("acquire, holding and release allocate %v times, want 0", n)
	}
	if n := testing.AllocsPerRun(1000, func() {
		if err := e.acquireWithin(ctx, h, time.Minute); err != nil {
			t.Fatal(err)
		}
		e.release()
	}); n != 0 {
		t.Errorf("a bounded acquire of a free engine allocates %v times, want 0", n)
	}
	if _, ok := e.holding(); ok {
		t.Error("a released engine has a holder")
	}
}

// A capture job's engine sections hold the engine as "capture <id>", and
// maintenance as "capture retention", so a busy answer names them.
func TestCaptureJobsHoldTheEngineByName(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	record := func(s *Server) {
		h, ok := s.engine.holding()
		if !ok {
			h.tool = "nobody"
		}
		mu.Lock()
		seen = append(seen, h.tool)
		mu.Unlock()
	}
	e := newJobEnv(t, jobOpts{setup: func(s *Server) {
		fault := s.captureFault
		s.captureFault = func(p faultPoint) error {
			if p == faultInEngine {
				record(s)
			}
			return fault(p)
		}
		s.captureProtect = func() []fileKey {
			record(s)
			return nil
		}
	}})
	j, v := e.run(e.request(time.Second))
	if jobStatus(v.Status) != jobDone {
		t.Fatalf("job %s: %+v", j.id, v)
	}
	e.s.maintenance(true)
	mu.Lock()
	defer mu.Unlock()
	// The engine section runs before the capture has an id, so it holds
	// by the job's.
	if want := "capture " + v.Job; !slices.Contains(seen, want) {
		t.Errorf("the job's engine section held as %q, want %q", seen, want)
	}
	if !slices.Contains(seen, "capture retention") {
		t.Errorf("maintenance held as %q, want capture retention", seen)
	}
	if slices.Contains(seen, "nobody") {
		t.Errorf("the engine was held with no holder recorded: %q", seen)
	}
}
