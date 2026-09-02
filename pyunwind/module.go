package pyunwind

import (
	"errors"
	"fmt"
	"log"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/unwind/interp"
)

// UnwinderID is this module's id: the value written into handoff_ranges, the
// interp_progs slot it is installed at (via interp.SlotForID), and the tags[]
// byte on every Python frame pair.
//
// It is 1 because FRAME_TAG_PYTHON was 1 before the walker became a module of
// its own (issue #83), and the wire format does not change just because the
// code did. Mirrors INTERP_ID_PYTHON in bpf/interp/python/python_walk.h.
const UnwinderID = 1

func init() {
	// The renderer is registered unconditionally, from package init, and
	// separately from the module factory: naming a frame needs no BPF session,
	// no privilege and no driver. A saved profile being converted on a laptop
	// must render "python:0x…" exactly as the agent that captured it did.
	interp.RegisterRenderer(UnwinderID, func(codeObject, _ uint64) string {
		return FrameName(codeObject)
	})
}

// FrameName renders an unsymbolized Python frame. Turning the code object into
// a file, a function and a line is a later slice; until then these carry the
// address, exactly as an unresolved native frame does. The prefix is what
// makes the frame legible as Python rather than as a stray pointer.
//
// One function, one format, for every consumer: the same frame reaching a user
// through the CPU profiler and through the GPU launch probe must not have two
// different names.
func FrameName(codeObject uint64) string {
	return fmt.Sprintf("python:%#x", codeObject)
}

// Module returns a fresh CPython unwinder bound to nothing yet.
//
// One instance per driver session: it owns BPF maps (py_procs and the counter
// array live in the object loaded for that driver), so two profilers running
// at once get two.
func Module() interp.Module { return &module{} }

type module struct {
	maps  *BPFMaps
	walkC *ebpf.Map
}

func (m *module) ID() uint32   { return UnwinderID }
func (m *module) Name() string { return "python" }

func (m *module) Spec() (*ebpf.CollectionSpec, error) { return loadPywalk() }

// ProgramName maps a driver's program type to the one program in this module's
// object that can be tail-called from it. All three do the same thing; they
// exist separately because a BPF prog array's entries must share the program
// type of whatever tail-calls into them.
func (m *module) ProgramName(f interp.Flavour) string {
	switch f {
	case interp.FlavourPerfEvent:
		return "interp_python_perf_event"
	case interp.FlavourSchedSwitch:
		return "interp_python_sched_switch"
	case interp.FlavourUprobeMulti:
		return "interp_python_uprobe"
	}
	return ""
}

func (m *module) Bind(coll *ebpf.Collection) error {
	procs := coll.Maps["py_procs"]
	if procs == nil {
		return fmt.Errorf("pyunwind: loaded object has no py_procs map")
	}
	m.maps = &BPFMaps{PyProcs: procs}
	m.walkC = coll.Maps["py_walk_counters"]
	return nil
}

// Enroll validates one process's CPython build against the live interpreter,
// installs its py_procs record, and reports the eval-loop range the core
// should hand off on.
//
// EVERY OUTCOME IS LOGGED, including the ones that are not errors. An operator
// whose Python frames are missing needs to be able to tell "this process is
// not Python", "it is Python but a version/ABI this build refuses" and "it
// attached and the walk found nothing" apart, and only the first two are
// visible from here. Result.Refused is written for exactly that reading.
//
// ok true with a zero Range means "recognised, and refused": the caller must
// install no claim, because a claim with no py_procs record behind it costs a
// dispatch per sample to be told nothing.
func (m *module) Enroll(pid uint32) (interp.Range, bool, error) {
	if m.maps == nil || m.maps.PyProcs == nil {
		return interp.Range{}, false, fmt.Errorf("pyunwind: module not bound")
	}
	libPath, found, res, err := EnrollTarget(pid, m.maps)
	switch {
	case !found && err == nil:
		// The overwhelmingly common case: not a Python process. No line.
		return interp.Range{}, false, nil
	case err != nil:
		return interp.Range{}, true, fmt.Errorf("%s: %w", libPath, err)
	case res.Refused != "":
		if errors.Is(res.Reason, ErrNoThreadHasState) {
			// Not a verdict on the process, a verdict on the moment: no
			// thread we may stop holds a PyThreadState YET. The caller
			// retries; see ErrNoThreadHasState for the measurement behind
			// this being worth retrying at all.
			return interp.Range{}, true, fmt.Errorf("%w: %s", interp.ErrRetryable, res.Refused)
		}
		log.Printf("python frames: pid %d: REFUSED %s (CPython %s): %s",
			pid, libPath, res.Version, res.Refused)
		return interp.Range{}, true, nil
	}

	tableID, err := TableIDForMapping(pid, libPath)
	if err != nil {
		return interp.Range{}, true, err
	}
	frags, err := EvalRangesForFile(libPath)
	if err != nil {
		return interp.Range{}, true, fmt.Errorf("%s: %w", libPath, err)
	}
	// The walker scans a fixed, measured number of spans per binary. When a
	// build has more fragments than that, the SMALLEST are dropped -- they are
	// sorted largest first -- and the drop is named. Covering less than the
	// claim says, silently, is the failure mode this whole path exists to
	// refuse; a line saying "3 of 4" lets an operator see it in the log rather
	// than infer it from missing frames.
	spans := make([]interp.Span, 0, interp.MaxSpans)
	for _, f := range frags {
		if len(spans) == interp.MaxSpans {
			log.Printf("python frames: pid %d: %s: eval loop is in %d fragments, claiming the "+
				"largest %d; samples in the rest will carry no Python frames",
				pid, libPath, len(frags), interp.MaxSpans)
			break
		}
		spans = append(spans, interp.Span{Lo: f.Lo, Hi: f.Hi})
	}
	log.Printf("python frames: pid %d: attached %s (CPython %s)", pid, libPath, res.Version)
	return interp.Range{TableID: tableID, Spans: spans}, true, nil
}

func (m *module) Detach(pid uint32) error { return DetachProcess(pid, m.maps) }

// Counters renders py_walk_counters as one line.
//
// The line is printed even when every counter is zero -- a zeroed line is
// itself the answer to "did the walker ever fire". Units differ by field and
// the line says so: FramesPushed counts FRAMES, every other counter counts
// SAMPLES. See pyunwind/counters.go.
//
// `enrolled` comes from the caller's Enroll pass. When it is false the line is
// printed only if something moved anyway -- which on a target with no
// interpreter would itself be news -- so an ordinary Go or Rust capture gains
// no output.
func (m *module) Counters(enrolled bool) string {
	if m.walkC == nil {
		return ""
	}
	c, err := ReadWalkCounters(m.walkC)
	if err != nil {
		return fmt.Sprintf("python walk counters unreadable: %v", err)
	}
	if !enrolled && c == (WalkCounters{}) {
		return ""
	}
	return fmt.Sprintf("python walk: frames_pushed=%d (frames); per-sample: tss_miss=%d no_proc_info=%d "+
		"tstate_read_fail=%d frame_read_fail=%d owner_implausible=%d chain_truncated=%d push_refused=%d "+
		"none_executable=%d chain_abandoned=%d",
		c.FramesPushed, c.TSSMiss, c.NoProcInfo, c.TStateReadFail, c.FrameReadFail,
		c.OwnerImplausible, c.ChainTruncated, c.PushRefused, c.NoneExecutable, c.ChainAbandoned)
}

func (m *module) Close() error {
	m.maps = nil
	m.walkC = nil
	return nil
}
