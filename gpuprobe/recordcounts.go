package gpuprobe

import (
	"fmt"
	"strings"

	"github.com/cilium/ebpf"
)

// RecordKindCounts holds record counts indexed by the GPU wire kind. Slot
// zero is unknown; reserved slots remain visible rather than being discarded.
// These are records, not producer batches or executions alone.
type RecordKindCounts [kindMax]uint64

// Executions returns the count of records carrying measured GPU durations.
func (counts RecordKindCounts) Executions() uint64 { return counts[kindExec] }

// Total sums counts of the same unit, including unknown and reserved kinds.
func (counts RecordKindCounts) Total() uint64 {
	var total uint64
	for _, n := range counts {
		total += n
	}
	return total
}

// String describes the nonzero kinds in wire order for diagnostics.
func (counts RecordKindCounts) String() string {
	names := [...]string{"unknown", "launch", "exec", "module", "pc", "sampled_launch", "kernel_name", "stall_map", "sampling_window", "config", "drop_report"}
	var parts []string
	for kind, n := range counts {
		if n == 0 {
			continue
		}
		name := fmt.Sprintf("kind_%d", kind)
		if kind < len(names) {
			name = names[kind]
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, n))
	}
	return strings.Join(parts, " ")
}

func readKindCounts(m *ebpf.Map) (RecordKindCounts, uint64) {
	var counts RecordKindCounts
	if m == nil {
		return counts, 0
	}
	var failures uint64
	for kind := range uint32(kindMax) {
		if err := m.Lookup(&kind, &counts[kind]); err != nil {
			failures++
		}
	}
	return counts, failures
}
