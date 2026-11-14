package perfmcp

import (
	"embed"
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// The eval suite replays questions through a real agent over frozen
// fixtures. It needs a model, so scripts/mcp-evals.sh drives it by hand and
// both tests skip unless PERF_AGENT_MCP_EVAL_DIR names the scratch directory.

//go:embed evals/questions.json
var evalFS embed.FS

const evalDirEnv = "PERF_AGENT_MCP_EVAL_DIR"

type evalQuestion struct {
	ID      string `json:"id"`
	Prompt  string `json:"prompt"`
	Profile string `json:"profile"`
	Execlog string `json:"execlog,omitzero"`
	Expect  struct {
		Page          bool     `json:"page"`
		CheckedValues []string `json:"checked_values"`
		Require       []string `json:"require"`
		Forbid        []string `json:"forbid"`
	} `json:"expect"`
}

func evalQuestions(t *testing.T) []evalQuestion {
	t.Helper()
	b, err := evalFS.ReadFile("evals/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	var qs []evalQuestion
	if err := json.Unmarshal(b, &qs); err != nil {
		t.Fatalf("questions.json: %v", err)
	}
	return qs
}

func evalDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(evalDirEnv)
	if dir == "" {
		t.Skipf("%s is not set: the eval suite needs a model and is run by scripts/mcp-evals.sh", evalDirEnv)
	}
	return dir
}

func TestWriteEvalFixtures(t *testing.T) {
	dir := filepath.Join(evalDir(t), "profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"cpu.pb.gz":      pmfixture.CPUBasic(),
		"cpu-new.pb.gz":  pmfixture.CPUBasicNew(),
		"gpu.pb.gz":      pmfixture.GPUBasic(),
		"gpu.executions": pmfixture.GPUBasicExeclog(t),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	evalClaimRE     = regexp.MustCompile(`(?s)<span class="claim"[^>]*>(.*?)</span>`)
	evalUncheckedRE = regexp.MustCompile(`<mark class="unchecked"`)
	evalPillRE      = regexp.MustCompile(`<span class="pill ok"[^>]*>(\d+) figures? (?:and|or) references? checked</span>|<span class="pill ok"[^>]*>(1) figure or reference checked</span>`)
	evalBlockRE     = regexp.MustCompile(`(?s)<(script|style)\b.*?</(script|style)>`)
)

// checkedFigures reads the count from the header pill.
func checkedFigures(page string) (int, bool) {
	m := evalPillRE.FindStringSubmatch(page)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1] + m[2])
	return n, err == nil
}

func TestEvalPages(t *testing.T) {
	dir := evalDir(t)
	for _, q := range evalQuestions(t) {
		t.Run(q.ID, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(dir, "out", q.ID+".html"))
			if err != nil {
				if q.Expect.Page {
					t.Fatalf("no page for %q: %v", q.ID, err)
				}
				return
			}
			page := string(b)
			if n, ok := checkedFigures(page); !ok || n < 1 {
				t.Errorf("header counts %d checked figures (found=%v), want at least 1", n, ok)
			}
			var claims []string
			for _, m := range evalClaimRE.FindAllStringSubmatch(page, -1) {
				claims = append(claims, strings.TrimSpace(html.UnescapeString(m[1])))
			}
			for _, want := range q.Expect.CheckedValues {
				if !slices.Contains(claims, want) {
					t.Errorf("no claim is exactly %q; claims = %q", want, claims)
				}
			}
			// Static markup and scripts carry words of their own, so the
			// text checks look at the page without script and style blocks.
			text := html.UnescapeString(evalBlockRE.ReplaceAllString(page, ""))
			for _, want := range q.Expect.Require {
				if !strings.Contains(text, want) {
					t.Errorf("page lacks %q", want)
				}
			}
			for _, bad := range q.Expect.Forbid {
				if strings.Contains(text, bad) {
					t.Errorf("page contains forbidden %q", bad)
				}
			}
			t.Logf("%s: %d claims, %d unchecked numbers", q.ID, len(claims), len(evalUncheckedRE.FindAllString(page, -1)))
		})
	}
}
