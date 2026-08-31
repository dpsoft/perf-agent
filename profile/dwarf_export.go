package profile

import (
	"fmt"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// PerfDwarf is a thin wrapper around the generated perf_dwarf BPF objects.
// Construct with LoadPerfDwarf; always Close() when done.
type PerfDwarf struct {
	objs perf_dwarfObjects
}

// PerfDwarfSampleRecordSize is the real, compiler-computed size of the
// generated perf_dwarfSampleRecord — bpf2go's Go mirror of
// bpf/unwind_common.h's struct sample_record, alignment padding included.
// Exported so unwind/dwarfagent can pin its own SampleRecordBytes constant
// against the actual generated struct rather than a hand-derived formula
// that can silently drift from it (issue #83's SampleRecordBytes review
// finding: the hand-derived formula omitted the struct's trailing
// alignment pad and nothing caught it).
const PerfDwarfSampleRecordSize = unsafe.Sizeof(perf_dwarfSampleRecord{})

// LoadPerfDwarf loads the BPF program and returns a handle. Caller must
// Close(). The program isn't attached to any perf event yet — the caller
// opens perf_event_open fds and attaches separately (see
// unwind/dwarfagent for the full wiring).
//
// kernelStacks gates the BPF program's kernel-stack capture (set from
// cfg.KernelStacks). When false, kernel-stack capture is fully bypassed
// at sample time; the CollectKernel bit on each pid_config entry is a
// no-op. When true, kernel stacks are captured for matched samples.
func LoadPerfDwarf(systemWide, kernelStacks bool) (*PerfDwarf, error) {
	// Match perfagent/agent.go's Start() ordering: promote caps to the
	// effective set, then raise RLIMIT_MEMLOCK via CAP_SYS_ADMIN, then
	// load the BPF program. Without RemoveMemlock the BPF_MAP_CREATE
	// syscall hits EPERM under lockdown-integrity + an 8 MB default
	// memlock limit (the library's error message mis-attributes this
	// as "operation not permitted" since the syscall returns EPERM
	// rather than ENOMEM).
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true,
		cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE); err != nil {
		return nil, fmt.Errorf("set capabilities: %w", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	newSpec := func() (*ebpf.CollectionSpec, error) {
		spec, err := loadPerf_dwarf()
		if err != nil {
			return nil, fmt.Errorf("load perf_dwarf spec: %w", err)
		}
		if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
			return nil, fmt.Errorf("set system_wide: %w", err)
		}
		if err := spec.Variables["kernel_stacks_enabled"].Set(kernelStacks); err != nil {
			return nil, fmt.Errorf("set kernel_stacks_enabled: %w", err)
		}
		return spec, nil
	}
	p := &PerfDwarf{}
	if err := loadWithPythonGate("perf_dwarf", newSpec, func(spec *ebpf.CollectionSpec) error {
		return spec.LoadAndAssign(&p.objs, nil)
	}); err != nil {
		return nil, fmt.Errorf("load and assign: %w", err)
	}
	return p, nil
}

// Program returns the PerfDwarf program for attaching to a perf_event_open fd.
func (p *PerfDwarf) Program() *ebpf.Program {
	return p.objs.PerfDwarf
}

// RingbufMap returns the stack_events ringbuf for ringbuf.NewReader.
func (p *PerfDwarf) RingbufMap() *ebpf.Map {
	return p.objs.StackEvents
}

// KernStackmap returns the BPF_MAP_TYPE_STACK_TRACE used for kernel
// stack-ID lookup. Populated only when kernel_stacks_enabled is true at
// BPF load; otherwise samples carry KernStack == -1 and userspace skips
// the lookup. Mirror of FP profiler's Stackmap accessor.
func (p *PerfDwarf) KernStackmap() *ebpf.Map {
	return p.objs.KernStackmap
}

// SetSystemWide is a no-op; the setting is baked in at load time via the
// systemWide argument to LoadPerfDwarf. Kept as a stable API placeholder.
func (p *PerfDwarf) SetSystemWide(v bool) error {
	return nil
}

// AddPID registers a target PID for sampling. Matches the semantics of
// profile.Profiler's PID filter in targeted mode.
func (p *PerfDwarf) AddPID(pid uint32) error {
	cfg := perf_dwarfPidConfig{
		Type:          0,
		CollectUser:   1,
		CollectKernel: 1, // gated by BPF kernel_stacks_enabled global
	}
	return p.objs.Pids.Update(pid, &cfg, ebpf.UpdateAny)
}

// Close releases all BPF objects.
func (p *PerfDwarf) Close() error {
	return p.objs.Close()
}

// PyProcsMap returns py_procs: the per-PID CPython layout record
// pyunwind.Attach installs after validating a target's interpreter. Without
// an entry here walk_step's interpreter arm counts PY_CNT_NO_PROC_INFO and
// pushes nothing.
func (p *PerfDwarf) PyProcsMap() *ebpf.Map {
	return p.objs.PyProcs
}

// PyEvalRangesMap returns py_eval_ranges: one eval-loop text range per
// libpython, keyed by table_id. This map is the interpreter arm's on-switch —
// until a range is installed, no PC ever falls inside one and the Python walk
// never runs. See pyunwind.InstallEvalRange.
func (p *PerfDwarf) PyEvalRangesMap() *ebpf.Map {
	return p.objs.PyEvalRanges
}

// PyWalkCountersMap returns py_walk_counters, the per-CPU array in which the
// Python walk records every frame it pushed and every way it refused. Read it
// with pyunwind.ReadWalkCounters: sample_header.walker_flags has no bits left
// for the Python path, so these counters are the only place a Python-side
// refusal is visible at all.
func (p *PerfDwarf) PyWalkCountersMap() *ebpf.Map {
	return p.objs.PyWalkCounters
}

// CFIRulesMap returns the cfi_rules HASH_OF_MAPS outer map.
func (p *PerfDwarf) CFIRulesMap() *ebpf.Map {
	return p.objs.CfiRules
}

// CFILengthsMap returns the cfi_lengths HASH keyed by table_id → u32 length.
func (p *PerfDwarf) CFILengthsMap() *ebpf.Map {
	return p.objs.CfiLengths
}

// CFIClassificationMap returns the cfi_classification HASH_OF_MAPS outer map.
func (p *PerfDwarf) CFIClassificationMap() *ebpf.Map {
	return p.objs.CfiClassification
}

// CFIClassificationLengthsMap returns the cfi_classification_lengths HASH.
func (p *PerfDwarf) CFIClassificationLengthsMap() *ebpf.Map {
	return p.objs.CfiClassificationLengths
}

// PIDMappingsMap returns the pid_mappings HASH_OF_MAPS outer map.
func (p *PerfDwarf) PIDMappingsMap() *ebpf.Map {
	return p.objs.PidMappings
}

// PIDMappingLengthsMap returns the pid_mapping_lengths HASH keyed by pid → u32 length.
func (p *PerfDwarf) PIDMappingLengthsMap() *ebpf.Map {
	return p.objs.PidMappingLengths
}

// CFIMissRingbuf returns the ringbuf the BPF walker writes lazy-CFI miss
// notifications to. Userspace drains this in lazy mode (--unwind auto).
func (p *PerfDwarf) CFIMissRingbuf() *ebpf.Map {
	return p.objs.CfiMissEvents
}

// CFIMissRatelimitMap returns the per-(pid, table_id) rate-limit map.
// Exposed for tests that want to introspect the rate-limit state; not
// needed by the production path.
func (p *PerfDwarf) CFIMissRatelimitMap() *ebpf.Map {
	return p.objs.CfiMissRatelimit
}
