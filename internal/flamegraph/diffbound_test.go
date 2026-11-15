package flamegraph

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// boundFixtures are diffs the export bound must hold on: real comparisons,
// literal ones with narrow and one-sided nodes, and one whose every string
// needs escaping.
func boundFixtures(t *testing.T) map[string]*profilemodel.Diff {
	t.Helper()
	nasty := `<a href="x">&'+` + "\x00\xff" + `</a> > "q"`
	root := dnode(nasty, 100, 220,
		withShare(dnode(nasty+"kept", 50, 60, dnode(nasty+"only", 0, 20)), 100, 220),
		dnode(nasty+"gone", 30, 0, dnode("under", 30, 0)), dnode("added", 0, 40))
	root.Children[1].Why = nasty
	root.Children[1].Interpretation = profilemodel.NotComparable
	root.Children[0].Match = profilemodel.MatchAmbiguous
	escaped := withIDs(diffOf("cpu"+nasty, "bytes"+nasty, 100, 220, root))
	escaped.QualityWarnings = []string{nasty, profilemodel.WarningPerturbationNotGPU}
	escaped.BaseNotes, escaped.NewNotes = []string{nasty}, []string{nasty + "new"}
	escaped.BaseHandle, escaped.NewHandle = profilemodel.Handle(nasty), profilemodel.Handle(nasty+"2")
	empty := withIDs(diffOf("cpu", "nanoseconds", 0, 0, dnode("", 0, 0)))
	return map[string]*profilemodel.Diff{
		"CPUBasic vs CPUBasicNew":   compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew()),
		"Deep(600) vs DeepNew(600)": compared(t, pmfixture.Deep(600), pmfixture.DeepNew(600)),
		"Deep(600) vs itself":       compared(t, pmfixture.Deep(600), pmfixture.Deep(600)),
		"HugeValue vs itself":       compared(t, pmfixture.HugeValue(), pmfixture.HugeValue()),
		"sample":                    sampleDiff(),
		"narrow nodes":              sampleWithNarrowNodes(),
		"escaped":                   escaped,
		"empty":                     empty,
		"wide":                      wideDiff(900, 40),
		"many rows":                 manyRows(700),
	}
}

// manyRows is a root over n one-sided children, more than the table lists.
func manyRows(n int) *profilemodel.Diff {
	var kids []*profilemodel.DiffNode
	var base int64
	for i := range n {
		kids = append(kids, dnode(fmt.Sprintf("gone_%04d", i), int64(1+i%9), 0))
		base += int64(1 + i%9)
	}
	return withIDs(diffOf("cpu", "nanoseconds", base, 0, dnode("", base, 0, kids...)))
}

func TestBoundedDiffExportMatchesWhenItFits(t *testing.T) {
	for name, d := range boundFixtures(t) {
		t.Run(name, func(t *testing.T) {
			opts := DiffPageOptions{Title: "export of " + name, BaseName: "base <b>", NewName: "new & co"}
			var want bytes.Buffer
			require.NoError(t, WriteDiffPage(&want, d, opts))
			bound, nodes := diffPageBound(d, opts)
			assert.GreaterOrEqual(t, bound, int64(want.Len()), "the bound holds the page")
			assert.Equal(t, strings.Count(want.String(), `<div class="n" `), nodes, "the bound counts the nodes the page draws")
			t.Logf("page %d bytes, bound %d (%.2fx), %d nodes", want.Len(), bound, float64(bound)/float64(want.Len()), nodes)

			var got bytes.Buffer
			require.NoError(t, WriteDiffPageBounded(&got, d, opts, int(bound), nodes))
			assert.Equal(t, want.String(), got.String())
			got.Reset()
			require.NoError(t, WriteDiffPageBounded(&got, d, opts, 0, 0), "no budget is no bound")
			assert.Equal(t, want.String(), got.String())

			if nodes > 1 {
				got.Reset()
				err := WriteDiffPageBounded(&got, d, opts, int(bound), nodes-1)
				require.ErrorIs(t, err, ErrPageTooLarge)
				assert.Zero(t, got.Len())
			}
		})
	}
}

// wideDeepDiff is n chains under the root, together frames nodes deep and
// wide, each frame named by a name of nameLen bytes. Each chain ends in a
// node on the new side only, so the table lists long paths too.
func wideDeepDiff(chains, frames, nameLen int) *profilemodel.Diff {
	name := func(c, i int) string {
		s := fmt.Sprintf("c%02d_f%04d_", c, i)
		return s + strings.Repeat("n", nameLen-len(s))
	}
	per := frames / chains
	var kids []*profilemodel.DiffNode
	for c := range chains {
		leaf := dnode(name(c, per-1), 0, 10)
		n := leaf
		for i := per - 2; i >= 0; i-- {
			n = dnode(name(c, i), 10, 20, n)
		}
		kids = append(kids, n)
	}
	total := int64(chains) * 10
	return withIDs(diffOf("cpu", "nanoseconds", total, 2*total, dnode("", total, 2*total, kids...)))
}

func TestBoundedDiffExportRefusesBeforeBuilding(t *testing.T) {
	d := wideDeepDiff(8, 3200, 1<<10) // 3200 wide nodes, 400 deep
	const budget = 4 << 20
	live, raw := diffLiveOf(t, d, DiffLiveOptions{MaxBytes: budget})
	assert.LessOrEqual(t, len(raw), budget)
	assert.NotEmpty(t, live["size_note"], "the model fits by leaving frames out")
	ex := live["export"].(map[string]any)
	assert.Equal(t, false, ex["ok"])
	reason, _ := ex["reason"].(string)
	assert.Contains(t, reason, fmt.Sprint(budget), "the reason names the budget")

	var w bytes.Buffer
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := WriteDiffPageBounded(&w, d, DiffPageOptions{Title: "wide and deep"}, budget, 20000)
	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, ErrPageTooLarge)
	assert.Zero(t, w.Len(), "nothing is written")
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("refusal allocated %d bytes: %v", alloc, err)
	assert.Less(t, alloc, uint64(1<<20), "the refusal builds nothing")
	assert.Contains(t, err.Error(), fmt.Sprint(budget))
	bound, nodes := diffPageBound(d, DiffPageOptions{Title: "wide and deep"})
	assert.Contains(t, err.Error(), fmt.Sprint(bound), "the error says how large the page would be")
	assert.Equal(t, 3201, nodes)

	// Over the node budget alone it is refused the same way.
	w.Reset()
	err = WriteDiffPageBounded(&w, d, DiffPageOptions{}, 1<<30, 3200)
	require.ErrorIs(t, err, ErrPageTooLarge)
	assert.Contains(t, err.Error(), "3201")
	assert.Zero(t, w.Len())
}

func TestDiffLiveModelExportStateFollowsTheBound(t *testing.T) {
	d := compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	bound, nodes := diffPageBound(d, DiffPageOptions{Title: "t"})
	for _, tc := range []struct {
		maxBytes, maxNodes int
		ok                 bool
	}{
		{0, 0, true},
		{int(bound), nodes, true},
		{int(bound) - 1, nodes, false},
		{1 << 20, nodes - 1, false},
	} {
		e := exportState(bound, nodes, tc.maxBytes, tc.maxNodes)
		assert.Equal(t, tc.ok, e.OK, "%+v", tc)
		assert.Equal(t, tc.ok, e.Reason == "", "%+v: %q", tc, e.Reason)
	}
	live, _ := diffLiveOf(t, d, DiffLiveOptions{Title: "t", MaxBytes: 1 << 20, MaxNodes: nodes - 1})
	ex := live["export"].(map[string]any)
	assert.Equal(t, false, ex["ok"])
	assert.Contains(t, ex["reason"], fmt.Sprint(nodes))
	live, _ = diffLiveOf(t, d, DiffLiveOptions{Title: "t", MaxBytes: 1 << 20, MaxNodes: nodes})
	assert.Equal(t, map[string]any{"ok": true, "reason": ""}, live["export"])
}
