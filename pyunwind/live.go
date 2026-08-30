package pyunwind

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// ErrNoInterpreterMapped means nothing in the process's address space looks
// like a CPython image. Distinct from ErrNotAnInterpreter, which is about
// one specific path that turned out not to be one.
var ErrNoInterpreterMapped = errors.New("pyunwind: no CPython image in this process's mappings")

// ErrUnsupportedLibc means the target's C library is not the one whose
// internal `struct pthread` layout this package measured.
//
// glibcTSDOffsets returns its numbers for GOARCH == amd64 and asks nothing
// about the libc, because until the walker was wired up nothing reached
// them at runtime. On a musl target those offsets are simply wrong:
// specific_1stblock is glibc's, musl's TSD lives elsewhere in a differently
// shaped struct, and the walk comes back with a plausible pointer that is
// not a PyThreadState. Only Offsets.Validate stands between that and a
// stack of invented frames, and Validate is a plausibility screen by
// design, not a detector. A refusal that exists only in a doc comment is
// not a named refusal.
var ErrUnsupportedLibc = errors.New("pyunwind: the target's C library is not glibc; no measured TSD offsets for it")

// muslSonameRe matches the musl loader and libc as they appear in
// /proc/<pid>/maps: "/lib/ld-musl-x86_64.so.1" and, on distributions that
// ship it separately, "libc.musl-x86_64.so.1". Anchored on the "musl"
// token rather than on the whole filename so a versioned or
// architecture-specific name still matches.
var muslSonameRe = regexp.MustCompile(`(?:^|/)(?:ld-musl|libc\.musl)[-.]`)

// RequireGlibc reports whether a process's C library is one this package
// has measured. It returns ErrUnsupportedLibc for a musl target and nil
// otherwise -- including for a target whose libc cannot be identified,
// which is the pre-existing assumption and is at least not a NEW silent
// wrong answer.
func RequireGlibc(pid uint32) error {
	mappings, err := procmap.NewResolver().Mappings(pid)
	if err != nil {
		return fmt.Errorf("pyunwind: read pid %d mappings: %w", pid, err)
	}
	for _, m := range mappings {
		if m.Path != "" && muslSonameRe.MatchString(m.Path) {
			return fmt.Errorf("%w: %s", ErrUnsupportedLibc, m.Path)
		}
	}
	return nil
}

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
	if err := RequireGlibc(pid); err != nil {
		v, _ := DetectFromSoname(libPath)
		return refuseWith(v, err), nil
	}
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

	if len(tids) > maxValidationThreads {
		tids = tids[:maxValidationThreads]
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
	if len(tids) == maxValidationThreads {
		// Say that the search was cut short, rather than reporting the last
		// thread's refusal as though it were the whole story.
		last.Refused = fmt.Sprintf("%s (gave up after %d of pid %d's threads; none held a PyThreadState)",
			last.Refused, maxValidationThreads, pid)
	}
	return last, nil
}

// maxValidationThreads bounds how many threads AttachProcess will try
// before giving up.
//
// Each attempt ptrace-stops one thread for the length of a GETREGS. That is
// cheap once and not cheap 200 times: a process that maps libpython but has
// never run Python -- an embedder that loaded it and never called in, a
// thread pool started before the interpreter -- refuses on every thread,
// and without a bound the attach path walks the whole pool to learn one
// thing. 16 is well past the number of threads a process is likely to have
// created before running its first Python code, and far short of a pool.
const maxValidationThreads = 16

// EnrollTarget is the whole per-process enrolment as a caller with BPF maps
// wants it: find the interpreter, key it the way the unwinder keys every
// other binary, validate, and install.
//
// It exists so the DWARF profilers and the GPU probe enrol Python the SAME
// way. They load different BPF programs, but the interpreter arm lives in
// walk_step, which all of them share -- so a target enrolled for one and not
// the other produces Python frames on some of its stacks and not others,
// for no reason a user could deduce. One function, two thin callers.
//
// Returns found == false, with no error, for a process that maps no CPython
// image at all -- which is nearly every process on a machine, and is not
// worth an error value.
func EnrollTarget(pid uint32, m *BPFMaps) (libPath string, found bool, res Result, err error) {
	libPath, _, ferr := FindInterpreter(pid)
	if ferr != nil {
		if errors.Is(ferr, ErrNoInterpreterMapped) {
			return "", false, Result{}, nil
		}
		return "", false, Result{}, ferr
	}
	tableID, terr := TableIDForMapping(pid, libPath)
	if terr != nil {
		return libPath, true, Result{}, terr
	}
	res, err = AttachProcess(pid, libPath, tableID, m)
	return libPath, true, res, err
}

// TableIDForMapping computes the same FNV-1a-of-build-id key the CFI tables
// use for a binary, so an eval range lands under the key walk_step already
// holds when it reaches the interpreter arm.
//
// It reads the build-id through the mapping's OPENABLE path rather than the
// symbolic one, for the reason ehmaps documents at length: a
// deleted-but-mapped binary, or one in another mount namespace, is
// reachable only through /proc/<pid>/map_files. A table_id derived from a
// different file than the one the tracker enrolled keys the eval range to a
// binary no PC ever resolves to -- the arm would be on and never fire.
func TableIDForMapping(pid uint32, libPath string) (uint64, error) {
	mappings, err := procmap.NewResolver().Mappings(pid)
	if err != nil {
		return 0, fmt.Errorf("pyunwind: read pid %d mappings: %w", pid, err)
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
			return 0, fmt.Errorf("pyunwind: build-id of %s: %w", openPath, err)
		}
		return ehmaps.TableIDForBuildID(buildID), nil
	}
	return 0, fmt.Errorf("pyunwind: no readable mapping for %s in pid %d", libPath, pid)
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
