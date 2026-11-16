package perfmcp

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// goTypeWords are what a Go decoding error says that an agent cannot act
// on: Go's own type and struct names.
var goTypeWords = regexp.MustCompile(`\bGo\b|\bstruct\b|captureArgs|waitArgs|listArgs|\bint\b|\bint64\b|\bfloat64\b|unmarshal`)

// goTypeWord is the first of goTypeWords in s, "" when there is none; "Go
// duration", the name of the format durations are passed in, is no Go type.
func goTypeWord(s string) string {
	return goTypeWords.FindString(strings.ReplaceAll(s, "Go duration", ""))
}

// An argument of the wrong JSON type is invalid_argument naming the
// argument and the type it takes, in JSON's words, with a next; never Go's
// type or field names.
func TestAMistypedArgumentIsRefusedPlainly(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	for _, tc := range []struct {
		tool string
		args map[string]any
		arg  string
		want string
	}{
		{"capture", map[string]any{"command": workloadCmd(t), "sample_rate": "99"}, "sample_rate", "integer"},
		{"capture", map[string]any{"command": workloadCmd(t), "keep": "true"}, "keep", "boolean"},
		{"capture", map[string]any{"command": "ls"}, "command", "array"},
		{"capture", map[string]any{"pid": 1.5}, "pid", "integer"},
		{"capture_wait", map[string]any{"id": 5}, "id", "string"},
		{"capture_list", map[string]any{"limit": "5"}, "limit", "integer"},
	} {
		text, isErr := c.call(tc.tool, tc.args)
		w := toolErrorOf(t, text)
		if !isErr || w.Code != string(capture.CodeInvalidArgument) {
			t.Errorf("%s %v: %s", tc.tool, tc.args, text)
			continue
		}
		if !strings.Contains(w.Message, tc.arg) || !strings.Contains(w.Message, tc.want) || w.Next == "" {
			t.Errorf("%s %v: message %q next %q, want %s named as taking %s, and a next", tc.tool, tc.args, w.Message, w.Next, tc.arg, tc.want)
		}
		if m := goTypeWord(w.Message + " " + w.Next); m != "" {
			t.Errorf("%s %v: %q says %q", tc.tool, tc.args, w.Message, m)
		}
	}
}

// A capture's result and capture_list carry integers as JSON numbers, so
// the settings an agent reads back can be passed again as they are.
func TestCaptureResultsCarryIntegersAsNumbers(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	_, text := waitResult(t, c, startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s"}))
	var r struct {
		Capture struct {
			SampleRate any `json:"sample_rate"`
			Settings   struct {
				SampleRate any `json:"sample_rate"`
			} `json:"settings"`
			Target struct {
				PID       any `json:"pid"`
				StartTime any `json:"start_time"`
			} `json:"target"`
		} `json:"capture"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatal(err)
	}
	for what, v := range map[string]any{
		"sample_rate": r.Capture.SampleRate, "settings.sample_rate": r.Capture.Settings.SampleRate,
		"target.pid": r.Capture.Target.PID, "target.start_time": r.Capture.Target.StartTime,
	} {
		if _, ok := v.(float64); !ok {
			t.Errorf("capture.%s is %T (%v), want a JSON number", what, v, v)
		}
	}
	// What was read back is passed again as it is.
	again := startCapture(t, c, map[string]any{"command": workloadCmd(t), "duration": "1s", "sample_rate": r.Capture.Settings.SampleRate})
	waitResult(t, c, again)

	var l struct {
		Captures []struct {
			Settings struct {
				SampleRate any `json:"sample_rate"`
			} `json:"settings"`
		} `json:"captures"`
		TotalRows  any `json:"total_rows"`
		InProgress any `json:"in_progress"`
	}
	c.callOK("capture_list", map[string]any{"series": "adhoc"}, &l)
	if len(l.Captures) == 0 {
		t.Fatal("no captures listed")
	}
	for what, v := range map[string]any{"captures[0].settings.sample_rate": l.Captures[0].Settings.SampleRate, "total_rows": l.TotalRows, "in_progress": l.InProgress} {
		if _, ok := v.(float64); !ok {
			t.Errorf("capture_list %s is %T (%v), want a JSON number", what, v, v)
		}
	}
	var s struct {
		Series []map[string]any `json:"series"`
	}
	c.callOK("capture_list", map[string]any{}, &s)
	if len(s.Series) != 1 {
		t.Fatalf("series %v", s.Series)
	}
	for _, k := range []string{"captures", "bytes", "leftovers", "in_progress"} {
		if _, ok := s.Series[0][k].(float64); !ok {
			t.Errorf("capture_list series %s is %T (%v), want a JSON number", k, s.Series[0][k], s.Series[0][k])
		}
	}
}
