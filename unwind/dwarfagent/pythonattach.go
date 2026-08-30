package dwarfagent

import (
	"errors"
	"fmt"
	"log"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/pyunwind"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// pythonMaps is the slice of the BPF handle the CPython walker needs. Only
// profile.PerfDwarf implements it today; profile.OffCPUDwarf does not
// expose the maps even though its program carries them, so the off-CPU
// session takes the "no accessor" path below and says so once rather than
// silently skipping.
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
	// Interpreter lookup FIRST, map capability second. The other order
	// prints "this profiler cannot do Python frames" for every off-CPU
	// capture of every non-Python process on the box, which is noise about
	// something nobody asked for; this way the line only appears when there
	// really is an interpreter that could have been walked.
	libPath, version, err := pyunwind.FindInterpreter(pid)
	if err != nil {
		if errors.Is(err, pyunwind.ErrNoInterpreterMapped) {
			// The overwhelmingly common case: not a Python process.
			return false
		}
		log.Printf("%s: python frames: pid %d: %v", logPrefix, pid, err)
		return false
	}

	maps, ok := objs.(pythonMaps)
	if !ok {
		log.Printf("%s: python frames: pid %d maps %s, but this profiler does not expose py_procs; stacks stay native-only",
			logPrefix, pid, libPath)
		return false
	}

	tableID, err := tableIDForPath(pid, libPath)
	if err != nil {
		log.Printf("%s: python frames: pid %d: %s: %v", logPrefix, pid, libPath, err)
		return true
	}

	res, err := pyunwind.AttachProcess(pid, libPath, tableID, &pyunwind.BPFMaps{
		PyProcs:    maps.PyProcsMap(),
		EvalRanges: maps.PyEvalRangesMap(),
	})
	switch {
	case err != nil:
		log.Printf("%s: python frames: pid %d: installing maps for %s: %v", logPrefix, pid, libPath, err)
	case res.Refused != "":
		log.Printf("%s: python frames: pid %d: REFUSED %s (CPython %s): %s",
			logPrefix, pid, libPath, res.Version, res.Refused)
	default:
		log.Printf("%s: python frames: pid %d: attached %s (CPython %s), table %#x",
			logPrefix, pid, libPath, version, tableID)
	}
	return true
}

// tableIDForPath computes the same FNV-1a-of-build-id key the CFI tables
// use for a binary, so the eval range lands under the key walk_step already
// holds when it reaches the interpreter arm.
//
// It reads the build-id through the mapping's openable path rather than the
// symbolic one, for the reason ehmaps documents at length: a
// deleted-but-mapped binary, or one in another mount namespace, is
// reachable only through /proc/<pid>/map_files. A table_id derived from a
// DIFFERENT file than the one the tracker enrolled would key the eval range
// to a binary that no PC ever resolves to -- the arm would be on and never
// fire.
func tableIDForPath(pid uint32, libPath string) (uint64, error) {
	mappings, err := procmap.NewResolver().Mappings(pid)
	if err != nil {
		return 0, fmt.Errorf("read mappings: %w", err)
	}
	for _, m := range mappings {
		if m.Path != libPath {
			continue
		}
		openPath := m.OpenablePath()
		if openPath == "" {
			continue
		}
		buildID, err := ehmaps.ReadBuildID(openPath)
		if err != nil {
			return 0, fmt.Errorf("build-id: %w", err)
		}
		return ehmaps.TableIDForBuildID(buildID), nil
	}
	return 0, fmt.Errorf("no readable mapping for %s", libPath)
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
