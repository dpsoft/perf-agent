package flamegraph

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden pages in testdata")

// checkGolden compares got with testdata/name byte for byte, or rewrites the
// file under -update-golden.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got, "%s differs; rerun with -update-golden only for an intended change", name)
}

func goldenResult() *foldedstacks.Result {
	return result(
		foldedstacks.Stack{Frames: []string{"z", "y"}, Value: 3},
		foldedstacks.Stack{Frames: []string{"a", "b"}, Value: 5},
		foldedstacks.Stack{Frames: []string{"a", "c"}, Value: 2},
	)
}

func TestRenderHTMLMatchesGolden(t *testing.T) {
	checkGolden(t, "render.golden.html", renderString(t, goldenResult(), Options{Title: "t"}))
}

func TestRenderFragmentMatchesGolden(t *testing.T) {
	var b strings.Builder
	require.NoError(t, RenderFragment(&b, goldenResult()))
	checkGolden(t, "fragment.golden.html", b.String())
}

func TestWriteDiffPageMatchesGolden(t *testing.T) {
	checkGolden(t, "diffpage.golden.html", pageOf(t, sampleDiff()))
}
