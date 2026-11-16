package perfmcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// syncBuffer is a log destination several goroutines write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// captureWriteOps are the named writes of a capture, as the writeErr seam
// sees them.
var captureWriteOps = []string{
	"seq", "fsync seq", "seq rename", "intent", "fsync intent", "heartbeat", "remove intent temp", "remove heartbeat temp",
	"intent rewrite", "staging", "probe", "fsync file", "fsync links", "manifest temp", "manifest link", "fsync manifest",
	"remove staging", "remove heartbeat", "remove intent", "fsync cleanup",
}

// No text an agent or a log reader sees names an owner token's prefix (the
// name of a temporary) or an absolute path: for a failure at every fault
// point and every named write, the job's wire error, its result (but the
// perf-agent path §4.3 shows), its failure manifest's error and every log
// line from Info up are scanned. Debug lines carry the children's own
// output and are left out.
func TestNoTokenOrAbsolutePathReachesAnAgent(t *testing.T) {
	type failure struct {
		name string
		arm  func(*Server)
	}
	var cases []failure
	for _, f := range faultPoints {
		p := f.p
		cases = append(cases, failure{"fault " + f.name, func(s *Server) { s.captureFault = inject(shot{p, 1, errInjected}) }})
	}
	for _, op := range captureWriteOps {
		cases = append(cases, failure{"write " + op, func(s *Server) { s.writeErr = failOpAt(op, 1, syscall.ENOSPC) }})
		// An error with no errno: its own text reaches the message.
		cases = append(cases, failure{"write " + op + " without an errno", func(s *Server) { s.writeErr = failOpAt(op, 1, errInjected) }})
	}
	cases = append(cases, failure{"a .seq this server did not write", func(s *Server) {
		ser := filepath.Join(s.profiles.Name(), defaultStdioCaptures, "adhoc")
		if err := os.MkdirAll(ser, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ser, seqFile), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
	}})
	cases = append(cases, failure{"manifest link EEXIST", func(s *Server) {
		s.link = func(r *os.Root, oldname, newname string) error {
			if strings.HasSuffix(newname, manifestSuffix) {
				return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
			}
			return r.Link(oldname, newname)
		}
	}})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := &syncBuffer{}
			e := newJobEnv(t, jobOpts{setup: func(s *Server) {
				s.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
				c.arm(s)
			}})
			req := e.request(300 * time.Millisecond)
			j, _ := e.run(req)
			texts := map[string]string{"logs": logs.String()}
			if ce := jobErr(j); ce != nil {
				texts["wire error"] = string(must(json.Marshal(toWireError(ce)))(t))
				// A message and its advice are one line each.
				if strings.ContainsAny(ce.Message+ce.Next, "\n\r") {
					t.Errorf("a line break in the wire error: %q / %q", ce.Message, ce.Next)
				}
			}
			j.mu.Lock()
			b := j.result
			j.mu.Unlock()
			if len(b) > 0 {
				// §4.3 names the perf-agent binary by its path; nothing
				// else in a result may hold one.
				var r captureResult
				if err := json.Unmarshal(b, &r); err != nil {
					t.Fatal(err)
				}
				r.Capture.PerfAgent.Path = ""
				texts["result"] = string(must(json.Marshal(r))(t))
			}
			tokens := e.tokens()
			if m, ok := e.manifestOf("adhoc", "001"); ok {
				texts["manifest error"] = string(must(json.Marshal(m.Error))(t))
				if m.Token != "" {
					tokens = append(tokens, m.Token)
				}
			}
			forbidden := []string{e.profiles, filepath.Dir(e.fake), "<nil>"}
			for _, tok := range tokens {
				forbidden = append(forbidden, tok[:16])
			}
			for where, text := range texts {
				for _, f := range forbidden {
					if strings.Contains(text, f) {
						t.Errorf("%s holds %q:\n%s", where, f, text)
					}
				}
			}
		})
	}
}

// tokens are the owner tokens of every intent and heartbeat left in the
// series.
func (e *jobEnv) tokens() []string {
	d, ok := e.dir()
	if !ok {
		return nil
	}
	defer d.close()
	ser, err := d.openSeries("adhoc")
	if err != nil {
		return nil
	}
	defer func() { _ = ser.Close() }()
	var toks []string
	names, _ := readDirNames(ser, ".")
	for _, n := range names {
		b, _, err := readSmall(ser, n, maxManifestBytes)
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(n, intentSuffix):
			var in intent
			if json.Unmarshal(b, &in) == nil && len(in.Token) >= 16 {
				toks = append(toks, in.Token)
			}
		case strings.HasSuffix(n, heartbeatSuffix) && len(b) > 16:
			toks = append(toks, strings.TrimSpace(string(b)))
		}
	}
	return toks
}

// The intent and the lock file record the capture's reaper while it runs,
// so recovery and a busy answer see it.
func TestTheReaperIsRecorded(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	j := e.start(e.request(time.Minute))
	if !eventuallyTrue(30*time.Second, func() bool { return j.view().Phase == capture.PhaseProfiling }) {
		t.Fatalf("never profiling: %+v", j.view())
	}
	e.noteRecord()
	d, ok := e.dir()
	if !ok {
		t.Fatal("no capture directory")
	}
	defer d.close()
	ser, err := d.openSeries("adhoc")
	if err != nil {
		t.Fatal(err)
	}
	in, _, err := readIntent(ser, intentName(1, "run"))
	_ = ser.Close()
	if err != nil {
		t.Fatal(err)
	}
	if in.ReaperPID <= 0 || in.ReaperStart == 0 || !capture.Alive(capture.DefaultProc(), in.ReaperPID, in.ReaperStart) {
		t.Errorf("intent reaper %d/%d", in.ReaperPID, in.ReaperStart)
	}
	b, err := os.ReadFile(filepath.Join(e.profiles, defaultStdioCaptures, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	var h capture.Holder
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.ReaperPID != in.ReaperPID || h.ReaperStart != in.ReaperStart || h.CaptureID != "adhoc/001" {
		t.Errorf("lock holder %+v, intent reaper %d/%d", h, in.ReaperPID, in.ReaperStart)
	}
	j.requestCancel()
	e.finish(j)
}

// A compare of two modes derives with two open handles: each mode's pair
// is opened and diffed before the next mode's, so the handles one mode
// needs are never evicted by the next's.
func TestCompareWithFewHandles(t *testing.T) {
	e := newJobEnv(t, jobOpts{limits: func(l *Limits) { l.MaxOpenHandles = 2 }})
	st := capture.Settings{Modes: []capture.Mode{capture.ModeCPU, capture.ModeOffCPU}, Duration: 300 * time.Millisecond, SampleRate: 99, Unwind: "auto"}
	e.commitSynthetic("adhoc", "base", st, manifest{})
	j, v := e.run(e.compareRequest("001"))
	r := e.result(j)
	if v.Derived != derivedReady || r.Compare == nil || len(r.Compare.Views) != 2 {
		t.Fatalf("view %+v, derive error %q, compare %+v", v, r.DeriveError, r.Compare)
	}
}

// After Close a capture is refused as the server closing, which is not
// retryable and is not "disabled"; a second Close does nothing.
func TestStartJobAfterCloseAndCloseTwice(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	if err := e.s.Close(); err != nil {
		t.Fatal(err)
	}
	_, ce := e.s.startJob(e.request(300 * time.Millisecond))
	if ce == nil || ce.Code != capture.CodeClosing || ce.Retryable {
		t.Fatalf("startJob after Close: %v", ce)
	}
	if err := e.s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A settled failure is rebuilt with what the agent acts on: its
// retryability, when to retry, what changed and who survived.
func TestErrorFromWireKeepsWhatAnAgentActsOn(t *testing.T) {
	e := &capture.Error{Code: capture.CodeStuckChild, Message: "m", Next: "n", Changed: "uid",
		Survivors: []capture.Survivor{{PID: 7, Comm: "c", StartTime: 9, State: "D"}}}
	got := errorFromWire(toWireError(e))
	if got.Code != e.Code || got.Message != "m" || got.Next != "n" || got.Changed != "uid" || got.Retryable ||
		len(got.Survivors) != 1 || got.Survivors[0] != e.Survivors[0] {
		t.Errorf("%+v", got)
	}
	b := capture.Errorf(capture.CodeBusy, "", "x")
	b.RetryAfter = 3 * time.Second
	if got := errorFromWire(toWireError(b)); !got.Retryable || got.RetryAfter != 3*time.Second {
		t.Errorf("%+v", got)
	}
	// A capability failure is not retryable, unlike perf_agent_failed by
	// default.
	f := capture.Errorf(capture.CodePerfAgentFailed, capture.SetcapLine, "missing capabilities")
	f.Retryable = false
	if got := errorFromWire(toWireError(f)); got.Retryable {
		t.Errorf("%+v", got)
	}
	if errorFromWire(nil) != nil {
		t.Error("nil is not nil")
	}
}

// A .seq this server did not write is refused with advice about the .seq,
// not about free space.
func TestAForeignSeqIsRefusedWithItsOwnAdvice(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	if _, v := e.run(e.request(300 * time.Millisecond)); v.Status != string(jobDone) {
		t.Fatalf("first %+v", v)
	}
	if err := os.WriteFile(filepath.Join(e.profiles, defaultStdioCaptures, "adhoc", seqFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, _ := e.run(e.request(300 * time.Millisecond))
	ce := jobErr(j)
	if ce == nil || strings.Contains(ce.Next, "free space") || !strings.Contains(ce.Next, ".seq") ||
		!strings.Contains(ce.Message, "series adhoc") || strings.Contains(ce.Message, e.profiles) {
		t.Fatalf("err %+v", ce)
	}
}

// A .seq that cannot be read, or is too large to be this server's, is
// refused naming the .seq, with the advice to move it away.
func TestAnUnreadableSeqIsRefusedWithItsOwnAdvice(t *testing.T) {
	capturetest.SkipIfPrivileged(t)
	for _, c := range []struct {
		name string
		set  func(string) error
	}{
		{"too large", func(p string) error { return os.WriteFile(p, bytes.Repeat([]byte("x"), 5000), 0o600) }},
		{"unreadable", func(p string) error { return os.Chmod(p, 0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newJobEnv(t, jobOpts{})
			if _, v := e.run(e.request(300 * time.Millisecond)); v.Status != string(jobDone) {
				t.Fatalf("first %+v", v)
			}
			seq := filepath.Join(e.profiles, defaultStdioCaptures, "adhoc", seqFile)
			if err := c.set(seq); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(seq, 0o600) })
			j, _ := e.run(e.request(300 * time.Millisecond))
			ce := jobErr(j)
			if ce == nil || !strings.Contains(ce.Message, ".seq of series adhoc") || !strings.Contains(ce.Next, "move captures/adhoc/.seq away") ||
				strings.Contains(ce.Next, "free space") || strings.Contains(ce.Message+ce.Next, e.profiles) {
				t.Fatalf("err %+v", ce)
			}
		})
	}
}
