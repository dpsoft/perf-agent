// pyunwind/attach_test.go
package pyunwind

import (
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// A refusal must name its reason. An operator whose Python frames are
// missing needs to distinguish "we found 3.11 and declined" from "we could
// not read the interpreter" from "this is not a Python process" from "this
// is a free-threaded build we don't support".
func TestAttachRefusalsAreNamed(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		expect string
	}{
		{"unsupported version", "/usr/lib/libpython3.11.so.1.0", "unsupported"},
		{"not python", "/usr/bin/nginx", "not an interpreter"},
		{"free-threaded 3.13", "/usr/lib/libpython3.13t.so.1.0", "free-threaded"},
		{"free-threaded 3.14", "/usr/lib/x86_64-linux-gnu/libpython3.14t.so.1.0", "free-threaded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classify(tc.path)
			if !strings.Contains(res.Refused, tc.expect) {
				t.Fatalf("reason %q does not mention %q", res.Refused, tc.expect)
			}
		})
	}
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
	_, res := prepareInfo(1, "/usr/lib64/libpython3.12.so.1.0", nil, fakeReader{})
	if !strings.Contains(res.Refused, "TLS base") {
		t.Fatalf("expected a TLS-base refusal, got %q", res.Refused)
	}
}

// TestPrepareInfoRefusesUnrecognisedTSSCode confirms the file-read step
// (ParseAutoTSSKeyOffset) is reached, and its refusal is named, before any
// process I/O -- garbage code and a bogus pid together must still produce a
// clean "cannot locate autoTSSkey" refusal, not a crash or a process-read
// error.
func TestPrepareInfoRefusesUnrecognisedTSSCode(t *testing.T) {
	garbage := []byte{0xf3, 0x0f, 0x1e, 0xfa, 0xc3}
	_, res := prepareInfo(1, "/usr/lib64/libpython3.12.so.1.0", garbage, fakeAttachReader{})
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
	_, res := prepareInfo(uint32(os.Getpid()), unmapped, code, fakeAttachReader{})
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

	const tssKey = 0
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
		pyRuntimeAddr + fx.keyOff:             tssKey,
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

	info, res := prepareInfo(uint32(os.Getpid()), path, code, r)
	if res.Refused != "" {
		t.Fatalf("unexpected refusal: %s", res.Refused)
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

	const tssKey = 0
	const tlsBase = 0x7f0000000000
	tsd, _ := glibcTSDOffsets("amd64")
	slot := tlsBase + uint64(tsd.Specific1stBlock) + uint64(tsd.KeyDataOff)

	off, err := TableFor(Version{3, fx.minor, 0})
	if err != nil {
		t.Fatal(err)
	}
	const tstate = 0x7f0000100000
	const cframe = 0x7f0000300000
	const frame = 0x7f0000200000

	u64 := map[uint64]uint64{
		pyRuntimeAddr + fx.keyOff:         tssKey,
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

	_, res := prepareInfo(uint32(os.Getpid()), path, code, r)
	if !strings.Contains(res.Refused, "offset validation failed") {
		t.Fatalf("expected an offset-validation refusal, got %q", res.Refused)
	}
}
