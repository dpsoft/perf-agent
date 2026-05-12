package symbolize

/*
#include <stdlib.h>
#include <string.h>
#include "blazesym.h"

static blaze_symbolizer_opts make_kernel_opts(_Bool code_info, _Bool inlined_fns, _Bool demangle) {
    blaze_symbolizer_opts opts;
    memset(&opts, 0, sizeof(opts));
    opts.type_size = sizeof(opts);
    opts.auto_reload = 1;
    opts.code_info = code_info;
    opts.inlined_fns = inlined_fns;
    opts.demangle = demangle;
    return opts;
}

static blaze_symbolize_src_kernel make_kernel_src(void) {
    blaze_symbolize_src_kernel src;
    memset(&src, 0, sizeof(src));
    src.type_size = sizeof(src);
    // kallsyms = NULL, vmlinux = NULL → blazesym uses /proc/kallsyms +
    // discovers vmlinux on disk if it can. debug_syms = true so the
    // bumped blazesym pin walks /proc/modules + /lib/modules/<release>/
    // for module .ko.debug DWARF.
    src.debug_syms = 1;
    return src;
}

// sym_at_kernel mirrors the user-mode sym_at helper in
// symbolize/debuginfod/dispatcher.go — flexible-array indexing without
// pointer arithmetic in Go.
static const blaze_sym* sym_at_kernel(const blaze_syms* syms, size_t i) {
    return &syms->syms[i];
}
*/
import "C"

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// LocalKernelSymbolizer wraps blazesym's kernel source: /proc/kallsyms
// for vmlinux + every loaded module's symbols. With debug_syms=true and
// the v1.1.0+ blazesym pin (≥ commit 29a609f), module functions resolve
// to function name + source :line when distro kernel-modules-debuginfo
// is installed locally.
type LocalKernelSymbolizer struct {
	csym   *C.blaze_symbolizer
	closed atomic.Bool
	mu     sync.Mutex
}

// NewLocalKernelSymbolizer returns a kernel symbolizer or
// ErrKernelSymbolsUnavailable when /proc/kallsyms is unreadable or
// kptr-restricted (first symbol address is 0).
func NewLocalKernelSymbolizer() (*LocalKernelSymbolizer, error) {
	if !kallsymsReadableInternal() {
		return nil, ErrKernelSymbolsUnavailable
	}

	copts := C.make_kernel_opts(
		C._Bool(true), // code_info — populates Frame.File/Line/Column
		C._Bool(true), // inlined_fns — populates Frame.Inlined chain
		C._Bool(true), // demangle — Rust kernel symbols, etc.
	)
	csym := C.blaze_symbolizer_new_opts(&copts)
	if csym == nil {
		return nil, fmt.Errorf("blaze_symbolizer_new_opts returned NULL")
	}
	return &LocalKernelSymbolizer{csym: csym}, nil
}

// SymbolizeKernel resolves kernel addresses via blazesym's kernel source.
// Returns one Frame per IP. Frames whose name couldn't be resolved get
// Name = "0x<hex>" + Reason = FailureMissingSymbols (matches Noop posture).
func (s *LocalKernelSymbolizer) SymbolizeKernel(ips []uint64) ([]Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}

	// Defend against blazesym calling back during a Close race: take the
	// lock; Close blocks on the same lock before freeing csym.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrClosed
	}

	src := C.make_kernel_src()
	caddr := (*C.uint64_t)(unsafe.Pointer(&ips[0]))
	syms := C.blaze_symbolize_kernel_abs_addrs(s.csym, &src, caddr, C.size_t(len(ips)))
	if syms == nil {
		errc := C.blaze_err_last()
		errStr := C.GoString(C.blaze_err_str(errc))
		return nil, fmt.Errorf("blaze_symbolize_kernel_abs_addrs: %s (code %d)", errStr, int(errc))
	}
	defer C.blaze_syms_free(syms)

	out := make([]Frame, 0, int(syms.cnt))
	for i := 0; i < int(syms.cnt); i++ {
		csym := C.sym_at_kernel(syms, C.size_t(i))
		out = append(out, frameFromKernelCSym(csym, ips[i]))
	}
	return out, nil
}

// Close releases the underlying blazesym symbolizer. Idempotent.
func (s *LocalKernelSymbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.csym != nil {
		C.blaze_symbolizer_free(s.csym)
		s.csym = nil
	}
	return nil
}

// frameFromKernelCSym mirrors the user-mode frameFromCSym helper from
// symbolize/debuginfod/dispatcher.go: copies name + offset + code_info
// (file/line/column) when blazesym resolved them, and walks the inlined
// chain so kernel module functions get full source attribution when
// blazesym has DWARF for the loaded modules.
//
// Module is always "[kernel.kallsyms]" — kernel addresses are a unified
// namespace, and the pprof builder routes them through kernelSentinel
// regardless of which module a particular function came from.
func frameFromKernelCSym(c *C.blaze_sym, addr uint64) Frame {
	f := Frame{Address: addr, Module: "[kernel.kallsyms]"}
	if c.name == nil {
		f.Name = fmt.Sprintf("0x%x", addr)
		f.Reason = FailureMissingSymbols
		return f
	}
	f.Name = C.GoString(c.name)
	f.Offset = uint64(c.offset)
	if c.code_info.file != nil {
		f.File = C.GoString(c.code_info.file)
		f.Line = int(c.code_info.line)
		f.Column = int(c.code_info.column)
	}
	for j := C.size_t(0); j < c.inlined_cnt; j++ {
		in := (*C.blaze_symbolize_inlined_fn)(unsafe.Pointer(uintptr(unsafe.Pointer(c.inlined)) + uintptr(j)*unsafe.Sizeof(*c.inlined)))
		inFrame := Frame{Address: addr, Module: f.Module}
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

// kallsymsReadableInternal mirrors the test helper but lives here so the
// constructor can short-circuit before allocating any cgo state.
func kallsymsReadableInternal() bool {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 1 {
		return false
	}
	n, err := strconv.ParseUint(fields[0], 16, 64)
	if err != nil {
		return false
	}
	return n != 0
}
