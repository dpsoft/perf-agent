package profile

import (
	"testing"

	"strings"

	"github.com/cilium/ebpf"
)

// ----- Issue #83 / T12-R6: the interpreter HANDOFF rides on the shared
// walker; the interpreters themselves do not.
//
// perf_dwarf, offcpu_dwarf and gpu_usdt all drive the same walk, so any
// language module reaches all three by construction rather than by three
// integrations. What has to be in each embedded object for that to work is a
// claim table, a tail-call table, somewhere to keep the cursor across a tail
// call, and the two programs a module hands control back through -- and
// NOTHING belonging to any particular language. Assert both halves here
// rather than discover either on a machine with capabilities.
func TestEmbeddedDWARFProgramsCarryTheInterpreterSeam(t *testing.T) {
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
			for _, m := range []string{"handoff_ranges", "interp_progs", "walk_states", "walker_scratch"} {
				if _, ok := spec.Maps[m]; !ok {
					t.Errorf("%s is missing: no interpreter can be dispatched from this program", m)
				}
			}
			if got := spec.Maps["handoff_ranges"].KeySize; got != 8 {
				t.Errorf("handoff_ranges key is %d bytes, want 8 (table_id, not pid)", got)
			}
			if got := spec.Maps["interp_progs"].Type; got != ebpf.ProgramArray {
				t.Errorf("interp_progs is %v, want ProgramArray", got)
			}
			if got := spec.Maps["walk_states"].Type; got != ebpf.PerCPUArray {
				t.Errorf("walk_states is %v, want PerCPUArray: the cursor must survive a tail call", got)
			}
			for _, p := range []string{"interp_resume_step", "interp_resume_walk"} {
				if _, ok := spec.Programs[p]; !ok {
					t.Errorf("%s is missing: a dispatched sample would lose its whole native tail", p)
				}
			}

			// The negative, and it is the point of the separation: a module
			// compiled into this object would be a module the core had to
			// know about. pyunwind's own object is where py_procs lives now.
			for name := range spec.Maps {
				if strings.HasPrefix(name, "py_") {
					t.Errorf("%s carries %s; a language module must be its own object", tc.name, name)
				}
			}
		})
	}
}

// Moving the interpreter walk out of the shared callback -- and the walk
// cursor out of struct walk_ctx and into a map -- must not have moved the
// sample record: walk_ctx lives on the BPF stack, the record lives in a map,
// and unwind/dwarfagent parses the record by byte offset.
func TestTheInterpreterSeamDidNotResizeTheSampleRecord(t *testing.T) {
	if PerfDwarfSampleRecordSize != 1184 {
		t.Fatalf("sample_record is %d bytes, want 1184: every offset in unwind/dwarfagent's parser moved",
			PerfDwarfSampleRecordSize)
	}
}
