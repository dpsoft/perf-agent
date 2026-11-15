package flamegraph

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strconv"
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
		"tight names and causes":    tightNamesAndCauses(),
		"tight table":               tightTable(),
		"row ties":                  rowTies(),
	}
}

// plusQuote is n bytes of '+' and '"', which the page writes 5 bytes each.
func plusQuote(n int, salt string) string {
	return salt + strings.Repeat(`+"`, (n-len(salt))/2)
}

// tightNamesAndCauses is 30 one-sided nodes whose names and causes are
// 1 KiB of characters escaped five times over, with positive shares and
// huge positive changes: the escaped strings are nearly the whole page, so
// a bound that leaves any of them out falls under the page.
func tightNamesAndCauses() *profilemodel.Diff {
	var kids []*profilemodel.DiffNode
	var next int64
	for i := range 30 {
		v := int64(1e15) + int64(i)
		n := dnode(plusQuote(1<<10, fmt.Sprintf("n%02d", i)), 0, v)
		n.Why = plusQuote(1<<10, fmt.Sprintf("w%02d", i))
		n.DeltaShare = shareFact(0.03)
		kids = append(kids, n)
		next += v
	}
	root := dnode(plusQuote(1<<10, "root"), 0, next, kids...)
	root.Presence = profilemodel.PresenceBoth
	root.DeltaShare = shareFact(0)
	return withIDs(diffOf("cpu", "nanoseconds", 1, next, root))
}

// tightTable is a 30-deep chain of '+' names over 700 one-sided leaves:
// the table's paths, 500 of them, are nearly the whole page.
func tightTable() *profilemodel.Diff {
	var leaves []*profilemodel.DiffNode
	var base int64
	for i := range 700 {
		leaves = append(leaves, dnode(fmt.Sprintf("leaf%03d", i)+strings.Repeat("+", 32), int64(100+i), 0))
		base += int64(100 + i)
	}
	n := dnode(strings.Repeat("+", 32)+"29", base, 1, leaves...)
	for i := 28; i >= 0; i-- {
		n = dnode(strings.Repeat("+", 32)+fmt.Sprint(i), base, 1, n)
	}
	return withIDs(diffOf("cpu", "nanoseconds", base, 1, dnode("", base, 1, n)))
}

// rowTies is more one-sided paths than the table lists, many with equal
// changes and some with equal paths, so the table's order rests on every
// key: the change, then the path, then the walk.
func rowTies() *profilemodel.Diff {
	var kids []*profilemodel.DiffNode
	var base, next int64
	for i := range 40 {
		var gs []*profilemodel.DiffNode
		var gb, gn int64
		for j := range 20 {
			// Every group has the same names: equal paths under equal
			// parents' names.
			c := dnode(fmt.Sprintf("g%02d", j%7), int64(j%3), 0)
			if j%2 == 1 {
				c = dnode(fmt.Sprintf("g%02d", j%7), 0, int64(j%4)+1)
			}
			if c.Combined == 0 {
				c = dnode(fmt.Sprintf("g%02d", j%7), 2, 0)
			}
			gs = append(gs, c)
			gb, gn = gb+c.BaseCum, gn+c.NewCum
		}
		k := dnode(fmt.Sprintf("p%02d", i%9), gb+1, gn+1, gs...)
		kids = append(kids, k)
		base, next = base+k.BaseCum, next+k.NewCum
	}
	return withIDs(diffOf("cpu", "nanoseconds", base, next, dnode("", base, next, kids...)))
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
	assert.Contains(t, err.Error(), "draws 3201 nodes, over its budget of 3200 nodes")
	assert.NotContains(t, err.Error(), "bytes", "only the limit exceeded is named")
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

// referenceRows is the table as the page first listed it: every topmost
// one-sided path, by a recursive walk, stably sorted by change and path,
// then cut to maxTableRows.
func referenceRows(d *profilemodel.Diff) ([]diffRow, int) {
	var rows []diffRow
	var collect func(n *profilemodel.DiffNode, path []string, parentOneSided bool)
	collect = func(n *profilemodel.DiffNode, path []string, parentOneSided bool) {
		if n.Name != "" || len(path) > 0 {
			path = append(path[:len(path):len(path)], n.Name)
		}
		oneSided := n.Presence != profilemodel.PresenceBoth
		if oneSided && !parentOneSided {
			rows = append(rows, diffRow{Path: strings.Join(path, " > "), Presence: string(n.Presence), Delta: signed(n.DeltaAbs, d.Unit),
				Interpretation: string(n.Interpretation), Why: n.Why, absDelta: absDelta(n.DeltaAbs)})
		}
		for _, c := range n.Children {
			collect(c, path, oneSided)
		}
	}
	collect(d.Root, nil, false)
	slices.SortStableFunc(rows, func(x, y diffRow) int {
		return cmp.Or(cmp.Compare(y.absDelta, x.absDelta), strings.Compare(x.Path, y.Path))
	})
	total := len(rows)
	return rows[:min(len(rows), maxTableRows)], total
}

func TestDiffPageTableIsTheLargestRowsInOrder(t *testing.T) {
	for name, d := range boundFixtures(t) {
		if d.Root == nil || d.Root.Combined <= 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			want, wantTotal := referenceRows(d)
			got, total := diffTable(d.Root, d.Unit)
			assert.Equal(t, wantTotal, total)
			require.Len(t, got, len(want))
			for i := range want {
				require.Equal(t, want[i], got[i], "row %d", i)
			}
		})
	}
}

// exportAlloc is what an admitted export of d allocates, and its page's
// length. The destination is sized first: its growth is not the export's.
func exportAlloc(t *testing.T, d *profilemodel.Diff) (alloc uint64, page int) {
	t.Helper()
	var w bytes.Buffer
	w.Grow(16 << 20)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	require.NoError(t, WriteDiffPageBounded(&w, d, DiffPageOptions{Title: "deep"}, 8<<20, 20000))
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, w.Len()
}

// deepOverLeaves is a chain depth deep over leaves one-sided leaves.
func deepOverLeaves(depth, leaves int) *profilemodel.Diff {
	var ls []*profilemodel.DiffNode
	var next int64
	for i := range leaves {
		ls = append(ls, dnode(fmt.Sprintf("leaf%04d", i), 0, int64(1+i%5)))
		next += int64(1 + i%5)
	}
	n := dnode(fmt.Sprintf("f%d", depth-1), 1, next, ls...)
	for i := depth - 2; i >= 0; i-- {
		n = dnode(fmt.Sprintf("f%d", i), 1, next, n)
	}
	return withIDs(diffOf("cpu", "nanoseconds", 1, next, dnode("", 1, next, n)))
}

func TestAdmittedDiffExportAllocatesInProportionToItsPage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		short, deep   *profilemodel.Diff
		shortN, deepN int
	}{
		{"Deep vs itself, all matched", compared(t, pmfixture.Deep(1000), pmfixture.Deep(1000)), compared(t, pmfixture.Deep(3000), pmfixture.Deep(3000)), 1000, 3000},
		{"a chain over 3000 one-sided leaves", deepOverLeaves(100, 3000), deepOverLeaves(300, 3000), 100, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sa, sp := exportAlloc(t, tc.short)
			da, dp := exportAlloc(t, tc.deep)
			perShort, perDeep := float64(sa)/float64(sp), float64(da)/float64(dp)
			t.Logf("depth %d: page %d, allocated %d (%.1f per page byte); depth %d: page %d, allocated %d (%.1f)",
				tc.shortN, sp, sa, perShort, tc.deepN, dp, da, perDeep)
			assert.Less(t, perDeep, 1.5*perShort, "allocation per page byte does not grow with depth")
			assert.Less(t, da, uint64(12*dp+(4<<20)), "an admitted export allocates in proportion to its page")
		})
	}
}

func TestDiffPageBoundHoldsOneStackFramePerDepth(t *testing.T) {
	d := wideDiff(200_000, 8)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, nodes := diffPageBound(d, DiffPageOptions{})
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("bounding %d drawn of 200001 nodes allocated %d bytes", nodes, alloc)
	assert.Less(t, alloc, uint64(64<<10), "the walk holds a frame per depth, not a node per sibling")
}

func TestDiffExportRefusalNamesOnlyTheLimitExceeded(t *testing.T) {
	e := exportState(1000, 10, 500, 20)
	assert.Equal(t, "a static copy of this diff needs up to 1000 bytes, over its budget of 500 bytes", e.Reason)
	e = exportState(1000, 10, 0, 5)
	assert.Equal(t, "a static copy of this diff draws 10 nodes, over its budget of 5 nodes", e.Reason)
	e = exportState(1000, 10, 500, 5)
	assert.Equal(t, "a static copy of this diff needs up to 1000 bytes, over its budget of 500 bytes, and draws 10 nodes, over its budget of 5 nodes", e.Reason)
}

// Each value's bound holds on its own, as the page escapes the value:
// none leans on the chrome's slack.
func TestDiffValueBoundsHoldEachValue(t *testing.T) {
	values := []int64{math.MinInt64, math.MinInt64 + 1, -1 << 60, -999_999_999_999, -1000, -1, 0, 1, 9, 10, 999, 1000, 1023, 1024,
		999_999, 1_000_000, 999_999_999, 1_000_000_000, 1 << 50, 1<<62 + 12345, math.MaxInt64}
	for _, unit := range []string{"nanoseconds", "bytes", "count", "+&<>\"'"} {
		u := htmlEscapedBound(unit)
		for _, v := range values {
			for _, s := range []string{withHuman(strconv.FormatInt(v, 10), v, unit), signed(v, unit)} {
				assert.GreaterOrEqual(t, valueBound(v, u), htmlEscapedBound(s), "%q", s)
			}
		}
	}
	for _, x := range []float64{-100, -99.995, -1, -0.004, 0, 0.004, 0.995, 9.995, 99.995, 100, 12345.678, 1e300} {
		s := fmt.Sprintf("%+.2f", x)
		assert.GreaterOrEqual(t, pointsBound(x), htmlEscapedBound(s), "%q", s)
	}
}
