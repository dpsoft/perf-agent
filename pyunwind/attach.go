// pyunwind/attach.go
package pyunwind

import (
	"debug/elf"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"unsafe"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// ErrNotAnInterpreter means the mapped path carries no recognisable CPython
// version at all. This is deliberately distinct from ErrUnsupportedVersion:
// an operator missing Python frames needs to know whether this process is
// not (visibly) Python at all, or whether it plainly is Python but a
// version this build declines to walk.
var ErrNotAnInterpreter = errors.New("pyunwind: not an interpreter")

// ErrFreeThreaded means the soname names a free-threaded (PEP 703,
// Py_GIL_DISABLED) build -- "libpython3.13t.so", "libpython3.14t.so", and
// so on. Every offset this package carries was measured against the
// ordinary GIL build: on a free-threaded build PyObject is 32 bytes, not
// 16, `owner` moves from 74 to 78 on 3.14t, and instr_ptr points into
// thread-local bytecode rather than the shared kind these offsets assume.
// Walking such a process with GIL-build offsets does not fail -- it
// produces a plausible stack of wrong frames, exactly the failure mode
// this whole package exists to avoid. See spec ruling T6-R1.
var ErrFreeThreaded = errors.New("pyunwind: free-threaded (Py_GIL_DISABLED) CPython build is not supported")

// ErrSymbolNotFound means a dynamic symbol Attach needed was not present
// in the target's dynsym, or the target process does not map libPath at
// all. Currently used for _PyRuntime (needed on every supported version,
// to read the live autoTSSkey value) and _Py_NoneStruct (needed from 3.13
// onward, to recognise the frame the walker must stop at).
var ErrSymbolNotFound = errors.New("pyunwind: required symbol not found")

// ErrUnsupportedArch means Attach has no measured glibc pthread-TSD struct
// offsets for runtime.GOARCH. py_tss_get's host-side replica (see
// hostTSSGet) cannot be run without them, and this package refuses to
// guess at glibc's internal layout the way it refuses to guess at a wrong
// CPython offset: a wrong guess here does not fail loudly, it walks a
// pointer through the wrong byte of the pthread struct and comes back
// with a plausible-looking, wrong PyThreadState*.
var ErrUnsupportedArch = errors.New("pyunwind: no measured glibc TSD offsets for this architecture")

// ErrTLSBaseUnavailable means the FrameReader passed to Attach does not
// also implement TLSBaseReader, so there is no way to learn the target
// thread's TLS base -- the one input py_tss_get's host-side replica needs
// that is not itself a memory address.
var ErrTLSBaseUnavailable = errors.New("pyunwind: FrameReader cannot report the target's TLS base")

// freeThreadedSonameRe matches the free-threaded build's soname suffix: a
// lone "t" immediately after the minor version, before the next
// non-word character ("libpython3.13t.so.1.0", "python3.14t"). Anchored
// with \b so an unrelated "t" elsewhere in the path cannot trigger it.
var freeThreadedSonameRe = regexp.MustCompile(`(?:libpython|python)\d+\.\d+t\b`)

// Result reports what Attach decided for a process.
type Result struct {
	Version Version
	// Refused is empty on success and carries an operator-readable reason
	// otherwise -- for logs and humans.
	Refused string
	// Reason is nil on success and, otherwise, one of this package's nine
	// sentinel errors (possibly wrapped with extra context via %w):
	// ErrNotAnInterpreter, ErrFreeThreaded, ErrUnsupportedVersion,
	// ErrUnsupportedArch, ErrTLSBaseUnavailable, ErrTSSPatternUnrecognised,
	// ErrSymbolNotFound, ErrOffsetsUnreadable, ErrOffsetsImplausible.
	// Callers -- Task 7's counters in particular -- must use errors.Is
	// against Reason, not strings.Contains/parsing against Refused: Refused
	// is prose for a human, Reason is the machine-checkable classification.
	Reason error
}

// refuseWith builds a Result from a single error so Refused and Reason can
// never drift apart: Refused is err's message, Reason is err itself (still
// unwrappable via errors.Is/errors.As to whichever sentinel it wraps).
func refuseWith(v Version, err error) Result {
	return Result{Version: v, Refused: err.Error(), Reason: err}
}

// classify decides from a mapped path alone, before any target memory is
// read. Split out so the decision is testable without a live process.
//
// Order: free-threaded is checked before Supported(), because a
// free-threaded 3.13 or 3.14 build numerically passes Supported() -- it is
// a version this build otherwise walks, just not in this ABI. Reporting it
// as "unsupported version" would send an operator looking for a version
// problem that isn't there.
func classify(path string) Result {
	v, ok := DetectFromSoname(path)
	if !ok {
		return refuseWith(v, fmt.Errorf("%w: no CPython version in the mapped path %q", ErrNotAnInterpreter, path))
	}
	if freeThreadedSonameRe.MatchString(path) {
		return refuseWith(v, fmt.Errorf(
			"%w: %q is a free-threaded build; this build's offsets assume the GIL build and are wrong for it", ErrFreeThreaded, path))
	}
	if !v.Supported() {
		return refuseWith(v, fmt.Errorf(
			"%w: CPython %s; this build walks 3.12 through 3.14 only", ErrUnsupportedVersion, v))
	}
	return Result{Version: v}
}

// pyProcInfo mirrors bpf/python_walk.h's struct py_proc_info. Two DIFFERENT
// mappings are in play here and they have opposite rules:
//
//  1. Offsets (pyunwind/offsets.go) <-> pyProcInfo, in prepareInfo below,
//     is genuinely by NAME: it is one Go struct literal assigning named
//     fields to named fields, so the two types' field ORDER is free to
//     differ (and does: pyProcInfo is regrouped by width, Offsets is not).
//     Field names follow bpf2go's plain per-underscore-segment
//     title-casing of the C name (e.g. CodeArgcount, ThreadstateFrame,
//     FrameOwnerCstack) rather than Offsets's camelCase (CodeArgCount,
//     ThreadStateFrame, FrameOwnerCStack) -- 6 of 13 shared fields differ
//     in capitalisation, and a mechanical name-based mapping between the
//     two types would silently mismatch on exactly those six, which is
//     why prepareInfo assigns every field explicitly instead.
//
//  2. pyProcInfo <-> struct py_proc_info, on the wire, is the OPPOSITE:
//     cilium/ebpf marshals a map value as this struct's raw backing
//     memory (sysenc.Marshal -> unsafeBackingMemory), so THIS struct's Go
//     DECLARATION ORDER *is* the byte layout the kernel reads. Reordering
//     two same-width fields here keeps unsafe.Sizeof() at 56 and keeps
//     the C side's _Static_assert happy -- neither notices -- while
//     silently swapping two byte offsets in the map. Do not reorder these
//     fields for readability or to match Offsets. Order must instead
//     match python_walk.h's declared order (name is checked too, but
//     order is what actually matters here).
//
// TestPyProcInfoSizeMirrorsC pins the size; TestPyProcInfoFieldOrderMatchesGenerated
// pins the order and names against the real bpf2go-generated struct, since
// a size check alone cannot see a reorder. Keep all three edits (this
// struct, the C struct, and updating the two tests if a field count
// changes) in the same commit.
type pyProcInfo struct {
	// CPython 3.13+ sentinel. Own block ahead of the u32 block: it needs
	// 8-byte alignment, so putting it first (widest field, front-loaded)
	// keeps every later field's natural alignment gap-free, exactly the
	// "regroup by width, widest first" rule the C side documents.
	NoneAddr uint64

	TssKey                  uint32
	PthreadSpecific1stblock uint32
	PthreadKeyDataSize      uint32
	PthreadKeyDataOff       uint32
	PthreadSize             uint32

	FramePrevious    uint16
	FrameExecutable  uint16
	FrameInstrPtr    uint16
	FrameOwner       uint16
	ThreadstateFrame uint16

	CodeArgcount       uint16
	CodeKwonlyargcount uint16
	CodeFlags          uint16
	CodeFirstlineno    uint16

	FrameOwnerMax            uint8
	FrameOwnerCstack         uint8
	FrameOwnerBoundary       uint8
	FrameExecutableTagged    uint8
	ThreadstateFrameIndirect uint8

	Enabled uint8
	_       [4]byte // mirrors the C side's explicit _pad[4]; keeps sizeof() at 56.
}

// BPFMaps is the set of map handles Attach needs. PyProcs is
// bpf/python_walk.h's py_procs: a BPF_MAP_TYPE_HASH keyed by pid, holding
// one pyProcInfo.
type BPFMaps struct {
	PyProcs *ebpf.Map
}

// EvalRange is one interpreter's eval-loop text range, in the same space
// bpf/unwind_common.h's mapping_for_pc reports rel_pc in: RELATIVE to the
// mapping's load bias, so one entry serves every process running that
// libpython. Mirrors struct py_eval_range in bpf/python_walk.h; Hi is
// exclusive.
type EvalRange struct {
	Lo uint64
	Hi uint64
}

// InstallEvalRange publishes one libpython's eval-loop range under its
// table_id -- the same FNV-1a-of-build-id key the CFI tables use, which is
// what walk_step already holds by the time it reaches the Python arm.
//
// Until a range is installed for a binary, walk_step's interpreter arm never
// fires for any PC in it: the range map is the on-switch, and py_procs alone
// is not enough.
func InstallEvalRange(m *ebpf.Map, tableID uint64, r EvalRange) error {
	if r.Hi <= r.Lo {
		return fmt.Errorf("pyunwind: eval range [%#x,%#x) for table %#x is empty or inverted", r.Lo, r.Hi, tableID)
	}
	if err := m.Update(&tableID, &r, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("pyunwind: install eval range for table %#x: %w", tableID, err)
	}
	return nil
}

// TLSBaseReader is implemented by a FrameReader that can also report the
// current thread's TLS base -- task_struct.thread.fsbase on x86-64,
// thread.uw.tp_value on arm64 -- the one input hostTSSGet needs that is a
// register, not a memory address, so FrameReader.ReadU64/ReadU8 cannot
// supply it. Attach type-asserts for this rather than adding it to
// FrameReader itself, which pyunwind/offsets.go defines and this package
// is told to consume, not redefine.
//
// A FrameReader that does not implement it makes Attach refuse by name
// (ErrTLSBaseUnavailable) rather than silently skipping validation.
type TLSBaseReader interface {
	TLSBase() (uint64, error)
}

// tsdOffsets is the glibc pthread TSD struct layout hostTSSGet needs to
// reimplement pthread_getspecific -- the same four numbers
// bpf/python_walk.h's py_tss_get takes from py_proc_info, since both walk
// the identical glibc struct.
type tsdOffsets struct {
	Specific1stBlock uint32
	KeyDataSize      uint32
	KeyDataOff       uint32
	PthreadSize      uint32 // arm64 only; 0 on amd64.
}

// glibcTSDOffsets returns the measured glibc pthread TSD offsets for
// goarch, or ErrUnsupportedArch.
//
// amd64 values were measured directly on this machine (glibc 2.43,
// Fedora, kernel 6.19) with a small probe: pthread_key_create a real key,
// pthread_setspecific a unique marker value, then scan memory from
// pthread_self() for that marker. It landed at offset 792 (0x318) for
// key 0, and a second probe with three keys confirmed a 16-byte stride
// between consecutive keys' data (792, 808, 824) -- i.e.
// struct pthread_key_data { uintptr_t seq; void *data; } is 16 bytes with
// `data` at offset 8, and specific_1stblock (where key 0's struct starts)
// is 792 - 8 = 784 = 0x310. That value has also long been publicly
// documented for glibc's `struct pthread` on x86-64 and has not moved in
// years, but it is recorded here as directly measured, not copied.
//
// arm64 is deliberately NOT included: this package has no equivalent
// measurement for it, and the design's own "Open risks" section already
// flags glibc struct offsets as carried per-libc-version and untested
// beyond the reference implementation this design took the mechanism
// from. Guessing a value here would be exactly the silent-wrong-offset
// failure mode this whole package exists to refuse. musl is out of scope
// for the same reason and is not attempted either.
func glibcTSDOffsets(goarch string) (tsdOffsets, error) {
	switch goarch {
	case "amd64":
		return tsdOffsets{
			Specific1stBlock: 0x310,
			KeyDataSize:      16,
			KeyDataOff:       8,
			PthreadSize:      0,
		}, nil
	default:
		return tsdOffsets{}, fmt.Errorf("%w: %s", ErrUnsupportedArch, goarch)
	}
}

// pyTSSKeysPerBlock mirrors PY_TSS_KEYS_PER_BLOCK in bpf/python_walk.h.
// Only the first TSD block is supported, on both sides, for the same
// reason: CPython's autoTSSkey is in practice 0 or a small integer, and a
// key past the first block would need the second-level array walk that
// neither this host-side replica nor py_tss_get implements.
const pyTSSKeysPerBlock = 32

// hostTSSGet is a host-side replica of bpf/python_walk.h's py_tss_get: it
// reimplements glibc's pthread_getspecific against a TSS key, entirely
// through r, to find the value CPython stored there -- for the current
// thread, its PyThreadState*. Returns an error, never a garbage pointer,
// on any failure to read or an out-of-range key.
//
// Kept independent of the BPF version rather than calling into it (there
// is nothing to call into from Go): the two must be kept in step by hand,
// which is a reasonable trade against needing a live BPF program just to
// validate one frame at attach time.
func hostTSSGet(r FrameReader, tlsBase uint64, off tsdOffsets, key uint32) (uint64, error) {
	if key >= pyTSSKeysPerBlock {
		return 0, fmt.Errorf("pyunwind: TSS key %d is outside the first TSD block (max %d)", key, pyTSSKeysPerBlock-1)
	}
	if off.PthreadSize != 0 {
		tlsBase -= uint64(off.PthreadSize)
	}
	slot := tlsBase + uint64(off.Specific1stBlock) +
		uint64(key)*uint64(off.KeyDataSize) + uint64(off.KeyDataOff)
	val, err := r.ReadU64(slot)
	if err != nil {
		return 0, fmt.Errorf("pyunwind: read TSS slot for key %d: %w", key, err)
	}
	return val, nil
}

// resolveCurrentFrame reads the current _PyInterpreterFrame pointer out of
// tstate, following Offsets.ThreadStateFrame -- and, on 3.12, the extra
// `cframe` indirection ThreadStateFrameIndirect documents (see offsets.go).
func resolveCurrentFrame(r FrameReader, off Offsets, tstate uint64) (uint64, error) {
	ptr, err := r.ReadU64(tstate + uint64(off.ThreadStateFrame))
	if err != nil {
		return 0, fmt.Errorf("pyunwind: read PyThreadState frame field: %w", err)
	}
	if off.ThreadStateFrameIndirect {
		if ptr == 0 {
			return 0, errors.New("pyunwind: cframe pointer is NULL")
		}
		ptr, err = r.ReadU64(ptr)
		if err != nil {
			return 0, fmt.Errorf("pyunwind: read current_frame through cframe: %w", err)
		}
	}
	if ptr == 0 {
		return 0, errors.New("pyunwind: current_frame is NULL")
	}
	return ptr, nil
}

// symbolResolver resolves dynamic-symbol addresses in one target process's
// mapping of one ELF file, opening that ELF and reading /proc/<pid>/maps
// only once regardless of how many symbols are looked up through it.
type symbolResolver struct {
	bias uint64
	syms []elf.Symbol
	path string
}

// newSymbolResolver builds a resolver for libPath as pid maps it.
//
// The load bias is computed the robust way -- from the actual PT_LOAD
// program header covering the mapping's file offset, not by assuming
// p_vaddr == p_offset for that segment (a common convention, not an ELF
// guarantee; the ELF spec only requires them to agree modulo the page
// size). This is the same derivation symbolize/local_test.go's
// mappedLibcFuncAddr already uses and has been checked against a real
// process's real libc.
func newSymbolResolver(mr *procmap.Resolver, pid uint32, libPath string) (*symbolResolver, error) {
	mappings, err := mr.Mappings(pid)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: read pid %d mappings: %w", pid, err)
	}
	var mapping *procmap.Mapping
	for i := range mappings {
		if mappings[i].Path == libPath {
			mapping = &mappings[i]
			break
		}
	}
	if mapping == nil {
		return nil, fmt.Errorf("%w: pid %d does not map %s", ErrSymbolNotFound, pid, libPath)
	}

	f, err := elf.Open(libPath)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: open %s: %w", libPath, err)
	}
	defer func() { _ = f.Close() }()

	var bias uint64
	found := false
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if mapping.Offset < p.Off || mapping.Offset >= p.Off+p.Filesz {
			continue
		}
		bias = mapping.Start - (p.Vaddr + (mapping.Offset - p.Off))
		found = true
		break
	}
	if !found {
		return nil, fmt.Errorf("pyunwind: %s: no PT_LOAD segment covers file offset %#x", libPath, mapping.Offset)
	}

	syms, err := f.DynamicSymbols()
	if err != nil {
		return nil, fmt.Errorf("pyunwind: dynamic symbols of %s: %w", libPath, err)
	}
	return &symbolResolver{bias: bias, syms: syms, path: libPath}, nil
}

// addr returns the absolute runtime address of a dynamic symbol.
func (s *symbolResolver) addr(name string) (uint64, error) {
	for _, sym := range s.syms {
		if sym.Name == name {
			return s.bias + sym.Value, nil
		}
	}
	return 0, fmt.Errorf("%w: %s in %s", ErrSymbolNotFound, name, s.path)
}

// Attach discovers a process's interpreter, validates the offsets against
// it, and installs py_procs. Every failure path returns a named
// Result.Refused/Reason; only a failure Attach itself cannot recover
// from -- the final map write -- returns a non-nil error. Neither path
// ever leaves a half-installed, walkable entry: see the Enabled comment on
// prepareInfo.
//
// The discovery/validation work is factored into prepareInfo so it can be
// unit-tested up to (but not through) the real kernel map write, which
// needs a live *ebpf.Map and so belongs to the root-gated integration
// suite, not here.
func Attach(pid uint32, libPath string, code []byte, m *BPFMaps, r FrameReader) (Result, error) {
	info, res := prepareInfo(pid, libPath, code, r)
	if res.Refused != "" {
		return res, nil
	}
	if err := m.PyProcs.Update(&pid, &info, ebpf.UpdateAny); err != nil {
		return res, fmt.Errorf("pyunwind: install py_procs: %w", err)
	}
	return res, nil
}

// prepareInfo does everything Attach needs before the map write: classify,
// look up the offset table, resolve the target's dynamic symbols, read its
// live TSS key, replay the pthread TSD lookup to find a live frame, and
// validate that frame. It never touches m, so it is testable without a
// live BPF map.
//
// Order: the cheapest, most decisive checks run first. classify from the
// path (no I/O at all), then the offset table and the glibc TSD layout for
// this GOARCH (no I/O), then the TLSBaseReader capability check (a type
// assertion), then file reads (the TSS key offset out of code, and dynamic
// symbols out of the target's own ELF, biased through its live mappings),
// and only last the reads that touch the target's own memory: the live
// autoTSSkey value, the TSS lookup itself, and Validate.
func prepareInfo(pid uint32, libPath string, code []byte, r FrameReader) (pyProcInfo, Result) {
	res := classify(libPath)
	if res.Refused != "" {
		return pyProcInfo{}, res
	}

	off, err := TableFor(res.Version)
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, err)
	}

	tsd, err := glibcTSDOffsets(runtime.GOARCH)
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, err)
	}

	tlsReader, ok := r.(TLSBaseReader)
	if !ok {
		return pyProcInfo{}, refuseWith(res.Version, ErrTLSBaseUnavailable)
	}

	keyOff, err := ParseAutoTSSKeyOffset(code)
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("cannot locate autoTSSkey: %w", err))
	}

	resolver, err := newSymbolResolver(procmap.NewResolver(), pid, libPath)
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("cannot resolve symbols in %s: %w", libPath, err))
	}

	pyRuntimeAddr, err := resolver.addr("_PyRuntime")
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("cannot locate _PyRuntime: %w", err))
	}

	var noneAddr uint64
	if res.Version.Minor >= 13 {
		noneAddr, err = resolver.addr("_Py_NoneStruct")
		if err != nil {
			return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("cannot locate _Py_NoneStruct: %w", err))
		}
	}

	// _PyRuntime.autoTSSkey is a Py_tss_t: `{ int _is_initialized;
	// pthread_key_t _key; }`. ParseAutoTSSKeyOffset's doc explains why
	// keyOff is the offset of that STRUCT, not of _key directly: the
	// parser requires the cmpl's and lea's offsets to agree, and both
	// instructions address the struct base (cmpl tests _is_initialized at
	// +0; lea computes &_key at +0 to pass to PyThread_tss_get, which
	// itself is `pthread_key_t *`-typed and reads _key). On a
	// little-endian target the raw 8-byte read below therefore packs
	// _is_initialized into the LOW 32 bits and _key into the HIGH 32
	// bits: reading uint32(rawKey) yields the init flag, not the key. A
	// live measurement caught this: on this reviewer's process,
	// autoTSSkey read as 0x0000000100000001 (is_initialized=1, key=1) --
	// key and flag coincidentally equal 1, which is exactly why a
	// same-value bug like this survives on one host and corrupts frames
	// on any host where CPython's key is 0, 2, 3, ...
	rawKey, err := r.ReadU64(pyRuntimeAddr + uint64(keyOff))
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("%w: read autoTSSkey value: %v", ErrOffsetsUnreadable, err))
	}
	tssKey := uint32(rawKey >> 32)

	tlsBase, err := tlsReader.TLSBase()
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("%w: read target TLS base: %v", ErrOffsetsUnreadable, err))
	}

	tstate, err := hostTSSGet(r, tlsBase, tsd, tssKey)
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("%w: current thread's PyThreadState via TSS key: %v", ErrOffsetsUnreadable, err))
	}
	if tstate == 0 {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("%w: TSS key %d holds no PyThreadState for the current thread", ErrOffsetsUnreadable, tssKey))
	}

	frame, err := resolveCurrentFrame(r, off, tstate)
	if err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("%w: %v", ErrOffsetsUnreadable, err))
	}

	if err := off.Validate(r, frame); err != nil {
		return pyProcInfo{}, refuseWith(res.Version, fmt.Errorf("offset validation failed: %w", err))
	}

	info := pyProcInfo{
		NoneAddr: noneAddr,

		TssKey:                  tssKey,
		PthreadSpecific1stblock: tsd.Specific1stBlock,
		PthreadKeyDataSize:      tsd.KeyDataSize,
		PthreadKeyDataOff:       tsd.KeyDataOff,
		PthreadSize:             tsd.PthreadSize,

		FramePrevious:    off.FramePrevious,
		FrameExecutable:  off.FrameExecutable,
		FrameInstrPtr:    off.FrameInstrPtr,
		FrameOwner:       off.FrameOwner,
		ThreadstateFrame: off.ThreadStateFrame,

		CodeArgcount:       off.CodeArgCount,
		CodeKwonlyargcount: off.CodeKwOnlyArgCount,
		CodeFlags:          off.CodeFlags,
		CodeFirstlineno:    off.CodeFirstLineNo,

		FrameOwnerMax:            off.FrameOwnerMax,
		FrameOwnerCstack:         off.FrameOwnerCStack,
		FrameOwnerBoundary:       off.FrameOwnerBoundary,
		FrameExecutableTagged:    boolToU8(off.FrameExecutableTagged),
		ThreadstateFrameIndirect: boolToU8(off.ThreadStateFrameIndirect),
	}

	// Enabled is set only here, after Validate has already returned nil
	// above, and it is the last field touched before prepareInfo returns --
	// so a process that fails any earlier step never gets an info with
	// Enabled set at all, and Attach only ever writes an info that reached
	// this line, fully populated and validated. There is no window in which
	// a reader of py_procs can observe Enabled=1 next to zeroed offsets.
	info.Enabled = 1

	return info, res
}

func boolToU8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// pyProcInfoSize is unsafe.Sizeof(pyProcInfo{}), pulled into a named
// constant so the size-pinning test's failure message can show both sides
// without repeating the unsafe call.
const pyProcInfoSize = unsafe.Sizeof(pyProcInfo{})
