package profilemodel

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func unionSize(d *Diff) int {
	n := 0
	walkDiff(d.Root, func(*DiffNode) { n++ })
	return n
}

func TestFlattenIsPreOrderAndParentsComeFirst(t *testing.T) {
	for name := range diffPairs() {
		t.Run(name, func(t *testing.T) {
			d := pairDiff(t, name)
			f := d.Flatten()
			if len(f.Nodes) != unionSize(d) {
				t.Fatalf("%d nodes, want %d", len(f.Nodes), unionSize(d))
			}
			if f.Nodes[0].Parent != "" || f.Nodes[0].Depth != 0 {
				t.Fatalf("first node parent %q depth %d, want root", f.Nodes[0].Parent, f.Nodes[0].Depth)
			}
			seen := map[UnionID]int{}
			for i, n := range f.Nodes {
				if _, dup := seen[n.ID]; dup {
					t.Fatalf("duplicate ID %q", n.ID)
				}
				if i > 0 {
					pd, ok := seen[n.Parent]
					if !ok {
						t.Fatalf("node %q: parent %q not seen earlier", n.ID, n.Parent)
					}
					if n.Depth != pd+1 {
						t.Fatalf("node %q depth %d, parent depth %d", n.ID, n.Depth, pd)
					}
				}
				seen[n.ID] = n.Depth
			}
			// Pre-order with children in Diff order: compare with a recursive walk.
			var want []string
			walkDiff(d.Root, func(n *DiffNode) { want = append(want, n.Name) })
			for i, n := range f.Nodes {
				if n.Name != want[i] {
					t.Fatalf("node %d is %q, pre-order wants %q", i, n.Name, want[i])
				}
			}
		})
	}
}

func TestFlattenUnionIDsNameBothSides(t *testing.T) {
	d := compareOf(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew(), WorkCounts{})
	f := d.Flatten()
	byName := map[string]FlatDiffNode{}
	for _, n := range f.Nodes {
		byName[n.Name] = n
	}
	main := oneDiffAt(t, d, "main")
	if want := UnionID(string(main.BaseID) + "/" + string(main.NewID)); main.BaseID == "" || main.NewID == "" || byName["main"].ID != want {
		t.Fatalf("main ID %q, want %q", byName["main"].ID, want)
	}
	added := oneDiffAt(t, d, "main", "d")
	if added.Presence != PresenceOnlyNew {
		t.Fatalf("main;d presence %s", added.Presence)
	}
	if want := UnionID("-/" + string(added.NewID)); byName["d"].ID != want {
		t.Fatalf("d ID %q, want %q", byName["d"].ID, want)
	}
	if byName["d"].Parent != byName["main"].ID {
		t.Fatalf("d parent %q, want %q", byName["d"].Parent, byName["main"].ID)
	}
	var removed *DiffNode
	walkDiff(d.Root, func(n *DiffNode) {
		if n.Presence == PresenceOnlyBase && removed == nil {
			removed = n
		}
	})
	if removed != nil {
		want := UnionID(string(removed.BaseID) + "/-")
		found := false
		for _, n := range f.Nodes {
			found = found || n.ID == want
		}
		if !found {
			t.Fatalf("no node with ID %q", want)
		}
	}
}

func TestFlattenCarriesEveryField(t *testing.T) {
	for name := range diffPairs() {
		t.Run(name, func(t *testing.T) {
			d := pairDiff(t, name)
			f := d.Flatten()
			var nodes []*DiffNode
			walkDiff(d.Root, func(n *DiffNode) { nodes = append(nodes, n) })
			skip := map[string]bool{"Base": true, "New": true, "Children": true}
			ft := reflect.TypeFor[FlatDiffNode]()
			for i, n := range nodes {
				nv, fv := reflect.ValueOf(*n), reflect.ValueOf(f.Nodes[i])
				for _, sf := range reflect.VisibleFields(reflect.TypeFor[DiffNode]()) {
					if skip[sf.Name] {
						continue
					}
					ff, ok := ft.FieldByName(sf.Name)
					if !ok {
						t.Fatalf("FlatDiffNode has no field %s", sf.Name)
					}
					if ff.Tag.Get("json") != sf.Tag.Get("json") {
						t.Errorf("%s json tag %q, DiffNode %q", sf.Name, ff.Tag.Get("json"), sf.Tag.Get("json"))
					}
					if !reflect.DeepEqual(nv.FieldByName(sf.Name).Interface(), fv.FieldByName(sf.Name).Interface()) {
						t.Errorf("node %d field %s differs", i, sf.Name)
					}
				}
			}
			dv, fv := reflect.ValueOf(*d), reflect.ValueOf(f)
			for _, sf := range reflect.VisibleFields(reflect.TypeFor[Diff]()) {
				// The memory estimate is bookkeeping, not part of the diff.
				if sf.Name == "Root" || !sf.IsExported() {
					continue
				}
				ff, ok := reflect.TypeFor[FlatDiff]().FieldByName(sf.Name)
				if !ok || ff.Tag.Get("json") != sf.Tag.Get("json") {
					t.Errorf("FlatDiff field %s missing or json tag differs", sf.Name)
					continue
				}
				if !reflect.DeepEqual(dv.FieldByName(sf.Name).Interface(), fv.FieldByName(sf.Name).Interface()) {
					t.Errorf("header field %s differs", sf.Name)
				}
			}
		})
	}
}

// A 600-frame stack must not nest the JSON: decoders limit depth.
func TestFlatDiffJSONNestingIsBounded(t *testing.T) {
	d := compareOf(t, pmfixture.Deep(600), pmfixture.DeepNew(600), WorkCounts{})
	f := d.Flatten()
	if len(f.Nodes) < 600 {
		t.Fatalf("%d nodes, want at least 600", len(f.Nodes))
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	depth, most := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				most = max(most, depth)
			default:
				depth--
			}
		}
	}
	if most > 6 {
		t.Fatalf("max nesting %d, want at most 6", most)
	}
}
