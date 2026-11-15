package flamegraph

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stripInlineScript removes the content between the page's last <script>
// and its </script>: the part of the page the shared panel script lives in.
func stripInlineScript(page string) string {
	start := strings.LastIndex(page, "<script>")
	if start < 0 {
		return page
	}
	start += len("<script>")
	end := strings.Index(page[start:], "</script>")
	if end < 0 {
		return page
	}
	return page[:start] + page[start+end:]
}

func TestExplainedPageGoldenOutsideItsScript(t *testing.T) {
	t.Run("cpu", func(t *testing.T) {
		_, b, _ := cpuBasicExplained(t)
		v, _, r := cpuExplained(t, cpuProse(b)+" It is 62% of time.")
		checkGolden(t, "explainpage-cpu.golden.html", stripInlineScript(exPage(t, v, r, exOpts())))
	})
	t.Run("gpu", func(t *testing.T) {
		v, _, r := gpuBasicExplained(t)
		checkGolden(t, "explainpage-gpu.golden.html", stripInlineScript(exPage(t, v, r, exOpts())))
	})
}

func TestExplainedPageScriptDefinesTheSharedPanel(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	page := exPage(t, v, r, exOpts())
	start := strings.LastIndex(page, "<script>")
	require.GreaterOrEqual(t, start, 0)
	script := page[start:]
	assert.Equal(t, 1, strings.Count(script, "function perfAgentPanel("))
	assert.Contains(t, script, "perfAgentPanel(model,")
}

func TestPanelScriptUsesTextContentOnly(t *testing.T) {
	src, err := os.ReadFile("explainpanel.js")
	require.NoError(t, err)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "setAttribute('style'", "cssText"} {
		assert.NotContains(t, string(src), bad)
	}
}
