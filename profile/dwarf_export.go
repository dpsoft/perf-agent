package profile

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// PerfDwarfForTest is a thin wrapper around the generated perf_dwarf
// objects, exported so cmd/perf-dwarf-test can exercise the BPF program
// standalone during S2 validation. S5 folds the same functionality into
// profile.Profiler under the --unwind dwarf path; once that lands, this
// wrapper can go away.
type PerfDwarfForTest struct {
	objs perf_dwarfObjects
}

// LoadPerfDwarfForTest loads the BPF program and returns a handle. Caller
// must Close(). The program isn't attached to any perf event yet — the
// caller opens perf_event_open fds and attaches separately.
func LoadPerfDwarfForTest() (*PerfDwarfForTest, error) {
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
	if err := spec.Variables["system_wide"].Set(false); err != nil {
		return nil, fmt.Errorf("set system_wide: %w", err)
	}
	t := &PerfDwarfForTest{}
	if err := spec.LoadAndAssign(&t.objs, nil); err != nil {
		return nil, fmt.Errorf("load and assign: %w", err)
	}
	return t, nil
}

// Program returns the PerfDwarf program for attaching to a perf_event_open fd.
func (p *PerfDwarfForTest) Program() *ebpf.Program {
	return p.objs.PerfDwarf
}

// RingbufMap returns the stack_events ringbuf for ringbuf.NewReader.
func (p *PerfDwarfForTest) RingbufMap() *ebpf.Map {
	return p.objs.StackEvents
}

// SetSystemWide is a no-op; the setting is baked in at load time. Kept as
// a stable API for the test CLI so the future profile.Profiler wiring can
// honor --unwind dwarf + -a without changing callers.
func (p *PerfDwarfForTest) SetSystemWide(v bool) error {
	if v {
		return fmt.Errorf("system_wide must be configured before LoadPerfDwarfForTest; currently defaults to false")
	}
	return nil
}

// AddPID registers a target PID for sampling. Matches the semantics of
// profile.Profiler's PID filter in targeted mode.
func (p *PerfDwarfForTest) AddPID(pid uint32) error {
	cfg := perf_dwarfPidConfig{
		Type:          0,
		CollectUser:   1,
		CollectKernel: 0,
	}
	return p.objs.Pids.Update(pid, &cfg, ebpf.UpdateAny)
}

// Close releases all BPF objects.
func (p *PerfDwarfForTest) Close() error {
	return p.objs.Close()
}

// CFIRulesMap returns the cfi_rules HASH_OF_MAPS outer map.
func (p *PerfDwarfForTest) CFIRulesMap() *ebpf.Map {
	return p.objs.CfiRules
}

// CFILengthsMap returns the cfi_lengths HASH keyed by table_id → u32 length.
func (p *PerfDwarfForTest) CFILengthsMap() *ebpf.Map {
	return p.objs.CfiLengths
}

// CFIClassificationMap returns the cfi_classification HASH_OF_MAPS outer map.
func (p *PerfDwarfForTest) CFIClassificationMap() *ebpf.Map {
	return p.objs.CfiClassification
}

// CFIClassificationLengthsMap returns the cfi_classification_lengths HASH.
func (p *PerfDwarfForTest) CFIClassificationLengthsMap() *ebpf.Map {
	return p.objs.CfiClassificationLengths
}

// PIDMappingsMap returns the pid_mappings HASH_OF_MAPS outer map.
func (p *PerfDwarfForTest) PIDMappingsMap() *ebpf.Map {
	return p.objs.PidMappings
}

// PIDMappingLengthsMap returns the pid_mapping_lengths HASH keyed by pid → u32 length.
func (p *PerfDwarfForTest) PIDMappingLengthsMap() *ebpf.Map {
	return p.objs.PidMappingLengths
}
