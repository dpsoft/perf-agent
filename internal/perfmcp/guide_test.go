package perfmcp

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/explain"
)

func TestGuideIsShortAndStatesTheRules(t *testing.T) {
	// The live page's section must come before the claimable metrics,
	// whose section runs to the end of the guide.
	live, metrics := strings.Index(guideText, "## Live page"), strings.Index(guideText, "## Claimable metrics")
	if live < 0 || metrics < 0 || live > metrics {
		t.Errorf("the Live page section is at %d and Claimable metrics at %d; want Live page first", live, metrics)
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
		"page_open",
		"give the user the URL",
		"same check as render_flamegraph",
		"every rejection the server kept for that profile and view, made through any handle, with counts for the rest",
		"the page lists the rejections the server kept and counts the ones it left out",
		"`page_open` takes no arguments",
		"`page_show` takes a `publication` and a `node`",
		"`diff` and `page_diff` take",
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
