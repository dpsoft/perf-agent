// Package interp is the seam between the native stack walker and the
// unwinders for languages whose frames are not on the machine stack.
//
// THE WHOLE POINT IS WHAT IS NOT HERE. This package names no language. It
// knows that an unwinder has an id, owns a BPF object of its own, claims a
// range of some binary's text, and can render the two-word frames it pushes.
// Everything else -- how CPython finds a PyThreadState, which byte of an
// _PyInterpreterFrame is the owner, what a Ruby control frame looks like --
// belongs to a module under its own package and appears in profile/,
// gpuprobe/, unwind/dwarfagent/ and here nowhere at all.
//
// WHERE A NEW LANGUAGE GOES, which is the test this design has to pass:
//
//  1. bpf/interp/<lang>/<lang>_walk.bpf.c, its own BPF object, sharing only
//     bpf/unwind_record.h with the core. One program per driver program type.
//  2. a Go package that implements Module below.
//  3. ONE line in unwind/interp/modules, which is the only place in the tree
//     that names any module at all.
//
// Nothing in this package, in the drivers, or in the BPF core changes.
package interp

import (
	"errors"
	"fmt"
	"sync"

	"github.com/cilium/ebpf"
)

// Flavour is a driver's BPF program type. It matters because every entry of a
// BPF prog array must share the program type of whatever tail-calls into it,
// so a module has to supply one program per flavour it can be reached from --
// and for BPF_PROG_TYPE_TRACING the kernel is stricter still, refusing a prog
// array whose entries attach to different BTF functions, which is why the
// sched_switch flavour names that tracepoint rather than "tp_btf".
type Flavour string

const (
	FlavourPerfEvent   Flavour = "perf_event"
	FlavourSchedSwitch Flavour = "tp_btf/sched_switch"
	FlavourUprobeMulti Flavour = "uprobe.multi"
)

// Shared map names, from bpf/unwind_record.h. A module's object declares all
// three and has them REPLACED at load time by the driver's, which is what lets
// the module append to the record the native walk is building without ever
// being compiled into the driver's object.
const (
	mapWalkerScratch = "walker_scratch"
	mapWalkStates    = "walk_states"
	mapInterpProgs   = "interp_progs"
)

// Prog-array slots, from bpf/unwind_record.h. Slots 0 and 1 are the driver's
// own resume programs; an unwinder with id N lives at slot N+1.
const (
	slotResumeStep = 0
	slotResumeWalk = 1
)

// SlotForID is the interp_progs index an unwinder's program is installed at.
// Mirrors INTERP_SLOT_UNWINDER in bpf/unwind_record.h.
func SlotForID(id uint32) uint32 { return id + 1 }

// Range is one claim on a binary's text: while the native walk is inside
// [Lo, Hi) of the binary keyed by TableID, the unwinder that installed it
// takes over.
//
// Lo/Hi are RELATIVE to the mapping's load bias -- the same space
// mapping_for_pc reports rel_pc in -- so one entry serves every process
// running that binary. TableID is the FNV-1a-of-build-id key the CFI tables
// already use, which is the value walk_step is holding by the time it asks.
type Range struct {
	TableID uint64
	Lo, Hi  uint64
}

// handoffRange mirrors struct handoff_range in bpf/unwind_common.h.
type handoffRange struct {
	Lo, Hi     uint64
	UnwinderID uint32
	_          uint32
}

// Driver is what a profiler exposes so interpreter unwinders can be attached
// to its walk. Implemented by profile.PerfDwarf, profile.OffCPUDwarf and
// gpuprobe's loaded objects.
//
// Every method returns something declared in bpf/unwind_record.h or
// bpf/unwind_common.h -- core things. None of them mentions a language, and
// none of them has to change when one is added.
type Driver interface {
	// Flavour is this driver's BPF program type.
	Flavour() Flavour
	// The three maps a module shares with the walk.
	WalkerScratchMap() *ebpf.Map
	WalkStatesMap() *ebpf.Map
	InterpProgsMap() *ebpf.Map
	// HandoffRangesMap is the table walk_step consults per frame to decide
	// whether something other than the native walker claims a PC.
	HandoffRangesMap() *ebpf.Map
	// The driver's own two resume programs, which go in slots 0 and 1.
	ResumeStepProgram() *ebpf.Program
	ResumeWalkProgram() *ebpf.Program
	// InterpEnabled reports whether the loaded program carries the handoff at
	// all. It is a load-time `const volatile` gate: on a kernel that refuses
	// the program with the handoff compiled in, userspace reloads without it
	// (see profile.loadWithInterpGate), and then installing a module would
	// write maps nothing reads.
	InterpEnabled() bool
}

// Module is one language's unwinder, bound to one driver.
//
// A module instance owns BPF maps, so there is one per driver session, built
// by the factory registered in unwind/interp/modules.
type Module interface {
	// ID is the unwinder id: the value written into handoff_ranges, the
	// interp_progs slot (via SlotForID), and the tags[] byte on every frame
	// pair this module pushes. Must match the module's own INTERP_ID_* in its
	// BPF source.
	ID() uint32
	// Name is what log lines call this language.
	Name() string
	// Spec is the module's own compiled BPF object. It must declare the three
	// shared maps and one program per Flavour it supports.
	Spec() (*ebpf.CollectionSpec, error)
	// ProgramName is the program in Spec() to install for this flavour, or ""
	// if the module cannot be dispatched from a driver of that type.
	ProgramName(Flavour) string
	// Bind hands the module the collection Attach loaded, so it can keep
	// handles on its own maps. Called once, before any Enroll.
	Bind(*ebpf.Collection) error
	// Enroll offers one process. ok is false, with no error, for a process
	// this module does not recognise -- which is nearly every process on a
	// machine and is not an error. The Range it returns is installed in
	// handoff_ranges under this module's id.
	//
	// A module that recognises a process but refuses it (wrong version, an ABI
	// it declines to walk) returns ok true and a zero Range: the refusal is
	// the module's to report, and the caller must not install a claim.
	Enroll(pid uint32) (r Range, ok bool, err error)
	// Detach drops a process's per-PID state. PID reuse is why it exists: a
	// recycled pid whose new occupant is a different build would otherwise be
	// walked with the previous process's offsets.
	Detach(pid uint32) error
	// Counters renders the module's BPF-side counters as one log line, or ""
	// if there is nothing worth saying.
	Counters(enrolled bool) string
	// Close releases the module's BPF objects.
	Close() error
}

// factories is the registry. Populated only from unwind/interp/modules.
var (
	mu        sync.Mutex
	factories []func() Module
)

// Register adds a module factory. Call it from unwind/interp/modules and
// nowhere else: that package existing is what keeps every other package in the
// tree free of any dependency on any language.
func Register(f func() Module) {
	mu.Lock()
	defer mu.Unlock()
	factories = append(factories, f)
}

// Registered reports how many modules are registered. Used by callers that
// want to skip the whole attach path when the binary was built without any.
func Registered() int {
	mu.Lock()
	defer mu.Unlock()
	return len(factories)
}

// Set is the modules attached to one driver.
type Set struct {
	driver  Driver
	entries []*entry
}

type entry struct {
	mod  Module
	coll *ebpf.Collection
}

// Attach loads every registered module against one driver and wires up its
// tail-call table.
//
// It is best-effort per module by design: a module whose object will not load
// on this kernel costs its own language's frames and nothing else. The native
// walk is unaffected either way, which is the invariant the whole handoff is
// built around.
//
// A nil Set is usable: every method on it is a no-op.
func Attach(d Driver) (*Set, error) {
	if d == nil || !d.InterpEnabled() {
		return nil, nil
	}
	progs := d.InterpProgsMap()
	if progs == nil {
		return nil, nil
	}

	mu.Lock()
	fs := append([]func() Module(nil), factories...)
	mu.Unlock()
	if len(fs) == 0 {
		return nil, nil
	}

	// The driver's own resume programs first. Without them a module could be
	// dispatched and would have nothing to hand control back to, which would
	// cost every claimed sample its native tail -- strictly worse than not
	// dispatching at all. So this failing is fatal to the whole set.
	if p := d.ResumeStepProgram(); p != nil {
		if err := progs.Update(uint32(slotResumeStep), p, ebpf.UpdateAny); err != nil {
			return nil, fmt.Errorf("interp: install resume-step program: %w", err)
		}
	}
	if p := d.ResumeWalkProgram(); p != nil {
		if err := progs.Update(uint32(slotResumeWalk), p, ebpf.UpdateAny); err != nil {
			return nil, fmt.Errorf("interp: install resume-walk program: %w", err)
		}
	}

	s := &Set{driver: d}
	var errs []error
	for _, f := range fs {
		m := f()
		coll, err := loadModule(m, d)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name(), err))
			continue
		}
		s.entries = append(s.entries, &entry{mod: m, coll: coll})
	}
	if len(s.entries) == 0 {
		return s, errors.Join(errs...)
	}
	return s, errors.Join(errs...)
}

func loadModule(m Module, d Driver) (*ebpf.Collection, error) {
	progName := m.ProgramName(d.Flavour())
	if progName == "" {
		return nil, fmt.Errorf("no program for driver flavour %q", d.Flavour())
	}
	spec, err := m.Spec()
	if err != nil {
		return nil, err
	}

	// The module's object declares walker_scratch, walk_states and
	// interp_progs so it can be compiled on its own; here they are replaced by
	// the driver's, which is what makes "append to the record the native walk
	// is building" work across two separately compiled objects.
	repl := map[string]*ebpf.Map{
		mapWalkerScratch: d.WalkerScratchMap(),
		mapWalkStates:    d.WalkStatesMap(),
		mapInterpProgs:   d.InterpProgsMap(),
	}
	for name, mp := range repl {
		if mp == nil {
			return nil, fmt.Errorf("driver exposes no %s map", name)
		}
	}

	// Only the program this flavour can reach is loaded. The others in the
	// object are for other drivers and would cost a verifier pass each.
	sub := &ebpf.CollectionSpec{
		Maps:      spec.Maps,
		Programs:  map[string]*ebpf.ProgramSpec{},
		Types:     spec.Types,
		ByteOrder: spec.ByteOrder,
	}
	ps, ok := spec.Programs[progName]
	if !ok {
		return nil, fmt.Errorf("object has no program %q", progName)
	}
	sub.Programs[progName] = ps

	coll, err := ebpf.NewCollectionWithOptions(sub, ebpf.CollectionOptions{
		MapReplacements: repl,
	})
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	if err := m.Bind(coll); err != nil {
		coll.Close()
		return nil, fmt.Errorf("bind: %w", err)
	}
	if err := d.InterpProgsMap().Update(SlotForID(m.ID()), coll.Programs[progName], ebpf.UpdateAny); err != nil {
		coll.Close()
		return nil, fmt.Errorf("install program in slot %d: %w", SlotForID(m.ID()), err)
	}
	return coll, nil
}

// Enroll offers one process to every attached module and installs the handoff
// range of whichever claims it.
//
// Returns whether ANY module recognised the process -- not whether one
// attached. That is what decides whether a shutdown counter line is worth
// printing: on a target with no interpreter in it the counters answer a
// question nobody asked, while on one that has an interpreter they are the
// difference between "refused" and "attached and walked nothing".
//
// Best-effort by construction: no failure here stops profiling. The native
// walk is unaffected, and a process no module claims simply produces
// native-only stacks.
func (s *Set) Enroll(pid uint32, logf func(format string, args ...any)) bool {
	if s == nil {
		return false
	}
	found := false
	for _, e := range s.entries {
		r, ok, err := e.mod.Enroll(pid)
		if err != nil {
			logf("%s frames: pid %d: REFUSED: %v", e.mod.Name(), pid, err)
			found = true
			continue
		}
		if !ok {
			continue
		}
		found = true
		if r.Hi <= r.Lo {
			// Recognised but not claimed: the module reported why itself.
			continue
		}
		if err := s.installRange(e.mod.ID(), r); err != nil {
			logf("%s frames: pid %d: REFUSED: %v", e.mod.Name(), pid, err)
		}
	}
	return found
}

// installRange publishes one claim under its binary's table_id.
//
// It is keyed by BINARY, not by pid, so it stays correct for every other
// process running the same image and is deliberately never removed on detach:
// re-deriving which claims are still in use would duplicate the refcounting
// the CFI table store already does for the same binaries. A stale claim costs
// one dispatch per sample in a process with no per-PID record, which the
// module answers by marking itself done for that sample.
func (s *Set) installRange(id uint32, r Range) error {
	m := s.driver.HandoffRangesMap()
	if m == nil {
		return errors.New("driver exposes no handoff_ranges map")
	}
	if r.Hi <= r.Lo {
		return fmt.Errorf("range [%#x,%#x) for table %#x is empty or inverted", r.Lo, r.Hi, r.TableID)
	}
	v := handoffRange{Lo: r.Lo, Hi: r.Hi, UnwinderID: id}
	if err := m.Update(&r.TableID, &v, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("install handoff range for table %#x: %w", r.TableID, err)
	}
	return nil
}

// Detach drops a process from every attached module.
func (s *Set) Detach(pid uint32, logf func(format string, args ...any)) {
	if s == nil {
		return
	}
	for _, e := range s.entries {
		if err := e.mod.Detach(pid); err != nil {
			logf("%s frames: pid %d: %v", e.mod.Name(), pid, err)
		}
	}
}

// LogCounters prints each attached module's counter line once, at shutdown.
//
// Without it an interpreter walk is unobservable from the outside: a run that
// produced no frames for a language looks exactly like a run with none of that
// language in it, and the module's counters are the only place the difference
// is recorded.
func (s *Set) LogCounters(enrolled bool, logf func(format string, args ...any)) {
	if s == nil {
		return
	}
	for _, e := range s.entries {
		if line := e.mod.Counters(enrolled); line != "" {
			logf("%s", line)
		}
	}
}

// Close releases every attached module.
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, e := range s.entries {
		errs = append(errs, e.mod.Close())
		e.coll.Close()
	}
	s.entries = nil
	return errors.Join(errs...)
}
