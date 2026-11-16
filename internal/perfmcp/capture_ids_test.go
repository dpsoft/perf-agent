package perfmcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// One name per meaning: id is a capture's number in its series (001), and
// the capture id an agent passes back (series/001) is capture in
// capture_list's rows and jobs and in the result, and capture_id in the
// answers of capture_wait and capture_cancel, as in a job's error. A bare number given to capture_wait or capture_cancel is refused
// naming the form to pass.
func TestCaptureIDsHaveOneNamePerMeaning(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	job := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s"})
	res, _ := waitResult(t, c, job)
	if res.Capture.ID != "001" || res.Capture.Capture != "adhoc/001" {
		t.Errorf("result: id %q capture %q, want 001 and adhoc/001", res.Capture.ID, res.Capture.Capture)
	}

	text := c.callOK("capture_list", map[string]any{"series": "adhoc"}, nil)
	var l struct {
		Jobs     []map[string]any `json:"jobs"`
		Captures []map[string]any `json:"captures"`
	}
	if err := json.Unmarshal([]byte(text), &l); err != nil || len(l.Jobs) != 1 || len(l.Captures) != 1 {
		t.Fatalf("capture_list: %s (%v)", text, err)
	}
	for what, row := range map[string]map[string]any{"job": l.Jobs[0], "capture": l.Captures[0]} {
		if row["id"] != "001" || row["capture"] != "adhoc/001" {
			t.Errorf("capture_list %s row: id %v capture %v, want 001 and adhoc/001", what, row["id"], row["capture"])
		}
	}

	var a map[string]string
	c.callOK("capture_cancel", map[string]any{"id": job}, &a)
	if a["capture_id"] != "adhoc/001" {
		t.Errorf("cancel answer %v names no capture adhoc/001", a)
	}

	for _, tool := range []string{"capture_wait", "capture_cancel"} {
		text, isErr := c.call(tool, map[string]any{"id": "001"})
		w := toolErrorOf(t, text)
		if !isErr || w.Code != string(capture.CodeInvalidArgument) || !strings.Contains(w.Message+w.Next, "<series>/001") {
			t.Errorf("%s with a bare 001: %s", tool, text)
		}
	}
}
