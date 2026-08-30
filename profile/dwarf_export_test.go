package profile

import (
	"testing"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/pyunwind"
)

// ----- Issue #83: the interpreter arm rides on the shared walk_step.
//
// perf_dwarf, offcpu_dwarf and gpu_usdt all drive the same bpf_loop callback,
// so the Python walk reaches all three by construction rather than by three
// integrations. That is only true if the maps it needs actually came along
// with the header into each embedded object — assert it here rather than
// discover it on a machine with capabilities.
func TestEmbeddedDWARFProgramsCarryThePythonMaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		load func() (*ebpf.CollectionSpec, error)
	}{
		{"perf_dwarf", loadPerf_dwarf},
		{"offcpu_dwarf", loadOffcpu_dwarf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := tc.load()
			if err != nil {
				t.Fatalf("load spec: %v", err)
			}
			for _, m := range []string{"py_procs", "py_eval_ranges", "py_walk_counters"} {
				if _, ok := spec.Maps[m]; !ok {
					t.Errorf("%s is missing: the Python walk cannot run in this program", m)
				}
			}
			if got := spec.Maps["py_walk_counters"].MaxEntries; got != pyunwind.PyCntMax {
				t.Errorf("py_walk_counters has %d slots, pyunwind expects %d", got, pyunwind.PyCntMax)
			}
			if got := spec.Maps["py_walk_counters"].Type; got != ebpf.PerCPUArray {
				t.Errorf("py_walk_counters is %v, want PerCPUArray", got)
			}
			if got := spec.Maps["py_eval_ranges"].KeySize; got != 8 {
				t.Errorf("py_eval_ranges key is %d bytes, want 8 (table_id, not pid)", got)
			}
		})
	}
}

// Adding the Python resume cursor to struct walk_ctx must not have moved the
// sample record: walk_ctx lives on the BPF stack, the record lives in a map,
// and unwind/dwarfagent parses the record by byte offset.
func TestThePythonCursorDidNotResizeTheSampleRecord(t *testing.T) {
	if PerfDwarfSampleRecordSize != 1184 {
		t.Fatalf("sample_record is %d bytes, want 1184: every offset in unwind/dwarfagent's parser moved",
			PerfDwarfSampleRecordSize)
	}
}
