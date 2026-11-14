package profilemodel

import (
	"fmt"
	"slices"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
	"github.com/dpsoft/perf-agent/internal/framename"
)

// Metric names of quality facts, as structured claims name them. Every one
// is a share and is rounded as share_of_total is.
const (
	MetricQualityJoinExact          = "quality_join_exact"
	MetricQualityJoinHeuristic      = "quality_join_heuristic"
	MetricQualityJoinUnmatched      = "quality_join_unmatched"
	MetricQualityAmbiguous          = "quality_ambiguous"
	MetricQualityGraphRefused       = "quality_graph_refused"
	MetricQualityUnsampled          = "quality_unsampled_launches"
	MetricQualitySerializedTrue     = "quality_serialized_true"
	MetricQualitySerializedFalse    = "quality_serialized_false"
	MetricQualitySerializedUnknown  = "quality_serialized_unknown"
	MetricQualitySerializedMissing  = "quality_serialized_missing"
	MetricQualityUnresolved         = "quality_unresolved"
	MetricQualityUnresolvedByModule = "quality_unresolved_by_module"
	MetricQualityCollision          = "quality_collision_possible"
)

var qualityMetrics = []string{
	MetricQualityJoinExact, MetricQualityJoinHeuristic, MetricQualityJoinUnmatched,
	MetricQualityAmbiguous, MetricQualityGraphRefused, MetricQualityUnsampled,
	MetricQualitySerializedTrue, MetricQualitySerializedFalse,
	MetricQualitySerializedUnknown, MetricQualitySerializedMissing,
	MetricQualityUnresolved, MetricQualityUnresolvedByModule, MetricQualityCollision,
}

// denominatorNode names a node's own cumulative value as a share's
// denominator; it is followed by the node's NodeID.
const denominatorNode = "node:"

// Join labels gpu/projection.go sets on GPU executions.
const (
	joinLabel         = "gpu_join"
	ambiguousLabel    = "gpu_ambiguous"
	graphRefusedLabel = "gpu_graph_refused"
)

// Quality says how far a view, or the part of it under one node, can be
// trusted. Every fact is a share of the scope's value: the view total
// (denominator "view_total") or the node's cumulative value ("node:<id>").
// A zero value makes every share unavailable "zero total"; the GPU-only
// facts are unavailable "not a GPU view" on other views. Only samples with
// a non-zero value at the view's index carry weight.
type Quality struct {
	// JoinExact, JoinHeuristic and JoinUnmatched are the shares whose
	// gpu_join label is exactly that one value. A sample without the label,
	// or with any other value or several values, is in none of them, so
	// the three may sum to less than 1. GPU views only.
	JoinExact     Fact `json:"join_exact"`
	JoinHeuristic Fact `json:"join_heuristic"`
	JoinUnmatched Fact `json:"join_unmatched"`
	// Ambiguous and GraphRefused are the shares whose gpu_ambiguous or
	// gpu_graph_refused label includes "true". GPU views only.
	Ambiguous    Fact `json:"ambiguous"`
	GraphRefused Fact `json:"graph_refused"`
	// UnsampledLaunches is the share whose stack starts at
	// foldedstacks.UnsampledLaunchFrame. GPU views only.
	UnsampledLaunches Fact `json:"unsampled_launches"`
	// SamplePeriods are as PathFacts.SamplePeriods, over the scope.
	SamplePeriods []string `json:"sample_periods"`
	// Serialized is the share in each gpu_serialized state, split as
	// PathFacts.Serialization is. GPU views only; on a GPU view with a
	// non-zero value the four sum to 1.
	Serialized Serialization `json:"serialized"`
	// Unresolved is the share whose stack has an unresolved frame (a bare
	// address, "<module>+0x<offset>", or "[unknown]") anywhere on it, not
	// only at the leaf: a GPU leaf is a named kernel above CPU callers.
	// For a node that is the whole stack of each of its samples, the
	// node's callers included.
	Unresolved Fact `json:"unresolved"`
	// UnresolvedByModule splits Unresolved by the unresolved frames'
	// ModuleRef.Key(): build id, else sentinel, else full path, and "" for
	// frames with no module. A sample counts once in each module it has an
	// unresolved frame in, however many there are, so the entries may sum
	// to more than Unresolved but none exceeds it. Never nil.
	UnresolvedByModule map[string]Fact `json:"unresolved_by_module"`
	// CollisionPossible is the share whose stack passes through a node
	// marked Node.CollisionPossible, each sample counted once.
	CollisionPossible Fact `json:"collision_possible"`
	// Executions is the view's executions log availability.
	Executions Availability `json:"executions"`
}

// Quality is the quality of the whole view.
func (v *View) Quality() Quality { return v.quality(v.Root, DenominatorViewTotal) }

// NodeQuality is the quality of the samples through node id, with the
// node's cumulative value as every share's denominator. The root's
// denominator is the view total, so its quality is Quality().
func (v *View) NodeQuality(id NodeID) (Quality, error) {
	n, ok := v.nodes[id]
	if !ok {
		return Quality{}, fmt.Errorf("%w: %s", ErrUnknownNode, id)
	}
	denominator := denominatorNode + string(n.ID)
	if n == v.Root {
		denominator = DenominatorViewTotal
	}
	return v.quality(n, denominator), nil
}

// isUnresolved is the rule foldedstacks uses to count unsymbolized frames.
func isUnresolved(name string) bool {
	return name == "" || name == foldedstacks.UnknownFrame || framename.IsAddressOnly(name)
}

// stackMarks are the samples through a node whose stacks have a property.
type stackMarks struct {
	unsampled, unresolved, collision map[int]struct{}
	byModule                         map[string]map[int]struct{}
}

// marksFor finds, for the samples through n, which stacks are unsampled,
// unresolved (and in which modules) and pass through a collision. Only n's
// ancestors and its subtree share samples with n: an ancestor's frame is on
// every one of n's stacks, and a subtree node's samples are a subset of n's.
func (v *View) marksFor(n *Node) stackMarks {
	m := stackMarks{
		unsampled:  map[int]struct{}{},
		unresolved: map[int]struct{}{},
		collision:  map[int]struct{}{},
		byModule:   map[string]map[int]struct{}{},
	}
	mark := func(at *Node, samples []int) {
		if len(samples) == 0 {
			// Only zero-value samples: nothing to weigh, and no module
			// entry for a frame seen only on them.
			return
		}
		var sets []map[int]struct{}
		if at.Depth == 1 && at.Function.Name == foldedstacks.UnsampledLaunchFrame {
			sets = append(sets, m.unsampled)
		}
		if at.CollisionPossible {
			sets = append(sets, m.collision)
		}
		if at.Depth > 0 && isUnresolved(at.Function.Name) {
			key := at.Function.Module.Key()
			if m.byModule[key] == nil {
				m.byModule[key] = map[int]struct{}{}
			}
			sets = append(sets, m.unresolved, m.byModule[key])
		}
		for _, set := range sets {
			for _, s := range samples {
				set[s] = struct{}{}
			}
		}
	}
	for p := n.Parent; p != nil; p = p.Parent {
		mark(p, n.samples)
	}
	for stack := []*Node{n}; len(stack) > 0; {
		at := stack[len(stack)-1]
		stack = append(stack[:len(stack)-1], at.Children...)
		mark(at, at.samples)
	}
	return m
}

// quality measures the samples through n against n.Cum. No sum here can
// overflow: each counts a subset of n's samples at most once, and n.Cum,
// their total, was accumulated with checks when the view was built.
func (v *View) quality(n *Node, denominator string) Quality {
	value := func(s int) int64 { return v.profile.Source.Sample[s].Value[v.Index] }
	sumOf := func(set map[int]struct{}) int64 {
		var sum int64
		for s := range set {
			sum += value(s)
		}
		return sum
	}
	share := func(metric string, part int64) Fact { return shareFact(metric, part, n.Cum, denominator) }

	m := v.marksFor(n)
	q := Quality{
		SamplePeriods:      v.samplePeriods(slices.Concat(n.samples, n.zeroSamples)),
		Unresolved:         share(MetricQualityUnresolved, sumOf(m.unresolved)),
		UnresolvedByModule: make(map[string]Fact, len(m.byModule)),
		CollisionPossible:  share(MetricQualityCollision, sumOf(m.collision)),
		Executions:         v.profile.Executions,
	}
	for key, set := range m.byModule {
		q.UnresolvedByModule[key] = share(MetricQualityUnresolvedByModule, sumOf(set))
	}

	if v.SampleType != gpuSampleType {
		gpuOnly := func(metric string) Fact { return unavailableFact(metric, UnitRatio, ReasonNotGPUView) }
		q.JoinExact = gpuOnly(MetricQualityJoinExact)
		q.JoinHeuristic = gpuOnly(MetricQualityJoinHeuristic)
		q.JoinUnmatched = gpuOnly(MetricQualityJoinUnmatched)
		q.Ambiguous = gpuOnly(MetricQualityAmbiguous)
		q.GraphRefused = gpuOnly(MetricQualityGraphRefused)
		q.UnsampledLaunches = gpuOnly(MetricQualityUnsampled)
		q.Serialized = Serialization{
			True:    gpuOnly(MetricQualitySerializedTrue),
			False:   gpuOnly(MetricQualitySerializedFalse),
			Unknown: gpuOnly(MetricQualitySerializedUnknown),
			Missing: gpuOnly(MetricQualitySerializedMissing),
		}
		return q
	}

	var exact, heuristic, unmatched, ambiguous, refused int64
	var serialized [serializedStates]int64
	for _, s := range n.samples {
		smp := v.profile.Source.Sample[s]
		val := smp.Value[v.Index]
		if join := smp.Label[joinLabel]; len(join) == 1 {
			switch join[0] {
			case "exact":
				exact += val
			case "heuristic":
				heuristic += val
			case "unmatched":
				unmatched += val
			}
		}
		if slices.Contains(smp.Label[ambiguousLabel], "true") {
			ambiguous += val
		}
		if slices.Contains(smp.Label[graphRefusedLabel], "true") {
			refused += val
		}
		serialized[serializedStateOf(smp.Label)] += val
	}
	q.JoinExact = share(MetricQualityJoinExact, exact)
	q.JoinHeuristic = share(MetricQualityJoinHeuristic, heuristic)
	q.JoinUnmatched = share(MetricQualityJoinUnmatched, unmatched)
	q.Ambiguous = share(MetricQualityAmbiguous, ambiguous)
	q.GraphRefused = share(MetricQualityGraphRefused, refused)
	q.UnsampledLaunches = share(MetricQualityUnsampled, sumOf(m.unsampled))
	q.Serialized = Serialization{
		True:    share(MetricQualitySerializedTrue, serialized[serializedTrue]),
		False:   share(MetricQualitySerializedFalse, serialized[serializedFalse]),
		Unknown: share(MetricQualitySerializedUnknown, serialized[serializedUnknown]),
		Missing: share(MetricQualitySerializedMissing, serialized[serializedMissing]),
	}
	return q
}
