package debuginfod

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestClassifierSkipPaths(t *testing.T) {
	c := newClassifier(nil /* cache unused for skip-path tests */, nil /* fetcher unused */)
	skipPaths := []string{"", "[vdso]", "[stack]", "[vsyscall]", "[heap]"}
	for _, p := range skipPaths {
		t.Run(p, func(t *testing.T) {
			m := procmap.Mapping{Path: p}
			got := c.classify(t.Context(), m)
			if got.route != routeSkip {
				t.Errorf("classify(%q) route = %v, want routeSkip", p, got.route)
			}
		})
	}
}
