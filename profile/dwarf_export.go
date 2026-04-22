package profile

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
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
	_ = rlimit.RemoveMemlock() // best-effort; modern kernels don't require it

	spec, err := loadPerf_dwarf()
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf spec: %w", err)
	}
	// system_wide must be set on the spec before LoadAndAssign so the
	// verifier sees the final const-volatile value. We pick targeted mode
	// (false) by default; the caller can still override via SetSystemWide
	// before calling this helper… actually, we force-set here because
	// the object isn't loaded yet at call time.
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
