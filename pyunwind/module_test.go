package pyunwind

import (
	"testing"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/internal/kernelver"
	"github.com/dpsoft/perf-agent/unwind/interp"
)

// The module's uprobe program is BPF_PROG_TYPE_KPROBE, which cilium/ebpf will
// not load without a kernel version -- and it cannot discover one in a setcap'd
// process, because file capabilities make the process non-dumpable and both the
// auxv and /proc/self/mem reads it needs are refused.
//
// This is the defect that stopped the GPU path dead while both perf_event
// drivers were unaffected:
//
//	gpuprobe: interpreter frames: python: load: program interp_python_uprobe:
//	  detecting kernel version: read auxv from runtime: no such file or directory
//
// Two halves, both asserted, because either alone passes while the bug is
// present: the hazard is real (the program IS Kprobe-typed and ships with no
// version), and the fix applies to it.
func TestTheUprobeProgramNeedsAndGetsAKernelVersion(t *testing.T) {
	spec, err := Module().Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	name := Module().ProgramName(interp.FlavourUprobeMulti)
	ps := spec.Programs[name]
	if ps == nil {
		t.Fatalf("no program %q in the module object", name)
	}
	if ps.Type != ebpf.Kprobe {
		t.Fatalf("%s is %v; if it is no longer Kprobe-typed this test's premise is gone", name, ps.Type)
	}
	if ps.KernelVersion != 0 {
		t.Fatalf("%s already carries version %#x; the object grew a `version` section and "+
			"this test no longer covers the loader forgetting to supply one", name, ps.KernelVersion)
	}

	kernelver.Apply(spec)
	if ps.KernelVersion == 0 {
		t.Error("after kernelver.Apply the uprobe program still has no kernel version; " +
			"loading it in a setcap'd process will fail with an error naming neither " +
			"capabilities nor uprobes")
	}

	// And the perf_event program, which never had the problem, must be
	// unharmed -- the fix is applied to the whole spec on purpose.
	if pe := spec.Programs[Module().ProgramName(interp.FlavourPerfEvent)]; pe == nil || pe.KernelVersion == 0 {
		t.Error("the perf_event program lost its version")
	}
}
