package pyunwind

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"

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

// interpreterSymbols are the symbols a mapped file must DEFINE before this
// package will treat it as a CPython image.
//
// THE PATH IS A HINT FOR ORDERING, NEVER A DECISION, and that distinction is
// the whole of the fix here. Before it, a candidate was accepted because its
// PATH matched a Python version, and in a virtualenv every shared object lives
// under `.venv/lib/python3.12/site-packages/`, so the DIRECTORY matched and
// every `.so` in site-packages looked like an interpreter. The first one
// alphabetically won. Measured on the PyTorch target this branch exists to
// serve: 123 shared objects in that venv, all of them matching the path rule,
// and `libgfortran-83c28eba-468e71e5.so.5.0.0` was selected as the CPython
// image. It refused for want of PyGILState_GetThisThreadState, terminally, and
// the real interpreter -- uv's statically linked python3.12, in a directory
// whose name matches nothing -- was never reached.
//
//	$ scan the same 123 objects for a DEFINITION of either symbol below
//	0 of 123
//
// So the rule is evidence, not resemblance: 123 candidates go to zero, and the
// only file in that process carrying the symbols is the one that is actually
// the interpreter. It also fixes the CLASS rather than the instance -- a
// library called libpython-anything, anywhere, would have failed identically.
//
// WHY THESE TWO. PyGILState_GetThisThreadState is not a proxy for "is this
// CPython", it is the exact precondition for what happens next: GILStateCode
// disassembles its body to recover the TSS key, and without it attach cannot
// proceed at all. _PyRuntime corroborates it as an interpreter rather than
// something that re-exports one function. Both are exported from the shared
// libpython and from a statically linked python executable (which must export
// the C API for extension modules to link against), measured on this host:
//
//	libpython3.14.so.1.0                  both present
//	/usr/bin/python3.14  (stub)           NEITHER -- correctly rejected, and
//	                                      the shared library beside it wins
//	uv cpython-3.12.14 python3.12 (static) both present
//
// A candidate that defines these and then refuses for some other reason -- a
// version this build declines, a free-threaded build -- no longer ends the
// search either. See FindInterpreters.
const (
	runtimeSymbol = "_PyRuntime"
)

// definesInterpreterSymbols reports whether path DEFINES every symbol in
// interpreterSymbols.
//
// DEFINES, not merely mentions. Every CPython extension module in
// site-packages -- torch/_C, numpy's, markupsafe's -- carries
// PyGILState_GetThisThreadState in its .dynsym as an UNDEFINED import, which is
// what linking against the interpreter looks like. Accepting an undefined
// symbol as evidence would put every extension module back in the candidate
// set and reintroduce this bug in a form that reads as a fix.
//
// A file that cannot be opened or parsed is not an interpreter for this
// purpose: the error is returned so a caller can say how many candidates were
// examined, but it never stops the search.
func definesInterpreterSymbols(path string) (bool, error) {
	osf, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("pyunwind: open %s: %w", path, err)
	}
	defer func() { _ = osf.Close() }()
	f, err := elf.NewFile(osf)
	if err != nil {
		return false, fmt.Errorf("pyunwind: parse %s: %w", path, err)
	}

	want := map[string]bool{gilStateSymbol: false, runtimeSymbol: false}
	// .dynsym first: on a stripped distro build it is the only symbol table
	// there is, and the interpreter's API is exported there by construction.
	// elf.ErrNoSymbols from either is not an error here.
	for _, get := range []func() ([]elf.Symbol, error){f.DynamicSymbols, f.Symbols} {
		syms, err := get()
		if err != nil {
			continue
		}
		for _, sym := range syms {
			if sym.Section == elf.SHN_UNDEF {
				continue
			}
			if _, ok := want[sym.Name]; ok {
				want[sym.Name] = true
			}
		}
	}
	for _, have := range want {
		if !have {
			return false, nil
		}
	}
	return true, nil
}

// candidateRank orders the files worth trying, best first. It is an
// OPTIMISATION and nothing rests on it: definesInterpreterSymbols decides, and
// a perfectly-ordered list and a shuffled one select the same file. What the
// ranking buys is that the common case opens one or two ELF files instead of
// every executable mapping in the process.
//
// A shared libpython first, because a process running a shared build maps BOTH
// it and the executable stub that links it, and only the library carries the
// symbols. Then the interpreter executable, for a statically linked build.
// Then everything else, with site-packages last -- it is the largest group by
// far and the least likely to hold anything.
func candidateRank(path string, isLib bool) int {
	base := filepath.Base(path)
	switch {
	case strings.HasPrefix(base, "libpython"):
		return 0
	case !isLib && strings.HasPrefix(base, "python"):
		return 1
	case strings.Contains(path, "/site-packages/"):
		return 4
	case isLib:
		return 3
	default:
		return 2
	}
}

// interpreterCandidates returns a process's distinct executable mappings,
// ordered by candidateRank and then by path so the answer is reproducible.
func interpreterCandidates(pid uint32) ([]string, error) {
	mappings, err := procmap.NewResolver().Mappings(pid)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: read pid %d mappings: %w", pid, err)
	}
	type cand struct {
		path string
		rank int
	}
	var cands []cand
	seen := map[string]bool{}
	for _, m := range mappings {
		if m.Path == "" || !m.IsExec || seen[m.Path] {
			continue
		}
		seen[m.Path] = true
		isLib := strings.HasPrefix(filepath.Base(m.Path), "lib")
		cands = append(cands, cand{path: m.Path, rank: candidateRank(m.Path, isLib)})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		return cands[i].path < cands[j].path
	})
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.path)
	}
	return out, nil
}

// Interpreter is one CPython image found in a process, with the version
// MEASURED from the file rather than inferred from its name.
type Interpreter struct {
	Path    string
	Version Version
}

// FindInterpreters returns every CPython image in a process's executable
// mappings, best candidate first.
//
// It returns a LIST because a refused candidate must not end the search. A
// process can map more than one interpreter image -- an embedder that dlopens
// a libpython beside a statically linked host, a venv whose base interpreter is
// also mapped -- and refusing the process because the first one was, say, a
// free-threaded build hides an interpreter that would have walked fine. Only
// when every candidate has been tried is the process itself refused, and the
// error says how many were examined so "no interpreter here" and "the
// interpreter refused" cannot read the same.
//
// The version is read from the file (Py_Version in .rodata), falling back to
// the soname when the file carries no such symbol. Both are measurements of
// the accepted image, made only after its symbols have already proved it is
// an interpreter -- the name is never what admitted it.
func FindInterpreters(pid uint32) ([]Interpreter, error) {
	candidates, err := interpreterCandidates(pid)
	if err != nil {
		return nil, err
	}
	var found []Interpreter
	for _, p := range candidates {
		ok, err := definesInterpreterSymbols(p)
		if err != nil || !ok {
			continue
		}
		v, verr := VersionFromELF(p)
		if verr != nil {
			// The symbols say interpreter; only the version is unreadable.
			// The soname is the fallback, and an unknown version is still
			// reported -- AttachProcess refuses an unsupported one by name,
			// which is a better answer than pretending nothing was found.
			v, _ = DetectFromSoname(p)
		}
		found = append(found, Interpreter{Path: p, Version: v})
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: pid %d, %d executable mappings examined",
			ErrNoInterpreterMapped, pid, len(candidates))
	}
	return found, nil
}

// FindInterpreter returns the best CPython image in a process, which is the
// first one FindInterpreters ranked. Kept for callers that want one answer;
// enrolment uses FindInterpreters so a refusal can fall through to the next.
func FindInterpreter(pid uint32) (path string, v Version, err error) {
	found, err := FindInterpreters(pid)
	if err != nil {
		return "", Version{}, err
	}
	return found[0].Path, found[0].Version, nil
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
	// Located, not installed. The eval-loop range is the ON-SWITCH and it is
	// no longer this package's to write: it goes into the core's
	// handoff_ranges under this module's unwinder id, and unwind/interp
	// installs it AFTER this function returns successfully. Locating it here
	// anyway is what keeps a target whose eval loop cannot be found from
	// attaching and then producing nothing -- a refusal with a reason beats a
	// py_procs entry no PC ever reaches.
	if _, err := EvalRangesForFile(libPath); err != nil {
		v, _ := DetectFromSoname(libPath)
		return refuseWith(v, err), nil
	}

	tids, err := ThreadIDs(pid)
	if err != nil {
		v, _ := DetectFromSoname(libPath)
		return refuseWith(v, err), nil
	}

	totalThreads := len(tids)
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
	if totalThreads > maxValidationThreads {
		// Say that the search was cut short, rather than reporting the last
		// thread tried as though it were the last thread there is.
		last.Refused = fmt.Sprintf("%s (tried %d of pid %d's %d threads; none held a PyThreadState)",
			last.Refused, maxValidationThreads, pid, totalThreads)
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
// way. They load different BPF programs, but the handoff is in the walk all of
// them share -- so a target enrolled for one and not the other produces Python
// frames on some of its stacks and not others, for no reason a user could
// deduce. One function, two thin callers.
//
// A REFUSED CANDIDATE DOES NOT END THE SEARCH. Every image FindInterpreters
// returned is tried in order, and the process is refused only when all of them
// have been. Before this, the first candidate's refusal was terminal: on a
// PyTorch venv that meant one library's missing symbol stood in for "this
// process has no Python in it", which is a different fact and was reported as
// the same one.
//
// The refusal that comes back is the LAST one, not the first, and both are
// arbitrary -- what matters is that it is a refusal from something that
// carried the interpreter's symbols. The log line names the path, so which
// image refused is never guesswork.
//
// Returns found == false, with no error, for a process that maps no CPython
// image at all -- which is nearly every process on a machine, and is not
// worth an error value.
func EnrollTarget(pid uint32, m *BPFMaps) (libPath string, found bool, res Result, err error) {
	interps, ferr := FindInterpreters(pid)
	if ferr != nil {
		if errors.Is(ferr, ErrNoInterpreterMapped) {
			return "", false, Result{}, nil
		}
		return "", false, Result{}, ferr
	}

	var lastRes Result
	var lastPath string
	for _, in := range interps {
		tableID, terr := TableIDForMapping(pid, in.Path)
		if terr != nil {
			// This image cannot be keyed the way the CFI tables key it, so a
			// range installed for it would sit under a table_id no PC
			// resolves to. Try the next rather than refuse the process.
			lastPath, lastRes = in.Path, refuseWith(in.Version, terr)
			continue
		}
		r, aerr := AttachProcess(pid, in.Path, tableID, m)
		if aerr != nil {
			lastPath, lastRes = in.Path, r
			continue
		}
		if r.Refused == "" {
			return in.Path, true, r, nil
		}
		lastPath, lastRes = in.Path, r
	}
	if len(interps) > 1 && lastRes.Refused != "" {
		// Say that more than one image was tried, so a reader does not take
		// the last one's reason as the only one there was.
		lastRes.Refused = fmt.Sprintf("%s (tried %d CPython images in pid %d; all refused)",
			lastRes.Refused, len(interps), pid)
	}
	return lastPath, true, lastRes, nil
}

// DetachProcess removes a process's py_procs record.
//
// PID REUSE IS THE REASON THIS EXISTS. py_procs is keyed by pid and
// walk_step trusts any entry whose `enabled` byte is set -- it has no way to
// tell that the pid it is walking belongs to a different process than the
// one userspace validated. On a long-lived agent (the GPU path enrols
// producers for as long as the daemon runs) a recycled pid whose new
// occupant is a DIFFERENT CPython build would be walked with the previous
// process's offsets: a plausible stack of wrong frames, which is the single
// failure mode this whole package refuses everywhere else.
//
// A missing key is success, not an error: the common case is a process that
// was never a Python target at all.
func DetachProcess(pid uint32, m *BPFMaps) error {
	if m == nil || m.PyProcs == nil {
		return nil
	}
	if err := m.PyProcs.Delete(&pid); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("pyunwind: delete py_procs[%d]: %w", pid, err)
	}
	return nil
}

// The eval RANGE is deliberately NOT removed here. It is keyed by table_id
// (a binary), not by pid, so it stays correct for every other process
// running that same libpython, and re-deriving which ranges are still in
// use would duplicate the refcounting the CFI TableStore already does for
// the same binaries. A stale range costs one hash lookup per eval-loop
// frame in a process with no py_procs entry, which walk_step already counts
// as PY_CNT_NO_PROC_INFO and handles by marking the sample done.

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
