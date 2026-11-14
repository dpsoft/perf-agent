package flamegraph

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

func shareFact(r float64) profilemodel.Fact {
	return profilemodel.Fact{Ratio: &r, Unit: "ratio", Status: profilemodel.StatusOK}
}

func unavailableShare() profilemodel.Fact {
	return profilemodel.Fact{Unit: "ratio", Status: profilemodel.StatusUnavailable, Reason: "zero total"}
}

func dnode(name string, base, next int64, kids ...*profilemodel.DiffNode) *profilemodel.DiffNode {
	n := &profilemodel.DiffNode{
		Name: name, Presence: profilemodel.PresenceBoth, Interpretation: profilemodel.Comparable,
		BaseCum: base, NewCum: next, Combined: base + next, DeltaAbs: next - base,
		DeltaShare: unavailableShare(), Children: kids,
	}
	switch {
	case base == 0:
		n.Presence = profilemodel.PresenceOnlyNew
	case next == 0:
		n.Presence = profilemodel.PresenceOnlyBase
	}
	return n
}

func withShare(n *profilemodel.DiffNode, base, next int64) *profilemodel.DiffNode {
	n.DeltaShare = shareFact(float64(n.NewCum)/float64(next) - float64(n.BaseCum)/float64(base))
	return n
}

func diffOf(sampleType, unit string, baseTotal, newTotal int64, root *profilemodel.DiffNode) *profilemodel.Diff {
	return &profilemodel.Diff{
		BaseHandle: "base.pb", NewHandle: "new.pb", SampleType: sampleType, Unit: unit,
		BaseTotal: baseTotal, NewTotal: newTotal, QualityWarnings: []string{}, Root: root,
	}
}

func pageOf(t *testing.T, d *profilemodel.Diff) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteDiffPage(&b, d, "diff"); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func sampleDiff() *profilemodel.Diff {
	removed := dnode("removed_fn", 30, 0)
	removed.Why = "frame unresolved on the new side"
	added := dnode("added_fn", 0, 40)
	root := dnode("", 100, 200,
		withShare(dnode("kept", 50, 60), 100, 200),
		removed, added)
	root.DeltaShare = shareFact(0)
	return diffOf("cpu", "nanoseconds", 100, 200, root)
}

func TestDiffPageNeverSaysFasterOrSlower(t *testing.T) {
	for _, d := range []*profilemodel.Diff{sampleDiff(), diffOf("alloc_space", "bytes", 100, 200, dnode("", 100, 200))} {
		page := strings.ToLower(pageOf(t, d))
		for _, w := range []string{"faster", "slower"} {
			if strings.Contains(page, w) {
				t.Errorf("page says %q", w)
			}
		}
	}
}

func TestDiffPageListsRemovedPathsWithInterpretation(t *testing.T) {
	page := html.UnescapeString(pageOf(t, sampleDiff()))
	for _, want := range []string{"removed_fn", "only_base", "-30 nanoseconds", "comparable", "frame unresolved on the new side", "added_fn", "only_new", "+40 nanoseconds", "does not prove"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestDiffPageEscapesNames(t *testing.T) {
	root := dnode("", 10, 10, dnode("<script>alert(1)</script>", 10, 10))
	page := pageOf(t, diffOf("cpu", "nanoseconds", 10, 10, root))
	if strings.Contains(page, "<script>") {
		t.Error("unescaped <script> in page")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("escaped name missing")
	}
}

func TestDiffPageHasNoExternalAssets(t *testing.T) {
	page := pageOf(t, sampleDiff())
	if re := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]|src=|@import|<link|<script`); re.MatchString(page) {
		t.Errorf("external reference: %q", re.FindString(page))
	}
}

func TestDiffPageStylesSurviveEscaping(t *testing.T) {
	if strings.Contains(pageOf(t, sampleDiff()), "ZgotmplZ") {
		t.Error("html/template rejected a style value")
	}
}

func TestDiffPageSharePlusRawCounterexample(t *testing.T) {
	page := pageOf(t, sampleDiff())
	m := regexp.MustCompile(`<div class="([^"]*)"[^>]*title="([^"]*)">kept</div>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no kept node")
	}
	if !strings.Contains(m[1], "share-lower") || strings.Contains(m[1], "share-higher") {
		t.Errorf("kept classes %q, want share-lower", m[1])
	}
	if !strings.Contains(html.UnescapeString(m[2]), "+10 nanoseconds") {
		t.Errorf("tooltip %q lacks +10", m[2])
	}
}

func TestDiffPageLegendUsesTheViewsUnit(t *testing.T) {
	root := dnode("", 100, 200, dnode("a", 50, 60))
	page := pageOf(t, diffOf("alloc_space", "bytes", 100, 200, root))
	if !strings.Contains(page, "lower / higher share of total alloc_space (bytes)") {
		t.Error("legend lacks sample type and unit")
	}
	if strings.Contains(strings.ToLower(page), "time") {
		t.Error("page says time for a bytes view")
	}
}

func TestDiffPageAllZero(t *testing.T) {
	page := pageOf(t, diffOf("cpu", "nanoseconds", 0, 0, dnode("", 0, 0)))
	if !strings.Contains(page, "Both views are empty") {
		t.Error("no empty statement")
	}
	if strings.Contains(page, `class="icicle"`) {
		t.Error("icicle drawn for empty views")
	}
	for _, bad := range []string{"NaN", "Infinity", "Inf"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains %s", bad)
		}
	}
}

func TestDiffPageShowsWarningsAndMarksNotComparable(t *testing.T) {
	d := sampleDiff()
	d.QualityWarnings = []string{"sample periods differ"}
	nc := d.Root.Children[0]
	nc.Interpretation, nc.Why = profilemodel.NotComparable, "module libfoo unresolved share moved"
	page := pageOf(t, d)
	for _, want := range []string{"sample periods differ", "not-comparable", "module libfoo unresolved share moved"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestDiffPageWidthIsCombinedOverRoot(t *testing.T) {
	page := pageOf(t, sampleDiff())
	// removed_fn: 30 of the root's 300 combined.
	if !strings.Contains(page, `style="width:10.0000%"><div class="b share-none only-base"`) {
		t.Error("removed node is not 10% wide")
	}
}

func TestWriteDiffPageSample(t *testing.T) {
	dir := os.Getenv("DIFFPAGE_SAMPLE_DIR")
	if dir == "" {
		t.Skip("DIFFPAGE_SAMPLE_DIR not set")
	}
	if err := os.WriteFile(dir+"/task-7-sample.html", []byte(pageOf(t, sampleDiff())), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiffPageCountsUndrawnNodes(t *testing.T) {
	root := dnode("", 100000, 100000, dnode("big", 99990, 99990), dnode("tiny1", 4, 4), dnode("tiny2", 3, 3))
	page := pageOf(t, diffOf("cpu", "nanoseconds", 100000, 100000, root))
	if !strings.Contains(page, "2 nodes under 0.05% of the total are not drawn") {
		t.Error("omitted count missing")
	}
	if strings.Contains(pageOf(t, sampleDiff()), "not drawn") {
		t.Error("omitted line shown with nothing omitted")
	}
}

func TestDiffPageTableCapKeepsLargestRemovals(t *testing.T) {
	var kids []*profilemodel.DiffNode
	for i := range maxTableRows + 20 {
		kids = append(kids, dnode(fmt.Sprintf("small_%03d", i), 1, 0))
	}
	kids = append(kids, dnode("huge_removal", 5000, 0))
	root := dnode("", 10000, 10000, kids...)
	page := pageOf(t, diffOf("cpu", "nanoseconds", 10000, 10000, root))
	if !strings.Contains(page, "huge_removal") {
		t.Error("largest removal lost to the cap")
	}
	if !strings.Contains(page, "Showing 500 of 521") {
		t.Error("cap not stated")
	}
}

func TestDiffPageShowsSamplingNotes(t *testing.T) {
	d := sampleDiff()
	d.BaseNotes = []string{"launches sampled 1 in 8"}
	d.NewNotes = []string{"no sampling here"}
	page := pageOf(t, d)
	for _, want := range []string{"base: launches sampled 1 in 8", "new: no sampling here"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestDiffPageNilDiff(t *testing.T) {
	if err := WriteDiffPage(&bytes.Buffer{}, nil, "x"); err == nil {
		t.Error("nil diff accepted")
	}
}
