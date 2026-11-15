package profilemodel

import (
	"slices"
	"strings"
)

// UnionID is n's union ID, as Flatten names it.
func (n *DiffNode) UnionID() UnionID { return unionID(n) }

// DiffIndex finds any node of a diff's union by its UnionID, with its path
// from the root. It covers the whole union, so a node a budgeted model left
// out is found all the same. It is built once, by an iterative walk, and is
// immutable after: concurrent readers need no lock.
type DiffIndex struct {
	nodes    []*DiffNode // in pre-order, children in Diff order
	parent   []int32     // index of each node's parent; -1 for the root
	byID     map[UnionID]int32
	retained int64
}

// NewDiffIndex indexes every node of d's union. A diff without a root
// indexes nothing.
func NewDiffIndex(d *Diff) *DiffIndex {
	ix := &DiffIndex{byID: map[UnionID]int32{}}
	if d == nil || d.Root == nil {
		ix.retained = indexRetained(ix)
		return ix
	}
	type item struct {
		n      *DiffNode
		parent int32
	}
	stack := []item{{n: d.Root, parent: -1}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		i := int32(len(ix.nodes))
		ix.nodes = append(ix.nodes, it.n)
		ix.parent = append(ix.parent, it.parent)
		ix.byID[it.n.UnionID()] = i
		// Pushed in reverse so the first child pops first.
		for _, c := range slices.Backward(it.n.Children) {
			stack = append(stack, item{n: c, parent: i})
		}
	}
	ix.retained = indexRetained(ix)
	return ix
}

// Len is how many union nodes the index holds.
func (ix *DiffIndex) Len() int { return len(ix.nodes) }

// Node is the union node id names.
func (ix *DiffIndex) Node(id UnionID) (*DiffNode, bool) {
	i, ok := ix.byID[id]
	if !ok {
		return nil, false
	}
	return ix.nodes[i], true
}

// Path is the union node id names and its callers, root first; nil when
// the index has no such node.
func (ix *DiffIndex) Path(id UnionID) []*DiffNode {
	i, ok := ix.byID[id]
	if !ok {
		return nil
	}
	var out []*DiffNode
	for ; i >= 0; i = ix.parent[i] {
		out = append(out, ix.nodes[i])
	}
	slices.Reverse(out)
	return out
}

// RetainedBytes estimates the heap the index keeps reachable beyond the
// diff it indexes: its node and parent slices, its map and the union IDs
// it keys the map by. It is computed once, when the index is built.
func (ix *DiffIndex) RetainedBytes() int64 { return ix.retained }

// FindDiffNode walks d's union for the node id names and returns it with
// its callers, root first. It builds no index: one lookup costs a walk of
// the union and a stack, and no map of it.
func FindDiffNode(d *Diff, id UnionID) ([]*DiffNode, bool) {
	base, next, ok := strings.Cut(string(id), "/")
	if d == nil || d.Root == nil || !ok {
		return nil, false
	}
	// The sides are compared as they are, so the walk allocates no ID.
	is := func(n *DiffNode) bool {
		return sideID(n.BaseID) == base && sideID(n.NewID) == next
	}
	type item struct {
		n     *DiffNode
		depth int
	}
	var path []*DiffNode
	stack := []item{{n: d.Root}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		path = append(path[:it.depth], it.n)
		if is(it.n) {
			return path, true
		}
		for _, c := range slices.Backward(it.n.Children) {
			stack = append(stack, item{n: c, depth: it.depth + 1})
		}
	}
	return nil, false
}

// sideID is one side of a union ID: the node ID, or "-" when absent.
func sideID(id NodeID) string {
	if id == "" {
		return "-"
	}
	return string(id)
}
