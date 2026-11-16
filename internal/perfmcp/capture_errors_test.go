package perfmcp

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// perfAgentPath matches the fake perf-agent's path.
var perfAgentPath = regexp.MustCompile(`/\S*/fakeperfagent`)

// failedCapture runs a capture of args that must be accepted on a server
// configured by o, and returns the error its wait ends with.
func failedCapture(t *testing.T, o jobOpts, args map[string]any, prepare func(*jobEnv)) string {
	t.Helper()
	e, c := newCapEnv(t, o)
	if prepare != nil {
		prepare(e)
	}
	if args["command"] == nil && args["pid"] == nil {
		args["command"] = workloadCmd(t)
	}
	text, isErr := waitEnd(t, c, startCapture(t, c, args))
	e.noteRecord()
	if !isErr {
		t.Fatalf("the capture did not fail: %s", text)
	}
	return text
}

// injected is a capture that fails at its staging step with code, as the
// capture itself would report it: for the failures only a privileged run,
// a GPU or a stuck kernel can produce.
func injected(code capture.Code) func(t *testing.T) string {
	return func(t *testing.T) string {
		return failedCapture(t, jobOpts{}, map[string]any{"duration": "1s"}, func(e *jobEnv) {
			e.faults.fail(faultAfterStaging, capture.Errorf(code, "what to do", "the capture failed with %s", code))
		})
	}
}

// refused is the synchronous error of a capture call.
func refused(t *testing.T, c *testClient, tool string, args map[string]any) string {
	t.Helper()
	text, isErr := c.call(tool, args)
	if !isErr {
		t.Fatalf("%s answered %s", tool, text)
	}
	return text
}

// Every code of §4.4 reaches an agent through the tools at least once, as
// one JSON object with the documented retryable, a retry_after exactly
// when it is retryable, a one-line message, and no path of the server's.
func TestEveryErrorCodeIsProducedOnce(t *testing.T) {
	cases := map[capture.Code]func(t *testing.T) string{
		capture.CodeDisabled: func(t *testing.T) string {
			return refused(t, newTestServer(t, testConfig(t)), "capture", map[string]any{"pid": 1})
		},
		capture.CodeInvalidArgument: func(t *testing.T) string {
			_, c := newCapEnv(t, jobOpts{})
			return refused(t, c, "capture", map[string]any{"pid": 0})
		},
		capture.CodeIncomparable: func(t *testing.T) string {
			return failedCapture(t, jobOpts{}, map[string]any{"compare_with": "001", "duration": "2s"}, func(e *jobEnv) {
				e.commitSynthetic("adhoc", "base", capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"}, manifest{})
			})
		},
		capture.CodeUnknownBaseline: func(t *testing.T) string {
			return failedCapture(t, jobOpts{}, map[string]any{"compare_with": "009"}, nil)
		},
		capture.CodeBusy: func(t *testing.T) string {
			_, c := newCapEnv(t, jobOpts{})
			id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
			text := refused(t, c, "capture", map[string]any{"command": workloadCmd(t)})
			c.callOK("capture_cancel", map[string]any{"id": id}, nil)
			waitEnd(t, c, id)
			return text
		},
		capture.CodeUnknownCapture: func(t *testing.T) string {
			_, c := newCapEnv(t, jobOpts{})
			return refused(t, c, "capture_wait", map[string]any{"id": "j7-0000"})
		},
		capture.CodeTargetChanged: func(t *testing.T) string {
			w := capturetest.Workload(t)
			return failedCapture(t, jobOpts{}, map[string]any{"command": []any{w, "drop-dumpable-after", "500ms", "60s"}, "duration": "5s"}, nil)
		},
		capture.CodeOwnerMismatch: func(t *testing.T) string {
			return failedCapture(t, jobOpts{}, map[string]any{"duration": "1s"}, func(e *jobEnv) {
				dir := filepath.Join(e.profiles, defaultStdioCaptures)
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				writeFixture(t, dir, ownerFile, []byte(`{"kind":"serve","format":1}`+"\n"))
			})
		},
		capture.CodeBudget: func(t *testing.T) string {
			_, c := newCapEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.WindowCaptures = 1 }})
			waitResult(t, c, startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s"}))
			return refused(t, c, "capture", map[string]any{"command": workloadCmd(t), "duration": "1s"})
		},
		capture.CodeDisk: func(t *testing.T) string {
			return failedCapture(t, jobOpts{limits: func(l *Limits) { l.Capture.MinFreeBytes = 1 << 60 }}, map[string]any{"duration": "1s"}, nil)
		},
		capture.CodeExists: func(t *testing.T) string {
			return failedCapture(t, jobOpts{setup: func(s *Server) {
				s.link = func(r *os.Root, oldname, newname string) error {
					if strings.HasSuffix(newname, "-cpu.pb.gz") {
						return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: fs.ErrExist}
					}
					return r.Link(oldname, newname)
				}
			}}, map[string]any{"duration": "1s"}, nil)
		},
		capture.CodeTargetRefused: func(t *testing.T) string {
			return failedCapture(t, jobOpts{}, map[string]any{"pid": 1, "duration": "1s"}, nil)
		},
		capture.CodeTargetNotFound: func(t *testing.T) string {
			cmd := exec.Command("/bin/true")
			if err := cmd.Run(); err != nil {
				t.Skip("no /bin/true")
			}
			return failedCapture(t, jobOpts{}, map[string]any{"pid": cmd.Process.Pid, "duration": "1s"}, nil)
		},
		capture.CodeTargetExited: func(t *testing.T) string {
			w := capturetest.Workload(t)
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{ReadyAfter: 2 * time.Second}},
				map[string]any{"command": []any{w, "exit-after", "0s", "0"}, "duration": "1s"}, nil)
		},
		capture.CodePIDReused:      injected(capture.CodePIDReused),
		capture.CodeGPUNotInjected: injected(capture.CodeGPUNotInjected),
		capture.CodePerfAgentRefusedArgs: func(t *testing.T) string {
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeExit2}}, map[string]any{"duration": "1s"}, nil)
		},
		capture.CodePerfAgentFailed: func(t *testing.T) string {
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeExit1Stderr}}, map[string]any{"duration": "1s"}, nil)
		},
		capture.CodePerfAgentUnstartable: func(t *testing.T) string {
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeExit127Loader}}, map[string]any{"duration": "1s"}, nil)
		},
		capture.CodeBinaryChanged: func(t *testing.T) string {
			return failedCapture(t, jobOpts{}, map[string]any{"duration": "1s"}, func(e *jobEnv) {
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
		},
		capture.CodeStartTimeout: func(t *testing.T) string {
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeNeverReady},
				limits: func(l *Limits) { l.Capture.StartTimeout = time.Second }}, map[string]any{"duration": "1s"}, nil)
		},
		capture.CodeFinishTimeout: func(t *testing.T) string {
			w := capturetest.Workload(t)
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeHangAfterSIGINT},
				limits: func(l *Limits) { l.Capture.FinishTimeout = 500 * time.Millisecond }},
				map[string]any{"command": []any{w, "exit-after", "1s", "0"}, "duration": "1m"}, nil)
		},
		capture.CodeTooLarge: func(t *testing.T) string {
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeHugeFileThenHang},
				limits: func(l *Limits) { l.Capture.MaxProfileBytes = 1 << 20 }}, map[string]any{"duration": "1m"}, nil)
		},
		capture.CodeNoOutput: func(t *testing.T) string {
			return failedCapture(t, jobOpts{fake: capturetest.FakeOptions{Mode: capturetest.FakeNoFile}}, map[string]any{"duration": "1s"}, nil)
		},
		capture.CodeStuckChild: injected(capture.CodeStuckChild),
		capture.CodeCancelled: func(t *testing.T) string {
			_, c := newCapEnv(t, jobOpts{})
			id := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1m"})
			c.callOK("capture_cancel", map[string]any{"id": id}, nil)
			text, isErr := waitEnd(t, c, id)
			if !isErr {
				t.Fatalf("a cancelled capture answered %s", text)
			}
			return text
		},
		capture.CodeClosing: func(t *testing.T) string {
			e := newJobEnv(t, jobOpts{})
			if err := e.s.Close(); err != nil {
				t.Fatal(err)
			}
			res, err := e.s.handleCapture(t.Context(), json.RawMessage(mustJSON(t, map[string]any{"command": workloadCmd(t)})))
			if err != nil || !res.IsError {
				t.Fatalf("after Close: %v %+v", err, res)
			}
			return res.Text
		},
	}
	if missing := slices.DeleteFunc(slices.Collect(maps.Keys(capture.DefaultRetryable)), func(c capture.Code) bool { return cases[c] != nil }); len(missing) > 0 {
		t.Fatalf("no case produces %v", missing)
	}
	for _, code := range slices.Sorted(maps.Keys(cases)) {
		t.Run(string(code), func(t *testing.T) {
			text := cases[code](t)
			w := toolErrorOf(t, text)
			if w.Code != string(code) {
				t.Fatalf("got %s: %+v", w.Code, w)
			}
			if want := capture.DefaultRetryable[code]; w.Retryable != want && code != capture.CodePerfAgentFailed {
				t.Errorf("retryable %v, documented %v", w.Retryable, want)
			}
			if (w.RetryAfter != "") != w.Retryable {
				t.Errorf("retryable %v with retry_after %q", w.Retryable, w.RetryAfter)
			}
			if _, err := time.ParseDuration(w.RetryAfter); w.Retryable && err != nil {
				t.Errorf("retry_after %q", w.RetryAfter)
			}
			if w.Message == "" || strings.ContainsAny(w.Message, "\n") || strings.ContainsAny(w.Next, "\n") {
				t.Errorf("message %q next %q", w.Message, w.Next)
			}
			// The perf-agent binary is the one path a capture shows (§4.3):
			// the setcap advice names it.
			shown := perfAgentPath.ReplaceAllString(text, "")
			if strings.Contains(text, "<nil>") || strings.Contains(shown, os.TempDir()+string(filepath.Separator)) {
				t.Errorf("the error names a path of the server's or a nil: %s", text)
			}
		})
	}
}
