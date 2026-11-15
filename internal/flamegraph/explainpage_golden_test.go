package flamegraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

// clickProbe counts the panel's renders (each sets #sel-id) while it
// clicks a reference in the explanation, then a path entry, and writes the
// count after each click.
const clickProbe = `<script>
addEventListener('load', () => {
  const counts = [];
  let renders = 0;
  new MutationObserver((records) => { renders += records.length; })
    .observe(document.getElementById('sel-id'), {childList: true, characterData: true, subtree: true});
  const step = (sel, then) => {
    const el = document.querySelector(sel);
    renders = 0;
    if (el) el.click();
    setTimeout(() => { counts.push(el ? String(renders) : 'missing'); then(); }, 50);
  };
  step('#exp button.cite', () => step('#path button', () => {
    const pre = document.createElement('pre');
    pre.id = 'clicks';
    pre.textContent = counts.join(' ');
    document.body.appendChild(pre);
  }));
});
</script>`

func TestPanelClicksSelectOnceInChrome(t *testing.T) {
	chrome := chromePath()
	if chrome == "" || testing.Short() {
		t.Skip("no headless Chrome")
	}
	v, _, r := cpuBasicExplained(t)
	page := exPage(t, v, r, exOpts())
	dir := t.TempDir()
	file := filepath.Join(dir, "page.html")
	require.NoError(t, os.WriteFile(file, []byte(strings.Replace(page, "</body>", clickProbe+"</body>", 1)), 0o644))
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-first-run",
		"--user-data-dir="+filepath.Join(dir, "profile"), "--window-size=1000,800", "--virtual-time-budget=2000",
		"--dump-dom", "file://"+file).Output()
	if err != nil {
		t.Skipf("headless Chrome did not run: %v", err)
	}
	m := regexp.MustCompile(`<pre id="clicks">(.*?)</pre>`).FindStringSubmatch(string(out))
	require.Len(t, m, 2, "the probe did not run")
	assert.Equal(t, "1 1", m[1], "one click on a reference, then on a path entry, renders the panel once each")
}
