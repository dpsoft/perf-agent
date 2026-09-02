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
	"log"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/internal/kernelver"
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
	// Shared too, but OPTIONALLY: a module that never counts a dispatch works
	// perfectly well against its own copy, and failing a module's load because
	// a driver does not expose a diagnostic map would trade frames for
	// telemetry. Bound when the driver has it so a module that DOES count
	// lands in the same place the core does, rather than in a private map
	// nothing reads.
	mapInterpStats = "interp_stats"
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

// MaxSpans is how many disjoint text spans one claim may cover. A module that
// finds more must drop its smallest and say so: covering less than you claim,
// silently, is the failure this whole seam is built to refuse.
//
// The number is a MEASURED verifier ceiling, not a design preference -- the
// scan is unrolled into the walk callback and four spans cost 2.9x what three
// do. See HANDOFF_MAX_RANGES in bpf/unwind_common.h for the table.
const MaxSpans = 3

// Span is one contiguous claim on a binary's text, RELATIVE to the mapping's
// load bias -- the same space mapping_for_pc reports rel_pc in -- so one entry
// serves every process running that binary. Hi is exclusive.
type Span struct {
	Lo, Hi uint64
}

// ErrRetryable marks a refusal that is a statement about WHEN a module was
// asked, not about the process. A module returns it from Enroll when looking
// again later could succeed.
//
// It exists for one measured case and is worth the mechanism because of how
// invisible that case is. The GPU probe LAUNCHES its workload and enrols it
// during CUDA init, which for CPython is the one moment when the application's
// own Python threads do not exist yet -- and the main thread, which would have
// answered, cannot be stopped there because it is our own child's leader and
// stopping it corrupts os/exec's bookkeeping. The first look is therefore
// guaranteed to fail on exactly the path that most needs it to succeed, and
// the failure is indistinguishable from "this process runs no Python".
var ErrRetryable = errors.New("interp: not yet; ask again")

// retrySchedule is when a retryable enrolment is tried again, measured from
// the first attempt.
//
// Front-loaded and then sparse: a Python worker usually exists within a second
// of startup, and a target that has not produced one after half a minute is
// one that never will. Bounded on purpose -- an unbounded retry against a
// process that simply has no Python in it is a ptrace stop every few seconds
// for the life of the profiler.
var retrySchedule = []time.Duration{
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// Range is one unwinder's claim on a binary: while the native walk is inside
// ANY of these spans of the binary keyed by TableID, that unwinder takes over.
//
// IT IS A SET OF SPANS AND NOT ONE SPAN because a compiler may split the
// function that matters across several partitions, and CPython's eval loop is
// exactly the function it does that to -- uv's cpython-3.12.14 has three, and
// the largest of them is the one marked cold. A single span forced the
// installer to choose, and choosing wrong is indistinguishable from the
// runtime not being there at all. See HANDOFF_MAX_RANGES.
//
// TableID is the FNV-1a-of-build-id key the CFI tables already use, which is
// the value walk_step is holding by the time it asks.
type Range struct {
	TableID uint64
	Spans   []Span
}

// handoffRange mirrors struct handoff_range in bpf/unwind_common.h. Unused
// spans stay zeroed, and zero matches no address, which is what lets the BPF
// side scan without a count.
type handoffRange struct {
	Spans      [MaxSpans]Span
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
	// InterpStatsMap is the core's per-CPU record of why a handoff did or did
	// not happen. See DispatchStats.
	InterpStatsMap() *ebpf.Map
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
	// stop closes when the Set does, so a retry in flight does not outlive
	// the maps it would write into.
	stop     chan struct{}
	stopOnce sync.Once
	retries  sync.WaitGroup
	// label names this Set in the fallback counter line, so a reader can tell
	// which driver's handoff the numbers belong to when two profilers run.
	label string
	// logged records that the counters have been rendered, so Close does not
	// repeat a line a caller already placed better. See Close.
	logMu  sync.Mutex
	logged bool
	// logSink is where Close's fallback line goes. nil means log.Printf; a
	// test replaces it to observe the guarantee rather than the log package.
	logSink func(format string, args ...any)
	// failed records modules whose BPF object would not load, by name and
	// reason. Kept rather than only logged at attach: a module that failed to
	// load produces exactly the same evidence as a process with none of that
	// language in it -- no frames, every counter zero -- and one line at
	// startup is easy to miss in a run that prints a summary at the end.
	failed []string
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

	s := &Set{driver: d, stop: make(chan struct{}), label: string(d.Flavour())}
	var errs []error
	for _, f := range fs {
		m := f()
		coll, err := loadModule(m, d)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name(), err))
			s.failed = append(s.failed, fmt.Sprintf("%s (%v)", m.Name(), err))
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
	// A module supplies one program per driver program type, and the
	// uprobe-typed one is BPF_PROG_TYPE_KPROBE -- which cilium/ebpf will not
	// load without a kernel version, and cannot discover one for in a setcap'd
	// process. Without this the GPU driver's module fails to load with an
	// error naming neither capabilities nor uprobes, while the two perf_event
	// drivers are unaffected. See internal/kernelver.
	kernelver.Apply(spec)

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
	if st := d.InterpStatsMap(); st != nil {
		if _, declared := spec.Maps[mapInterpStats]; declared {
			repl[mapInterpStats] = st
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
			if errors.Is(err, ErrRetryable) {
				found = true
				s.scheduleRetry(e, pid, logf)
				continue
			}
			logf("%s frames: pid %d: REFUSED: %v", e.mod.Name(), pid, err)
			found = true
			continue
		}
		if !ok {
			continue
		}
		found = true
		if len(r.Spans) == 0 {
			// Recognised but not claimed: the module reported why itself.
			continue
		}
		if err := s.installRange(e.mod.ID(), r); err != nil {
			logf("%s frames: pid %d: REFUSED: %v", e.mod.Name(), pid, err)
			continue
		}
		// Said out loud, because every other way of learning it needs
		// capabilities: this is the claim the walker will look up, under the
		// key it will compute, in the address space it reports rel_pc in.
		s.logInstalled(e, pid, r, logf)
	}
	return found
}

// scheduleRetry asks one module again, later, on retrySchedule.
//
// One goroutine per (module, pid), which is bounded by how many processes a
// profiler enrols and ends at the first success, at the end of the schedule, or
// when the Set closes -- whichever comes first. It writes only through the same
// installRange the first attempt would have used.
func (s *Set) scheduleRetry(e *entry, pid uint32, logf func(string, ...any)) {
	logf("%s frames: pid %d: no thread holds interpreter state yet (it is still starting up); "+
		"retrying for %s", e.mod.Name(), pid, retrySchedule[len(retrySchedule)-1])
	s.retries.Add(1)
	go func() {
		defer s.retries.Done()
		start := time.Now()
		for _, at := range retrySchedule {
			select {
			case <-s.stop:
				return
			case <-time.After(time.Until(start.Add(at))):
			}
			r, ok, err := e.mod.Enroll(pid)
			if err != nil {
				if errors.Is(err, ErrRetryable) {
					continue
				}
				logf("%s frames: pid %d: REFUSED: %v", e.mod.Name(), pid, err)
				return
			}
			if !ok || len(r.Spans) == 0 {
				return
			}
			if err := s.installRange(e.mod.ID(), r); err != nil {
				logf("%s frames: pid %d: REFUSED: %v", e.mod.Name(), pid, err)
				return
			}
			s.logInstalled(e, pid, r, logf)
			return
		}
		logf("%s frames: pid %d: gave up after %s: no thread ever held interpreter state. "+
			"Stacks stay native-only", e.mod.Name(), pid, retrySchedule[len(retrySchedule)-1])
	}()
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
	// Guarded rather than assumed: this runs from a retry goroutine as well as
	// from Enroll, and a Set built without a driver would otherwise panic
	// inside that goroutine -- taking the whole profiler down to report a
	// claim it could not install.
	if s.driver == nil {
		return errors.New("no driver to install a claim into")
	}
	m := s.driver.HandoffRangesMap()
	if m == nil {
		return errors.New("driver exposes no handoff_ranges map")
	}
	if len(r.Spans) == 0 {
		return fmt.Errorf("claim for table %#x covers no text", r.TableID)
	}
	if len(r.Spans) > MaxSpans {
		return fmt.Errorf("claim for table %#x has %d spans, the walker scans %d",
			r.TableID, len(r.Spans), MaxSpans)
	}
	v := handoffRange{UnwinderID: id}
	for i, sp := range r.Spans {
		if sp.Hi <= sp.Lo {
			return fmt.Errorf("span %d [%#x,%#x) for table %#x is empty or inverted",
				i, sp.Lo, sp.Hi, r.TableID)
		}
		v.Spans[i] = sp
	}
	if err := m.Update(&r.TableID, &v, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("install handoff range for table %#x: %w", r.TableID, err)
	}
	// READ IT BACK. A claim that was not installed, or was installed under a
	// key the walker does not compute, is indistinguishable from a walk that
	// never reached the runtime -- both are silence plus a row of zeroed
	// counters, and telling them apart from outside the kernel cost an hour
	// once. One extra syscall per process at attach turns that into a fact in
	// the log.
	var back handoffRange
	if err := m.Lookup(&r.TableID, &back); err != nil {
		return fmt.Errorf("handoff range for table %#x did not read back: %w", r.TableID, err)
	}
	if back != v {
		return fmt.Errorf("handoff range for table %#x read back as %+v, wrote %+v", r.TableID, back, v)
	}
	return nil
}

// logInstalled says what the walker will look up, under which key, covering
// what. Every other way of learning it needs capabilities.
func (s *Set) logInstalled(e *entry, pid uint32, r Range, logf func(string, ...any)) {
	spans := make([]string, 0, len(r.Spans))
	for _, sp := range r.Spans {
		spans = append(spans, fmt.Sprintf("[%#x,%#x)", sp.Lo, sp.Hi))
	}
	logf("%s frames: pid %d: handoff installed, table %#x unwinder %d spans %s",
		e.mod.Name(), pid, r.TableID, e.mod.ID(), strings.Join(spans, " "))
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
	s.logMu.Lock()
	s.logged = true
	s.logMu.Unlock()
	// A module that never loaded, said as plainly as the counters are. Its
	// absence is indistinguishable from "this workload runs no such language"
	// -- no frames, every counter zero -- so it has to be stated rather than
	// inferred, and stated HERE rather than only at startup, where a run that
	// ends in a summary will have scrolled past it.
	for _, f := range s.failed {
		logf("interpreter frames: %s NEVER LOADED: no frames from that runtime were possible "+
			"in this run, and its counters below are zero for that reason and not because "+
			"the workload had none", f)
	}

	// The CORE's account first, and unconditionally, because ZERO IS THE
	// SIGNAL here. A module reporting all-zero counters has two completely
	// different causes -- it ran and refused, or it was never reached -- and
	// only these numbers separate them.
	// ALWAYS A LINE. If the map is unreachable that is itself the answer, and
	// printing nothing is the same silence this whole guarantee exists to
	// remove -- a run with no interpreter frames and no statement about why.
	if m := s.statsMap(); m == nil {
		logf("interpreter handoff: counters unavailable (no driver exposes interp_stats); " +
			"whether the handoff fired cannot be determined for this run")
	} else if st, err := readDispatchStats(m); err != nil {
		logf("interpreter handoff: counters unreadable: %v", err)
	} else {
		logf("interpreter handoff: range_hit=%d in_range=%d claimed=%d dispatched=%d "+
			"tail_call_failed=%d budget_exhausted=%d resumed=%d -- %s",
			st.RangeHit, st.InRange, st.Claimed, st.Dispatched,
			st.TailCallFailed, st.Budget, st.Resumed, st.Diagnose())
	}

	for _, e := range s.entries {
		if line := e.mod.Counters(enrolled); line != "" {
			logf("%s", line)
		}
	}
}

// DispatchStats is the core's own account of why a handoff did or did not
// happen, summed across CPUs. Mirrors INTERP_STAT_* in bpf/unwind_record.h.
type DispatchStats struct {
	RangeHit       uint64
	InRange        uint64
	Claimed        uint64
	Dispatched     uint64
	TailCallFailed uint64
	Budget         uint64
	Resumed        uint64
}

// Diagnose turns the counters into the one sentence a reader actually wants:
// which step of the handoff broke, or that none did.
//
// It exists because the raw numbers are only legible to someone holding the
// dispatch path in their head, and the person reading this log line at 2am is
// not that person.
func (d DispatchStats) Diagnose() string {
	switch {
	case d.RangeHit == 0:
		return "NO CLAIM WAS EVER MATCHED: either nothing installed a handoff range, " +
			"or the range was keyed by a table_id the walker does not compute for those frames. " +
			"Check the 'handoff installed' line against the binary the samples land in"
	case d.InRange == 0:
		return "A CLAIM WAS FOUND BUT NO PC EVER FELL INSIDE IT: the installer and the walker " +
			"disagree about the address space (a range in file offsets against a load-bias-relative pc, say)"
	case d.Claimed == 0:
		return "every matching frame belonged to an unwinder that had already finished with that sample"
	case d.Dispatched == 0:
		return "frames were claimed and none was dispatched; the round-trip budget is the only thing that does that"
	case d.TailCallFailed > 0:
		return "SOME TAIL CALLS DID NOT HAPPEN: the program array slot is empty, or the kernel's " +
			"tail-call limit was reached. Those samples stop at the claimed frame"
	case d.Resumed < d.Dispatched:
		return "an unwinder was entered and did not hand control back"
	default:
		return "handoff healthy"
	}
}

// readDispatchStats sums the per-CPU counters.
func readDispatchStats(m *ebpf.Map) (DispatchStats, error) {
	var out DispatchStats
	slots := []*uint64{
		&out.RangeHit, &out.InRange, &out.Claimed, &out.Dispatched,
		&out.TailCallFailed, &out.Budget, &out.Resumed,
	}
	for i, dst := range slots {
		var percpu []uint64
		if err := m.Lookup(uint32(i), &percpu); err != nil {
			return out, fmt.Errorf("read interp_stats[%d]: %w", i, err)
		}
		for _, v := range percpu {
			*dst += v
		}
	}
	return out, nil
}

// statsMap is the driver's interp_stats, or nil when there is no driver --
// which only a test builds, but a nil dereference in the shutdown path would
// turn a missing counter into a crash.
func (s *Set) statsMap() *ebpf.Map {
	if s.driver == nil {
		return nil
	}
	return s.driver.InterpStatsMap()
}

// Close releases every attached module.
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	// COUNTERS BEFORE ANYTHING ELSE, AND UNCONDITIONALLY IF NOBODY ELSE HAS.
	//
	// This is a structural guarantee, not a convenience. Four times on this
	// branch the difference between a minute and a day was whether these
	// numbers were rendered, and three of those were a path that HAD them and
	// never printed them -- most recently the GPU driver, which held a Set,
	// filled its counters, and closed it in silence while a run produced no
	// interpreter frames and no way to tell why.
	//
	// A caller that renders them itself gets a better line (it knows its own
	// prefix and whether the target was enrolled) and marks them logged. Any
	// caller that forgets gets them anyway. There is no longer a way to have
	// a Set and not see its counters.
	s.logMu.Lock()
	unrendered := !s.logged
	s.logMu.Unlock()
	if unrendered {
		sink := s.logSink
		if sink == nil {
			sink = func(format string, args ...any) {
				log.Printf("interp[%s]: "+format, append([]any{s.label}, args...)...)
			}
		}
		s.LogCounters(false, sink)
	}
	// Stop retries and WAIT for them: they write into the driver's maps, which
	// the caller closes immediately after this returns.
	s.stopOnce.Do(func() { close(s.stop) })
	s.retries.Wait()
	var errs []error
	for _, e := range s.entries {
		errs = append(errs, e.mod.Close())
		// Guarded: Close is a teardown path and must not panic. An entry with
		// no collection cannot occur in production -- Attach drops a module
		// whose object would not load -- but a crash while shutting down would
		// lose the counters that say why a run produced nothing.
		if e.coll != nil {
			e.coll.Close()
		}
	}
	s.entries = nil
	return errors.Join(errs...)
}
