package profilemodel

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

func compareOf(t *testing.T, base, next []byte, work WorkCounts) *Diff {
	t.Helper()
	d, err := Compare(viewOf(t, base), viewOf(t, next), work)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func pairDiff(t *testing.T, name string) *Diff {
	t.Helper()
	p, ok := diffPairs()[name]
	if !ok {
		t.Fatalf("no diff pair %q", name)
	}
	return compareOf(t, p.base(), p.new(), WorkCounts{})
}

// diffsAt returns every union node whose root-first name path is names.
func diffsAt(d *Diff, names ...string) []*DiffNode {
	cur := []*DiffNode{d.Root}
	for _, name := range names {
		var next []*DiffNode
		for _, n := range cur {
			for _, c := range n.Children {
				if c.Name == name {
					next = append(next, c)
				}
			}
		}
		cur = next
	}
	return cur
}

func oneDiffAt(t *testing.T, d *Diff, names ...string) *DiffNode {
	t.Helper()
	ns := diffsAt(d, names...)
	if len(ns) != 1 {
		t.Fatalf("path %v: %d union nodes, want 1", names, len(ns))
	}
	return ns[0]
}

func walkDiff(n *DiffNode, fn func(*DiffNode)) {
	fn(n)
	for _, c := range n.Children {
		walkDiff(c, fn)
	}
}

func wantPresence(t *testing.T, n *DiffNode, p Presence, m Match, i Interpretation) {
	t.Helper()
	if n.Presence != p || n.Match != m || n.Interpretation != i {
		t.Fatalf("%s: {%s %q %s} why %q, want {%s %q %s}", n.Name, n.Presence, n.Match, n.Interpretation, n.Why, p, m, i)
	}
}

// everyDiff runs fn on every diff pair, on every fixture against itself,
// and on every two fixtures with the same sample type and unit.
func everyDiff(t *testing.T, fn func(t *testing.T, d *Diff)) {
	work := WorkCounts{Base: 3, New: 7}
	for name, p := range diffPairs() {
		t.Run("pair/"+name, func(t *testing.T) { fn(t, compareOf(t, p.base(), p.new(), work)) })
	}
	fixtures := allFixtures()
	names := slices.Sorted(maps.Keys(fixtures))
	for _, a := range names {
		for _, b := range names {
			t.Run("fixtures/"+a+"/"+b, func(t *testing.T) {
				d, err := Compare(viewOf(t, fixtures[a]()), viewOf(t, fixtures[b]()), work)
				if errors.Is(err, ErrIncompatibleViews) {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				fn(t, d)
			})
		}
	}
}

func TestDiffTotalsEqualInputs(t *testing.T) {
	d := compareOf(t,
		cpuProfile(fx{stack: []string{"main"}, value: 100}),
		cpuProfile(fx{stack: []string{"main"}, value: 80}),
		WorkCounts{})
	if d.BaseTotal != 100 || d.NewTotal != 80 {
		t.Fatalf("totals %d, %d", d.BaseTotal, d.NewTotal)
	}
	r := d.Root
	if r.BaseCum != 100 || r.NewCum != 80 || r.DeltaAbs != -20 || r.Combined != 180 {
		t.Fatalf("root %+v", r)
	}
	if r.Presence != PresenceBoth || r.Match != "" || r.Interpretation != Comparable {
		t.Fatalf("root %s %q %s", r.Presence, r.Match, r.Interpretation)
	}
	if d.SampleType != "cpu" || d.Unit != "nanoseconds" {
		t.Fatalf("view %s/%s", d.SampleType, d.Unit)
	}
	wantRatio(t, "root share delta", r.DeltaShare, 0, DenominatorViewTotal, StatusZero)
}

func TestUnionGeometryIsAdditive(t *testing.T) {
	everyDiff(t, func(t *testing.T, d *Diff) {
		walkDiff(d.Root, func(n *DiffNode) {
			sum := n.BaseSelf + n.NewSelf
			for _, c := range n.Children {
				sum += c.Combined
			}
			if n.Combined != sum {
				t.Errorf("%s: combined %d, children and self %d", n.Name, n.Combined, sum)
			}
			if n.Combined != n.BaseCum+n.NewCum || n.DeltaAbs != n.NewCum-n.BaseCum {
				t.Errorf("%s: combined %d delta %d from %d, %d", n.Name, n.Combined, n.DeltaAbs, n.BaseCum, n.NewCum)
			}
		})
		if d.Root.Combined != d.BaseTotal+d.NewTotal {
			t.Errorf("root combined %d, totals %d + %d", d.Root.Combined, d.BaseTotal, d.NewTotal)
		}
	})
}

// The structural invariants of every union: each non-zero node of each
// view appears exactly once, presence agrees with the nodes, matches and
// candidates agree with presence, children are in order, and every fact
// is well formed and marshals.
func TestDiffInvariantsOnEveryPair(t *testing.T) {
	everyDiff(t, func(t *testing.T, d *Diff) {
		seen := [2]map[*Node]int{{}, {}}
		walkDiff(d.Root, func(n *DiffNode) {
			for s, node := range [2]*Node{n.Base, n.New} {
				if node != nil {
					seen[s][node]++
				}
			}
			switch n.Presence {
			case PresenceBoth:
				if n.Base == nil || n.New == nil {
					t.Errorf("%s: both with a missing side", n.Name)
				}
				if n != d.Root && !slices.Contains([]Match{MatchExact, MatchByPath, MatchByName}, n.Match) {
					t.Errorf("%s: both with match %q", n.Name, n.Match)
				}
			case PresenceOnlyBase, PresenceOnlyNew:
				if (n.Base == nil) != (n.Presence == PresenceOnlyNew) || (n.New == nil) != (n.Presence == PresenceOnlyBase) {
					t.Errorf("%s: %s with base %v new %v", n.Name, n.Presence, n.Base != nil, n.New != nil)
				}
				if n.Match != "" && n.Match != MatchAmbiguous {
					t.Errorf("%s: %s with match %q", n.Name, n.Presence, n.Match)
				}
			default:
				t.Errorf("%s: presence %q", n.Name, n.Presence)
			}
			if (n.Match == MatchAmbiguous) != (len(n.Candidates) > 0) {
				t.Errorf("%s: match %q with %d candidates", n.Name, n.Match, len(n.Candidates))
			}
			other := SideNew
			if n.Presence == PresenceOnlyNew {
				other = SideBase
			}
			for _, c := range n.Candidates {
				if c.Side != other || c.Node == "" || c.Handle == "" {
					t.Errorf("%s: candidate %+v", n.Name, c)
				}
			}
			if n.Base != nil {
				if n.BaseID != n.Base.ID || n.BaseCum != n.Base.Cum || n.BaseSelf != n.Base.Self || n.Name != n.Base.Function.Name {
					t.Errorf("%s: base fields disagree with the node", n.Name)
				}
				if n != d.Root && n.Base.Cum == 0 {
					t.Errorf("%s: a zero-value base node is present", n.Name)
				}
			} else if n.BaseID != "" || n.BaseCum != 0 || n.BaseSelf != 0 {
				t.Errorf("%s: base fields without a base node", n.Name)
			}
			if n.New != nil {
				if n.NewID != n.New.ID || n.NewCum != n.New.Cum || n.NewSelf != n.New.Self || n.Name != n.New.Function.Name {
					t.Errorf("%s: new fields disagree with the node", n.Name)
				}
				if n != d.Root && n.New.Cum == 0 {
					t.Errorf("%s: a zero-value new node is present", n.Name)
				}
			} else if n.NewID != "" || n.NewCum != 0 || n.NewSelf != 0 {
				t.Errorf("%s: new fields without a new node", n.Name)
			}
			switch n.Interpretation {
			case Comparable:
			case NotComparable:
				if n.Why == "" {
					t.Errorf("%s: not comparable without a reason", n.Name)
				}
			default:
				t.Errorf("%s: interpretation %q", n.Name, n.Interpretation)
			}
			if !slices.IsSortedFunc(n.Children, func(a, b *DiffNode) int {
				return cmp.Or(cmp.Compare(absDelta(b.DeltaAbs), absDelta(a.DeltaAbs)), cmp.Compare(a.Name, b.Name))
			}) {
				t.Errorf("%s: children out of order", n.Name)
			}
			checkFact(t, n.Name+" share delta", n.DeltaShare, RoundingFor(MetricShareDelta))
			if n.DeltaShare.Status != StatusUnavailable && n.DeltaShare.Denominator != DenominatorViewTotal {
				t.Errorf("%s: share delta denominator %q", n.Name, n.DeltaShare.Denominator)
			}
			if (n.DeltaPerUnit != nil) != d.PerUnit.OK {
				t.Errorf("%s: per-unit delta %v with diff per-unit %+v", n.Name, n.DeltaPerUnit, d.PerUnit)
			}
			if n.DeltaPerUnit != nil {
				checkFact(t, n.Name+" per-unit delta", *n.DeltaPerUnit, RoundingFor(MetricPerWorkDelta))
				if n.DeltaPerUnit.Unit != d.Unit+"/work" {
					t.Errorf("%s: per-unit unit %q", n.Name, n.DeltaPerUnit.Unit)
				}
			}
		})
		for s, side := range []*Node{d.Root.Base, d.Root.New} {
			var walk func(*Node)
			walk = func(n *Node) {
				if want := 1; n == side || n.Cum > 0 {
					if seen[s][n] != want {
						t.Errorf("side %d node %s (%s) appears %d times", s, n.ID, n.Function.Name, seen[s][n])
					}
				} else if seen[s][n] != 0 {
					t.Errorf("side %d zero node %s appears", s, n.Function.Name)
				}
				for _, c := range n.Children {
					walk(c)
				}
			}
			walk(side)
		}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"NaN", "Inf"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Fatalf("diff JSON contains %s", bad)
			}
		}
	})
}

// One profile pair mixes the two main paths: the rebuilt application
// matches by name, the library that was not recompiled matches exactly.
func TestRebuiltBinaryMatchesByName(t *testing.T) {
	d := pairDiff(t, "rebuilt-app-unchanged-lib")
	wantPresence(t, oneDiffAt(t, d, "main"), PresenceBoth, MatchByName, Comparable)
	wantPresence(t, oneDiffAt(t, d, "main", "work"), PresenceBoth, MatchByName, Comparable)
}

func TestUnchangedLibraryMatchesExactly(t *testing.T) {
	d := pairDiff(t, "rebuilt-app-unchanged-lib")
	c := oneDiffAt(t, d, "main", "compress")
	wantPresence(t, c, PresenceBoth, MatchExact, Comparable)
	if c.BaseCum != 40 || c.NewCum != 50 || c.DeltaAbs != 10 {
		t.Fatalf("compress %d → %d (%d)", c.BaseCum, c.NewCum, c.DeltaAbs)
	}
	// Children are ordered by the size of the change: work moved by 30.
	if names := []string{d.Root.Children[0].Children[0].Name, d.Root.Children[0].Children[1].Name}; !slices.Equal(names, []string{"work", "compress"}) {
		t.Fatalf("children %v", names)
	}
}

func TestMissingBuildIDIsNeverExact(t *testing.T) {
	d := pairDiff(t, "no-build-id")
	wantPresence(t, oneDiffAt(t, d, "main"), PresenceBoth, MatchByPath, Comparable)
	wantPresence(t, oneDiffAt(t, d, "main", "foo"), PresenceBoth, MatchByPath, Comparable)
}

// Exact needs a build id on both sides: the keys of a frame with a build
// id and of the same frame without one are both module identities, and
// equal paths alone are by_path.
func TestBuildIDOnOneSideIsByPath(t *testing.T) {
	with := rich("cpu", "nanoseconds", rs{stack: []frame{in("/usr/bin/app", "a", 1, ln("main"))}, value: 1})
	without := rich("cpu", "nanoseconds", rs{stack: []frame{in("/usr/bin/app", "", 1, ln("main"))}, value: 1})
	for _, d := range []*Diff{compareOf(t, with, without, WorkCounts{}), compareOf(t, without, with, WorkCounts{})} {
		wantPresence(t, oneDiffAt(t, d, "main"), PresenceBoth, MatchByPath, Comparable)
	}
}

func TestMovedModulePathMatchesByName(t *testing.T) {
	d := pairDiff(t, "moved-module")
	f := oneDiffAt(t, d, "main", "foo")
	wantPresence(t, f, PresenceBoth, MatchByName, Comparable)
	if f.DeltaAbs != 2 {
		t.Fatalf("delta %d", f.DeltaAbs)
	}
}

func TestAmbiguityInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		pair       string
		base, next int // foo nodes on each side
	}{{"one-vs-two", 1, 2}, {"two-vs-one", 2, 1}} {
		t.Run(tc.pair, func(t *testing.T) {
			d := pairDiff(t, tc.pair)
			foos := diffsAt(d, "main", "foo")
			if len(foos) != tc.base+tc.next {
				t.Fatalf("%d foo nodes, want %d: nothing may merge", len(foos), tc.base+tc.next)
			}
			var onlyBase, onlyNew int
			for _, f := range foos {
				if f.Match != MatchAmbiguous {
					t.Fatalf("%s %s: match %q, want ambiguous", f.Name, f.Presence, f.Match)
				}
				switch f.Presence {
				case PresenceOnlyBase:
					onlyBase++
					if len(f.Candidates) != tc.next {
						t.Fatalf("base foo has %d candidates, want %d", len(f.Candidates), tc.next)
					}
				case PresenceOnlyNew:
					onlyNew++
					if len(f.Candidates) != tc.base {
						t.Fatalf("new foo has %d candidates, want %d", len(f.Candidates), tc.base)
					}
				default:
					t.Fatalf("foo presence %s", f.Presence)
				}
			}
			if onlyBase != tc.base || onlyNew != tc.next {
				t.Fatalf("%d only_base, %d only_new", onlyBase, onlyNew)
			}
		})
	}
}

func TestAmbiguousCandidatesCarrySide(t *testing.T) {
	p := diffPairs()["one-vs-two"]
	base, next := viewOf(t, p.base()), viewOf(t, p.new())
	if base.Handle == next.Handle {
		t.Fatal("fixture views share a handle")
	}
	d, err := Compare(base, next, WorkCounts{})
	if err != nil {
		t.Fatal(err)
	}
	newFoos := nodesAt(next, "main", "foo")
	for _, f := range diffsAt(d, "main", "foo") {
		switch f.Presence {
		case PresenceOnlyBase:
			var ids []NodeID
			for _, c := range f.Candidates {
				if c.Side != SideNew || c.Handle != next.Handle {
					t.Fatalf("base foo candidate %+v, want side new handle %s", c, next.Handle)
				}
				if _, ok := next.Node(c.Node); !ok {
					t.Fatalf("candidate %s is not in the new view", c.Node)
				}
				ids = append(ids, c.Node)
			}
			for _, n := range newFoos {
				if !slices.Contains(ids, n.ID) {
					t.Fatalf("candidates %v miss %s", ids, n.ID)
				}
			}
		case PresenceOnlyNew:
			if len(f.Candidates) != 1 {
				t.Fatalf("new foo candidates %+v", f.Candidates)
			}
			c := f.Candidates[0]
			if c.Side != SideBase || c.Handle != base.Handle || c.Node != oneNodeAt(t, base, "main", "foo").ID {
				t.Fatalf("new foo candidate %+v", c)
			}
		}
	}
}

func TestRemovedPathStaysInTheUnion(t *testing.T) {
	d := pairDiff(t, "removed-kernel")
	k := oneDiffAt(t, d, "main", "do_syscall_64")
	wantPresence(t, k, PresenceOnlyBase, "", Comparable)
	if k.DeltaAbs != -37 || k.BaseCum != 37 || k.NewCum != 0 || k.Combined != 37 || k.New != nil || k.NewID != "" {
		t.Fatalf("kernel %+v", k)
	}
	if k.Why != "" {
		t.Fatalf("why %q on an equal-quality removal", k.Why)
	}
	wantPresence(t, oneDiffAt(t, d, "main", "work"), PresenceBoth, MatchByName, Comparable)
}

func TestDegradedSymbolsArePresentButNotComparable(t *testing.T) {
	d := pairDiff(t, "degraded-same-module")
	foo := oneDiffAt(t, d, "main", "foo")
	wantPresence(t, foo, PresenceOnlyBase, "", NotComparable)
	if !strings.Contains(foo.Why, "libfoo.so") {
		t.Fatalf("why %q does not name the module", foo.Why)
	}
	addr := oneDiffAt(t, d, "main", "libfoo.so+0x123")
	wantPresence(t, addr, PresenceOnlyNew, "", NotComparable)
	if !strings.Contains(addr.Why, "libfoo.so") {
		t.Fatalf("why %q does not name the module", addr.Why)
	}
	// main and work are resolved on both sides.
	wantPresence(t, oneDiffAt(t, d, "main"), PresenceBoth, MatchByName, Comparable)
	wantPresence(t, oneDiffAt(t, d, "main", "work"), PresenceBoth, MatchByName, Comparable)
	if !slices.ContainsFunc(d.QualityWarnings, func(w string) bool { return strings.Contains(w, "symbols of libfoo.so") }) {
		t.Fatalf("quality warnings %q do not name libfoo.so", d.QualityWarnings)
	}
}

// The degraded module is found on the other side by build id, else full
// path, else base name; a different module's degradation says nothing
// about foo.
func TestDegradedModuleIsMatchedAcrossSides(t *testing.T) {
	for pair, want := range map[string]Interpretation{
		"degraded-same-module":   NotComparable,
		"degraded-same-path":     NotComparable,
		"degraded-same-basename": NotComparable,
		"degraded-other-module":  Comparable,
	} {
		t.Run(pair, func(t *testing.T) {
			foo := oneDiffAt(t, pairDiff(t, pair), "main", "foo")
			if foo.Presence != PresenceOnlyBase || foo.Interpretation != want {
				t.Fatalf("foo %s %s (%q), want only_base %s", foo.Presence, foo.Interpretation, foo.Why, want)
			}
		})
	}
}

// A higher rung of the module ladder wins over a lower one even when the
// lower one would find a bigger shift.
func TestModuleLadderPrefersTheStrongerMatch(t *testing.T) {
	for _, pair := range []string{"ladder-build-id-before-path", "ladder-path-before-base-name"} {
		t.Run(pair, func(t *testing.T) {
			foo := oneDiffAt(t, pairDiff(t, pair), "main", "foo")
			wantPresence(t, foo, PresenceOnlyBase, "", Comparable)
		})
	}
}

// An unavailable unresolved share is never read as 0: half the base is
// unsymbolized libfoo, the new side has no value, and that is reported as
// a comparison that could not be made.
func TestUnavailableUnresolvedShareIsNotZero(t *testing.T) {
	d := pairDiff(t, "unresolved-base-zero-new")
	work := oneDiffAt(t, d, "main", "work")
	wantPresence(t, work, PresenceOnlyBase, "", Comparable)
	if !strings.Contains(work.Why, "symbols of no module: comparison unavailable in new: zero total") {
		t.Fatalf("why %q", work.Why)
	}
	// The address is not comparable for its own frame, not for a share
	// read as 0.
	addr := oneDiffAt(t, d, "main", "libfoo.so+0x1")
	wantPresence(t, addr, PresenceOnlyBase, "", NotComparable)
	if addr.Why != ReasonOwnFrameUnresolved {
		t.Fatalf("why %q", addr.Why)
	}
	if !slices.Contains(d.QualityWarnings, "symbols: comparison unavailable in new: zero total") {
		t.Fatalf("quality warnings %q", d.QualityWarnings)
	}
}

func TestPerturbationShiftIsNotComparable(t *testing.T) {
	d := pairDiff(t, "perturbation-shift")
	for _, path := range [][]string{
		{"main"},
		{"main", "[gpu:launch]"},
		{"main", "[gpu:launch]", "[gpu:kernel:k1]"},
	} {
		n := oneDiffAt(t, d, path...)
		wantPresence(t, n, PresenceBoth, MatchByName, NotComparable)
		if !strings.HasPrefix(n.Why, "perturbation") {
			t.Fatalf("%s: why %q", n.Name, n.Why)
		}
	}
	if d.Root.Interpretation != NotComparable {
		t.Fatalf("root %s", d.Root.Interpretation)
	}
	// k2 ran unserialized on both sides.
	wantPresence(t, oneDiffAt(t, d, "main", "[gpu:launch]", "[gpu:kernel:k2]"), PresenceBoth, MatchByName, Comparable)
}

func TestSerializationMissingToFalseIsNotComparable(t *testing.T) {
	d := pairDiff(t, "serialized-missing-to-false")
	k := oneDiffAt(t, d, "main", "[gpu:launch]", "[gpu:kernel:k]")
	wantPresence(t, k, PresenceBoth, MatchByName, NotComparable)
	if !strings.Contains(k.Why, "perturbation") {
		t.Fatalf("why %q", k.Why)
	}
}

func TestSerializationUnknownToFalseIsNotComparable(t *testing.T) {
	d := pairDiff(t, "serialized-unknown-to-false")
	k := oneDiffAt(t, d, "main", "[gpu:launch]", "[gpu:kernel:k]")
	wantPresence(t, k, PresenceBoth, MatchByName, NotComparable)
	if !strings.Contains(k.Why, "perturbation") {
		t.Fatalf("why %q", k.Why)
	}
}

// The second rule of the perturbation policy on its own: no state moves
// past the threshold, yet one side knows its perturbation and the other
// does not.
func TestSerializationKnownVersusUnknownIsNotComparable(t *testing.T) {
	d := pairDiff(t, "serialized-known-vs-unknown")
	k := oneDiffAt(t, d, "main", "[gpu:launch]", "[gpu:kernel:k]")
	wantPresence(t, k, PresenceBoth, MatchByName, NotComparable)
	if !strings.Contains(k.Why, "known in new") {
		t.Fatalf("why %q", k.Why)
	}
}

func TestSmallSerializationShiftIsComparable(t *testing.T) {
	d := pairDiff(t, "serialized-small-shift")
	walkDiff(d.Root, func(n *DiffNode) {
		if n.Interpretation != Comparable {
			t.Fatalf("%s: %s (%q)", n.Name, n.Interpretation, n.Why)
		}
	})
}

func TestZeroTotalSideIsUnavailableNotZero(t *testing.T) {
	d := pairDiff(t, "zero-total-new")
	if d.NewTotal != 0 || d.BaseTotal != 100 {
		t.Fatalf("totals %d, %d", d.BaseTotal, d.NewTotal)
	}
	r := d.Root
	wantUnavailable(t, "root share delta", r.DeltaShare, ReasonZeroTotal)
	if r.Interpretation != Comparable {
		t.Fatalf("root %s (%q): a zero made it not comparable", r.Interpretation, r.Why)
	}
	if !strings.Contains(r.Why, "perturbation: comparison unavailable in new: zero total") {
		t.Fatalf("root why %q does not report the perturbation comparison unavailable", r.Why)
	}
	m := oneDiffAt(t, d, "main")
	wantPresence(t, m, PresenceOnlyBase, "", Comparable)
	wantUnavailable(t, "main share delta", m.DeltaShare, ReasonZeroTotal)
	if !strings.Contains(m.Why, "perturbation: comparison unavailable") {
		t.Fatalf("main why %q", m.Why)
	}
	if !slices.ContainsFunc(d.QualityWarnings, func(w string) bool { return strings.Contains(w, "perturbation: comparison unavailable") }) {
		t.Fatalf("quality warnings %q", d.QualityWarnings)
	}
}

func TestBothTotalsZeroIsAnEmptyUnion(t *testing.T) {
	zero := allFixtures()["cpu-all-zero"]()
	d := compareOf(t, zero, zero, WorkCounts{Base: 1, New: 1})
	r := d.Root
	if r.Combined != 0 || len(r.Children) != 0 {
		t.Fatalf("root combined %d with %d children", r.Combined, len(r.Children))
	}
	wantUnavailable(t, "share delta", r.DeltaShare, ReasonZeroTotal)
	if r.Interpretation != Comparable {
		t.Fatalf("root %s (%q)", r.Interpretation, r.Why)
	}
}

// A matched node is judged on the frames strictly below it: its own frame
// is the same on both sides, and counting an unresolved one would put its
// module's share at 1 on both sides and hide any change under it.
func TestUnresolvedMatchedNodeDoesNotSaturateItsBasis(t *testing.T) {
	d := pairDiff(t, "unresolved-below-matched-unresolved")
	wantPresence(t, oneDiffAt(t, d, "main"), PresenceBoth, MatchByName, Comparable)
	unk := oneDiffAt(t, d, "main", "[unknown]")
	if unk.Presence != PresenceBoth || unk.Match != MatchByName {
		t.Fatalf("matched [unknown]: %s %q", unk.Presence, unk.Match)
	}
	const shift = "symbols of no module: unresolved share 1.000 in base, 0.000 in new"
	gone := oneDiffAt(t, d, "main", "[unknown]", "[unknown]")
	wantPresence(t, gone, PresenceOnlyBase, "", NotComparable)
	if gone.Why != shift {
		t.Fatalf("one-sided [unknown] why %q, want %q", gone.Why, shift)
	}
	foo := oneDiffAt(t, d, "main", "[unknown]", "[unknown]", "foo")
	wantPresence(t, foo, PresenceOnlyBase, "", NotComparable)
	bar := oneDiffAt(t, d, "main", "[unknown]", "bar")
	wantPresence(t, bar, PresenceOnlyNew, "", NotComparable)
	if bar.Why != shift {
		t.Fatalf("bar why %q, want %q", bar.Why, shift)
	}
}

// A one-sided node whose own frame is unresolved names nothing the other
// side could be checked for: it is not comparable even when no share
// moved.
func TestOneSidedUnresolvedFrameIsNotComparable(t *testing.T) {
	d := pairDiff(t, "own-frame-unresolved")
	wantPresence(t, oneDiffAt(t, d, "main"), PresenceBoth, MatchByName, Comparable)
	wantPresence(t, oneDiffAt(t, d, "main", "foo"), PresenceBoth, MatchByName, Comparable)
	for _, tc := range []struct {
		name string
		p    Presence
	}{{"0xdead", PresenceOnlyBase}, {"0xbeef", PresenceOnlyNew}} {
		n := oneDiffAt(t, d, "main", tc.name)
		wantPresence(t, n, tc.p, "", NotComparable)
		if n.Why != ReasonOwnFrameUnresolved {
			t.Fatalf("%s why %q, want %q", tc.name, n.Why, ReasonOwnFrameUnresolved)
		}
	}
}

func TestGPUAncestorDegradationIsNotComparable(t *testing.T) {
	d := pairDiff(t, "gpu-ancestor-degradation")
	foo := oneDiffAt(t, d, "main", "foo")
	wantPresence(t, foo, PresenceOnlyBase, "", NotComparable)
	if !strings.Contains(foo.Why, "libfoo.so") {
		t.Fatalf("why %q does not name libfoo.so", foo.Why)
	}
	// The kernel under foo lost its caller, not its cost: it is judged
	// with foo.
	k := oneDiffAt(t, d, "main", "foo", "[gpu:kernel:k]")
	wantPresence(t, k, PresenceOnlyBase, "", NotComparable)
	if k.Why != foo.Why {
		t.Fatalf("kernel why %q, foo why %q", k.Why, foo.Why)
	}
	wantPresence(t, oneDiffAt(t, d, "main", "libfoo.so+0x123"), PresenceOnlyNew, "", NotComparable)
}

func TestSamplePeriodChangeIsNotComparable(t *testing.T) {
	k := oneDiffAt(t, pairDiff(t, "sample-period-change"), "main", "[gpu:launch]", "[gpu:kernel:k]")
	wantPresence(t, k, PresenceBoth, MatchByName, NotComparable)
	if k.Why != "sample period: 8 in base, 16 in new" {
		t.Fatalf("why %q", k.Why)
	}
}

// The synthetic root has no module: an unmapped unresolved shift is a
// view-level warning, and only the nodes that carry it are judged on it.
func TestRootIsNotJudgedOnTheNoModuleShare(t *testing.T) {
	d := pairDiff(t, "unmapped-unresolved-shift")
	if d.Root.Interpretation != Comparable || d.Root.Why != "" {
		t.Fatalf("root %s (%q)", d.Root.Interpretation, d.Root.Why)
	}
	want := "symbols of no module: unresolved share 0.000 in base, 0.300 in new"
	if !slices.Contains(d.QualityWarnings, want) {
		t.Fatalf("quality warnings %q lack %q", d.QualityWarnings, want)
	}
	wantPresence(t, oneDiffAt(t, d, "main"), PresenceBoth, MatchExact, Comparable)
	addr := oneDiffAt(t, d, "0x7f00")
	wantPresence(t, addr, PresenceOnlyNew, "", NotComparable)
	if !strings.HasPrefix(addr.Why, "symbols of no module:") {
		t.Fatalf("bare address why %q", addr.Why)
	}
}

// A period named only by a zero-value sample carries no weight and does
// not make the node not comparable.
func TestZeroValuePeriodIsNotCompared(t *testing.T) {
	d := pairDiff(t, "zero-value-period")
	walkDiff(d.Root, func(n *DiffNode) {
		if n.Interpretation != Comparable {
			t.Fatalf("%s: %s (%q)", n.Name, n.Interpretation, n.Why)
		}
	})
	if slices.ContainsFunc(d.QualityWarnings, func(w string) bool { return strings.HasPrefix(w, "sample period:") }) {
		t.Fatalf("quality warnings %q", d.QualityWarnings)
	}
}

// Each rule's threshold is "more than 0.10", decided on the integer
// weights: 30 → 40 of 100 is exactly 0.10, although 0.4 - 0.3 is
// 0.10000000000000003 in float64.
func TestThresholdsAreExact(t *testing.T) {
	cases := []struct {
		base, next int64
		want       Interpretation
	}{
		{30, 39, Comparable}, {30, 40, Comparable}, {30, 41, NotComparable},
		{39, 30, Comparable}, {40, 30, Comparable}, {41, 30, NotComparable},
	}
	kernel := []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}
	labelled := func(label, part, rest string) func(int64) []byte {
		return func(x int64) []byte {
			return gpuProfile(
				fx{stack: kernel, value: x, labels: map[string][]string{label: {part}}},
				fx{stack: kernel, value: 100 - x, labels: map[string][]string{label: {rest}}},
			)
		}
	}
	rules := map[string]struct {
		build  func(part int64) []byte // part of 100 under the judged node
		path   []string
		prefix string
	}{
		// foo is in libfoo; part of its 100 ends in an unsymbolized libfoo frame.
		"unresolved": {
			build: func(x int64) []byte {
				foo := in("/usr/lib/libfoo.so", "f", 0x10, ln("foo"))
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), foo, in("/usr/lib/libfoo.so", "f", 0x9)}, value: x},
					rs{stack: []frame{fn("main"), foo, fn("bar")}, value: 100 - x},
				)
			},
			path:   []string{"main", "foo"},
			prefix: "symbols of libfoo.so:",
		},
		"join":       {build: labelled("gpu_join", "exact", "heuristic"), path: kernel, prefix: "join coverage:"},
		"serialized": {build: labelled("gpu_serialized", "true", "false"), path: kernel, prefix: "perturbation:"},
	}
	for name, r := range rules {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/%d-to-%d", name, tc.base, tc.next), func(t *testing.T) {
				n := oneDiffAt(t, compareOf(t, r.build(tc.base), r.build(tc.next), WorkCounts{}), r.path...)
				if n.Interpretation != tc.want {
					t.Fatalf("%s: %s (%q), want %s", n.Name, n.Interpretation, n.Why, tc.want)
				}
				if tc.want == NotComparable && !strings.HasPrefix(n.Why, r.prefix) {
					t.Fatalf("why %q, want prefix %q", n.Why, r.prefix)
				}
			})
		}
	}
}

func TestShiftExceedsAtTheLimits(t *testing.T) {
	limit := threshold{num: 1, den: 10}
	for _, tc := range []struct {
		a, da, b, db int64
		want         bool
	}{
		{0, math.MaxInt64, math.MaxInt64, math.MaxInt64, true},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, false},
		{1, 10, 2, 10, false}, // exactly 0.1
		{1, 3, 2, 7, false},   // 2/7 - 1/3 = -1/21
		{1, 7, 3, 7, true},
	} {
		if got := shiftExceeds(tc.a, tc.da, tc.b, tc.db, limit); got != tc.want {
			t.Errorf("shiftExceeds(%d/%d → %d/%d) = %v, want %v", tc.a, tc.da, tc.b, tc.db, got, tc.want)
		}
	}
}

// Each view's sampling notes are those its path facts carry, GPU only.
func TestDiffCarriesEachViewsSamplingNotes(t *testing.T) {
	sampled := allFixtures()["gpu-sampled-and-unsampled"]()
	other := gpuProfile(fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 50,
		labels: map[string][]string{"gpu_sample_period": {"4"}}})
	d := compareOf(t, sampled, other, WorkCounts{})
	want := foldedstacks.SamplingNotes(30, 100, "8", 1)
	if len(want) != 2 || !slices.Equal(d.BaseNotes, want) {
		t.Fatalf("base notes %q, want %q", d.BaseNotes, want)
	}
	if pf := factsAt(t, viewOf(t, sampled), "main"); !slices.Equal(d.BaseNotes, pf.Notes) {
		t.Fatalf("base notes %q, path facts %q", d.BaseNotes, pf.Notes)
	}
	pf := factsAt(t, viewOf(t, other), "main")
	if len(pf.Notes) == 0 || !slices.Equal(d.NewNotes, pf.Notes) {
		t.Fatalf("new notes %q, path facts %q", d.NewNotes, pf.Notes)
	}
	cpu := pairDiff(t, "removed-kernel")
	if cpu.BaseNotes != nil || cpu.NewNotes != nil {
		t.Fatalf("CPU notes %q %q", cpu.BaseNotes, cpu.NewNotes)
	}
}

func TestWhyNamesTheFirstCause(t *testing.T) {
	d := pairDiff(t, "join-and-period-change")
	k := oneDiffAt(t, d, "main", "[gpu:launch]", "[gpu:kernel:k]")
	wantPresence(t, k, PresenceBoth, MatchByName, NotComparable)
	if !strings.HasPrefix(k.Why, "join coverage:") || strings.Contains(k.Why, "sample period") {
		t.Fatalf("why %q, want the join cause alone", k.Why)
	}
	// The view-level warnings list every cause.
	for _, want := range []string{"join coverage:", "sample period:"} {
		if !slices.ContainsFunc(d.QualityWarnings, func(w string) bool { return strings.HasPrefix(w, want) }) {
			t.Fatalf("quality warnings %q lack %q", d.QualityWarnings, want)
		}
	}
}

func TestCPUDiffReportsPerturbationUnavailable(t *testing.T) {
	d := pairDiff(t, "removed-kernel")
	want := WarningPerturbationNotGPU
	if want != "perturbation: comparison unavailable: "+ReasonNotGPUView {
		t.Fatalf("warning text %q", want)
	}
	if !slices.Contains(d.QualityWarnings, want) {
		t.Fatalf("quality warnings %q lack %q", d.QualityWarnings, want)
	}
}

func TestUnmappedSameNameChangingFilenameDoesNotMerge(t *testing.T) {
	d := pairDiff(t, "unmapped-filename-change")
	inits := diffsAt(d, "main", "__init__")
	if len(inits) != 2 {
		t.Fatalf("%d __init__ nodes, want 2", len(inits))
	}
	got := []Presence{inits[0].Presence, inits[1].Presence}
	slices.Sort(got)
	if !slices.Equal(got, []Presence{PresenceOnlyBase, PresenceOnlyNew}) {
		t.Fatalf("presences %v", got)
	}
	for _, n := range inits {
		if n.Match != "" {
			t.Fatalf("%s: match %q", n.Presence, n.Match)
		}
	}
}

func TestUnmappedSiblingsWithStableFilenamesMatchOneToOne(t *testing.T) {
	d := pairDiff(t, "unmapped-stable-siblings")
	inits := diffsAt(d, "main", "__init__")
	if len(inits) != 2 {
		t.Fatalf("%d __init__ nodes, want 2", len(inits))
	}
	deltas := map[string]int64{}
	for _, n := range inits {
		wantPresence(t, n, PresenceBoth, MatchByName, Comparable)
		deltas[n.Base.Function.Filename] = n.DeltaAbs
		if n.Base.Function.Filename != n.New.Function.Filename {
			t.Fatalf("%s matched %s", n.Base.Function.Filename, n.New.Function.Filename)
		}
	}
	if deltas["a.py"] != 3 || deltas["b.py"] != 3 {
		t.Fatalf("deltas %v", deltas)
	}
}

// A node whose only samples have value 0 on one side is absent there: it
// neither makes the other side's node "both" nor stands as a candidate.
func TestZeroValueNodesAreAbsent(t *testing.T) {
	d := pairDiff(t, "zero-value-nodes")
	wantPresence(t, oneDiffAt(t, d, "main", "idle"), PresenceOnlyNew, "", Comparable)
	foo := oneDiffAt(t, d, "main", "foo")
	wantPresence(t, foo, PresenceBoth, MatchByName, Comparable)
	if foo.Base.Function.Module.File != "/b/libfoo.so" {
		t.Fatalf("base foo from %s", foo.Base.Function.Module.File)
	}
	wantPresence(t, oneDiffAt(t, d, "main", "work"), PresenceOnlyBase, "", Comparable)
}

func TestCombinedOverflowIsAnError(t *testing.T) {
	huge := cpuProfile(fx{stack: []string{"main"}, value: math.MaxInt64})
	_, err := Compare(viewOf(t, huge), viewOf(t, huge), WorkCounts{})
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("err %v, want ErrOverflow", err)
	}
}

// twoTypes is one sample under two declared sample types.
func twoTypes(a, b *profile.ValueType) []byte {
	p := &profile.Profile{SampleType: []*profile.ValueType{a, b}}
	m := &profile.Mapping{ID: 1}
	f := &profile.Function{ID: 1, Name: "main"}
	l := &profile.Location{ID: 1, Mapping: m, Line: []profile.Line{{Function: f}}}
	p.Mapping, p.Function, p.Location = []*profile.Mapping{m}, []*profile.Function{f}, []*profile.Location{l}
	p.Sample = []*profile.Sample{{Location: []*profile.Location{l}, Value: []int64{3, 4}}}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestDiffRefusesIncompatibleViews(t *testing.T) {
	view := func(raw []byte, idx int) *View {
		t.Helper()
		v, err := loadBytes(t, raw).View(ViewOptions{SampleIndex: idx})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cpu := fixtureView(t, "cpu-one-sample")
	gpu := fixtureView(t, "gpu-two-samples")
	alloc := twoTypes(&profile.ValueType{Type: "alloc_objects", Unit: "count"}, &profile.ValueType{Type: "alloc_space", Unit: "bytes"})
	same := twoTypes(&profile.ValueType{Type: "cpu", Unit: "nanoseconds"}, &profile.ValueType{Type: "cpu", Unit: "nanoseconds"})
	collapsed := *cpu
	collapsed.Options.CollapseVendorRuns = true
	for name, pair := range map[string][2]*View{
		"cpu vs gpu":                {cpu, gpu},
		"alloc_objects vs space":    {view(alloc, 0), view(alloc, 1)},
		"same type, other index":    {view(same, 0), view(same, 1)},
		"same type, other collapse": {cpu, &collapsed},
	} {
		if _, err := Compare(pair[0], pair[1], WorkCounts{}); !errors.Is(err, ErrIncompatibleViews) {
			t.Errorf("%s: err %v, want ErrIncompatibleViews", name, err)
		}
	}
	// SampleIndex -1 and the index it resolves to are the same view.
	auto := view(same, -1)
	if _, err := Compare(auto, view(same, auto.Index), WorkCounts{}); err != nil {
		t.Fatalf("resolved index refused: %v", err)
	}
}

func TestPerUnitDeltaNeedsPositiveFiniteWork(t *testing.T) {
	base := cpuProfile(fx{stack: []string{"main"}, value: 100})
	next := cpuProfile(fx{stack: []string{"main"}, value: 150})
	for _, tc := range []struct {
		work   WorkCounts
		reason string
	}{
		{WorkCounts{}, ReasonNoWorkCount},
		{WorkCounts{Base: 0, New: 100}, ReasonNoWorkCount},
		{WorkCounts{Base: 100, New: 0}, ReasonNoWorkCount},
		{WorkCounts{Base: math.Inf(1), New: 1}, ReasonBadWorkCount},
		{WorkCounts{Base: 1, New: math.NaN()}, ReasonBadWorkCount},
		{WorkCounts{Base: -5, New: 1}, ReasonBadWorkCount},
	} {
		// Unusable work counts are stated once on the diff, not per node.
		d := compareOf(t, base, next, tc.work)
		if d.PerUnit != (Availability{Reason: tc.reason}) {
			t.Fatalf("work %+v: per-unit %+v, want unavailable %q", tc.work, d.PerUnit, tc.reason)
		}
		walkDiff(d.Root, func(n *DiffNode) {
			if n.DeltaPerUnit != nil {
				t.Fatalf("work %+v: %s carries a per-unit delta %+v", tc.work, n.Name, *n.DeltaPerUnit)
			}
		})
	}
	d := compareOf(t, base, next, WorkCounts{Base: 100, New: 100})
	if !d.PerUnit.OK || d.PerUnit.Reason != "" {
		t.Fatalf("per-unit %+v, want ok", d.PerUnit)
	}
	f := d.Root.DeltaPerUnit
	if f == nil || f.Status != StatusOK || f.Derived == nil || *f.Derived != 0.5 {
		t.Fatalf("per-unit delta %+v, want ok 0.5", f)
	}
	same := compareOf(t, base, base, WorkCounts{Base: 100, New: 100}).Root.DeltaPerUnit
	if same == nil || same.Status != StatusZero || same.Derived == nil || *same.Derived != 0 {
		t.Fatalf("per-unit delta of equal views %+v, want zero", same)
	}
}

func TestPerUnitDeltaKeepsFractionsAndUnit(t *testing.T) {
	one := cpuProfile(fx{stack: []string{"main"}, value: 1})
	two := cpuProfile(fx{stack: []string{"main"}, value: 2})
	work := WorkCounts{Base: 2, New: 2}
	for _, tc := range []struct {
		base, next []byte
		want       float64
	}{{one, two, 0.5}, {two, one, -0.5}} {
		f := compareOf(t, tc.base, tc.next, work).Root.DeltaPerUnit
		if f == nil || f.Derived == nil || *f.Derived != tc.want || f.Int != nil || f.Ratio != nil {
			t.Fatalf("per-unit delta %+v, want derived %v alone", f, tc.want)
		}
		if f.Unit != "nanoseconds/work" || f.Status != StatusOK || f.Rounding != RoundingFor(MetricPerWorkDelta) {
			t.Fatalf("per-unit delta %+v", f)
		}
	}
}

func TestPerUnitDeltaRejectsNonFiniteResult(t *testing.T) {
	huge := cpuProfile(fx{stack: []string{"main"}, value: math.MaxInt64})
	empty := allFixtures()["cpu-all-zero"]()
	d := compareOf(t, huge, empty, WorkCounts{Base: math.SmallestNonzeroFloat64, New: 1})
	if !d.PerUnit.OK || d.Root.DeltaPerUnit == nil {
		t.Fatalf("per-unit %+v, root delta %v: want usable work counts", d.PerUnit, d.Root.DeltaPerUnit)
	}
	wantUnavailable(t, "per-unit delta", *d.Root.DeltaPerUnit, ReasonNonFinite)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("Inf")) || bytes.Contains(b, []byte("NaN")) {
		t.Fatalf("JSON carries a non-finite value: %s", b)
	}
}

// The JSON keys are a contract, and the union marshals without following
// the views' parent pointers.
func TestDiffJSONKeys(t *testing.T) {
	d := pairDiff(t, "one-vs-two")
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	wantKeys := func(what string, m map[string]json.RawMessage, want ...string) {
		t.Helper()
		if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
			t.Fatalf("%s keys %q\nwant %q", what, got, slices.Sorted(slices.Values(want)))
		}
	}
	wantKeys("diff", top, "base_handle", "new_handle", "sample_type", "unit", "base_total", "new_total",
		"base_quality", "new_quality", "quality_warnings", "base_notes", "new_notes", "per_unit", "view", "root")
	if d.View != "cpu/nanoseconds#0" {
		t.Fatalf("view %q", d.View)
	}
	// Without work counts no node carries delta_per_unit: per_unit says why
	// once.
	common := []string{"name", "presence", "interpretation", "base_cum", "new_cum", "base_self",
		"new_self", "combined", "delta_abs", "delta_share"}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(top["root"], &root); err != nil {
		t.Fatal(err)
	}
	wantKeys("root", root, append(slices.Clone(common), "base_id", "new_id", "children")...)

	var foo map[string]json.RawMessage
	for _, n := range diffsAt(d, "main", "foo") {
		if n.Presence != PresenceOnlyBase {
			continue
		}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &foo); err != nil {
			t.Fatal(err)
		}
	}
	wantKeys("only_base foo", foo, append(slices.Clone(common), "base_id", "match", "candidates")...)
	var cands []map[string]json.RawMessage
	if err := json.Unmarshal(foo["candidates"], &cands); err != nil {
		t.Fatal(err)
	}
	wantKeys("candidate", cands[0], "side", "handle", "node")

	p := diffPairs()["one-vs-two"]
	withWork, err := json.Marshal(compareOf(t, p.base(), p.new(), WorkCounts{Base: 1, New: 2}).Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(withWork, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["delta_per_unit"]; !ok {
		t.Fatalf("root with work counts lacks delta_per_unit: %s", withWork)
	}
}
