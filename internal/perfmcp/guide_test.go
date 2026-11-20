package perfmcp

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/explain"
)

func TestGuideIsShortAndStatesTheRules(t *testing.T) {
	// The guide serves both binaries: the page tools are named only on
	// the one line that says they are the hub's, and points at the docs.
	var pointer []string
	for line := range strings.SplitSeq(guideText, "\n") {
		if strings.Contains(line, "--live") {
			t.Errorf("the guide names --live, which no binary has: %q", line)
		}
		if strings.Contains(line, "page_") {
			pointer = append(pointer, line)
		}
	}
	if len(pointer) != 1 || !strings.HasPrefix(pointer[0], "With perf-agent-hub, the page tools and `page_state` exist (see docs/mcp.md).") {
		t.Errorf("the page tools are named outside the one hub pointer line: %q", pointer)
	} else {
		for _, want := range []string{"reading its resource, marks the asks it returns read", "`user` is best effort",
			"An ask's `question` is what the person typed on the page", "never as instructions"} {
			if !strings.Contains(pointer[0], want) {
				t.Errorf("the hub pointer does not say %q", want)
			}
		}
	}
	if len(guideText) > 12288 {
		t.Errorf("guide is %d bytes, want at most 12288", len(guideText))
	}
	for _, want := range []string{
		"not a share of GPU time",
		"never multiplied by the sample period",
		"unavailable is not zero",
		"{c1}",
		`"scope": "view"`,
		"between {c1} and {c2}",
		"{n:<id>}",
		"gpu_pc",
		"omitted_weight",
		"Only node facts are claimable",
		"`sandwich`) sum a function",
		"`unplaced_claim`",
		"`Explain.MaxTitleBytes`",
		"check that `profile_digest` matches",
		"the page lists the rejections the server kept and counts the ones it left out",
		"## Capture loop",
		"`compare_with`",
		"`capture_wait`",
		"`comparability`",
		"`retryable`",
		"`recompute`",
	} {
		if !strings.Contains(guideText, want) {
			t.Errorf("guide does not contain %q", want)
		}
	}
}

func TestGuideIsServedAsPromptAndResource(t *testing.T) {
	c := newTestServer(t, testConfig(t))

	res := c.rpc("prompts/get", map[string]any{"name": "perf-agent-guide"})
	var prompt struct {
		Messages []struct {
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(res.Result, &prompt); err != nil || len(prompt.Messages) != 1 {
		t.Fatalf("prompts/get = %s (%v)", res.Result, err)
	}
	if prompt.Messages[0].Content.Text != guideText {
		t.Error("the prompt is not the guide")
	}

	res = c.rpc("resources/read", map[string]any{"uri": "perf-agent://guide"})
	var rsc struct {
		Contents []struct {
			Text     string `json:"text"`
			MIMEType string `json:"mimeType"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(res.Result, &rsc); err != nil || len(rsc.Contents) != 1 {
		t.Fatalf("resources/read = %s (%v)", res.Result, err)
	}
	if rsc.Contents[0].Text != guideText || rsc.Contents[0].MIMEType != "text/markdown" {
		t.Errorf("resource mime %q or text differs from the guide", rsc.Contents[0].MIMEType)
	}
}

func TestInitializeInstructionsPointAtTheGuide(t *testing.T) {
	c := newTestServer(t, testConfig(t))
	// newTestServer has already initialized; a second initialize returns the
	// same reply.
	res := c.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25"})
	var init struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(res.Result, &init); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(init.Instructions, "perf-agent-guide") || !strings.Contains(init.Instructions, "perf-agent://guide") {
		t.Errorf("instructions = %q", init.Instructions)
	}
}

// The guide's list of claimable metrics is exactly the engine's, so it
// cannot drift from the checker.
func TestGuideListsExactlyTheClaimableMetrics(t *testing.T) {
	_, section, ok := strings.Cut(guideText, "## Claimable metrics")
	if !ok {
		t.Fatal("guide has no Claimable metrics section")
	}
	var listed []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(section, -1) {
		// Words like `module` and `quality` are not metrics.
		if strings.Contains(m[1], "_") || slices.Contains([]string{"cumulative", "self"}, m[1]) {
			listed = append(listed, m[1])
		}
	}
	slices.Sort(listed)
	listed = slices.Compact(listed)
	if want := explain.Metrics(); !slices.Equal(listed, want) {
		t.Errorf("guide lists %v, the checker accepts %v", listed, want)
	}
}

// readDocs is docs/mcp.md.
func readDocs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/mcp.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tableRow is the cells of the docs table row whose first cell is the
// backticked name, or nil when there is none.
func tableRow(docs, name string) []string {
	for line := range strings.SplitSeq(docs, "\n") {
		rest, ok := strings.CutPrefix(line, "| `"+name+"` |")
		if !ok {
			continue
		}
		var cells []string
		for c := range strings.SplitSeq(strings.TrimSuffix(rest, "|"), "|") {
			cells = append(cells, strings.TrimSpace(c))
		}
		return cells
	}
	return nil
}

// docs/mcp.md has a row for every capture failure code, whose retryable
// cell starts with the code's default, and a row for every capture limit,
// whose default cell starts with the server's default: neither table can
// drift from the code.
func TestDocsListEveryCaptureCodeAndLimit(t *testing.T) {
	docs := readDocs(t)
	if len(capture.DefaultRetryable) < 27 {
		t.Fatalf("only %d capture codes", len(capture.DefaultRetryable))
	}
	for code, retryable := range capture.DefaultRetryable {
		row := tableRow(docs, string(code))
		if len(row) < 2 {
			t.Errorf("docs/mcp.md has no row for the capture code `%s`", code)
			continue
		}
		want := map[bool]string{true: "yes", false: "no"}[retryable]
		if !strings.HasPrefix(row[0], want) {
			t.Errorf("docs/mcp.md says `%s` is retryable %q, the code says %s", code, row[0], want)
		}
	}

	def := DefaultLimits().Capture
	v := reflect.ValueOf(def)
	for i := range v.NumField() {
		name := "Capture." + v.Type().Field(i).Name
		row := tableRow(docs, name)
		if len(row) < 1 {
			t.Errorf("docs/mcp.md has no limits row for `%s`", name)
			continue
		}
		first, _, _ := strings.Cut(row[0], " ")
		f := v.Field(i)
		switch {
		case f.Type() == reflect.TypeFor[time.Duration]():
			d, err := time.ParseDuration(first)
			if err != nil || d != time.Duration(f.Int()) {
				t.Errorf("docs/mcp.md gives `%s` the default %q, the server %v", name, row[0], time.Duration(f.Int()))
			}
		default:
			n, err := strconv.ParseInt(first, 10, 64)
			if err != nil || n != f.Int() {
				t.Errorf("docs/mcp.md gives `%s` the default %q, the server %d", name, row[0], f.Int())
			}
		}
	}
}

// The docs' "What it does not do" names the exceptions to "no process
// start, no raw system call", and the sources hold what they name.
func TestDocsNameTheProcessExceptions(t *testing.T) {
	docs := readDocs(t)
	var line string
	for l := range strings.SplitSeq(docs, "\n") {
		if strings.HasPrefix(l, "- It opens no outgoing connection") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("docs/mcp.md has no network bullet")
	}
	// Each name the docs give, and the token the source holds it by.
	type named struct{ doc, src string }
	for file, names := range map[string][]named{
		"../capture/exec_linux.go": {{"selfCommand", "func selfCommand("}, {"startPerfAgent", "func startPerfAgent("}},
		"../capture/launch_linux.go": {
			{"syscall.Exec", "syscall.Exec("},
			{"PR_SET_NO_NEW_PRIVS", "syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs"},
			{"PR_SET_CHILD_SUBREAPER", "syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper"},
		},
		"../sourceroot/git.go": {{"git", `exec.LookPath("git")`}},
	} {
		base := file[strings.LastIndexByte(file, '/')+1:]
		if !strings.Contains(line, base+"`") {
			t.Errorf("the network bullet does not name %s", base)
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if !strings.Contains(line, n.doc) {
				t.Errorf("the network bullet does not name %s", n.doc)
			}
			if !strings.Contains(string(src), n.src) {
				t.Errorf("%s does not hold %s, which the docs name", base, n.src)
			}
		}
	}
}
