package profile

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// OffCPUDwarf is a thin wrapper around the generated offcpu_dwarf BPF
// objects. Construct with LoadOffCPUDwarf; always Close() when done.
type OffCPUDwarf struct {
	objs offcpu_dwarfObjects
}

// LoadOffCPUDwarf loads the BPF program and returns a handle. Caller
// must Close(). The tp_btf/sched_switch program isn't attached yet —
// see unwind/dwarfagent.OffCPUProfiler for the attach wiring via
// link.AttachTracing.
//
// kernelStacks gates the BPF program's kernel-stack capture (set from
// cfg.KernelStacks). When false, kernel-stack capture is fully bypassed
// at sample time; the CollectKernel bit on each pid_config entry is a
// no-op. When true, kernel stacks are captured for matched samples.
func LoadOffCPUDwarf(systemWide, kernelStacks bool) (*OffCPUDwarf, error) {
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true,
		cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE); err != nil {
		return nil, fmt.Errorf("set capabilities: %w", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	spec, err := loadOffcpu_dwarf()
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf spec: %w", err)
	}
	if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
		return nil, fmt.Errorf("set system_wide: %w", err)
	}
	if err := spec.Variables["kernel_stacks_enabled"].Set(kernelStacks); err != nil {
		return nil, fmt.Errorf("set kernel_stacks_enabled: %w", err)
	}
	p := &OffCPUDwarf{}
	if err := spec.LoadAndAssign(&p.objs, nil); err != nil {
		return nil, fmt.Errorf("load and assign: %w", err)
	}
	return p, nil
}

// Program returns the tp_btf/sched_switch program. Attach via
// link.AttachTracing (not link.AttachRawLink — this isn't a perf_event).
func (p *OffCPUDwarf) Program() *ebpf.Program {
	return p.objs.OffcpuDwarfSchedSwitch
}

// RingbufMap returns the stack_events ringbuf for ringbuf.NewReader.
func (p *OffCPUDwarf) RingbufMap() *ebpf.Map {
	return p.objs.StackEvents
}

// KernStackmap returns the BPF_MAP_TYPE_STACK_TRACE used for kernel
// stack-ID lookup. Populated only when kernel_stacks_enabled is true at
// BPF load; otherwise samples carry KernStack == -1 and userspace skips
// the lookup. Mirror of FP off-CPU profiler's Stackmap accessor.
func (p *OffCPUDwarf) KernStackmap() *ebpf.Map {
	return p.objs.KernStackmap
}

// AddPID registers a target PID for sampling. Semantics match
// profile.PerfDwarf.AddPID — inserts into the `pids` filter.
func (p *OffCPUDwarf) AddPID(pid uint32) error {
	cfg := offcpu_dwarfPidConfig{
		Type:          0,
		CollectUser:   1,
		CollectKernel: 1, // gated by BPF kernel_stacks_enabled global
	}
	return p.objs.Pids.Update(pid, &cfg, ebpf.UpdateAny)
}

// Close releases all BPF objects.
func (p *OffCPUDwarf) Close() error {
	return p.objs.Close()
}

// CFIRulesMap returns the cfi_rules HASH_OF_MAPS outer map.
func (p *OffCPUDwarf) CFIRulesMap() *ebpf.Map {
	return p.objs.CfiRules
}

// CFILengthsMap returns the cfi_lengths HASH keyed by table_id → u32 length.
func (p *OffCPUDwarf) CFILengthsMap() *ebpf.Map {
	return p.objs.CfiLengths
}

// CFIClassificationMap returns the cfi_classification HASH_OF_MAPS outer map.
func (p *OffCPUDwarf) CFIClassificationMap() *ebpf.Map {
	return p.objs.CfiClassification
}

// CFIClassificationLengthsMap returns the cfi_classification_lengths HASH.
func (p *OffCPUDwarf) CFIClassificationLengthsMap() *ebpf.Map {
	return p.objs.CfiClassificationLengths
}

// PIDMappingsMap returns the pid_mappings HASH_OF_MAPS outer map.
func (p *OffCPUDwarf) PIDMappingsMap() *ebpf.Map {
	return p.objs.PidMappings
}

// PIDMappingLengthsMap returns the pid_mapping_lengths HASH keyed by pid → u32 length.
func (p *OffCPUDwarf) PIDMappingLengthsMap() *ebpf.Map {
	return p.objs.PidMappingLengths
}
