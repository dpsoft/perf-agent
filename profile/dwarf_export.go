package profile

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// PerfDwarf is a thin wrapper around the generated perf_dwarf BPF objects.
// Construct with LoadPerfDwarf; always Close() when done.
type PerfDwarf struct {
	objs perf_dwarfObjects
}

// LoadPerfDwarf loads the BPF program and returns a handle. Caller must
// Close(). The program isn't attached to any perf event yet — the caller
// opens perf_event_open fds and attaches separately (see
// unwind/dwarfagent for the full wiring).
func LoadPerfDwarf(systemWide bool) (*PerfDwarf, error) {
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

	spec, err := loadPerf_dwarf()
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf spec: %w", err)
	}
	if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
		return nil, fmt.Errorf("set system_wide: %w", err)
	}
	p := &PerfDwarf{}
	if err := spec.LoadAndAssign(&p.objs, nil); err != nil {
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
		CollectKernel: 0,
	}
	return p.objs.Pids.Update(pid, &cfg, ebpf.UpdateAny)
}

// Close releases all BPF objects.
func (p *PerfDwarf) Close() error {
	return p.objs.Close()
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
