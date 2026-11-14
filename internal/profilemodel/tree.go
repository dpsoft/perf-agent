package profilemodel

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"slices"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

// Node is one root-to-node path of function identities. Recursion gives a
// distinct node per depth; several PCs of one function under one parent are
// one node.
type Node struct {
	ID       NodeID
	Function FunctionID
	// CollisionPossible is set when the function has no module, no filename
	// and no start line, so two different functions sharing its name would
	// be merged into this node.
	CollisionPossible bool
	Parent            *Node   `json:"-"`
	Children          []*Node `json:"-"` // sorted by Cum desc, then Function.Key()
	Depth             int     // 0 for the synthetic root
	// CallSites are the distinct locations that produced this node, sorted
	// by module key, offset and inline depth. A frame invented for an empty
	// stack has none.
	CallSites []LocationID
	// Cum, Self and Inexact are in the view's sample unit. Inexact is the
	// part of Cum from samples matching foldedstacks.DefaultInexactLabels.
	Cum, Self, Inexact int64
	samples            []int // sample indices contributing to Cum
	zeroSamples        []int // zero-value samples whose stack passes through the node (for executions only)
}

// ViewOptions select how a profile is turned into a tree.
type ViewOptions struct {
	SampleIndex        int  // -1 = foldedstacks.ChooseSampleIndex
	CollapseVendorRuns bool // rejected in M1 with ErrUnsupportedView
}

// View is a profile's node tree at one sample index.
type View struct {
	Handle           Handle
	Options          ViewOptions
	SampleType, Unit string
	Index            int
	Root             *Node // synthetic "all"
	Total            int64
	nodes            map[NodeID]*Node
	profile          *Profile
}

var (
	ErrUnsupportedView = errors.New("profilemodel: view option not supported yet")
	ErrNegativeValue   = errors.New("profilemodel: negative value at the selected sample index")
	ErrOverflow        = errors.New("profilemodel: value overflow") // wrapped with the quantity named
)

// rootName is the synthetic root's display name, as on the flame graph page.
const rootName = "all"

// View builds the node tree for opts.
//
// Each sample's stack becomes a root-first path with one node per inline
// level, named as foldedstacks.Fold names frames. A zero-value sample adds
// no value but is recorded on every node it passes through, creating them
// if needed, so its executions still count. An empty stack is filed under
// "[unknown]", as Fold does.
func (p *Profile) View(opts ViewOptions) (*View, error) {
	if opts.CollapseVendorRuns {
		return nil, fmt.Errorf("%w: CollapseVendorRuns", ErrUnsupportedView)
	}
	src := p.Source
	if len(src.SampleType) == 0 {
		return nil, errors.New("profilemodel: profile declares no sample types")
	}
	idx := opts.SampleIndex
	if idx == -1 {
		idx = foldedstacks.ChooseSampleIndex(src)
	}
	if idx < 0 || idx >= len(src.SampleType) {
		return nil, fmt.Errorf("profilemodel: sample index %d out of range (profile has %d sample types)", opts.SampleIndex, len(src.SampleType))
	}

	b := newBuilder()
	root := &Node{Function: FunctionID{Name: rootName}}
	rootState := sha256.New()
	root.ID = nodeIDOf(rootState)
	b.state[root] = rootState.(hash.Cloner)
	v := &View{
		Handle:     p.Handle,
		Options:    opts,
		SampleType: src.SampleType[idx].Type,
		Unit:       src.SampleType[idx].Unit,
		Index:      idx,
		Root:       root,
		nodes:      map[NodeID]*Node{root.ID: root},
		profile:    p,
	}
	rules := foldedstacks.DefaultInexactLabels()

	for i, s := range src.Sample {
		if idx >= len(s.Value) {
			return nil, fmt.Errorf("profilemodel: sample %d carries %d values, need index %d", i, len(s.Value), idx)
		}
		val := s.Value[idx]
		if val < 0 {
			return nil, fmt.Errorf("%w: sample %d has %d at index %d", ErrNegativeValue, i, val, idx)
		}
		c := credit{sample: i, value: val, inexact: val > 0 && foldedstacks.MatchesInexact(s.Label, rules)}
		if err := c.apply(root, false, "total"); err != nil {
			return nil, err
		}
		frames := b.framesOf(s)
		cur := root
		for j, fr := range frames {
			next, err := b.child(v, cur, fr)
			if err != nil {
				return nil, err
			}
			if err := c.apply(next, j == len(frames)-1, "cumulative value"); err != nil {
				return nil, err
			}
			cur = next
		}
	}
	v.Total = root.Cum

	for _, n := range v.nodes {
		slices.SortFunc(n.Children, func(a, b *Node) int {
			return cmp.Or(cmp.Compare(b.Cum, a.Cum), cmp.Compare(a.Function.Key(), b.Function.Key()))
		})
		slices.SortFunc(n.CallSites, compareLocations)
	}
	return v, nil
}

// Node returns the node with id.
func (v *View) Node(id NodeID) (*Node, bool) {
	n, ok := v.nodes[id]
	return n, ok
}

// Walk visits the tree in pre-order, children in their sorted order. When
// fn returns false the node's children are skipped.
func (v *View) Walk(fn func(*Node) bool) {
	stack := []*Node{v.Root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !fn(n) {
			continue
		}
		for i := len(n.Children) - 1; i >= 0; i-- {
			stack = append(stack, n.Children[i])
		}
	}
}

// Path returns the nodes from the root's child down to n, root excluded.
func (v *View) Path(n *Node) []*Node {
	var out []*Node
	for ; n != nil && n.Parent != nil; n = n.Parent {
		out = append(out, n)
	}
	slices.Reverse(out)
	return out
}

func compareLocations(a, b LocationID) int {
	return cmp.Or(
		cmp.Compare(a.Module.Key(), b.Module.Key()),
		cmp.Compare(a.Module.File, b.Module.File),
		cmp.Compare(a.Offset, b.Offset),
		cmp.Compare(a.Inline, b.Inline),
	)
}

func nodeIDOf(h hash.Hash) NodeID {
	return NodeID(hex.EncodeToString(h.Sum(nil))[:16])
}

// credit is one sample's contribution to the nodes on its path.
type credit struct {
	sample  int
	value   int64
	inexact bool
}

// apply adds the sample to n, refusing any sum that would overflow. what
// names the quantity in the error.
func (c credit) apply(n *Node, leaf bool, what string) error {
	if c.value == 0 {
		n.zeroSamples = append(n.zeroSamples, c.sample)
		return nil
	}
	var err error
	if n.Cum, err = addChecked(n.Cum, c.value, what); err != nil {
		return err
	}
	n.samples = append(n.samples, c.sample)
	if c.inexact {
		if n.Inexact, err = addChecked(n.Inexact, c.value, "inexact value"); err != nil {
			return err
		}
	}
	if leaf {
		if n.Self, err = addChecked(n.Self, c.value, "self value"); err != nil {
			return err
		}
	}
	return nil
}

// addChecked returns a+b for b >= 0, or ErrOverflow naming what.
func addChecked(a, b int64, what string) (int64, error) {
	if a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: %s", ErrOverflow, what)
	}
	return a + b, nil
}

// frameRef is one emitted frame: its function, the function's key, and the
// location it came from (absent for a frame invented for a missing one).
type frameRef struct {
	fn      FunctionID
	key     string
	site    LocationID
	hasSite bool
}

type edge struct {
	parent *Node
	key    string
}

type siteOf struct {
	node *Node
	site LocationID
}

// builder holds the state needed only while a tree is built.
type builder struct {
	frames map[*profile.Location][]frameRef
	kids   map[edge]*Node
	sites  map[siteOf]struct{}
	// state is each node's hash after its path keys, cloned to give a
	// child its ID without rehashing the whole path.
	state map[*Node]hash.Cloner
}

func newBuilder() *builder {
	return &builder{
		frames: map[*profile.Location][]frameRef{},
		kids:   map[edge]*Node{},
		sites:  map[siteOf]struct{}{},
		state:  map[*Node]hash.Cloner{},
	}
}

var unknownFrames = []frameRef{unknownFrame()}

func unknownFrame() frameRef {
	fn := FunctionID{Name: foldedstacks.UnknownFrame}
	return frameRef{fn: fn, key: fn.Key()}
}

// framesOf returns a sample's frames root first. pprof stores the leaf at
// Location[0].
func (b *builder) framesOf(s *profile.Sample) []frameRef {
	if len(s.Location) == 0 {
		return unknownFrames
	}
	var out []frameRef
	for i := len(s.Location) - 1; i >= 0; i-- {
		out = append(out, b.locationFrames(s.Location[i])...)
	}
	return out
}

// locationFrames names one location's frames, outermost inline level
// first, exactly as foldedstacks emits them.
func (b *builder) locationFrames(loc *profile.Location) []frameRef {
	if loc == nil {
		return unknownFrames
	}
	if fr, ok := b.frames[loc]; ok {
		return fr
	}
	var mod ModuleRef
	if loc.Mapping != nil {
		mod = moduleOf(loc.Mapping.File, loc.Mapping.BuildID)
	}
	var out []frameRef
	if len(loc.Line) == 0 {
		fn := FunctionID{Module: mod, Name: foldedstacks.AddressName(loc)}
		out = append(out, frameRef{fn: fn, key: fn.Key(), site: LocationID{Module: mod, Offset: loc.Address}, hasSite: true})
	}
	// Line[0] is innermost; the last Line is the function the others were
	// inlined into.
	for i := len(loc.Line) - 1; i >= 0; i-- {
		ln := loc.Line[i]
		fn := FunctionID{Module: mod, Name: foldedstacks.LineName(loc, ln)}
		if fn.unmapped() && ln.Function != nil {
			fn.Filename, fn.StartLine = ln.Function.Filename, ln.Function.StartLine
		}
		site := LocationID{Module: mod, Offset: loc.Address, Inline: len(loc.Line) - 1 - i}
		out = append(out, frameRef{fn: fn, key: fn.Key(), site: site, hasSite: true})
	}
	b.frames[loc] = out
	return out
}

// child returns parent's child for fr, creating and indexing it if needed,
// and records fr's call site on it.
func (b *builder) child(v *View, parent *Node, fr frameRef) (*Node, error) {
	e := edge{parent, fr.key}
	n := b.kids[e]
	if n == nil {
		c, err := b.state[parent].Clone()
		if err != nil {
			return nil, fmt.Errorf("profilemodel: node id: %w", err)
		}
		c.Write([]byte(fr.key))
		c.Write([]byte{0})
		n = &Node{
			ID:                nodeIDOf(c),
			Function:          fr.fn,
			CollisionPossible: fr.fn.unmapped() && fr.fn.Filename == "" && fr.fn.StartLine == 0,
			Parent:            parent,
			Depth:             parent.Depth + 1,
		}
		if _, dup := v.nodes[n.ID]; dup {
			return nil, fmt.Errorf("profilemodel: node id %s names two paths", n.ID)
		}
		b.state[n] = c
		b.kids[e] = n
		v.nodes[n.ID] = n
		parent.Children = append(parent.Children, n)
	}
	if fr.hasSite {
		k := siteOf{n, fr.site}
		if _, ok := b.sites[k]; !ok {
			b.sites[k] = struct{}{}
			n.CallSites = append(n.CallSites, fr.site)
		}
	}
	return n, nil
}
