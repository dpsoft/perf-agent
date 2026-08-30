package pyunwind

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// ErrNoInterpreterMapped means nothing in the process's address space looks
// like a CPython image. Distinct from ErrNotAnInterpreter, which is about
// one specific path that turned out not to be one.
var ErrNoInterpreterMapped = errors.New("pyunwind: no CPython image in this process's mappings")

// FindInterpreter picks the CPython image to walk out of a process's
// executable mappings.
//
// Two shapes qualify and they are tried in that order:
//
//  1. A shared libpython ("libpython3.12.so.1.0"), which is what every
//     distro, container image and CI toolchain build ships.
//  2. The python executable itself ("/usr/bin/python3.12"), for a
//     statically linked interpreter -- the same symbols live there instead.
//
// The preference matters: a process running a shared build maps BOTH (the
// executable is a stub that links the library), and the stub carries
// neither the eval loop nor _PyRuntime. Ordering by "libpython first"
// rather than by whichever mapping /proc happens to list first is what
// keeps that from being a coin flip.
func FindInterpreter(pid uint32) (path string, v Version, err error) {
	mappings, err := procmap.NewResolver().Mappings(pid)
	if err != nil {
		return "", Version{}, fmt.Errorf("pyunwind: read pid %d mappings: %w", pid, err)
	}
	var libs, exes []string
	seen := map[string]bool{}
	for _, m := range mappings {
		if m.Path == "" || !m.IsExec || seen[m.Path] {
			continue
		}
		if _, ok := DetectFromSoname(m.Path); !ok {
			continue
		}
		seen[m.Path] = true
		if strings.HasPrefix(filepath.Base(m.Path), "lib") {
			libs = append(libs, m.Path)
		} else {
			exes = append(exes, m.Path)
		}
	}
	sort.Strings(libs)
	sort.Strings(exes)
	for _, p := range append(libs, exes...) {
		if ver, ok := DetectFromSoname(p); ok {
			return p, ver, nil
		}
	}
	return "", Version{}, fmt.Errorf("%w: pid %d", ErrNoInterpreterMapped, pid)
}

// AttachProcess is the whole per-process enrolment: locate the interpreter
// image, validate this build's offsets against the LIVE process, install
// py_procs, and only then switch the interpreter arm on for that binary by
// installing its eval-loop range.
//
// THE ORDER IS THE POINT. py_eval_ranges is the on-switch: walk_step does
// nothing for a PC in a binary with no range. Installing the range last
// means there is no window in which the arm fires for a process whose
// py_procs entry is missing or unvalidated -- which would otherwise show up
// as a burst of PyCntNoProcInfo for every python process on the box that
// happens to share the libpython of one that attached.
//
// tableID is the FNV-1a-of-build-id key the CFI tables already use for this
// binary; the caller computes it because that hash lives in the unwinder's
// map layer, not in this package.
//
// WHY IT LOOPS OVER THREADS. Validation reads a real PyThreadState, and the
// only way to reach one from outside the process is the pthread TSD slot of
// a thread that has actually run Python. A thread pool's idle worker, a
// thread created by a C library, and the main thread of a program that has
// handed everything to a worker all have an empty slot -- legitimately. The
// loop tries threads until one has a state; only refusals that are the same
// for every thread (wrong version, free-threaded build, unsupported
// architecture) stop it early.
func AttachProcess(pid uint32, libPath string, tableID uint64, m *BPFMaps) (Result, error) {
	code, err := GILStateCode(libPath)
	if err != nil {
		v, _ := DetectFromSoname(libPath)
		return refuseWith(v, err), nil
	}
	evalRange, err := EvalRangeForFile(libPath)
	if err != nil {
		v, _ := DetectFromSoname(libPath)
		return refuseWith(v, err), nil
	}

	tids, err := ThreadIDs(pid)
	if err != nil {
		v, _ := DetectFromSoname(libPath)
		return refuseWith(v, err), nil
	}

	var last Result
	for _, tid := range tids {
		res, err := Attach(pid, libPath, code, m, NewProcReader(int(pid), tid))
		if err != nil {
			return res, err
		}
		if res.Refused == "" {
			if err := InstallEvalRange(m.EvalRanges, tableID, evalRange); err != nil {
				return res, err
			}
			return res, nil
		}
		last = res
		// Only a per-thread refusal is worth retrying on another thread.
		// Everything else -- the version, the build's ABI, this
		// architecture, an unparseable binary -- gives the same answer for
		// every thread in the process, and retrying it 200 times on a
		// thread pool would ptrace-stop 200 threads to learn nothing.
		if !errors.Is(res.Reason, ErrOffsetsUnreadable) {
			return res, nil
		}
	}
	if last.Refused == "" {
		v, _ := DetectFromSoname(libPath)
		return refuseWith(v, fmt.Errorf("%w: pid %d has no threads to validate against", ErrOffsetsUnreadable, pid)), nil
	}
	return last, nil
}

// ThreadIDs lists a process's threads, main thread first.
//
// Main first because it is the one most likely to hold a PyThreadState (a
// process that runs any Python at all runs its top level there), so the
// common case costs one ptrace stop rather than one per thread. The rest
// are sorted so a failure is reproducible rather than dependent on readdir
// order.
func ThreadIDs(pid uint32) ([]int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, fmt.Errorf("pyunwind: list threads of pid %d: %w", pid, err)
	}
	var rest []int
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil || tid == int(pid) {
			continue
		}
		rest = append(rest, tid)
	}
	sort.Ints(rest)
	return append([]int{int(pid)}, rest...), nil
}
