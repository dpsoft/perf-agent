package profilemodel

import (
	"slices"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func TestDiffIndexCoversTheWholeUnion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base, new []byte
	}{
		{"Deep(2000) vs DeepNew(2000)", pmfixture.Deep(2000), pmfixture.DeepNew(2000)},
		{"CPUBasic vs CPUBasicNew", pmfixture.CPUBasic(), pmfixture.CPUBasicNew()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := compareOf(t, tc.base, tc.new, WorkCounts{})
			ix := NewDiffIndex(d)
			flat := d.Flatten()
			if ix.Len() != len(flat.Nodes) {
				t.Fatalf("index holds %d nodes, the union has %d", ix.Len(), len(flat.Nodes))
			}
			parent := make(map[UnionID]UnionID, len(flat.Nodes))
			for _, f := range flat.Nodes {
				parent[f.ID] = f.Parent
			}
			for _, f := range flat.Nodes {
				n, ok := ix.Node(f.ID)
				if !ok {
					t.Fatalf("node %s is not in the index", f.ID)
				}
				if n.UnionID() != f.ID || n.Name != f.Name || n.Combined != f.Combined {
					t.Fatalf("node %s: index has %s %q %d", f.ID, n.UnionID(), n.Name, n.Combined)
				}
				var want []UnionID
				for id := f.ID; id != ""; id = parent[id] {
					want = append(want, id)
				}
				slices.Reverse(want)
				var got []UnionID
				for _, p := range ix.Path(f.ID) {
					got = append(got, p.UnionID())
				}
				if !slices.Equal(got, want) {
					t.Fatalf("path of %s is %v, the flattened parents give %v", f.ID, got, want)
				}
			}
			if ix.Path(flat.Nodes[0].ID)[0] != d.Root {
				t.Errorf("a path does not start at the diff's root")
			}
		})
	}
}

func TestDiffIndexRefusesAnUnknownNode(t *testing.T) {
	d := compareOf(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew(), WorkCounts{})
	ix := NewDiffIndex(d)
	for _, id := range []UnionID{"", "-/-", "nope", d.Root.UnionID() + "x"} {
		if _, ok := ix.Node(id); ok {
			t.Errorf("Node(%q) found a node", id)
		}
		if p := ix.Path(id); p != nil {
			t.Errorf("Path(%q) = %v, want nil", id, p)
		}
	}
	empty := NewDiffIndex(&Diff{})
	if empty.Len() != 0 {
		t.Errorf("a diff without a root indexes %d nodes", empty.Len())
	}
}

func TestFindDiffNodeWalksToAnyNode(t *testing.T) {
	d := compareOf(t, pmfixture.Deep(600), pmfixture.DeepNew(600), WorkCounts{})
	ix := NewDiffIndex(d)
	for _, f := range d.Flatten().Nodes {
		path, ok := FindDiffNode(d, f.ID)
		if !ok {
			t.Fatalf("FindDiffNode(%s) found nothing", f.ID)
		}
		if !slices.Equal(path, ix.Path(f.ID)) {
			t.Fatalf("FindDiffNode(%s) and the index disagree on the path", f.ID)
		}
	}
	if _, ok := FindDiffNode(d, "nope"); ok {
		t.Error("an unknown node was found")
	}
	if _, ok := FindDiffNode(&Diff{}, "nope"); ok {
		t.Error("a node was found in a diff without a root")
	}
}

func TestDiffRetainedEstimatesAreFixedAtConstruction(t *testing.T) {
	d := compareOf(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew(), WorkCounts{})
	ix := NewDiffIndex(d)
	db, ib := d.RetainedBytes(), ix.RetainedBytes()
	if db <= 0 || ib <= 0 {
		t.Fatalf("estimates %d and %d, want both positive", db, ib)
	}
	_ = d.Flatten()
	if _, ok := ix.Node(d.Root.UnionID()); !ok {
		t.Fatal("the root is not indexed")
	}
	if d.RetainedBytes() != db || ix.RetainedBytes() != ib {
		t.Errorf("estimates moved after a read: %d→%d, %d→%d", db, d.RetainedBytes(), ib, ix.RetainedBytes())
	}
	// A diff not built by Compare is estimated from its nodes all the same.
	lit := &Diff{Root: &DiffNode{Name: "all", Children: []*DiffNode{{Name: "a"}}}}
	if lit.RetainedBytes() <= 0 {
		t.Errorf("a literal diff is estimated at %d bytes", lit.RetainedBytes())
	}
}
