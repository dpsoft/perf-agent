package pyunwind

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMuslSonameMatching pins the musl detection against the paths that
// actually appear in /proc/<pid>/maps, and against the glibc paths that
// must NOT match.
//
// It matters because the alternative to detecting musl is not "no Python
// frames" -- it is a walk through glibc's struct pthread offsets applied to
// musl's TSD, which returns a plausible pointer and produces invented
// frames. Only Offsets.Validate stands behind it, and that is a
// plausibility screen by design.
func TestMuslSonameMatching(t *testing.T) {
	musl := []string{
		"/lib/ld-musl-x86_64.so.1",
		"/lib/ld-musl-aarch64.so.1",
		"/usr/lib/libc.musl-x86_64.so.1",
		"/lib/libc.musl-aarch64.so.1",
	}
	for _, p := range musl {
		if !muslSonameRe.MatchString(p) {
			t.Errorf("%s was not recognised as musl", p)
		}
	}
	glibc := []string{
		"/usr/lib64/libc.so.6",
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/lib64/ld-linux-x86-64.so.2",
		"/usr/lib64/libpython3.12.so.1.0",
		"/usr/bin/python3.12",
		// A path that merely contains the word: a musl-named directory is
		// not a musl libc, and matching it would refuse a glibc target.
		"/opt/musl-toolchain/lib/libc.so.6",
	}
	for _, p := range glibc {
		if muslSonameRe.MatchString(p) {
			t.Errorf("%s was wrongly recognised as musl", p)
		}
	}
}

// RequireGlibc must return nil for this very process, which is glibc on
// every machine this test runs on. Without this the test above could pass
// against a matcher that matched nothing at all.
func TestRequireGlibcAcceptsThisProcess(t *testing.T) {
	if err := RequireGlibc(uint32(selfPID())); err != nil {
		if errors.Is(err, ErrUnsupportedLibc) {
			t.Fatalf("this test process was classified as musl: %v", err)
		}
		t.Skipf("cannot read own mappings: %v", err)
	}
}

func selfPID() int { return os.Getpid() }

// TestClassifyReportsAMeasuredMicroVersion drives classify against a real
// interpreter file and requires the micro version to come back measured.
//
// This is the H2 fix at the point where it matters: Result.Version is what
// enrolment logs, so an operator reading "CPython 3.12.0" for a machine
// running 3.12.3 has been told the identity of a build that was never
// there.
func TestClassifyReportsAMeasuredMicroVersion(t *testing.T) {
	path := findSystemLibpython(t)
	res := classify(path)
	if res.Refused != "" {
		t.Skipf("%s: %s", path, res.Refused)
	}
	if res.Version.Micro == MicroUnknown {
		t.Fatalf("%s classified as %v; the micro version is readable from Py_Version and must be reported",
			path, res.Version)
	}
	fromELF, err := VersionFromELF(path)
	if err != nil {
		t.Fatalf("VersionFromELF(%s): %v", path, err)
	}
	if res.Version != fromELF {
		t.Fatalf("classify reported %v, the file says %v", res.Version, fromELF)
	}
}

// TestClassifyRefusesAVersionMismatch: the offset table is chosen by minor
// version, so a file whose name and whose Py_Version disagree must be
// refused rather than walked with the table its NAME selects.
//
// The mismatch is constructed by copying a real interpreter to a path named
// for a different minor version -- which is exactly the shape the hazard
// takes in the wild (a build system symlinking or copying an interpreter
// under a version-suffixed name).
func TestClassifyRefusesAVersionMismatch(t *testing.T) {
	real := findSystemLibpython(t)
	v, err := VersionFromELF(real)
	if err != nil {
		t.Skipf("%s has no readable Py_Version: %v", real, err)
	}
	wrongMinor := v.Minor + 1
	if wrongMinor > 14 {
		wrongMinor = 12
	}
	if wrongMinor == v.Minor {
		t.Fatalf("test constructed no actual mismatch for %v", v)
	}
	lie := filepath.Join(t.TempDir(), fmt.Sprintf("libpython%d.%d.so.1.0", v.Major, wrongMinor))
	in, err := os.ReadFile(real)
	if err != nil {
		t.Skipf("cannot read %s: %v", real, err)
	}
	if err := os.WriteFile(lie, in, 0o644); err != nil {
		t.Fatalf("write %s: %v", lie, err)
	}

	res := classify(lie)
	if res.Refused == "" {
		t.Fatalf("%s (really %v) was accepted as %v", lie, v, res.Version)
	}
	if !errors.Is(res.Reason, ErrVersionMismatch) {
		t.Fatalf("Reason = %v, want errors.Is(..., ErrVersionMismatch)", res.Reason)
	}

	// Control: the same bytes under their HONEST name must classify
	// cleanly, so the refusal above is about the disagreement and not about
	// the copy.
	honest := filepath.Join(t.TempDir(), fmt.Sprintf("libpython%d.%d.so.1.0", v.Major, v.Minor))
	if err := os.WriteFile(honest, in, 0o644); err != nil {
		t.Fatalf("write %s: %v", honest, err)
	}
	if res := classify(honest); res.Refused != "" {
		t.Fatalf("the same bytes under an honest name were refused: %s", res.Refused)
	}
}

// TestClassifyIdentifiesAnUnversionedInterpreter is M2: an interpreter
// whose path carries no version at all must be identified from its own
// Py_Version, not reported as "not an interpreter".
//
// /usr/local/bin/python, a pyenv shim and a conda environment all look like
// this, and before the second pass they came back found=false, which
// enrolment logged as nothing whatsoever.
func TestClassifyIdentifiesAnUnversionedInterpreter(t *testing.T) {
	real := findSystemLibpython(t)
	v, err := VersionFromELF(real)
	if err != nil {
		t.Skipf("%s has no readable Py_Version: %v", real, err)
	}
	in, err := os.ReadFile(real)
	if err != nil {
		t.Skipf("cannot read %s: %v", real, err)
	}
	// No version anywhere in the name, exactly like a pyenv shim.
	anon := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(anon, in, 0o755); err != nil {
		t.Fatalf("write %s: %v", anon, err)
	}
	if _, ok := DetectFromSoname(anon); ok {
		t.Fatalf("test fixture %s still carries a version in its path", anon)
	}

	res := classify(anon)
	if res.Refused != "" {
		t.Fatalf("%s (really CPython %v) was refused: %s", anon, v, res.Refused)
	}
	if res.Version != v {
		t.Fatalf("classified as %v, the file says %v", res.Version, v)
	}
}

// TestExtensionSuffixScreenAgainstThisMachinesInterpreter is the control
// that keeps the free-threaded ELF screen from being a scan that finds
// nothing and reports agreement.
//
// It asserts BOTH directions against a real interpreter: the GIL-form
// extension suffix IS found (so the scanner, the section choice and the
// regexp shape all work), and the free-threaded form is NOT (so the screen
// does not refuse an ordinary build). Without the first assertion, a
// scanner that silently matched nothing -- wrong section, wrong regexp,
// unreadable file -- would look exactly like a clean pass.
//
// What it cannot assert is the positive case: no free-threaded build is
// available anywhere this runs. See the provenance note on
// freeThreadedExtSuffixRe.
func TestExtensionSuffixScreenAgainstThisMachinesInterpreter(t *testing.T) {
	path := findSystemLibpython(t)

	gil, err := scanReadOnlyDataFor(path, gilExtSuffixRe)
	if err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if !gil {
		t.Fatalf("%s embeds no .cpython-3XX-<arch>-linux-<abi>.so suffix; the screen below would be scanning for "+
			"something that is not there in the first place", path)
	}
	ft, err := scanReadOnlyDataFor(path, freeThreadedExtSuffixRe)
	if err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if ft {
		t.Fatalf("%s, a GIL build, matched the free-threaded suffix; the screen would refuse it", path)
	}
	if res := classify(path); res.Refused != "" {
		t.Fatalf("%s was refused: %s", path, res.Refused)
	}
}

// TestFreeThreadedScreenRefusesAMatchingImage drives the screen's refusal
// path with an image that carries the free-threaded suffix, since no real
// one is available.
//
// The fixture is a real interpreter with its OWN suffix string rewritten in
// place to the free-threaded form -- same length, so every offset in the
// file stays valid and the ELF still parses. That is as close to the real
// article as this can get without a free-threaded build, and it exercises
// the refusal end to end: scan, match, ErrFreeThreaded, named path.
func TestFreeThreadedScreenRefusesAMatchingImage(t *testing.T) {
	path := findSystemLibpython(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}
	loc := gilExtSuffixRe.FindIndex(raw)
	if loc == nil {
		t.Fatalf("%s carries no GIL extension suffix to rewrite", path)
	}
	// ".cpython-314-x86_64-linux-gnu.so" -> ".cpython-31t-x86_64-linux-gnu.so":
	// replacing the minor version's last digit with "t" keeps the length
	// identical, so nothing in the ELF shifts.
	suffix := string(raw[loc[0]:loc[1]])
	dash := strings.Index(suffix[len(".cpython-3"):], "-") + len(".cpython-3")
	mutant := append([]byte{}, raw...)
	mutant[loc[0]+dash-1] = 't'
	if !freeThreadedExtSuffixRe.Match(mutant[loc[0]:loc[1]]) {
		t.Fatalf("the rewritten suffix %q does not match the screen's pattern", string(mutant[loc[0]:loc[1]]))
	}

	out := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(out, mutant, 0o644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
	res := classify(out)
	if res.Refused == "" {
		t.Fatalf("%s carries a free-threaded extension suffix and was accepted", out)
	}
	if !errors.Is(res.Reason, ErrFreeThreaded) {
		t.Fatalf("Reason = %v, want errors.Is(..., ErrFreeThreaded)", res.Reason)
	}

	// Control: the same bytes WITHOUT the rewrite must classify clean, so
	// the refusal is about the suffix and not about the copy.
	honest := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(honest, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", honest, err)
	}
	if res := classify(honest); res.Refused != "" {
		t.Fatalf("the unmodified copy was refused: %s", res.Refused)
	}
}

// ----- The virtualenv defect, and the shape that got past every other test.
//
// In a venv EVERY shared object lives under `.venv/lib/python3.12/
// site-packages/`, so the DIRECTORY carries a Python version and the old
// path-matching rule accepted the first `.so` it sorted to. On the PyTorch
// target this branch exists to serve that was
// `numpy.libs/libgfortran-83c28eba-468e71e5.so.5.0.0`, which refused for want
// of PyGILState_GetThisThreadState -- terminally, so the real interpreter (uv's
// statically linked python3.12, in a directory whose name matches nothing) was
// never reached, and the process was reported as having no walkable Python.
//
// CI never saw it because CI profiles the system /usr/bin/python3.12 with no
// venv, which is the least representative Python deployment there is.
//
// These tests are the fixture that would have caught it: a candidate list with
// venv-shaped paths ahead of the real interpreter, asserted at both levels --
// the ranking that decides what is tried first, and the symbol check that
// decides what is accepted at all.

// venvCandidates is the ordering problem in miniature: five site-packages
// libraries whose paths all contain "python3.12", and one real interpreter
// whose path contains no version at all.
var venvCandidates = []string{
	"/home/u/proj/.venv/lib/python3.12/site-packages/markupsafe/_speedups.cpython-312-x86_64-linux-gnu.so",
	"/home/u/proj/.venv/lib/python3.12/site-packages/numpy.libs/libgfortran-83c28eba-468e71e5.so.5.0.0",
	"/home/u/proj/.venv/lib/python3.12/site-packages/numpy.libs/libquadmath-2284e583-a9307bba.so.0.0.0",
	"/home/u/proj/.venv/lib/python3.12/site-packages/torch/_C.cpython-312-x86_64-linux-gnu.so",
	"/home/u/proj/.venv/lib/python3.12/site-packages/torch/lib/libtorch_python.so",
	"/home/u/.local/share/uv/python/cpython-3.12.14-linux-x86_64-gnu/bin/python3.12",
}

// TestVenvSitePackagesRankBelowTheInterpreter pins the ORDERING half: no
// site-packages library may be tried before the interpreter executable.
//
// Ordering is an optimisation and not the correctness fix -- the symbol check
// below is -- but it is what keeps the common case to one or two ELF opens
// instead of the 123 shared objects that PyTorch venv maps.
func TestVenvSitePackagesRankBelowTheInterpreter(t *testing.T) {
	type ranked struct {
		path string
		rank int
	}
	var got []ranked
	for _, p := range venvCandidates {
		isLib := strings.HasPrefix(filepath.Base(p), "lib")
		got = append(got, ranked{p, candidateRank(p, isLib)})
	}

	interp := got[len(got)-1]
	if !strings.HasSuffix(interp.path, "/python3.12") {
		t.Fatalf("fixture drift: last candidate is %s, expected the interpreter", interp.path)
	}
	for _, g := range got[:len(got)-1] {
		if g.rank <= interp.rank {
			t.Errorf("%s ranks %d, at or above the interpreter's %d: a site-packages library "+
				"would be opened first, which is how libgfortran was selected as the CPython image",
				g.path, g.rank, interp.rank)
		}
	}

	// And the ranking must not have achieved that by demoting every library:
	// a shared libpython is the BEST candidate there is, because a process
	// running a shared build maps both it and an executable stub that carries
	// none of the symbols.
	lib := candidateRank("/usr/lib64/libpython3.12.so.1.0", true)
	if lib >= interp.rank {
		t.Errorf("libpython ranks %d, not above the executable's %d: on a shared build the "+
			"stub would be tried first and it carries neither the eval loop nor _PyRuntime",
			lib, interp.rank)
	}
}

// TestOnlyDefinedInterpreterSymbolsAdmitACandidate pins the CORRECTNESS half,
// and it is a property of the FILE, not of its name.
//
// Run against this machine's own executable mappings rather than a synthetic
// ELF: every process maps libc, the loader and the test binary, none of which
// is CPython, and none of which may be accepted. If a real interpreter is
// mapped -- the test binary is not one, but a CI runner's may map one -- it
// must be accepted, and by its symbols.
func TestOnlyDefinedInterpreterSymbolsAdmitACandidate(t *testing.T) {
	candidates, err := interpreterCandidates(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("interpreterCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("this process maps no executable files, which cannot be")
	}
	for _, p := range candidates {
		ok, _ := definesInterpreterSymbols(p)
		if ok {
			t.Errorf("%s was accepted as a CPython image; this test binary maps none", p)
		}
	}
}

// A C extension module IMPORTS PyGILState_GetThisThreadState -- that is what
// linking against the interpreter looks like -- so accepting an undefined
// symbol as evidence would put every one of site-packages' extension modules
// back in the candidate set, and reintroduce the bug in a form that reads as
// its fix.
//
// Asserted against real files when this machine has them, skipped when it does
// not, because a synthetic ELF would only prove the test's own construction.
func TestAnUndefinedSymbolIsNotEvidence(t *testing.T) {
	var ext string
	for _, glob := range []string{
		"/home/*/*/.venv/lib/python3.*/site-packages/*/*.cpython-*.so",
		"/usr/lib64/python3.*/lib-dynload/*.cpython-*.so",
	} {
		if m, _ := filepath.Glob(glob); len(m) > 0 {
			ext = m[0]
			break
		}
	}
	if ext == "" {
		t.Skip("no CPython extension module on this machine to test against")
	}
	ok, err := definesInterpreterSymbols(ext)
	if err != nil {
		t.Fatalf("definesInterpreterSymbols(%s): %v", ext, err)
	}
	if ok {
		t.Errorf("%s was accepted as an interpreter; it imports the CPython API, it does not define it", ext)
	}
}

// The real interpreter on this machine must be accepted, whichever shape it
// has -- a shared libpython or a statically linked executable. Without this
// the two tests above are satisfied by a function that accepts nothing.
func TestARealInterpreterIsAccepted(t *testing.T) {
	var found []string
	for _, glob := range []string{
		"/usr/lib64/libpython3.*.so*",
		"/usr/lib/x86_64-linux-gnu/libpython3.*.so*",
		"/home/*/.local/share/uv/python/cpython-3.*/bin/python3.*",
	} {
		m, _ := filepath.Glob(glob)
		found = append(found, m...)
	}
	var tested int
	for _, p := range found {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() || strings.HasSuffix(p, "-config") {
			continue
		}
		ok, err := definesInterpreterSymbols(p)
		if err != nil {
			continue // not an ELF, or unreadable: not what this test is about
		}
		tested++
		if !ok {
			t.Errorf("%s is a CPython image and was NOT accepted; the symbol check is too strict "+
				"and this target would silently produce no Python frames", p)
		}
	}
	if tested == 0 {
		t.Skip("no CPython image on this machine to test against")
	}
}

// TestThePathRuleWouldHaveAcceptedLibgfortran is the assertion that proves the
// three above can fail: it shows the OLD rule accepting the exact file that
// broke, so "the new rule rejects it" is a change and not a tautology.
//
// DetectFromSoname is still used -- to REPORT a version when the file carries
// no Py_Version, and to rank candidates -- so it still answers true here. What
// changed is that its answer no longer admits anything.
func TestThePathRuleWouldHaveAcceptedLibgfortran(t *testing.T) {
	const libgfortran = "/home/u/proj/.venv/lib/python3.12/site-packages/" +
		"numpy.libs/libgfortran-83c28eba-468e71e5.so.5.0.0"

	v, ok := DetectFromSoname(libgfortran)
	if !ok {
		t.Fatalf("fixture drift: the path rule no longer matches %s, so this test no longer "+
			"demonstrates anything", libgfortran)
	}
	if v.Minor != 12 {
		t.Errorf("the path rule read version %s out of a Fortran runtime's directory", v)
	}
	// That is the whole defect in one line: a Fortran runtime, reported as
	// CPython 3.12, because the directory it sits in is called python3.12.
}

// ----- Never stop the leader of a process we launched.
//
// A ptrace stop of a child is reported to that child's PARENT by wait(2), and
// when the profiler is the parent, os/exec is what is waiting. Go's
// os.Process.wait does one wait4 and does not loop past a stopped status, so it
// returns "stopped" and Cmd.Wait reports the workload as finished when it is
// not. Observed on the GPU path, where the workload is a child of
// gpu-cuda-profile:
//
//	workload: stop signal: trace/breakpoint trap (trap 128)
//
// trap 128 is PTRACE_EVENT_STOP -- our own PTRACE_INTERRUPT surfacing through
// the launcher's Cmd.Wait, after which the workload is never reaped or
// released and both processes sit forever.
//
// There is a second, quieter way it corrupts the parent: releaseTracee's own
// wait4 treats a non-stopped status as "exited under us", which for the leader
// of our child REAPS ITS EXIT STATUS -- and Cmd.Wait then fails with ECHILD.
//
// Only the leader is affected, because wait4(pid) matches that pid alone.

func TestOwnChildIsRecognised(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	if !isOwnChild(uint32(cmd.Process.Pid)) {
		t.Errorf("pid %d is our own child and was not recognised as one; its leader would be "+
			"ptraced and the stop would surface through Cmd.Wait", cmd.Process.Pid)
	}
	// This process is not its own child, and neither is init. Without these
	// the check above is satisfied by a function that always returns true --
	// which would refuse to ptrace every target, CPU path included.
	if isOwnChild(uint32(os.Getpid())) {
		t.Error("this process reported as its own child")
	}
	if isOwnChild(1) {
		t.Error("pid 1 reported as our child")
	}
}

func TestTheLeaderOfOurOwnChildIsNotAPtraceCandidate(t *testing.T) {
	// A child with more than one thread, so there is something left after the
	// leader is removed -- which is the case that matters: a CUDA workload
	// always has several.
	cmd := exec.Command("sh", "-c", "sleep 30 & sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child: %v", err)
	}
	pid := uint32(cmd.Process.Pid)
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	all, err := ThreadIDs(pid)
	if err != nil {
		t.Skipf("cannot list threads of the child: %v", err)
	}
	if all[0] != int(pid) {
		t.Fatalf("ThreadIDs put %d first, expected the leader %d", all[0], pid)
	}

	got, err := ptraceCandidates(pid)
	if err != nil {
		// A single-threaded child is refused BY NAME rather than silently
		// ptraced, which is the other half of the contract.
		if !errors.Is(err, ErrTraceeIsOwnChild) {
			t.Fatalf("ptraceCandidates: %v", err)
		}
		return
	}
	for _, tid := range got {
		if tid == int(pid) {
			t.Fatalf("the leader (%d) is still a ptrace candidate for a process we launched", pid)
		}
	}
}

// And the CPU path must be untouched: for a target we did NOT launch, the
// leader stays first, because a process that runs any Python at all runs its
// top level there and trying it first costs one ptrace stop instead of one per
// thread.
func TestTheLeaderIsStillPreferredForATargetWeDidNotLaunch(t *testing.T) {
	self := uint32(os.Getpid())
	got, err := ptraceCandidates(self)
	if err != nil {
		t.Fatalf("ptraceCandidates(self): %v", err)
	}
	if len(got) == 0 || got[0] != int(self) {
		t.Fatalf("candidates start with %v, want the leader %d first", got, self)
	}
}
