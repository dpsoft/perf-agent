package perfmcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/explain"
)

// skillArgs are backticked words in the skill that are neither tools, metrics
// nor failure codes: tool arguments, response fields and literals.
var skillArgs = []string{
	"path", "execlog", "handle", "view", "notes", "rows", "functions", "nodes",
	"next_cursor", "cursor", "attempt", "out", "ok", "zero", "unavailable",
	"denominator", "unit", "value", "metric", "scope", "node", "facts",
	"view_total", "source_unverified", "unsampled_portion", "base_handle", "base_view", "new_handle", "new_view",
	"view_scope", "id", "publication",
	// The capture loop's arguments, result fields and values; its failure
	// codes are capture.DefaultRetryable's.
	"command", "pid", "series", "label", "keep", "job", "timeout", "compare_with", "compare", "next",
	"error", "code", "retryable", "retry_after", "recompute", "handles", "reopen",
	"comparability", "status", "done", "interrupted", "derived", "capture_id",
	"sample_rate", "start_time", "perf_agent_exit",
	// page_state's answer.
	"asks", "user",
}

// captureCodes are the capture failure codes, which the skill may cite.
func captureCodes() []string {
	var codes []string
	for c := range capture.DefaultRetryable {
		codes = append(codes, string(c))
	}
	return codes
}

// explainCodes reads the failure and notice codes declared in
// internal/explain.
func explainCodes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../explain/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no explain sources: %v", err)
	}
	re := regexp.MustCompile(`(?:Code\s+|Notice[A-Za-z]+\s+)= "([a-z_]+)"`)
	var codes []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			codes = append(codes, m[1])
		}
	}
	return codes
}

// The skill points at the server, so what it names must exist there.
func TestSkillNamesExistInTheServer(t *testing.T) {
	b, err := os.ReadFile("../../skills/perf-agent-mcp/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	skill := string(b)

	// The hub's tool list: the stdio one and the page tools.
	cfg := testConfig(t)
	cfg.Page = livePage()
	c := newTestServer(t, cfg)
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.rpc("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	var tools []string
	for _, tl := range list.Tools {
		tools = append(tools, tl.Name)
	}
	if !slices.Contains(tools, "page_state") {
		t.Fatalf("the hub's tool list %v lacks page_state", tools)
	}
	metrics := explain.Metrics()
	codes := append(explainCodes(t), captureCodes()...)

	cited := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z][a-z_]*)`").FindAllStringSubmatch(skill, -1) {
		cited[m[1]] = true
	}
	for word := range cited {
		known := slices.Contains(tools, word) || slices.Contains(metrics, word) ||
			slices.Contains(codes, word) || slices.Contains(skillArgs, word)
		if !known {
			t.Errorf("the skill cites `%s`, which is no tool, metric, failure, notice or capture code or known argument", word)
		}
	}

	// The skill must cite every tool, so none is left out of the workflow.
	for _, tool := range tools {
		if !cited[tool] {
			t.Errorf("the skill never mentions the tool `%s`", tool)
		}
	}

	// The guide is the authority; the skill names both ways to read it.
	for _, name := range []string{guidePrompts()[0].Name, guideResources()[0].URI} {
		if !strings.Contains(skill, name) {
			t.Errorf("the skill does not name the guide as %q", name)
		}
	}

	// The rules agents most often get wrong are stated as the guide states them.
	for _, rule := range []string{
		"Only node facts are claimable", "Never just copy the engine's value", "`facts` under `view_scope`",
		"`comparability`", "`closing`", "`stuck_child`",
	} {
		if !strings.Contains(skill, rule) {
			t.Errorf("the skill does not say %q", rule)
		}
	}

	// Frontmatter: a name and a "Use when" description.
	if !strings.HasPrefix(skill, "---\nname: perf-agent-mcp\ndescription: Use when ") {
		t.Error("the skill frontmatter must start with name and a Use-when description")
	}
}
