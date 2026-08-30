// pyunwind/attach_test.go
package pyunwind

import (
	"debug/elf"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// A refusal must name its reason. An operator whose Python frames are
// missing needs to distinguish "we found 3.11 and declined" from "we could
// not read the interpreter" from "this is not a Python process" from "this
// is a free-threaded build we don't support". Refused (a string, for
// humans) and Reason (an error, for errors.Is -- see the sentinels this
// package exports) must both name it: a caller like Task 7's counters
// cannot bucket refusals by parsing English out of Refused.
func TestAttachRefusalsAreNamed(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		expect     string
		wantReason error
	}{
		{"unsupported version", "/usr/lib/libpython3.11.so.1.0", "unsupported", ErrUnsupportedVersion},
		{"not python", "/usr/bin/nginx", "not an interpreter", ErrNotAnInterpreter},
		{"free-threaded 3.13", "/usr/lib/libpython3.13t.so.1.0", "free-threaded", ErrFreeThreaded},
		{"free-threaded 3.14", "/usr/lib/x86_64-linux-gnu/libpython3.14t.so.1.0", "free-threaded", ErrFreeThreaded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classify(tc.path)
			if !strings.Contains(res.Refused, tc.expect) {
				t.Fatalf("reason %q does not mention %q", res.Refused, tc.expect)
			}
			if !errors.Is(res.Reason, tc.wantReason) {
				t.Fatalf("Reason = %v, want errors.Is(..., %v)", res.Reason, tc.wantReason)
			}
		})
	}
}

// WHY THESE TESTS PIN THE ARCHITECTURE.
//
// Every refusal below the architecture gate is architecture-independent --
// the TLSBaseReader capability check, the autoTSSkey parse, symbol
// resolution, Validate -- but the gate is not, and it fires first. Run on
// arm64, which has no measured glibc TSD offsets and is refused by name,
// every one of these tests collapsed into a single assertion about the
// architecture; the specific refusal each is named for was never reached.
// That is what CI reported the first time this package ran on an arm64
// runner (it had never been in the unit-test invocation before).
//
// So they pass "amd64" explicitly, via prepareInfoForArch. Not to pretend
// the host is amd64: the numbers glibcTSDOffsets returns for it are
// measured, and the plumbing under test does not depend on them being the
// host's. The alternative -- skipping on arm64 -- would keep the arm64 job
// green while testing nothing, and this branch has spent two rounds on what
// silent declines cost.
//
// The gate itself is pinned separately, and now on every architecture, by
// TestPrepareInfoRefusesAnUnmeasuredArch below.

// TestPrepareInfoReasonsAreErrorsIsable extends the same guarantee to
// prepareInfo's own refusal points, not just classify's: every one of them
// must set Reason to something errors.Is-able against one of this
// package's sentinels, not just a Refused string a human can read.
func TestPrepareInfoReasonsAreErrorsIsable(t *testing.T) {
	t.Run("no TLSBaseReader", func(t *testing.T) {
		_, res := prepareInfoForArch("amd64", 1, "/usr/lib64/libpython3.12.so.1.0", nil, fakeReader{})
		if !errors.Is(res.Reason, ErrTLSBaseUnavailable) {
			t.Fatalf("Reason = %v, want errors.Is(..., ErrTLSBaseUnavailable)", res.Reason)
		}
	})
	t.Run("unrecognised TSS code", func(t *testing.T) {
		garbage := []byte{0xf3, 0x0f, 0x1e, 0xfa, 0xc3}
		_, res := prepareInfoForArch("amd64", 1, "/usr/lib64/libpython3.12.so.1.0", garbage, fakeAttachReader{})
		if !errors.Is(res.Reason, ErrTSSPatternUnrecognised) {
			t.Fatalf("Reason = %v, want errors.Is(..., ErrTSSPatternUnrecognised)", res.Reason)
		}
	})
	t.Run("pid does not map library", func(t *testing.T) {
		path := findSystemLibpython(t)
		unmapped := "/nonexistent-for-pyunwind-tests/" + filepath.Base(path)
		code := readGilstateCode(t, gilstateFixtures[0])
		_, res := prepareInfoForArch("amd64", uint32(os.Getpid()), unmapped, code, fakeAttachReader{})
		if !errors.Is(res.Reason, ErrSymbolNotFound) {
			t.Fatalf("Reason = %v, want errors.Is(..., ErrSymbolNotFound)", res.Reason)
		}
	})
}

// A supported, non-free-threaded soname must classify cleanly with no
// refusal at all -- the free-threaded check must not misfire on an
// ordinary "t"-free path.
func TestClassifyAcceptsSupportedVersions(t *testing.T) {
	for _, path := range []string{
		"/usr/lib64/libpython3.12.so.1.0",
		"/usr/lib64/libpython3.13.so.1.0",
		"/usr/lib64/libpython3.14.so.1.0",
	} {
		res := classify(path)
		if res.Refused != "" {
			t.Fatalf("%s: unexpected refusal: %s", path, res.Refused)
		}
	}
}

// TestPyProcInfoSizeMirrorsC pins pyProcInfo's size against
// bpf/python_walk.h's _Static_assert(sizeof(struct py_proc_info) == 56, ...).
// A field added on only one side does not fail to compile -- it silently
// writes offsets at the wrong byte position -- so this arithmetic check is
// the only thing standing between a one-sided edit and garbage frames.
func TestPyProcInfoSizeMirrorsC(t *testing.T) {
	const wantSize = 56
	if got := unsafe.Sizeof(pyProcInfo{}); got != wantSize {
		t.Fatalf("pyProcInfo is %d bytes; bpf/python_walk.h's struct py_proc_info is %d bytes -- "+
			"a drift here writes offsets at the wrong byte position and produces plausible-but-wrong frames",
			got, wantSize)
	}
	if got := pyProcInfoSize; got != wantSize {
		t.Fatalf("pyProcInfoSize const = %d, want %d", got, wantSize)
	}
}

// --- hostTSSGet -------------------------------------------------------

func TestHostTSSGetReadsTheSlot(t *testing.T) {
	tsd := tsdOffsets{Specific1stBlock: 0x310, KeyDataSize: 16, KeyDataOff: 8}
	const tlsBase = 0x7f0000000000
	const key = 3
	slot := uint64(tlsBase) + uint64(tsd.Specific1stBlock) + key*uint64(tsd.KeyDataSize) + uint64(tsd.KeyDataOff)
	r := fakeReader{u64: map[uint64]uint64{slot: 0xdeadbeef}}

	got, err := hostTSSGet(r, tlsBase, tsd, key)
	if err != nil {
		t.Fatalf("hostTSSGet: %v", err)
	}
	if got != 0xdeadbeef {
		t.Fatalf("got %#x, want %#x", got, 0xdeadbeef)
	}
}

func TestHostTSSGetRefusesKeyPastFirstBlock(t *testing.T) {
	if _, err := hostTSSGet(fakeReader{}, 0, tsdOffsets{}, pyTSSKeysPerBlock); err == nil {
		t.Fatal("expected refusal for a key outside the first TSD block")
	}
}

func TestHostTSSGetSubtractsPthreadSizeBeforeIndexing(t *testing.T) {
	tsd := tsdOffsets{Specific1stBlock: 0x310, KeyDataSize: 16, KeyDataOff: 8, PthreadSize: 0x10}
	const tp = 0x7f0000001000
	adjustedBase := uint64(tp) - uint64(tsd.PthreadSize)
	slot := adjustedBase + uint64(tsd.Specific1stBlock) + uint64(tsd.KeyDataOff) // key=0
	r := fakeReader{u64: map[uint64]uint64{slot: 42}}

	got, err := hostTSSGet(r, tp, tsd, 0)
	if err != nil {
		t.Fatalf("hostTSSGet: %v", err)
	}
	if got != 42 {
		t.Fatalf("pthread_size subtraction not applied: got %#x, want 42", got)
	}
}

func TestHostTSSGetPropagatesReadFailure(t *testing.T) {
	if _, err := hostTSSGet(fakeReader{}, 0x1000, tsdOffsets{Specific1stBlock: 8}, 0); err == nil {
		t.Fatal("expected the underlying read failure to propagate")
	}
}

// --- resolveCurrentFrame ------------------------------------------------

func TestResolveCurrentFrameDirect(t *testing.T) {
	off, err := TableFor(Version{3, 13, 15})
	if err != nil {
		t.Fatal(err)
	}
	const tstate = 0x7f0000000000
	const frame = 0x7f0000001000
	r := fakeReader{u64: map[uint64]uint64{tstate + uint64(off.ThreadStateFrame): frame}}

	got, err := resolveCurrentFrame(r, off, tstate)
	if err != nil {
		t.Fatalf("resolveCurrentFrame: %v", err)
	}
	if got != frame {
		t.Fatalf("got %#x, want %#x", got, uint64(frame))
	}
}

func TestResolveCurrentFrameIndirectThroughCFrame(t *testing.T) {
	off, err := TableFor(Version{3, 12, 14})
	if err != nil {
		t.Fatal(err)
	}
	if !off.ThreadStateFrameIndirect {
		t.Fatal("sanity: 3.12 must be the indirect case")
	}
	const tstate = 0x7f0000000000
	const cframe = 0x7f0000002000
	const frame = 0x7f0000003000
	r := fakeReader{u64: map[uint64]uint64{
		tstate + uint64(off.ThreadStateFrame): cframe,
		cframe:                                frame,
	}}

	got, err := resolveCurrentFrame(r, off, tstate)
	if err != nil {
		t.Fatalf("resolveCurrentFrame: %v", err)
	}
	if got != frame {
		t.Fatalf("got %#x, want %#x", got, uint64(frame))
	}
}

func TestResolveCurrentFrameRefusesNullFrame(t *testing.T) {
	off, _ := TableFor(Version{3, 13, 15})
	const tstate = 0x7f0000000000
	r := fakeReader{u64: map[uint64]uint64{tstate + uint64(off.ThreadStateFrame): 0}}
	if _, err := resolveCurrentFrame(r, off, tstate); err == nil {
		t.Fatal("a NULL current_frame must be refused")
	}
}

func TestResolveCurrentFrameRefusesNullCFrame(t *testing.T) {
	off, _ := TableFor(Version{3, 12, 14})
	const tstate = 0x7f0000000000
	r := fakeReader{u64: map[uint64]uint64{tstate + uint64(off.ThreadStateFrame): 0}}
	if _, err := resolveCurrentFrame(r, off, tstate); err == nil {
		t.Fatal("a NULL cframe pointer must be refused")
	}
}

// --- symbolResolver -------------------------------------------------

// fakeAttachReader adds TLSBase to fakeReader so prepareInfo's TLSBaseReader
// type assertion succeeds in tests that need to get past it.
type fakeAttachReader struct {
	fakeReader
	tlsBase uint64
	tlsErr  error
}

func (f fakeAttachReader) TLSBase() (uint64, error) { return f.tlsBase, f.tlsErr }

func TestNewSymbolResolverRefusesUnmappedLibrary(t *testing.T) {
	_, err := newSymbolResolver(procmap.NewResolver(), uint32(os.Getpid()), "/nonexistent/libpython3.12.so.1.0")
	if !errors.Is(err, ErrSymbolNotFound) {
		t.Fatalf("expected ErrSymbolNotFound, got %v", err)
	}
}

// findSystemLibpython looks for a real CPython shared library on this
// machine and returns its build-real (symlink-resolved) path. Skips the
// test, rather than failing, when none is found -- the same convention
// offsets_fixture_test.go uses for podman and symbolize/local_test.go uses
// for a mapped libc: this package's correctness does not depend on the
// test host happening to have python3-devel installed.
func findSystemLibpython(t *testing.T) string {
	t.Helper()
	patterns := []string{
		"/usr/lib64/libpython3.1[234].so*",
		"/usr/lib/x86_64-linux-gnu/libpython3.1[234].so*",
		"/usr/lib/aarch64-linux-gnu/libpython3.1[234].so*",
		"/usr/lib/libpython3.1[234].so*",
	}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil || fi.IsDir() {
				continue
			}
			real, err := filepath.EvalSymlinks(m)
			if err != nil {
				continue
			}
			return real
		}
	}
	t.Skip("no system libpython3.12/3.13/3.14 found; skipping real-ELF test")
	return ""
}

// mmapPage0IntoSelf mmaps file offset 0 of path into this test process with
// PROT_READ|PROT_EXEC, so it shows up as a real, executable mapping in our
// own /proc/self/maps -- exactly the entry procmap.Resolver would report
// for a live process that dlopen'd this library. Returns the mapped page's
// runtime address, which lets the test independently predict what
// newSymbolResolver ought to compute without going through its own bias
// arithmetic.
func mmapPage0IntoSelf(t *testing.T, path string) uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	page, err := unix.Mmap(int(f.Fd()), 0, os.Getpagesize(), unix.PROT_READ|unix.PROT_EXEC, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap %s: %v", path, err)
	}
	t.Cleanup(func() { _ = unix.Munmap(page) })
	return uint64(uintptr(unsafe.Pointer(&page[0])))
}

// TestSymbolResolverRealSystemLibpython exercises newSymbolResolver end to
// end against a real CPython shared library and a real (self-inflicted)
// process mapping: no fixture, no fake bias math on either side of the
// comparison. mmapPage0IntoSelf maps file offset 0, whose covering PT_LOAD
// segment starts at Vaddr 0 and Off 0 in every real libpython build this
// package supports (confirmed against 3.12/3.13/3.14 on this machine), so
// the load bias equals the mapped page's own runtime address -- giving an
// independent way to predict the expected symbol address without
// re-deriving newSymbolResolver's own arithmetic.
func TestSymbolResolverRealSystemLibpython(t *testing.T) {
	path := findSystemLibpython(t)
	mmapAddr := mmapPage0IntoSelf(t, path)

	sr, err := newSymbolResolver(procmap.NewResolver(), uint32(os.Getpid()), path)
	if err != nil {
		t.Fatalf("newSymbolResolver(%s): %v", path, err)
	}

	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("elf.Open(%s): %v", path, err)
	}
	defer func() { _ = f.Close() }()
	syms, err := f.DynamicSymbols()
	if err != nil {
		t.Fatalf("DynamicSymbols(%s): %v", path, err)
	}
	want := map[string]uint64{}
	for _, s := range syms {
		if s.Name == "_PyRuntime" || s.Name == "_Py_NoneStruct" {
			want[s.Name] = s.Value
		}
	}
	if len(want) != 2 {
		t.Fatalf("%s: expected both _PyRuntime and _Py_NoneStruct in dynsym, found %v", path, want)
	}

	for name, val := range want {
		got, err := sr.addr(name)
		if err != nil {
			t.Fatalf("addr(%s): %v", name, err)
		}
		wantAddr := mmapAddr + val
		if got != wantAddr {
			t.Fatalf("addr(%s) = %#x, want %#x (mmap base %#x + dynsym value %#x)", name, got, wantAddr, mmapAddr, val)
		}
	}

	if _, err := sr.addr("_Py_ThisSymbolDoesNotExist"); !errors.Is(err, ErrSymbolNotFound) {
		t.Fatalf("expected ErrSymbolNotFound for a missing symbol, got %v", err)
	}
}

// --- glibcTSDOffsets --------------------------------------------------

func TestGlibcTSDOffsetsKnowsAmd64(t *testing.T) {
	off, err := glibcTSDOffsets("amd64")
	if err != nil {
		t.Fatalf("amd64 must be supported: %v", err)
	}
	if off.KeyDataSize == 0 || off.Specific1stBlock == 0 {
		t.Fatalf("suspiciously zeroed offsets: %+v", off)
	}
}

// arm64 is a deliberate, named refusal, not a guess: this package has no
// measured glibc TSD offsets for it (see glibcTSDOffsets's doc comment).
func TestGlibcTSDOffsetsRefusesUnmeasuredArch(t *testing.T) {
	if _, err := glibcTSDOffsets("arm64"); !errors.Is(err, ErrUnsupportedArch) {
		t.Fatalf("expected ErrUnsupportedArch for arm64, got %v", err)
	}
	if _, err := glibcTSDOffsets("riscv64"); !errors.Is(err, ErrUnsupportedArch) {
		t.Fatalf("expected ErrUnsupportedArch for riscv64, got %v", err)
	}
}

// --- prepareInfo / Attach --------------------------------------------

// gilstateFixture maps a supported minor version to its real,
// disassembly-derived PyGILState_GetThisThreadState body and the autoTSSkey
// offset that body encodes -- both already committed in testdata/ for T2's
// parser tests.
type gilstateFixture struct {
	minor  int
	file   string
	keyOff uint64
}

var gilstateFixtures = []gilstateFixture{
	{12, "testdata/gilstate_312.bin", 0x608},
	{13, "testdata/gilstate_313.bin", 0x870},
	{14, "testdata/gilstate_314.bin", 0x920},
}

func readGilstateCode(t *testing.T, f gilstateFixture) []byte {
	t.Helper()
	code, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatalf("read %s: %v", f.file, err)
	}
	return code
}

// fixtureForPath returns the gilstateFixture matching path's OWN detected
// CPython minor version. The wiring tests below use one real system
// library for both classification (via its soname) and dynsym resolution
// (via its real ELF), so the TSS-key fixture and its keyOff must agree with
// whichever version that library actually is -- a fixture for a different
// minor would encode the wrong autoTSSkey offset for this file. Skips if
// the discovered library's version has no matching fixture.
func fixtureForPath(t *testing.T, path string) gilstateFixture {
	t.Helper()
	v, ok := DetectFromSoname(path)
	if !ok {
		t.Skipf("%s: cannot detect a CPython version from this path", path)
	}
	for _, fx := range gilstateFixtures {
		if fx.minor == v.Minor {
			return fx
		}
	}
	t.Skipf("%s: no TSS-key fixture for CPython 3.%d", path, v.Minor)
	return gilstateFixture{}
}

// TestPrepareInfoRefusesWithoutTLSBaseReader exercises the capability gate:
// a FrameReader that cannot report a TLS base must be refused by name
// rather than silently skipped, and this must happen before any file or
// process I/O -- so a bare fakeReader with no filesystem behind it is
// enough to prove it.
func TestPrepareInfoRefusesWithoutTLSBaseReader(t *testing.T) {
	_, res := prepareInfoForArch("amd64", 1, "/usr/lib64/libpython3.12.so.1.0", nil, fakeReader{})
	if !strings.Contains(res.Refused, "TLS base") {
		t.Fatalf("expected a TLS-base refusal, got %q", res.Refused)
	}
}

// TestPrepareInfoRefusesAnUnmeasuredArch pins the gate the tests above step
// around, and pins it from EVERY host: before prepareInfoForArch existed,
// this refusal could only be observed by running the suite on the
// architecture in question, which is to say it was covered nowhere and
// discovered by a CI failure.
//
// Two claims, and the second is the one that matters. The refusal is named
// (ErrUnsupportedArch, not a bare string). And it fires BEFORE the
// TLSBaseReader check: the reader passed here cannot report a TLS base, so
// a gate that had drifted below that check would return
// ErrTLSBaseUnavailable instead -- a plausible refusal for the wrong
// reason, on a build where every later step would have been wrong.
func TestPrepareInfoRefusesAnUnmeasuredArch(t *testing.T) {
	for _, goarch := range []string{"arm64", "riscv64", "386"} {
		t.Run(goarch, func(t *testing.T) {
			_, res := prepareInfoForArch(goarch, 1, "/usr/lib64/libpython3.12.so.1.0", nil, fakeReader{})
			if !errors.Is(res.Reason, ErrUnsupportedArch) {
				t.Fatalf("Reason = %v, want errors.Is(..., ErrUnsupportedArch)", res.Reason)
			}
			if !strings.Contains(res.Refused, goarch) {
				t.Fatalf("Refused = %q, want it to name %s", res.Refused, goarch)
			}
		})
	}
	// The control: with a measured architecture the same call gets past the
	// gate and refuses for the reason the reader actually earns. Without
	// this, a gate that refused everything would pass the loop above.
	_, res := prepareInfoForArch("amd64", 1, "/usr/lib64/libpython3.12.so.1.0", nil, fakeReader{})
	if !errors.Is(res.Reason, ErrTLSBaseUnavailable) {
		t.Fatalf("amd64: Reason = %v, want the TLS-base refusal that comes after the arch gate", res.Reason)
	}
}

// TestPrepareInfoRefusesUnrecognisedTSSCode confirms the file-read step
// (ParseAutoTSSKeyOffset) is reached, and its refusal is named, before any
// process I/O -- garbage code and a bogus pid together must still produce a
// clean "cannot locate autoTSSkey" refusal, not a crash or a process-read
// error.
func TestPrepareInfoRefusesUnrecognisedTSSCode(t *testing.T) {
	garbage := []byte{0xf3, 0x0f, 0x1e, 0xfa, 0xc3}
	_, res := prepareInfoForArch("amd64", 1, "/usr/lib64/libpython3.12.so.1.0", garbage, fakeAttachReader{})
	if !strings.Contains(res.Refused, "autoTSSkey") {
		t.Fatalf("expected an autoTSSkey refusal, got %q", res.Refused)
	}
}

// TestPrepareInfoRefusesWhenPidDoesNotMapLibrary drives prepareInfo with a
// real, valid TSS-key fixture and a real pid (our own test process), but a
// libPath our test process never mapped: this exercises the real
// procmap.Resolver path (no fixture maps file) and must refuse by name
// rather than reading zero bytes from nowhere.
func TestPrepareInfoRefusesWhenPidDoesNotMapLibrary(t *testing.T) {
	path := findSystemLibpython(t)
	// Deliberately do NOT mmap path into this process -- ensure the
	// resolver has never mapped it under this exact path, unlike
	// TestSymbolResolverRealSystemLibpython. If some other test in this
	// binary already mapped it and didn't unmap it, this test would give a
	// false pass rather than a false failure, so this is intentionally its
	// own path when possible; if every discoverable path is already mapped
	// this test still exercises the case where a DIFFERENT unmapped path is
	// used, via /nonexistent below.
	unmapped := "/nonexistent-for-pyunwind-tests/" + filepath.Base(path)

	code := readGilstateCode(t, gilstateFixtures[0])
	_, res := prepareInfoForArch("amd64", uint32(os.Getpid()), unmapped, code, fakeAttachReader{})
	if !strings.Contains(res.Refused, "cannot resolve symbols") {
		t.Fatalf("expected a symbol-resolution refusal naming the unmapped library, got %q", res.Refused)
	}
}

// TestPrepareInfoInstallsAFullyValidatedRecord is the end-to-end success
// path: a real system libpython (mmapped into this test process so
// procmap.Resolver finds it, exactly as it would for a real interpreter
// process), its real PyGILState_GetThisThreadState body, and a fake
// FrameReader standing in for the target's memory -- wired together so
// every step (symbol resolution, the live autoTSSkey read, the TSD lookup,
// current_frame resolution, and Validate) succeeds for real, using real
// dynsym addresses on one side and a coordinated fake on the other.
//
// This is the test that would catch a mis-wired field in prepareInfo's
// pyProcInfo{} literal: every value written into info is checked against
// the exact Offsets/tsdOffsets it came from.
func TestPrepareInfoInstallsAFullyValidatedRecord(t *testing.T) {
	path := findSystemLibpython(t)
	fx := fixtureForPath(t, path)
	mmapAddr := mmapPage0IntoSelf(t, path)
	code := readGilstateCode(t, fx)

	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("elf.Open(%s): %v", path, err)
	}
	syms, err := f.DynamicSymbols()
	_ = f.Close()
	if err != nil {
		t.Fatalf("DynamicSymbols(%s): %v", path, err)
	}
	var pyRuntimeVal, noneVal uint64
	var haveRuntime, haveNone bool
	for _, s := range syms {
		switch s.Name {
		case "_PyRuntime":
			pyRuntimeVal, haveRuntime = s.Value, true
		case "_Py_NoneStruct":
			noneVal, haveNone = s.Value, true
		}
	}
	if !haveRuntime || !haveNone {
		t.Fatalf("%s: missing _PyRuntime/_Py_NoneStruct in dynsym", path)
	}
	pyRuntimeAddr := mmapAddr + pyRuntimeVal
	noneAddr := mmapAddr + noneVal

	// tssKey is deliberately NOT 0 or 1: 1 would be indistinguishable from
	// Py_tss_t._is_initialized if the low/high-half read were still wrong
	// (the bug a live measurement caught -- see prepareInfo's comment at
	// the autoTSSkey read), and 0 is Py_tss_NEEDS_INIT's key value, which a
	// real interpreter never has once initialised. rawKeyWord encodes the
	// real on-the-wire Py_tss_t{_is_initialized: 1, _key: tssKey} as a
	// little-endian u64: _is_initialized in the low 32 bits, _key in the
	// high 32 bits.
	const tssKey = 7
	const rawKeyWord = uint64(tssKey)<<32 | 1
	const tlsBase = 0x7f0000000000
	tsd, err := glibcTSDOffsets("amd64")
	if err != nil {
		t.Fatal(err)
	}
	slot := tlsBase + uint64(tsd.Specific1stBlock) + uint64(tssKey)*uint64(tsd.KeyDataSize) + uint64(tsd.KeyDataOff)

	off, err := TableFor(Version{3, fx.minor, 0})
	if err != nil {
		t.Fatal(err)
	}
	const tstate = 0x7f0000100000
	const frame = 0x7f0000200000

	u64 := map[uint64]uint64{
		pyRuntimeAddr + fx.keyOff:             rawKeyWord,
		slot:                                  tstate,
		tstate + uint64(off.ThreadStateFrame): frame,
		frame + uint64(off.FramePrevious):     0,
	}
	u8 := map[uint64]uint8{
		frame + uint64(off.FrameOwner): off.FrameOwnerCStack,
	}
	if off.ThreadStateFrameIndirect {
		// tstate+ThreadStateFrame holds a cframe pointer whose offset-0
		// field is the real frame pointer.
		const cframe = 0x7f0000300000
		u64[tstate+uint64(off.ThreadStateFrame)] = cframe
		u64[cframe] = frame
	}
	r := fakeAttachReader{
		fakeReader: fakeReader{u64: u64, u8: u8},
		tlsBase:    tlsBase,
	}

	info, res := prepareInfoForArch("amd64", uint32(os.Getpid()), path, code, r)
	if res.Refused != "" {
		t.Fatalf("unexpected refusal: %s", res.Refused)
	}
	if res.Reason != nil {
		t.Fatalf("Reason must be nil on success, got %v", res.Reason)
	}
	if res.Version.Minor != fx.minor {
		t.Fatalf("classified minor = %d, want %d", res.Version.Minor, fx.minor)
	}
	if info.Enabled != 1 {
		t.Fatal("Enabled must be 1 on a successful prepareInfo")
	}
	if info.TssKey != tssKey {
		t.Fatalf("TssKey = %d, want %d", info.TssKey, tssKey)
	}
	wantNone := uint64(0)
	if fx.minor >= 13 {
		wantNone = noneAddr
	}
	if info.NoneAddr != wantNone {
		t.Fatalf("NoneAddr = %#x, want %#x (minor %d)", info.NoneAddr, wantNone, fx.minor)
	}
	if info.FramePrevious != off.FramePrevious || info.FrameExecutable != off.FrameExecutable ||
		info.FrameInstrPtr != off.FrameInstrPtr || info.FrameOwner != off.FrameOwner ||
		info.ThreadstateFrame != off.ThreadStateFrame {
		t.Fatalf("frame/threadstate offsets do not match the table: %+v vs %+v", info, off)
	}
	if info.CodeArgcount != off.CodeArgCount || info.CodeKwonlyargcount != off.CodeKwOnlyArgCount ||
		info.CodeFlags != off.CodeFlags || info.CodeFirstlineno != off.CodeFirstLineNo {
		t.Fatalf("code offsets do not match the table: %+v vs %+v", info, off)
	}
	if info.FrameOwnerMax != off.FrameOwnerMax || info.FrameOwnerCstack != off.FrameOwnerCStack {
		t.Fatalf("owner enum facts do not match the table: %+v vs %+v", info, off)
	}
	if (info.FrameExecutableTagged == 1) != off.FrameExecutableTagged {
		t.Fatalf("FrameExecutableTagged = %d, want bool %v", info.FrameExecutableTagged, off.FrameExecutableTagged)
	}
	if (info.ThreadstateFrameIndirect == 1) != off.ThreadStateFrameIndirect {
		t.Fatalf("ThreadstateFrameIndirect = %d, want bool %v", info.ThreadstateFrameIndirect, off.ThreadStateFrameIndirect)
	}
	if info.PthreadSpecific1stblock != tsd.Specific1stBlock || info.PthreadKeyDataSize != tsd.KeyDataSize ||
		info.PthreadKeyDataOff != tsd.KeyDataOff || info.PthreadSize != tsd.PthreadSize {
		t.Fatalf("pthread TSD offsets do not match: %+v vs %+v", info, tsd)
	}
}

// TestPrepareInfoRefusesOnValidationFailure confirms a self-inconsistent
// frame (an owner byte outside the enum) is refused by prepareInfo with
// the "offset validation failed" reason, using the same wiring as the
// success test above but with one corrupted value.
func TestPrepareInfoRefusesOnValidationFailure(t *testing.T) {
	path := findSystemLibpython(t)
	fx := fixtureForPath(t, path)
	mmapAddr := mmapPage0IntoSelf(t, path)
	code := readGilstateCode(t, fx)

	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("elf.Open(%s): %v", path, err)
	}
	syms, err := f.DynamicSymbols()
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var pyRuntimeVal uint64
	for _, s := range syms {
		if s.Name == "_PyRuntime" {
			pyRuntimeVal = s.Value
		}
	}
	pyRuntimeAddr := mmapAddr + pyRuntimeVal

	const tssKey = 7
	const rawKeyWord = uint64(tssKey)<<32 | 1
	const tlsBase = 0x7f0000000000
	tsd, _ := glibcTSDOffsets("amd64")
	slot := tlsBase + uint64(tsd.Specific1stBlock) + uint64(tssKey)*uint64(tsd.KeyDataSize) + uint64(tsd.KeyDataOff)

	off, err := TableFor(Version{3, fx.minor, 0})
	if err != nil {
		t.Fatal(err)
	}
	const tstate = 0x7f0000100000
	const cframe = 0x7f0000300000
	const frame = 0x7f0000200000

	u64 := map[uint64]uint64{
		pyRuntimeAddr + fx.keyOff:         rawKeyWord,
		slot:                              tstate,
		frame + uint64(off.FramePrevious): 0,
	}
	if off.ThreadStateFrameIndirect {
		u64[tstate+uint64(off.ThreadStateFrame)] = cframe
		u64[cframe] = frame
	} else {
		u64[tstate+uint64(off.ThreadStateFrame)] = frame
	}
	r := fakeAttachReader{
		fakeReader: fakeReader{
			u64: u64,
			u8: map[uint64]uint8{
				frame + uint64(off.FrameOwner): 0x5a, // not a valid _frameowner value
			},
		},
		tlsBase: tlsBase,
	}

	_, res := prepareInfoForArch("amd64", uint32(os.Getpid()), path, code, r)
	if !strings.Contains(res.Refused, "offset validation failed") {
		t.Fatalf("expected an offset-validation refusal, got %q", res.Refused)
	}
	if !errors.Is(res.Reason, ErrOffsetsImplausible) {
		t.Fatalf("Reason = %v, want errors.Is(..., ErrOffsetsImplausible)", res.Reason)
	}
}

// --- pyProcInfo wire-order check ---------------------------------------

// fieldSpec is one field of a parsed Go struct: its name and its width in
// bytes on the wire.
type fieldSpec struct {
	name string
	size int
}

// arrayTypeRe matches a fixed-size array type expression like "[5]uint8".
var arrayTypeRe = regexp.MustCompile(`^\[(\d+)\](\w+)$`)

// primitiveSize returns the byte width of a Go primitive integer type or a
// fixed-size array of one, or (0, false) for anything else (notably
// structs.HostLayout, bpf2go's zero-size compile-time layout marker).
func primitiveSize(typeStr string) (int, bool) {
	switch typeStr {
	case "uint8", "int8":
		return 1, true
	case "uint16", "int16":
		return 2, true
	case "uint32", "int32":
		return 4, true
	case "uint64", "int64":
		return 8, true
	}
	if m := arrayTypeRe.FindStringSubmatch(typeStr); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		elemSize, ok := primitiveSize(m[2])
		if !ok {
			return 0, false
		}
		return n * elemSize, true
	}
	return 0, false
}

// exprString renders the small subset of Go type expressions this parser
// needs back into source text: bare identifiers (uint64), fixed-size
// arrays ([5]uint8), and qualified identifiers (structs.HostLayout).
func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.ArrayType:
		n := ""
		if lit, ok := t.Len.(*ast.BasicLit); ok {
			n = lit.Value
		}
		return "[" + n + "]" + exprString(t.Elt)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	default:
		return ""
	}
}

// generatedStructFields parses typeName out of the Go source file at path
// and returns its fields (name, byte width) in DECLARED order -- the order
// that actually matters, since cilium/ebpf marshals a map value as this
// struct's raw backing memory. Zero-width fields (structs.HostLayout, the
// bpf2go compile-time layout-lock marker) are dropped since they occupy no
// bytes and were never candidates for the reorder bug this test exists to
// catch.
//
// This reads the CURRENT, checked-in bpf2go output from disk -- not a
// hand-copied snapshot of it -- so a future regen that reorders
// python_walk.h's fields (accidentally or not) changes what this function
// returns and fails the comparison in TestPyProcInfoFieldOrderMatchesGenerated,
// the same day it happens.
func generatedStructFields(t *testing.T, path, typeName string) []fieldSpec {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []fieldSpec
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		found = true
		for _, field := range st.Fields.List {
			size, known := primitiveSize(exprString(field.Type))
			if !known {
				continue // e.g. the structs.HostLayout marker: zero bytes, not a real field
			}
			names := field.Names
			if len(names) == 0 {
				names = []*ast.Ident{{Name: "_"}}
			}
			for _, id := range names {
				out = append(out, fieldSpec{name: id.Name, size: size})
			}
		}
		return false
	})
	if !found {
		t.Fatalf("%s: type %s not found", path, typeName)
	}
	return out
}

// TestPyProcInfoFieldOrderMatchesGenerated is the order-aware check
// TestPyProcInfoSizeMirrorsC cannot be: cilium/ebpf marshals pyProcInfo as
// raw backing memory in Go DECLARATION order (sysenc.Marshal ->
// unsafeBackingMemory), so swapping two same-width fields keeps
// unsafe.Sizeof() at 56 and keeps bpf/python_walk.h's _Static_assert
// happy while silently swapping two byte offsets in the map. This compares
// pyProcInfo's actual reflect-derived (name, offset) sequence against the
// real, currently-checked-in bpf2go-generated gpuusdtPyProcInfo -- name and
// offset, in order -- not against a hand-maintained expectation that could
// itself drift out of sync with a real regen.
//
// The x86 generated file is read on every host, arm64 included, and that is
// correct rather than an oversight: both generated files are checked in,
// and bpf2go emits a byte-identical gpuusdtPyProcInfo declaration for both
// architectures (the struct has no pointer-width members). Reading one of
// them is reading the mirror; reading the host's would only make the test
// harder to reason about.
func TestPyProcInfoFieldOrderMatchesGenerated(t *testing.T) {
	genPath := filepath.Join("..", "gpuprobe", "gpuusdt_x86_bpfel.go")
	if _, err := os.Stat(genPath); err != nil {
		t.Skipf("generated file not found: %v", err)
	}
	want := generatedStructFields(t, genPath, "gpuusdtPyProcInfo")

	rt := reflect.TypeOf(pyProcInfo{})
	if rt.NumField() != len(want) {
		t.Fatalf("pyProcInfo has %d fields, %s has %d:\npyProcInfo: %+v\ngenerated:  %+v",
			rt.NumField(), genPath, len(want), rt, want)
	}

	offset := uintptr(0)
	for i, w := range want {
		got := rt.Field(i)
		if got.Offset != offset {
			t.Fatalf("field %d (%s): pyProcInfo offset %d, want %d (from %s's declared order) -- "+
				"a field has been reordered relative to the generated struct",
				i, got.Name, got.Offset, offset, genPath)
		}
		// bpf2go names its zero-value-name padding field "Pad"; the Go
		// struct literal in pyProcInfo spells the same slot as the blank
		// identifier "_". Both are "no real name, just padding" -- compare
		// names everywhere else, but not there.
		if got.Name != "_" && w.name != "_" && got.Name != w.name {
			t.Fatalf("field %d: pyProcInfo name %q, generated struct name %q -- order or naming has drifted",
				i, got.Name, w.name)
		}
		offset += uintptr(w.size)
	}
	if offset != pyProcInfoSize {
		t.Fatalf("%s's fields sum to %d bytes, pyProcInfo is %d", genPath, offset, pyProcInfoSize)
	}
}
