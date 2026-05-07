package debuginfod

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "blazesym.h"

extern char* goDispatchCb(char* maps_file, char* symbolic_path, void* ctx);

// dispatch_thunk adapts cgo's goDispatchCb (char*, char*, void*) signature
// to blazesym's blaze_symbolizer_dispatch.dispatch_cb signature
// (const char*, const char*, void*). The conversion is safe: blazesym
// owns the input strings, the Go side only reads them via C.GoString,
// and the Go callback never mutates the buffers.
static char* dispatch_thunk(const char* maps_file,
                            const char* symbolic_path,
                            void* ctx) {
    return goDispatchCb((char*)maps_file, (char*)symbolic_path, ctx);
}

// alloc_dispatch allocates a blaze_symbolizer_dispatch in C-managed memory
// (matched with libc free in free_dispatch). Storing the dispatch struct
// in C memory keeps `blaze_symbolizer_opts` free of Go pointers, satisfying
// cgo's pointer-passing rules: opts itself may live on the Go stack, but
// its fields (including process_dispatch) point only into C memory.
//
// The handle is taken as uintptr_t so the Go caller can avoid the
// unsafe.Pointer<->uintptr round-trip that go vet (rightly) flags; the
// cast to void* happens in C, where the rule does not apply.
static blaze_symbolizer_dispatch* alloc_dispatch(uintptr_t handle) {
    blaze_symbolizer_dispatch* d =
        (blaze_symbolizer_dispatch*)malloc(sizeof(*d));
    if (d == NULL) {
        return NULL;
    }
    d->dispatch_cb = dispatch_thunk;
    d->ctx = (void*)handle;
    return d;
}

static void free_dispatch(blaze_symbolizer_dispatch* d) {
    if (d != NULL) {
        free(d);
    }
}

static blaze_symbolizer* new_symbolizer(blaze_symbolizer_dispatch* dispatch,
                                        _Bool code_info,
                                        _Bool inlined_fns,
                                        _Bool demangle) {
    blaze_symbolizer_opts opts;
    memset(&opts, 0, sizeof(opts));
    opts.type_size = sizeof(opts);
    opts.auto_reload = 1;
    opts.code_info = code_info;
    opts.inlined_fns = inlined_fns;
    opts.demangle = demangle;
    opts.process_dispatch = dispatch;
    return blaze_symbolizer_new_opts(&opts);
}

static blaze_symbolize_src_process make_process_src(uint32_t pid) {
    blaze_symbolize_src_process src;
    memset(&src, 0, sizeof(src));
    src.type_size = sizeof(src);
    src.pid = pid;
    src.debug_syms = 1;
    return src;
}

// sym_at lets Go index into the trailing flexible array member without
// performing pointer arithmetic on the Go side.
static const blaze_sym* sym_at(const blaze_syms* syms, size_t i) {
    return &syms->syms[i];
}

// inlined_at indexes a blaze_sym's inlined function array.
static const blaze_symbolize_inlined_fn* inlined_at(const blaze_sym* s, size_t i) {
    return &s->inlined[i];
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime/cgo"
	"unsafe"

	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
)

// goDispatchCb is the C-callable callback installed in
// blaze_symbolizer_dispatch. blazesym invokes it for every process member
// with a file path; we route it through the dispatcher decision tree and
// return either a malloc'd path string (taking the override branch) or
// NULL (let blazesym try the default).
//
// Memory contract: blazesym frees the returned pointer with libc free;
// C.CString uses libc malloc, so the pair is balanced.
//
//export goDispatchCb
func goDispatchCb(mapsFile, symbolicPath *C.char, ctx unsafe.Pointer) (ret *C.char) {
	h := cgo.Handle(uintptr(ctx))
	s, ok := h.Value().(*Symbolizer)
	if !ok {
		return nil
	}

	s.inflight.Add(1)
	defer s.inflight.Done()
	defer func() {
		if r := recover(); r != nil {
			s.stats.dispatcherPanics.Add(1)
			ret = nil
		}
	}()
	return s.cgoDispatch(C.GoString(mapsFile), C.GoString(symbolicPath))
}

// cgoDispatch is the per-call wrapper that derives a context with
// FetchTimeout, runs the pure-Go decision tree, and converts the result
// to a malloc'd C string (or NULL).
func (s *Symbolizer) cgoDispatch(mapsFile, symbolicPath string) *C.char {
	ctx := context.Background()
	if s.opts.FetchTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.FetchTimeout)
		defer cancel()
	}
	path := s.dispatchDecision(ctx, mapsFile, symbolicPath)
	if path == "" {
		return nil
	}
	return C.CString(path)
}

// dispatchDecision is the production entry: reads the build-id from the ELF,
// then delegates to dispatchWithBuildID for the four-case routing logic.
// Returns the override path string for blazesym, or "" meaning "use the default".
//
// See spec §"Dispatcher decision tree".
func (s *Symbolizer) dispatchDecision(ctx context.Context, mapsFile, symbolicPath string) string {
	s.stats.dispatcherCalls.Add(1)
	buildID := readBuildID(mapsFile, symbolicPath)
	if buildID == "" {
		return ""
	}
	return s.dispatchWithBuildID(ctx, symbolicPath, buildID)
}

// dispatchDecisionForTest is a test-only entry that lets tests bypass
// readBuildID (which depends on the ELF having .note.gnu.build-id) by
// supplying an explicit buildID. Delegates to the same dispatchWithBuildID
// helper as the production path so both share identical routing logic.
func (s *Symbolizer) dispatchDecisionForTest(ctx context.Context, _, symbolicPath, buildID string) string {
	s.stats.dispatcherCalls.Add(1)
	return s.dispatchWithBuildID(ctx, symbolicPath, buildID)
}

// dispatchWithBuildID is the shared four-case routing logic. Both the
// production entry (dispatchDecision) and the test entry
// (dispatchDecisionForTest) delegate here so the routing logic lives in
// exactly one place. dispatcherCalls must be incremented by the caller, not
// here, to avoid double-counting.
func (s *Symbolizer) dispatchWithBuildID(ctx context.Context, symbolicPath, buildID string) string {
	// Case 1: cached executable from prior fetch.
	if s.cache.Has(buildID, cache.KindExecutable) {
		s.stats.cacheHits.Add(1)
		return s.cache.AbsPath(buildID, cache.KindExecutable)
	}

	// Case 2: blazesym default would work (binary on disk, DWARF or
	// debug_dirs covers it).
	if s.localResolutionPossible(symbolicPath, buildID) {
		s.stats.dispatcherSkippedLocal.Add(1)
		return ""
	}

	// Case 3: binary on disk, no DWARF locally → fetch /debuginfo into
	// the build-id cache; blazesym will find it via debug_dirs.
	if binaryReadable(symbolicPath) {
		s.stats.cacheMisses.Add(1)
		if _, err := s.sf.fetchAndStore(ctx, "debuginfo", buildID); err != nil {
			s.recordFetchErr(err)
		} else {
			s.stats.fetchSuccessDebuginfo.Add(1)
		}
		return ""
	}

	// Case 4: binary not on disk → fetch /executable, return that path.
	s.stats.cacheMisses.Add(1)
	abs, err := s.sf.fetchAndStore(ctx, "executable", buildID)
	if err != nil {
		s.recordFetchErr(err)
		return ""
	}
	s.stats.fetchSuccessExecutable.Add(1)
	return abs
}

func (s *Symbolizer) localResolutionPossible(path, buildID string) bool {
	if s.cache.Has(buildID, cache.KindDebuginfo) {
		return true
	}
	if hasDwarf(path) {
		return true
	}
	if hasResolvableDebuglink(path, []string{s.opts.CacheDir}) {
		return true
	}
	return false
}

func (s *Symbolizer) recordFetchErr(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrNotFound) {
		s.stats.fetch404s.Add(1)
		return
	}
	s.stats.fetchErrors.Add(1)
}

// cgoState bundles the C-side handles whose lifetimes track the Symbolizer.
// All three (csym, dispatch, handle) must outlive any in-flight dispatcher
// callback; close() tears them down in the only safe order.
type cgoState struct {
	csym     *C.blaze_symbolizer
	dispatch *C.blaze_symbolizer_dispatch
	handle   cgo.Handle
}

func newCgoState(s *Symbolizer) (*cgoState, error) {
	st := &cgoState{}
	st.handle = cgo.NewHandle(s)

	st.dispatch = C.alloc_dispatch(C.uintptr_t(st.handle))
	if st.dispatch == nil {
		st.handle.Delete()
		return nil, fmt.Errorf("debuginfod: alloc_dispatch returned NULL")
	}

	st.csym = C.new_symbolizer(
		st.dispatch,
		C._Bool(s.opts.CodeInfo),
		C._Bool(s.opts.InlinedFns),
		C._Bool(s.opts.Demangle),
	)
	if st.csym == nil {
		C.free_dispatch(st.dispatch)
		st.handle.Delete()
		return nil, fmt.Errorf("debuginfod: blaze_symbolizer_new_opts returned NULL")
	}
	return st, nil
}

// close tears down the cgo state. Caller MUST have already drained
// in-flight dispatcher callbacks (Symbolizer.Close awaits inflight before
// invoking us). Order: free blazesym (which releases the Rust closure
// holding the cb/ctx pair) → free the dispatch struct → delete the cgo
// handle. Reversing any pair would risk a use-after-free if a callback
// were still running, hence the inflight.Wait() precondition.
//
// close is idempotent: each field is checked and zeroed before freeing,
// so a second call is a safe no-op.
func (st *cgoState) close() {
	if st.csym != nil {
		C.blaze_symbolizer_free(st.csym)
		st.csym = nil
	}
	if st.dispatch != nil {
		C.free_dispatch(st.dispatch)
		st.dispatch = nil
	}
	if st.handle != 0 {
		st.handle.Delete()
		st.handle = 0
	}
}

// symbolizeProcess is the C-side symbolize call. Returns one Frame per IP.
func (st *cgoState) symbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	src := C.make_process_src(C.uint32_t(pid))
	caddr := (*C.uint64_t)(unsafe.Pointer(&ips[0]))
	syms := C.blaze_symbolize_process_abs_addrs(st.csym, &src, caddr, C.size_t(len(ips)))
	if syms == nil {
		return nil, fmt.Errorf("debuginfod: blaze_symbolize_process_abs_addrs returned NULL")
	}
	defer C.blaze_syms_free(syms)

	cnt := int(syms.cnt)
	out := make([]symbolize.Frame, 0, cnt)
	for i := range cnt {
		csym := C.sym_at(syms, C.size_t(i))
		var addr uint64
		if i < len(ips) {
			addr = ips[i]
		}
		out = append(out, frameFromCSym(csym, addr))
	}
	return out, nil
}

// frameFromCSym translates one C blaze_sym to a Go Frame. The Inlined
// chain is left in caller-most-to-callee order (no reversal here; the
// reversal lives in symbolize.ToProfFrames).
func frameFromCSym(c *C.blaze_sym, addr uint64) symbolize.Frame {
	f := symbolize.Frame{Address: addr}
	if c.name == nil {
		f.Reason = symbolize.FailureUnknownAddress
		return f
	}
	f.Name = C.GoString(c.name)
	if c.module != nil {
		f.Module = C.GoString(c.module)
	}
	f.Offset = uint64(c.offset)
	if c.code_info.file != nil {
		f.File = C.GoString(c.code_info.file)
		f.Line = int(c.code_info.line)
		f.Column = int(c.code_info.column)
	}
	for j := C.size_t(0); j < c.inlined_cnt; j++ {
		in := C.inlined_at(c, j)
		inFrame := symbolize.Frame{Address: addr, Module: f.Module}
		if in.name != nil {
			inFrame.Name = C.GoString(in.name)
		}
		if in.code_info.file != nil {
			inFrame.File = C.GoString(in.code_info.file)
			inFrame.Line = int(in.code_info.line)
			inFrame.Column = int(in.code_info.column)
		}
		f.Inlined = append(f.Inlined, inFrame)
	}
	return f
}
