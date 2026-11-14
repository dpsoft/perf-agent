package profilemodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

func loadBytes(t *testing.T, raw []byte) *Profile {
	t.Helper()
	p, err := FromBytes(raw, nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func viewOf(t *testing.T, raw []byte) *View {
	t.Helper()
	v, err := loadBytes(t, raw).View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func fixtureView(t *testing.T, name string) *View {
	t.Helper()
	build, ok := allFixtures()[name]
	if !ok {
		t.Fatalf("no fixture %q", name)
	}
	return viewOf(t, build())
}

// nodesAt returns every node whose root-first display-name path is names.
func nodesAt(v *View, names ...string) []*Node {
	cur := []*Node{v.Root}
	for _, name := range names {
		var next []*Node
		for _, n := range cur {
			for _, c := range n.Children {
				if c.Function.Name == name {
					next = append(next, c)
				}
			}
		}
		cur = next
	}
	return cur
}

func oneNodeAt(t *testing.T, v *View, names ...string) *Node {
	t.Helper()
	ns := nodesAt(v, names...)
	if len(ns) != 1 {
		t.Fatalf("path %v: %d nodes, want 1", names, len(ns))
	}
	return ns[0]
}

// idOf recomputes a node ID from the documented formula.
func idOf(keys ...string) NodeID {
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	return NodeID(hex.EncodeToString(h.Sum(nil))[:16])
}

func TestTreeInvariantsHoldOnEveryFixture(t *testing.T) {
	for name, build := range allFixtures() {
		t.Run(name, func(t *testing.T) {
			raw := build()
			p := loadBytes(t, raw)
			v, err := p.View(ViewOptions{SampleIndex: -1})
			if err != nil {
				t.Fatal(err)
			}
			if v.profile != p {
				t.Fatal("view does not point back at its profile")
			}
			var sum int64
			for _, s := range p.Source.Sample {
				sum += s.Value[v.Index]
			}
			if v.Root.Cum != v.Total || v.Total != sum {
				t.Fatalf("Root.Cum %d, Total %d, sum of samples %d", v.Root.Cum, v.Total, sum)
			}
			if v.Root.ID != idOf() || v.Root.Parent != nil || v.Root.Depth != 0 {
				t.Fatalf("root: id %s parent %v depth %d", v.Root.ID, v.Root.Parent, v.Root.Depth)
			}
			seen := map[NodeID]bool{}
			v.Walk(func(n *Node) bool {
				if seen[n.ID] {
					t.Errorf("duplicate node id %s", n.ID)
				}
				seen[n.ID] = true
				if got, ok := v.Node(n.ID); !ok || got != n {
					t.Errorf("Node(%s) does not return the node", n.ID)
				}
				var kids int64
				for i, c := range n.Children {
					kids += c.Cum
					if c.Parent != n || c.Depth != n.Depth+1 {
						t.Errorf("%s: bad parent or depth", c.Function.Name)
					}
					if i > 0 {
						a := n.Children[i-1]
						if a.Cum < c.Cum || (a.Cum == c.Cum && a.Function.Key() >= c.Function.Key()) {
							t.Errorf("children of %s not sorted by Cum desc, then Key", n.Function.Name)
						}
					}
				}
				if n.Cum != n.Self+kids {
					t.Errorf("%s: Cum %d != Self %d + children %d", n.Function.Name, n.Cum, n.Self, kids)
				}
				if n.Inexact < 0 || n.Inexact > n.Cum {
					t.Errorf("%s: Inexact %d outside [0, %d]", n.Function.Name, n.Inexact, n.Cum)
				}
				var cum int64
				for _, s := range n.samples {
					cum += p.Source.Sample[s].Value[v.Index]
				}
				if cum != n.Cum {
					t.Errorf("%s: samples sum to %d, Cum %d", n.Function.Name, cum, n.Cum)
				}
				for _, s := range n.zeroSamples {
					if p.Source.Sample[s].Value[v.Index] != 0 {
						t.Errorf("%s: zeroSamples holds non-zero sample %d", n.Function.Name, s)
					}
				}
				if n != v.Root {
					var keys []string
					for _, q := range v.Path(n) {
						keys = append(keys, q.Function.Key())
					}
					if want := idOf(keys...); n.ID != want {
						t.Errorf("%s: id %s, want %s", n.Function.Name, n.ID, want)
					}
				}
				return true
			})
			if len(seen) != len(v.nodes) {
				t.Errorf("walk saw %d nodes, index has %d", len(seen), len(v.nodes))
			}
			if _, err := json.Marshal(v.Root); err != nil {
				t.Errorf("marshal root: %v", err)
			}
		})
	}
}

// The display names along every path must be the names Fold gives the same
// profile, so the model and the existing flame graph never disagree.
func TestDisplayNamesMatchFold(t *testing.T) {
	for name, build := range allFixtures() {
		t.Run(name, func(t *testing.T) {
			p := loadBytes(t, build())
			res, err := foldedstacks.Fold(p.Source, foldedstacks.Options{SampleIndex: -1})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int64{}
			for _, st := range res.Stacks {
				want[strings.Join(st.Frames, ";")] += st.Value
			}
			v, err := p.View(ViewOptions{SampleIndex: -1})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int64{}
			v.Walk(func(n *Node) bool {
				if n.Self > 0 {
					var names []string
					for _, q := range v.Path(n) {
						names = append(names, q.Function.Name)
					}
					got[strings.Join(names, ";")] += n.Self
				}
				return true
			})
			if !maps.Equal(got, want) {
				t.Fatalf("tree stacks %v\nfold stacks %v", got, want)
			}
		})
	}
}

func TestRecursionGetsDistinctNodesPerDepth(t *testing.T) {
	v := fixtureView(t, "recursion")
	path := []*Node{
		oneNodeAt(t, v, "main", "a"),
		oneNodeAt(t, v, "main", "a", "b"),
		oneNodeAt(t, v, "main", "a", "b", "a"),
		oneNodeAt(t, v, "main", "a", "b", "a", "b"),
	}
	ids := map[NodeID]bool{}
	for _, n := range path {
		ids[n.ID] = true
		if n.Cum != 10 {
			t.Errorf("%s at depth %d: Cum %d, want 10", n.Function.Name, n.Depth, n.Cum)
		}
	}
	if len(ids) != 4 {
		t.Fatalf("%d distinct ids, want 4", len(ids))
	}
}

func TestSameNameInDifferentModulesStaysSeparate(t *testing.T) {
	v := fixtureView(t, "same-name-two-modules")
	foos := nodesAt(v, "main", "foo")
	if len(foos) != 2 {
		t.Fatalf("%d foo nodes, want 2", len(foos))
	}
	a, b := foos[0], foos[1]
	if a.Function.Key() == b.Function.Key() || a.ID == b.ID {
		t.Fatalf("same key or id: %q %q", a.Function.Key(), b.Function.Key())
	}
	if a.Function.Name != "foo" || b.Function.Name != "foo" {
		t.Fatalf("names %q %q", a.Function.Name, b.Function.Name)
	}
	builds := []string{a.Function.Module.BuildID, b.Function.Module.BuildID}
	slices.Sort(builds)
	if !slices.Equal(builds, []string{"x", "y"}) {
		t.Fatalf("build ids %v", builds)
	}
	// By name, the base file names differ too, so they stay apart.
	if a.Function.NameKey() == b.Function.NameKey() {
		t.Fatalf("same NameKey %q", a.Function.NameKey())
	}
}

func TestSeveralPCsOfOneFunctionAreOneNode(t *testing.T) {
	v := fixtureView(t, "one-function-two-pcs")
	foo := oneNodeAt(t, v, "main", "foo")
	if foo.Cum != 100 || foo.Self != 100 {
		t.Fatalf("Cum %d Self %d, want 100", foo.Cum, foo.Self)
	}
	mod := foo.Function.Module
	want := []LocationID{{Module: mod, Offset: 0x10}, {Module: mod, Offset: 0x20}}
	if !slices.Equal(foo.CallSites, want) {
		t.Fatalf("CallSites %+v, want %+v", foo.CallSites, want)
	}
}

func TestEmptyFirstMappingIsNoModule(t *testing.T) {
	v := fixtureView(t, "gpu-two-samples")
	n := 0
	v.Walk(func(nd *Node) bool {
		n++
		if nd.Function.Module != (ModuleRef{}) {
			t.Errorf("%s: module %+v, want none", nd.Function.Name, nd.Function.Module)
		}
		if nd != v.Root && !nd.CollisionPossible {
			t.Errorf("%s: unmapped frame without filename should flag a possible collision", nd.Function.Name)
		}
		return true
	})
	if n != 5 { // all, main, launch, kernel_a, kernel_b
		t.Fatalf("%d nodes, want 5", n)
	}
	// The zero-value sample adds nothing but is remembered on its path.
	kb := oneNodeAt(t, v, "main", "launch", "[gpu:kernel_b]")
	if kb.Cum != 0 || !slices.Equal(kb.zeroSamples, []int{1}) || len(kb.samples) != 0 {
		t.Fatalf("kernel_b: Cum %d samples %v zeroSamples %v", kb.Cum, kb.samples, kb.zeroSamples)
	}
	launch := oneNodeAt(t, v, "main", "launch")
	if !slices.Equal(launch.samples, []int{0}) || !slices.Equal(launch.zeroSamples, []int{1}) {
		t.Fatalf("launch: samples %v zeroSamples %v", launch.samples, launch.zeroSamples)
	}
}

func TestSentinelMappingsIdentifyByName(t *testing.T) {
	v := fixtureView(t, "sentinel-jit")
	foos := nodesAt(v, "main", "foo")
	if len(foos) != 2 {
		t.Fatalf("%d foo nodes, want 2", len(foos))
	}
	var jit, lib *Node
	for _, n := range foos {
		if n.Function.Module.Sentinel == "[jit]" {
			jit = n
		} else {
			lib = n
		}
	}
	if jit == nil || lib == nil {
		t.Fatalf("modules %+v %+v", foos[0].Function.Module, foos[1].Function.Module)
	}
	if lib.Function.Module.Sentinel != "" || lib.Function.Module.File != "/usr/lib/libfoo.so" {
		t.Fatalf("lib module %+v", lib.Function.Module)
	}
	if jit.CollisionPossible {
		t.Fatal("a sentinel module is a module")
	}
	k := oneNodeAt(t, v, "main", "do_syscall_64")
	if k.Function.Module.Sentinel != "[kernel]" {
		t.Fatalf("kernel module %+v", k.Function.Module)
	}
	if jit.Function.NameKey() == k.Function.NameKey() {
		t.Fatal("different sentinels share a NameKey")
	}
}

func TestUnmappedSameNameUsesFilenameAndStartLine(t *testing.T) {
	v := fixtureView(t, "unmapped-init-with-filename")
	inits := nodesAt(v, "main", "__init__")
	if len(inits) != 2 {
		t.Fatalf("%d __init__ nodes, want 2", len(inits))
	}
	for _, n := range inits {
		if n.CollisionPossible {
			t.Errorf("%s (%s): CollisionPossible with a filename", n.Function.Name, n.Function.Filename)
		}
		if n.Function.StartLine != 1 {
			t.Errorf("StartLine %d", n.Function.StartLine)
		}
	}
	if inits[0].Function.NameKey() == inits[1].Function.NameKey() {
		t.Fatal("filenames should separate the NameKeys")
	}

	v = fixtureView(t, "unmapped-init-bare")
	n := oneNodeAt(t, v, "main", "__init__")
	if !n.CollisionPossible || n.Cum != 5 || len(n.CallSites) != 2 {
		t.Fatalf("CollisionPossible %v Cum %d CallSites %v", n.CollisionPossible, n.Cum, n.CallSites)
	}
}

func TestInlineFramesBecomeNodesRootFirst(t *testing.T) {
	v := fixtureView(t, "inline")
	outer := oneNodeAt(t, v, "main", "outer")
	inner := oneNodeAt(t, v, "main", "outer", "inner")
	if outer.Cum != 9 || outer.Self != 0 || inner.Cum != 9 || inner.Self != 9 {
		t.Fatalf("outer %d/%d inner %d/%d", outer.Cum, outer.Self, inner.Cum, inner.Self)
	}
	mod := outer.Function.Module
	if !slices.Equal(outer.CallSites, []LocationID{{Module: mod, Offset: 0x40, Inline: 0}}) ||
		!slices.Equal(inner.CallSites, []LocationID{{Module: mod, Offset: 0x40, Inline: 1}}) {
		t.Fatalf("call sites outer %+v inner %+v", outer.CallSites, inner.CallSites)
	}
}

func TestAddressOnlyAndEmptyStacks(t *testing.T) {
	v := fixtureView(t, "address-only-and-empty-stack")
	a := oneNodeAt(t, v, "main", "libfoo.so+0x123")
	if a.Function.Module.File != "/usr/lib/libfoo.so" || a.Cum != 6 {
		t.Fatalf("address-only frame: %+v Cum %d", a.Function, a.Cum)
	}
	oneNodeAt(t, v, "main", "0x7f00")
	u := oneNodeAt(t, v, foldedstacks.UnknownFrame)
	if u.Cum != 4 || !u.CollisionPossible || len(u.CallSites) != 0 {
		t.Fatalf("[unknown]: Cum %d CollisionPossible %v CallSites %v", u.Cum, u.CollisionPossible, u.CallSites)
	}
	idle := oneNodeAt(t, v, "main", "idle")
	if idle.Cum != 0 || !slices.Equal(idle.zeroSamples, []int{3}) {
		t.Fatalf("idle: Cum %d zeroSamples %v", idle.Cum, idle.zeroSamples)
	}
}

func TestInexactIsCumulative(t *testing.T) {
	v := fixtureView(t, "inexact-labels")
	if v.Root.Inexact != 35 {
		t.Fatalf("root Inexact %d, want 35", v.Root.Inexact)
	}
	if n := oneNodeAt(t, v, "main", "launch"); n.Inexact != 20 {
		t.Fatalf("launch Inexact %d, want 20", n.Inexact)
	}
	if n := oneNodeAt(t, v, "main", "other", "[gpu:k]"); n.Inexact != 15 {
		t.Fatalf("other leaf Inexact %d, want 15", n.Inexact)
	}
}

func TestNodeIDsAreStableAcrossLoads(t *testing.T) {
	for name, build := range allFixtures() {
		t.Run(name, func(t *testing.T) {
			a, b := viewOf(t, build()), viewOf(t, build())
			ka := slices.Sorted(maps.Keys(a.nodes))
			kb := slices.Sorted(maps.Keys(b.nodes))
			if !slices.Equal(ka, kb) {
				t.Fatalf("ids differ between loads:\n%v\n%v", ka, kb)
			}
		})
	}
	// Pin the formula so a change to the key format is a visible change.
	v := fixtureView(t, "cpu-one-sample")
	work := oneNodeAt(t, v, "main", "work")
	if want := idOf(`-|"main"|""|0`, `-|"work"|""|0`); work.ID != want {
		t.Fatalf("work id %s, want %s", work.ID, want)
	}
	if len(work.ID) != 16 || strings.ToLower(string(work.ID)) != string(work.ID) {
		t.Fatalf("id %q is not 16 lowercase hex chars", work.ID)
	}
}

func TestCollapsedViewIsRejected(t *testing.T) {
	p := loadBytes(t, allFixtures()["cpu-one-sample"]())
	if _, err := p.View(ViewOptions{SampleIndex: -1, CollapseVendorRuns: true}); !errors.Is(err, ErrUnsupportedView) {
		t.Fatalf("err %v, want ErrUnsupportedView", err)
	}
}

func TestViewRejectsBadSampleIndex(t *testing.T) {
	p := loadBytes(t, allFixtures()["cpu-one-sample"]())
	if _, err := p.View(ViewOptions{SampleIndex: 1}); err == nil {
		t.Fatal("index past the sample types accepted")
	}
	// Any negative index chooses automatically, as foldedstacks does.
	for _, idx := range []int{-1, -2, math.MinInt} {
		v, err := p.View(ViewOptions{SampleIndex: idx})
		if err != nil || v.Index != 0 {
			t.Fatalf("index %d: view %+v err %v, want the automatic choice", idx, v, err)
		}
	}
	v, err := p.View(ViewOptions{SampleIndex: 0})
	if err != nil || v.SampleType != "cpu" || v.Unit != "nanoseconds" || v.Index != 0 || v.Handle != p.Handle {
		t.Fatalf("view %+v err %v", v, err)
	}
}

func TestViewRejectsNegativeValues(t *testing.T) {
	p := loadBytes(t, cpuProfile(fx{stack: []string{"main"}, value: 3}, fx{stack: []string{"main", "x"}, value: -5}))
	if _, err := p.View(ViewOptions{SampleIndex: -1}); !errors.Is(err, ErrNegativeValue) {
		t.Fatalf("err %v, want ErrNegativeValue", err)
	}
}

func TestViewRejectsOverflowingAccumulation(t *testing.T) {
	p := loadBytes(t, cpuProfile(
		fx{stack: []string{"main", "a"}, value: math.MaxInt64},
		fx{stack: []string{"main", "a"}, value: math.MaxInt64},
	))
	if _, err := p.View(ViewOptions{SampleIndex: -1}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("err %v, want ErrOverflow", err)
	}
	// Separate paths still overflow the total.
	p = loadBytes(t, cpuProfile(
		fx{stack: []string{"a"}, value: math.MaxInt64},
		fx{stack: []string{"b"}, value: 1},
	))
	if _, err := p.View(ViewOptions{SampleIndex: -1}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("err %v, want ErrOverflow", err)
	}
}

func TestPathKeyOnlyForRealModules(t *testing.T) {
	cases := []struct {
		f  FunctionID
		ok bool
	}{
		{FunctionID{Name: "__init__", Filename: "a.py", StartLine: 1}, false},
		{FunctionID{Module: ModuleRef{File: "[jit]", Sentinel: "[jit]"}, Name: "foo"}, false},
		{FunctionID{Module: ModuleRef{File: "[kernel]", Sentinel: "[kernel]", BuildID: "k"}, Name: "foo"}, false},
		{FunctionID{Module: ModuleRef{File: "/opt/a/libfoo.so"}, Name: "foo"}, true},
	}
	for _, c := range cases {
		key, ok := c.f.PathKey()
		if ok != c.ok {
			t.Errorf("%+v: ok %v, want %v", c.f, ok, c.ok)
		}
		if ok && key != `"/opt/a/libfoo.so"|"foo"` {
			t.Errorf("PathKey %q", key)
		}
		if !ok && key != "" {
			t.Errorf("PathKey %q with ok false", key)
		}
	}
}

func TestKeyFormats(t *testing.T) {
	bid := FunctionID{Module: ModuleRef{File: "/opt/a/libfoo.so", BuildID: "ab"}, Name: "foo"}
	nobid := FunctionID{Module: ModuleRef{File: "/opt/a/libfoo.so"}, Name: "foo"}
	jit := FunctionID{Module: ModuleRef{File: "[jit]", Sentinel: "[jit]"}, Name: "foo"}
	un := FunctionID{Name: "f|\"x", Filename: "a.py", StartLine: 3}
	for _, c := range []struct{ got, want string }{
		{bid.Module.Key(), `b:"ab"`},
		{nobid.Module.Key(), `f:"/opt/a/libfoo.so"`},
		{jit.Module.Key(), `s:"[jit]"`},
		{un.Module.Key(), ""},
		{bid.Key(), `b:"ab"|"foo"`},
		{nobid.Key(), `f:"/opt/a/libfoo.so"|"foo"`},
		{jit.Key(), `s:"[jit]"|"foo"`},
		{un.Key(), `-|"f|\"x"|"a.py"|3`},
		{bid.NameKey(), `m:"libfoo.so"|"foo"`},
		{nobid.NameKey(), `m:"libfoo.so"|"foo"`},
		{jit.NameKey(), `s:"[jit]"|"foo"`},
		{un.NameKey(), `-|"f|\"x"|"a.py"|3`},
	} {
		if c.got != c.want {
			t.Errorf("got %s, want %s", c.got, c.want)
		}
	}
}

func TestWalkStopsDescentAndPathExcludesRoot(t *testing.T) {
	v := fixtureView(t, "recursion")
	var names []string
	v.Walk(func(n *Node) bool {
		names = append(names, n.Function.Name)
		return n.Depth < 2
	})
	if !slices.Equal(names, []string{"all", "main", "a"}) {
		t.Fatalf("walk %v", names)
	}
	leaf := oneNodeAt(t, v, "main", "a", "b", "a", "b")
	var got []string
	for _, n := range v.Path(leaf) {
		got = append(got, n.Function.Name)
	}
	if !slices.Equal(got, []string{"main", "a", "b", "a", "b"}) {
		t.Fatalf("path %v", got)
	}
	if len(v.Path(v.Root)) != 0 {
		t.Fatal("root path not empty")
	}
	if _, ok := v.Node("0000000000000000"); ok {
		t.Fatal("unknown id found")
	}
}
