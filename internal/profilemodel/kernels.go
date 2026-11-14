package profilemodel

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrNotGPUView is returned by a GPU-only accessor on another view.
var ErrNotGPUView = errors.New("profilemodel: not a GPU view")

// KernelRow is one kernel name at or below a node.
type KernelRow struct {
	Name string
	// Value is the sum of the values of the samples through the node whose
	// frames at or below it include the kernel, once per sample; never
	// scaled by the launch sampling period.
	Value        Fact
	ShareOfNode  Fact // Value / the node's cumulative; denominator "node:<id>"
	ShareOfTotal Fact // Value / the view total
	// Executions counts distinct ordinals of the checked log over those
	// samples, zero-value ones included; unavailable without a usable log.
	Executions Fact
}

// KernelsBelow lists the kernels at or below id, largest value first, then
// by name. A kernel seen only in zero-value samples is a row with status
// "zero".
func (v *View) KernelsBelow(id NodeID) ([]KernelRow, error) {
	n, ok := v.nodes[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownNode, id)
	}
	if v.SampleType != gpuSampleType {
		return nil, ErrNotGPUView
	}
	type acc struct {
		value   int64
		samples []int
	}
	b := newBuilder()
	from := max(0, n.Depth-1)
	byName := map[string]*acc{}
	visit := func(samples []int, weighted bool) error {
		for _, s := range samples {
			frames := b.framesOf(v.profile.Source.Sample[s])
			seen := map[string]bool{}
			for _, fr := range frames[min(from, len(frames)):] {
				name, ok := strings.CutPrefix(fr.fn.Name, kernelFramePrefix)
				if !ok {
					continue
				}
				if name, ok = strings.CutSuffix(name, "]"); !ok || seen[name] {
					continue
				}
				seen[name] = true
				a := byName[name]
				if a == nil {
					a = &acc{}
					byName[name] = a
				}
				a.samples = append(a.samples, s)
				if weighted {
					var err error
					if a.value, err = addChecked(a.value, v.profile.Source.Sample[s].Value[v.Index], "kernel value"); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := visit(n.samples, true); err != nil {
		return nil, err
	}
	if err := visit(n.zeroSamples, false); err != nil {
		return nil, err
	}
	node := "node:" + string(n.ID)
	rows := make([]KernelRow, 0, len(byName))
	for name, a := range byName {
		rows = append(rows, KernelRow{
			Name:         name,
			Value:        intFact(MetricCumulative, a.value, v.Unit),
			ShareOfNode:  shareFact(MetricShareOfTotal, a.value, n.Cum, node),
			ShareOfTotal: shareFact(MetricShareOfTotal, a.value, v.Total, DenominatorViewTotal),
			Executions:   v.executionsBelow(a.samples),
		})
	}
	slices.SortFunc(rows, func(x, y KernelRow) int {
		return cmp.Or(cmp.Compare(*y.Value.Int, *x.Value.Int), cmp.Compare(x.Name, y.Name))
	})
	return rows, nil
}
