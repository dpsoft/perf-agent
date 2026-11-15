package pmfixture

import (
	"bytes"
	"fmt"
	"math/rand/v2"

	"github.com/google/pprof/profile"
)

// Shape of Wide: its functions, modules, locations and call tree.
const (
	wideModules   = 16
	wideFunctions = 4096
	wideLocations = 16384
	wideTreeNodes = 20000
	wideRoots     = 8
	wideMaxDepth  = 40
)

// Wide is cpu/nanoseconds with n samples, drawn the same way on every call:
// 4096 functions in 16 modules at 16384 locations (every eighth with an
// inlined line), on a call tree of 20000 paths at most 40 frames deep. Each
// sample's stack is the path to a random node of that tree, its value is 1
// to 100, and every other sample carries a "thread" label. It stands in for
// a real profile where an estimate of retained memory has to hold.
func Wide(n int) []byte {
	rng := rand.New(rand.NewPCG(1, 2))
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
	}
	for i := range wideModules {
		p.Mapping = append(p.Mapping, &profile.Mapping{
			ID: uint64(i + 1), Start: uint64(i+1) << 32, Limit: uint64(i+2) << 32,
			File: fmt.Sprintf("/opt/wide/lib%02d.so", i), BuildID: fmt.Sprintf("wide%02d", i),
			HasFunctions: true,
		})
	}
	for i := range wideFunctions {
		p.Function = append(p.Function, &profile.Function{
			ID: uint64(i + 1), Name: fmt.Sprintf("wide::module%02d::function%04d", i%wideModules, i),
			SystemName: fmt.Sprintf("_ZN4wide8module%02d12function%04dEv", i%wideModules, i),
			Filename:   fmt.Sprintf("/src/wide/module%02d/file%03d.cc", i%wideModules, i/32), StartLine: int64(10 + i%500),
		})
	}
	for i := range wideLocations {
		f := p.Function[i%wideFunctions]
		loc := &profile.Location{ID: uint64(i + 1), Mapping: p.Mapping[i%wideModules], Address: uint64(0x1000 + 16*i),
			Line: []profile.Line{{Function: f, Line: f.StartLine + int64(i/wideFunctions)}}}
		if i%8 == 0 {
			g := p.Function[(i*7+3)%wideFunctions]
			loc.Line = append([]profile.Line{{Function: g, Line: g.StartLine + 1}}, loc.Line...)
		}
		p.Location = append(p.Location, loc)
	}

	// The call tree: each node is a location under a parent. A new node
	// mostly extends one of the latest ones, so paths grow deep, and starts
	// anywhere once its parent is at the maximum depth.
	type treeNode struct {
		loc    *profile.Location
		parent int
		depth  int
	}
	tree := make([]treeNode, 0, wideTreeNodes)
	for range wideRoots {
		tree = append(tree, treeNode{loc: p.Location[rng.IntN(wideLocations)], parent: -1, depth: 1})
	}
	for len(tree) < wideTreeNodes {
		parent := len(tree) - 1 - rng.IntN(min(len(tree), 64))
		if tree[parent].depth >= wideMaxDepth {
			parent = rng.IntN(len(tree))
			if tree[parent].depth >= wideMaxDepth {
				continue
			}
		}
		tree = append(tree, treeNode{loc: p.Location[rng.IntN(wideLocations)], parent: parent, depth: tree[parent].depth + 1})
	}

	threads := []string{"main", "worker-1", "worker-2", "worker-3", "io", "gc"}
	for i := range n {
		var stack []*profile.Location // leaf first, as pprof stores it
		for j := rng.IntN(len(tree)); j >= 0; j = tree[j].parent {
			stack = append(stack, tree[j].loc)
		}
		s := &profile.Sample{Location: stack, Value: []int64{1 + rng.Int64N(100)}}
		if i%2 == 0 {
			s.Label = map[string][]string{"thread": {threads[rng.IntN(len(threads))]}}
		}
		p.Sample = append(p.Sample, s)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
