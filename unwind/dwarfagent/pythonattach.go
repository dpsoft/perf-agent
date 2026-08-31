package dwarfagent

import (
	"log"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/pyunwind"
)

// pythonMaps is the slice of the BPF handle the CPython walker needs. Only
// profile.PerfDwarf implements it today; profile.OffCPUDwarf does not
// expose the maps even though its program carries them, so an off-CPU
// session over a Python target says so once rather than skipping silently.
type pythonMaps interface {
	PyProcsMap() *ebpf.Map
	PyEvalRangesMap() *ebpf.Map
	PyWalkCountersMap() *ebpf.Map
}

// enrollPython gives one process's CPython frames to the walker: validate
// this build's offsets against the live interpreter, install py_procs, and
// switch the interpreter arm on for that libpython by installing its
// eval-loop range.
//
// EVERY OUTCOME IS LOGGED, including the ones that are not errors. An
// operator whose Python frames are missing needs to be able to tell "this
// process is not Python", "it is Python but a version/ABI this build
// refuses", and "it attached and the walk found nothing" apart, and only
// the first two are visible from here. pyunwind.Result.Refused is written
// for exactly that reading.
//
// Best-effort by construction: no failure here stops profiling. The native
// walk is unaffected, and a process with no py_procs entry simply produces
// native-only stacks.
//
// Returns whether an interpreter was found at all -- not whether it
// attached. That is what decides whether the shutdown counter line is worth
// printing: for a target with no Python in it the counters answer a
// question nobody asked, while for one that HAS an interpreter they are the
// difference between "refused" and "attached and walked nothing", which is
// exactly the reading an operator needs.
func enrollPython(objs sessionObjs, pid uint32, logPrefix string) bool {
	maps, ok := objs.(pythonMaps)
	if !ok {
		// The interpreter lookup runs first even here: checking the map
		// capability before it would print "no Python frames on this
		// profiler" for every off-CPU capture of every non-Python process
		// on the box, which is noise about something nobody asked for.
		if _, _, err := pyunwind.FindInterpreter(pid); err != nil {
			return false
		}
		log.Printf("%s: python frames: pid %d maps a CPython image, but this profiler does not expose py_procs; stacks stay native-only",
			logPrefix, pid)
		return false
	}

	// Nothing to enrol against if the arm was compiled out: py_procs and the
	// eval range would be written and never read. Reported here rather than
	// silently skipped, because "we found your interpreter" followed by no
	// frames is the shape this branch keeps refusing.
	if attempted, enabled, reason := profile.PythonWalkState(); attempted && !enabled {
		log.Printf("%s: python frames: pid %d: NOT ENROLLED -- the interpreter arm is not in the loaded "+
			"program (verifier: %s)", logPrefix, pid, reason)
		return false
	}

	libPath, found, res, err := pyunwind.EnrollTarget(pid, &pyunwind.BPFMaps{
		PyProcs:    maps.PyProcsMap(),
		EvalRanges: maps.PyEvalRangesMap(),
	})
	switch {
	case !found && err == nil:
		// The overwhelmingly common case: not a Python process. No line.
		return false
	case err != nil:
		// Worded as a REFUSAL, like every other outcome that leaves the arm
		// uninstalled: a log line naming a pid and no refusal reads as
		// success to anything checking for one.
		log.Printf("%s: python frames: pid %d: REFUSED %s: %v", logPrefix, pid, libPath, err)
	case res.Refused != "":
		log.Printf("%s: python frames: pid %d: REFUSED %s (CPython %s): %s",
			logPrefix, pid, libPath, res.Version, res.Refused)
	default:
		log.Printf("%s: python frames: pid %d: attached %s (CPython %s)",
			logPrefix, pid, libPath, res.Version)
	}
	return true
}

// logPythonWalkCounters prints py_walk_counters once, at shutdown.
//
// Without it the Python walk is unobservable from the outside: a run that
// produced no Python frames looks exactly like a run with no Python in it,
// and the counters are the only place the difference is recorded (there are
// no walker_flags bits left). The line is printed even when every counter
// is zero -- a zeroed line is itself the answer to "did the arm ever fire".
//
// Units differ by field and the line says so: FramesPushed counts FRAMES,
// every other counter counts SAMPLES. See pyunwind/counters.go.
//
// hadInterpreter comes from enrollPython. When it is false the line is
// printed only if something moved anyway -- which on a target with no
// interpreter would itself be news -- so an ordinary Go or Rust capture
// gains no output.
func logPythonWalkCounters(objs sessionObjs, logPrefix string, hadInterpreter bool) {
	maps, ok := objs.(pythonMaps)
	if !ok || maps.PyWalkCountersMap() == nil {
		return
	}
	// DISABLED is a state, not a row of zeros. If the verifier refused the
	// program with the interpreter arm compiled in, userspace reloaded it
	// without the arm (profile.loadWithPythonGate) -- and ten zeroed counters
	// read exactly like "this workload ran no Python", which is the
	// ambiguity this counter set exists to remove. Say which it was.
	if attempted, enabled, reason := profile.PythonWalkState(); attempted && !enabled {
		log.Printf("%s: python walk: DISABLED (the verifier rejected the interpreter arm on this kernel, "+
			"so the program was loaded without it; every counter below would read zero and mean nothing). "+
			"Verifier said: %s", logPrefix, reason)
		return
	}
	c, err := pyunwind.ReadWalkCounters(maps.PyWalkCountersMap())
	if err != nil {
		log.Printf("%s: python walk counters unreadable: %v", logPrefix, err)
		return
	}
	if !hadInterpreter && c == (pyunwind.WalkCounters{}) {
		return
	}
	log.Printf("%s: python walk: frames_pushed=%d (frames); per-sample: tss_miss=%d no_proc_info=%d "+
		"tstate_read_fail=%d frame_read_fail=%d owner_implausible=%d chain_truncated=%d push_refused=%d "+
		"none_executable=%d chain_abandoned=%d",
		logPrefix, c.FramesPushed, c.TSSMiss, c.NoProcInfo, c.TStateReadFail, c.FrameReadFail,
		c.OwnerImplausible, c.ChainTruncated, c.PushRefused, c.NoneExecutable, c.ChainAbandoned)
}
