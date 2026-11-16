package perfmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// waitAnswer is a capture_wait answer that is not an error: a state, or
// loading, or in_progress.
type waitAnswer struct {
	ID         string `json:"id"`
	Capture    string `json:"capture_id"`
	Status     string `json:"status"`
	RetryAfter string `json:"retry_after"`
	Next       string `json:"next"`
	Holder     string `json:"holder"`
}

func waitByID(t *testing.T, c *testClient, id string) (waitAnswer, string, bool) {
	t.Helper()
	text, isErr := c.call("capture_wait", map[string]any{"id": id, "timeout": "1s"})
	var a waitAnswer
	if !isErr {
		if err := json.Unmarshal([]byte(text), &a); err != nil {
			t.Fatalf("capture_wait %s: %s (%v)", id, text, err)
		}
	}
	return a, text, isErr
}

// Before the start-up recovery has run (its pass waits for the directory,
// here held by the test), a capture id with no manifest may be one
// recovery has yet to record: the wait answers loading, saying why and
// when to give up, never unknown_capture. Once the pass has run, the same
// id is unknown.
func TestAWaitBeforeTheStartPassAnswersLoading(t *testing.T) {
	profiles := t.TempDir()
	if err := os.Mkdir(filepath.Join(profiles, defaultStdioCaptures), 0o700); err != nil {
		t.Fatal(err)
	}
	e, c := newCapEnv(t, jobOpts{profiles: profiles, setup: func(s *Server) { s.dirMu <- struct{}{} }})
	a, text, isErr := waitByID(t, c, "adhoc/001")
	if isErr || a.Status != statusLoading || a.Next == "" || a.RetryAfter == "" {
		t.Fatalf("capture_wait before the start-up pass: %s", text)
	}
	e.s.unlockDir()
	select {
	case <-e.s.startPassDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the start-up pass never ran")
	}
	if _, text, isErr := waitByID(t, c, "adhoc/001"); !isErr || toolErrorOf(t, text).Code != string(capture.CodeUnknownCapture) {
		t.Fatalf("capture_wait after the start-up pass: %s", text)
	}
}

// A capture whose intent is on disk and whose manifest is not yet (another
// server's running capture) answers in_progress by capture id, naming the
// server that writes it, with a retry_after; once its manifest is there,
// the wait answers from it.
func TestAWaitForACaptureInProgressNamesItsHolder(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	<-e.s.startPassDone
	if !e.s.lockDir(nil) {
		t.Fatal("the server closed")
	}
	d, ce := e.s.openCaptureDir()
	if ce != nil {
		t.Fatal(ce)
	}
	defer d.close()
	ser, err := d.series("adhoc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	id, err := e.s.nextID(ser)
	if err != nil {
		t.Fatal(err)
	}
	st := capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"}
	txn, err := e.s.beginTxn(d, "adhoc", id, "run", toWire(st), capture.ExpectedFiles(st.Modes))
	if err != nil {
		t.Fatal(err)
	}
	key := captureKey("adhoc", formatID(id))
	a, text, isErr := waitByID(t, c, key)
	if isErr || a.Status != "in_progress" || a.Capture != key || a.Next == "" ||
		!strings.Contains(a.Holder, fmt.Sprintf("perf-agent-mcp pid %d", os.Getpid())) {
		t.Fatalf("capture_wait %s with its intent on disk: %s", key, text)
	}
	if !strings.Contains(a.Next, "another perf-agent-mcp is running this capture") || !strings.Contains(a.Next, "stop waiting") {
		t.Errorf("next %q does not say who runs it, or when to stop waiting", a.Next)
	}
	if d, err := time.ParseDuration(a.RetryAfter); err != nil || d < time.Second || d > 5*time.Second {
		t.Errorf("retry_after %q, want 1s to 5s", a.RetryAfter)
	}
	if err := txn.rollback(capture.Errorf(capture.CodeDisk, "", "the test ended it"), manifest{}); err != nil {
		t.Fatal(err)
	}
	e.s.unlockDir()
	e.s.historyChanged()
	if _, text, isErr := waitByID(t, c, key); !isErr || toolErrorOf(t, text).Code != string(capture.CodeDisk) {
		t.Fatalf("capture_wait %s once its failure manifest is written: %s", key, text)
	}
}

// The in_progress answer judges the holder by its server alone, says when
// a dead server's reaper still ends the capture, and always says when to
// stop asking: an answer that may repeat (an intent recovery never
// records, an owner stopped with a stale heartbeat) has an end.
func TestTheInProgressAnswerSaysWhoAndWhenToStop(t *testing.T) {
	in := intent{ServerPID: 41, ReaperPID: 42}
	for _, tc := range []struct {
		name           string
		server, reaper bool
		holder, next   string
	}{
		{"server alive", true, true, "perf-agent-mcp pid 41 (reaper pid 42)", "another perf-agent-mcp is running this capture"},
		{"server alive, reaper gone", true, false, "perf-agent-mcp pid 41 (reaper pid 42)", "another perf-agent-mcp is running this capture"},
		{"server gone, reaper alive", false, true, "perf-agent-mcp pid 41, which is gone; its reaper pid 42 still runs", "its reaper is ending it"},
		{"both gone", false, false, "perf-agent-mcp pid 41 (reaper pid 42), which is gone", "and its reaper are gone"},
	} {
		ip := &inProgressError{in: in, server: tc.server, reaper: tc.reaper, retry: time.Second, giveUp: "after 2m0s of waiting"}
		var a waitAnswer
		if err := json.Unmarshal([]byte(ip.answer("adhoc/001", "adhoc/001").Text), &a); err != nil {
			t.Fatal(err)
		}
		if a.Holder != tc.holder || !strings.Contains(a.Next, tc.next) {
			t.Errorf("%s: holder %q, next %q; want %q, and next with %q", tc.name, a.Holder, a.Next, tc.holder, tc.next)
		}
		if !strings.Contains(a.Next, "if it still answers in_progress after 2m0s of waiting, stop waiting: call capture_list and tell the user") {
			t.Errorf("%s: next %q has no give-up clause", tc.name, a.Next)
		}
		if !tc.server && strings.Contains(a.Next, "another perf-agent-mcp is running") {
			t.Errorf("%s: next %q says a gone server runs it", tc.name, a.Next)
		}
	}
}
