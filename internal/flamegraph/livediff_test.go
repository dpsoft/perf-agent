package flamegraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// compared is the diff of two fixture profiles' default views.
func compared(t *testing.T, base, next []byte) *profilemodel.Diff {
	t.Helper()
	d, err := profilemodel.Compare(exView(t, base, nil), exView(t, next, nil), profilemodel.WorkCounts{})
	require.NoError(t, err)
	return d
}

// withIDs gives every node of a literal diff node IDs, as Compare would:
// both sides for a node on both, one for a one-sided node.
func withIDs(d *profilemodel.Diff) *profilemodel.Diff {
	k := 0
	stack := []*profilemodel.DiffNode{d.Root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		k++
		if n.Presence != profilemodel.PresenceOnlyNew {
			n.BaseID = profilemodel.NodeID(fmt.Sprintf("b%05d", k))
		}
		if n.Presence != profilemodel.PresenceOnlyBase {
			n.NewID = profilemodel.NodeID(fmt.Sprintf("n%05d", k))
		}
		stack = append(stack, n.Children...)
	}
	return d
}

func diffLiveOf(t *testing.T, d *profilemodel.Diff, opts DiffLiveOptions) (map[string]any, []byte) {
	t.Helper()
	raw, err := DiffLiveModel(d, opts)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out, raw
}

// pageNode is one drawn node of WriteDiffPage, in document order.
type pageNode struct {
	width, classes, hue, tint, tooltip, label string
}

var pageNodeRE = regexp.MustCompile(`<div class="n" style="width:([^"%]*)%"><div class="([^"]*)" style="--h:([^;]*);--t:([^"]*)" title="([^"]*)"><span class="l">([^<]*)</span>`)

func pageNodes(t *testing.T, d *profilemodel.Diff) []pageNode {
	t.Helper()
	var out []pageNode
	for _, m := range pageNodeRE.FindAllStringSubmatch(pageOf(t, d), -1) {
		out = append(out, pageNode{width: m[1], classes: m[2], hue: m[3], tint: m[4], tooltip: html.UnescapeString(m[5]), label: html.UnescapeString(m[6])})
	}
	return out
}

// drawnOnPage lists the nodes WriteDiffPage draws, in pre-order, with
// their parents.
func drawnOnPage(d *profilemodel.Diff) (nodes, parents []*profilemodel.DiffNode) {
	type item struct{ n, parent *profilemodel.DiffNode }
	stack := []item{{n: d.Root}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		nodes, parents = append(nodes, it.n), append(parents, it.parent)
		for i := len(it.n.Children) - 1; i >= 0; i-- {
			c := it.n.Children[i]
			if c.Combined > 0 && float64(c.Combined) >= minDrawFraction*float64(d.Root.Combined) {
				stack = append(stack, item{n: c, parent: it.n})
			}
		}
	}
	return nodes, parents
}

func TestDiffLiveModelMatchesTheDiffPage(t *testing.T) {
	for name, d := range map[string]*profilemodel.Diff{
		"CPUBasic vs CPUBasicNew": compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew()),
		"sample":                  withIDs(sampleDiff()),
		"narrow nodes":            withIDs(sampleWithNarrowNodes()),
	} {
		t.Run(name, func(t *testing.T) {
			live, _ := diffLiveOf(t, d, DiffLiveOptions{Title: "diff"})
			assert.Equal(t, "diff", live["kind"])
			page := pageNodes(t, d)
			nodes, parents := drawnOnPage(d)
			drawn := list(t, live, "drawn")
			frames := list(t, live, "frames")
			require.Len(t, page, len(nodes), "the page draws the nodes the omission rule keeps")
			require.Len(t, drawn, len(page), "the model draws what the page draws")
			widthOf := map[*profilemodel.DiffNode]float64{}
			for i, p := range page {
				dr := drawn[i]
				n := nodes[i]
				f := frames[i]
				assert.Equal(t, strconv.Itoa(i), str(dr, "i"))
				assert.Equal(t, string(n.UnionID()), str(f, "id"))
				assert.Equal(t, "f"+strconv.Itoa(i), str(f, "ref"))
				// The page's width is of the parent, the model's of the root.
				pw, err := strconv.ParseFloat(p.width, 64)
				require.NoError(t, err)
				abs := pw
				if parents[i] != nil {
					abs = widthOf[parents[i]] * pw / 100
					assert.Equal(t, percent(n.Combined, parents[i].Combined), p.width)
				}
				widthOf[n] = abs
				mw, err := strconv.ParseFloat(str(dr, "width"), 64)
				require.NoError(t, err)
				assert.InDelta(t, abs, mw, 0.001*float64(len(parents)), "node %d width", i)
				assert.Equal(t, percent(n.Combined, d.Root.Combined), str(dr, "width"))
				assert.Equal(t, p.hue, str(dr, "hue"), "node %d hue", i)
				assert.Equal(t, p.tint, str(dr, "tint"), "node %d tint", i)
				assert.Equal(t, p.classes, str(dr, "cls"), "node %d classes", i)
				assert.Equal(t, p.tooltip, str(dr, "tip"), "node %d tooltip", i)
				marker, label := "", p.label
				if m, rest, ok := strings.Cut(p.label, " "); ok && m == "!" {
					marker, label = m, rest
				}
				assert.Equal(t, marker, str(dr, "marker"), "node %d marker", i)
				assert.Equal(t, label, str(f, "name"), "node %d name", i)
			}
		})
	}
}

func TestDiffLiveModelShowsEveryFactAsTheEngineGivesIt(t *testing.T) {
	d := compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	live, _ := diffLiveOf(t, d, DiffLiveOptions{Title: "b went down 111", BaseName: "old.pb.gz", NewName: "new.pb.gz"})
	assert.Equal(t, "b went down 111", live["doc_title"])
	segs := list(t, live, "title_segs")
	var marked []string
	for _, s := range segs {
		if str(s, "kind") == "unchecked" {
			marked = append(marked, str(s, "text"))
		}
	}
	assert.Equal(t, []string{"111"}, marked, "the title's numbers are marked, as an explained title's are")
	assert.Equal(t, "old.pb.gz", live["base_name"])
	assert.Equal(t, "new.pb.gz", live["new_name"])
	assert.Equal(t, "1000 nanoseconds (1.00 µs)", live["base_total"])
	assert.Equal(t, d.View, live["view"])
	byName := map[string]map[string]any{}
	for _, f := range list(t, live, "frames") {
		byName[str(f, "name")] = f
	}
	b := byName["b"]
	require.NotNil(t, b)
	assert.Equal(t, "411 nanoseconds (411 ns)", str(b, "base_cum"))
	assert.Equal(t, "300 nanoseconds (300 ns)", str(b, "new_cum"))
	assert.Equal(t, "-111 nanoseconds (-111 ns)", str(b, "delta_abs"))
	assert.Equal(t, "-11.1%", str(b, "delta_share"))
	assert.True(t, strings.HasPrefix(str(b, "delta_share_title"), "-0.111 ratio of view_total"), "the title has the exact digits: %s", str(b, "delta_share_title"))
	assert.Equal(t, "unavailable: no work count", str(b, "delta_per_unit"))
	assert.Equal(t, "both", str(b, "presence"))
	d2 := byName["d"]
	require.NotNil(t, d2)
	assert.Equal(t, "only_new", str(d2, "presence"))
	assert.Equal(t, "0 nanoseconds (0 ns)", str(d2, "base_cum"), "a measured zero on one side is zero")
	assert.Equal(t, str(byName["a"], "id"), str(b, "parent"))
	assert.Equal(t, "", str(byName["all"], "parent"))
	ex := live["export"].(map[string]any)
	assert.Equal(t, true, ex["ok"])
	assert.Equal(t, "", ex["reason"])
	assert.Equal(t, string(d.Root.UnionID()), live["selected"])
	assert.Equal(t, "", live["focus"])
}

func TestDiffLiveModelIsFlatOnDeepStacks(t *testing.T) {
	d := compared(t, pmfixture.Deep(2000), pmfixture.DeepNew(2000))
	live, raw := diffLiveOf(t, d, DiffLiveOptions{})
	assert.LessOrEqual(t, jsonDepth(t, raw), 8)
	drawn := list(t, live, "drawn")
	assert.GreaterOrEqual(t, len(drawn), 2001, "the chain, its new leaf and the root are drawn")
	assert.Equal(t, "40040", live["height"], "2002 rows: the root, the chain and its new leaf")
	for _, dr := range drawn {
		_, err := strconv.Atoi(str(dr, "top"))
		assert.NoError(t, err, "top %v", dr["top"])
	}
}

func TestDiffLiveModelNeverShowsUnavailableAsZero(t *testing.T) {
	// One side empty: no node has a share of its view's total.
	root := dnode("", 0, 200, dnode("added_fn", 0, 150), dnode("other", 0, 50))
	d := withIDs(diffOf("cpu", "nanoseconds", 0, 200, root))
	live, _ := diffLiveOf(t, d, DiffLiveOptions{})
	for _, f := range list(t, live, "frames") {
		assert.Equal(t, "unavailable: zero total", str(f, "delta_share"), "frame %s", str(f, "name"))
		assert.NotEqual(t, "0", str(f, "delta_share"))
		assert.True(t, strings.HasPrefix(str(f, "delta_per_unit"), "unavailable: "), "frame %s", str(f, "name"))
	}
	assert.Contains(t, live["warnings"], "One view is empty, so no node has a share of its total to compare; every node is grey and only the raw change is shown.")

	// A real zero stays "0".
	same := compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasic())
	live, _ = diffLiveOf(t, same, DiffLiveOptions{})
	for _, f := range list(t, live, "frames") {
		assert.Equal(t, "0", str(f, "delta_share"), "frame %s", str(f, "name"))
	}
}

func TestDiffLiveModelHugeValues(t *testing.T) {
	d := compared(t, pmfixture.HugeValue(), pmfixture.HugeValue())
	_, raw := diffLiveOf(t, d, DiffLiveOptions{})
	assert.Contains(t, string(raw), "9007199254740993")
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		_, isNum := tok.(json.Number)
		assert.False(t, isNum, "bare number %v: every value is a string", tok)
	}
}

// wideDiff is a root over n children, every one a node on both sides with
// a name of nameLen bytes and its own IDs.
func wideDiff(n, nameLen int) *profilemodel.Diff {
	var kids []*profilemodel.DiffNode
	for i := range n {
		name := fmt.Sprintf("fn%05d", i)
		name += strings.Repeat("x", max(0, nameLen-len(name)))
		kids = append(kids, withShare(dnode(name, 10, 10+int64(i%7)), 10*int64(n), 10*int64(n)+int64(3*n)))
	}
	var base, next int64
	for _, k := range kids {
		base, next = base+k.BaseCum, next+k.NewCum
	}
	return withIDs(diffOf("cpu", "nanoseconds", base, next, dnode("", base, next, kids...)))
}

func TestDiffLiveModelStaysUnderItsByteBudget(t *testing.T) {
	d := wideDiff(1500, 300)
	live, raw := diffLiveOf(t, d, DiffLiveOptions{MaxBytes: 64 << 10})
	assert.LessOrEqual(t, len(raw), 64<<10)
	assert.Greater(t, len(raw), 32<<10, "the budget is spent on frames, not wasted")
	assert.Regexp(t, `^\d+ nodes beyond the page's size budget are not drawn\.$`, live["size_note"])

	_, err := DiffLiveModel(d, DiffLiveOptions{MaxBytes: 512})
	require.ErrorIs(t, err, ErrPageTooLarge)
	assert.Contains(t, err.Error(), "512")
}

func TestDiffLiveModelCountsWhatItLeavesOut(t *testing.T) {
	// Children of 1 in 10000 are narrower than the page draws; one has a
	// child of its own, which goes with it.
	kids := []*profilemodel.DiffNode{dnode("bulk", 4990, 4990), dnode("narrow_a", 0, 1), dnode("narrow_b", 1, 0, dnode("under", 1, 0)), dnode("zero", 0, 0)}
	d := withIDs(diffOf("cpu", "nanoseconds", 4992, 4991, dnode("", 4992, 4991, kids...)))
	live, _ := diffLiveOf(t, d, DiffLiveOptions{})
	page := pageOf(t, d)
	m := regexp.MustCompile(`<p class="meta">(\d+ nodes under 0\.05% of the total are not drawn\.)</p>`).FindStringSubmatch(page)
	require.NotNil(t, m)
	assert.Equal(t, "3 nodes under 0.05% of the total are not drawn.", m[1])
	assert.Equal(t, m[1], live["narrow_note"], "the model counts the narrow nodes as the page does")

	live, _ = diffLiveOf(t, wideDiff(40, 10), DiffLiveOptions{MaxNodes: 5})
	assert.Len(t, list(t, live, "drawn"), 5)
	assert.Equal(t, "5 of 41 frames drawn; 36 left out by the page's limit of 5 frames.", live["limit_note"])
}

func TestDiffLiveModelFocusLaysOutTheSubtree(t *testing.T) {
	d := compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	ix := profilemodel.NewDiffIndex(d)
	var a *profilemodel.DiffNode
	for _, n := range d.Root.Children[0].Children {
		if n.Name == "a" {
			a = n
		}
	}
	require.NotNil(t, a)
	live, _ := diffLiveOf(t, d, DiffLiveOptions{Focus: a.UnionID()})
	assert.Equal(t, string(a.UnionID()), live["focus"])
	assert.Equal(t, string(a.UnionID()), live["selected"])
	drawn := list(t, live, "drawn")
	frames := list(t, live, "frames")
	require.NotEmpty(t, drawn)
	assert.Equal(t, string(a.UnionID()), str(frames[0], "id"))
	assert.Equal(t, "0", str(drawn[0], "top"))
	assert.Equal(t, "100.0000", str(drawn[0], "width"))
	assert.Equal(t, "0.0000", str(drawn[0], "left"))
	assert.Contains(t, live["axis"], "widths relative to a")
	// The focus's callers are in the model, undrawn, so the path panel can
	// name them.
	ids := map[string]bool{}
	for _, f := range frames {
		ids[str(f, "id")] = true
	}
	for _, p := range ix.Path(a.UnionID()) {
		assert.True(t, ids[string(p.UnionID())], "caller %s is in the model", p.Name)
	}
	assert.Len(t, drawn, 2, "a and b")

	_, err := DiffLiveModel(d, DiffLiveOptions{Focus: "nope/nope"})
	require.ErrorIs(t, err, profilemodel.ErrUnknownNode)
}

func TestDiffLiveModelCapsLongNames(t *testing.T) {
	long := strings.Repeat("ž", 32<<10) // 64 KiB
	root := dnode("", 100, 200, withShare(dnode(long, 100, 200), 100, 200))
	d := withIDs(diffOf("cpu", "nanoseconds", 100, 200, root))
	live, raw := diffLiveOf(t, d, DiffLiveOptions{})
	assert.Less(t, len(raw), 8<<10, "the model does not carry the whole name")
	frames := list(t, live, "frames")
	drawn := list(t, live, "drawn")
	require.Len(t, frames, 2)
	name := str(frames[1], "name")
	assert.LessOrEqual(t, len(name), maxShownName)
	assert.True(t, utf8.ValidString(name))
	assert.True(t, strings.HasSuffix(name, "…"))
	tip := str(drawn[1], "tip")
	first, _, _ := strings.Cut(tip, "\n")
	assert.Equal(t, name, first, "the tooltip names the frame as the frame does")
	child := d.Root.Children[0]
	assert.Equal(t, string(child.UnionID()), str(frames[1], "id"), "the union ID is the node's exact identity")
	assert.Contains(t, tip, "base id "+string(child.BaseID)+", new id "+string(child.NewID))

	ix := profilemodel.NewDiffIndex(d)
	fraw, err := DiffLiveFrame(d, ix, child.UnionID())
	require.NoError(t, err)
	assert.Less(t, len(fraw), 8<<10, "the frame response does not carry the whole name")
}

func TestDiffLiveFrameFindsANodeTheModelLeftOut(t *testing.T) {
	d := wideDiff(600, 64)
	ix := profilemodel.NewDiffIndex(d)
	live, _ := diffLiveOf(t, d, DiffLiveOptions{MaxBytes: 64 << 10})
	inModel := map[string]bool{}
	for _, f := range list(t, live, "frames") {
		inModel[str(f, "id")] = true
	}
	var x *profilemodel.DiffNode
	for _, c := range d.Root.Children {
		if !inModel[string(c.UnionID())] {
			x = c
			break
		}
	}
	require.NotNil(t, x, "the budget left a node out")

	raw, err := DiffLiveFrame(d, ix, x.UnionID())
	require.NoError(t, err)
	var out struct {
		Node  string           `json:"node"`
		Frame map[string]any   `json:"frame"`
		Path  []map[string]any `json:"path"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, string(x.UnionID()), out.Node)
	assert.Equal(t, string(x.UnionID()), str(out.Frame, "id"))
	assert.Equal(t, "", str(out.Frame, "ref"), "a single frame knows no model's refs")
	assert.Equal(t, x.Name, str(out.Frame, "name"))
	assert.Equal(t, signed(x.DeltaAbs, "nanoseconds"), str(out.Frame, "delta_abs"))
	assert.Equal(t, withHuman(strconv.FormatInt(x.BaseCum, 10), x.BaseCum, "nanoseconds"), str(out.Frame, "base_cum"))
	assert.Equal(t, string(d.Root.UnionID()), str(out.Frame, "parent"))
	require.Len(t, out.Path, 1, "the path runs from below the root to the node")
	assert.Equal(t, string(x.UnionID()), str(out.Path[0], "id"))
	assert.Equal(t, x.Name, str(out.Path[0], "name"))
	assert.Equal(t, str(out.Frame, "delta_share"), str(out.Path[0], "delta_share"))
	assert.Equal(t, "", str(out.Path[0], "ref"))

	// The same frame as the model shows it, for a node it holds.
	first := d.Root.Children[0]
	require.True(t, inModel[string(first.UnionID())])
	raw, err = DiffLiveFrame(d, ix, first.UnionID())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &out))
	for _, f := range list(t, live, "frames") {
		if str(f, "id") == string(first.UnionID()) {
			f["ref"] = ""
			assert.Equal(t, f, out.Frame)
		}
	}

	_, err = DiffLiveFrame(d, ix, "nope/nope")
	require.True(t, errors.Is(err, profilemodel.ErrUnknownNode), "err = %v", err)
}

func TestWriteDiffPageUnchanged(t *testing.T) {
	checkGolden(t, "diffpage.golden.html", pageOf(t, sampleDiff()))
}

// HTTP handlers read a published diff and its index from many goroutines:
// every reader leaves both as they were.
func TestDiffLiveReadersShareTheDiff(t *testing.T) {
	d := compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	ix := profilemodel.NewDiffIndex(d)
	nodes := d.Flatten().Nodes
	want, err := DiffLiveModel(d, DiffLiveOptions{MaxBytes: 1 << 20})
	require.NoError(t, err)
	retained, indexed := d.RetainedBytes(), ix.RetainedBytes()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 20 {
				n := nodes[(g+i)%len(nodes)]
				if _, err := DiffLiveFrame(d, ix, n.ID); err != nil {
					t.Error(err)
				}
				if _, err := DiffLiveModel(d, DiffLiveOptions{Focus: n.ID}); err != nil {
					t.Error(err)
				}
				var b bytes.Buffer
				if err := WriteDiffPageBounded(&b, d, DiffPageOptions{}, 1<<20, 0); err != nil {
					t.Error(err)
				}
				_ = d.RetainedBytes() + ix.RetainedBytes()
			}
		})
	}
	wg.Wait()
	got, err := DiffLiveModel(d, DiffLiveOptions{MaxBytes: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
	assert.Equal(t, retained, d.RetainedBytes())
	assert.Equal(t, indexed, ix.RetainedBytes())
}
