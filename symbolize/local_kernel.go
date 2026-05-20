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

// make_kernel_src returns the kernel source blazesym uses by default:
// kallsyms=NULL → /proc/kallsyms, vmlinux=NULL → blazesym auto-scans
// /sys/kernel/btf/vmlinux, /boot/vmlinux-*, /proc/kcore, and friends
// for DWARF. On hosts with kernel lockdown=integrity (Secure Boot) one
// of those open() calls returns EACCES — most commonly /proc/kcore,
// which has CAP_SYS_RAWIO + CAP_DAC_READ_SEARCH requirements — and
// blazesym surfaces it as BLAZE_ERR_PERMISSION_DENIED for the whole
// batch. SymbolizeKernel handles this by falling back to a pure-Go
// /proc/kallsyms symbolizer (kallsyms.go).
static blaze_symbolize_src_kernel make_kernel_src(void) {
    blaze_symbolize_src_kernel src;
    memset(&src, 0, sizeof(src));
    src.type_size = sizeof(src);
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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// errBlazePermissionDenied signals that blazesym returned
// BLAZE_ERR_PERMISSION_DENIED for the kernel source. The
// SymbolizeKernel fallback ladder converts this into a switch to the
// pure-Go /proc/kallsyms symbolizer for the symbolizer's lifetime.
var errBlazePermissionDenied = errors.New("symbolize: blazesym permission denied (kernel lockdown?)")

// forceFallbackEnv lets operators (and integration tests) force the
// pure-Go /proc/kallsyms fallback without waiting for blazesym to
// fail first. Set PERFAGENT_FORCE_KERNEL_FALLBACK=1 to skip the CGO
// blazesym path on hosts known to be locked down — avoids one wasted
// CGO call per sample batch — and to exercise the fallback in CI on
// hosts that don't naturally hit EPERM.
const forceFallbackEnv = "PERFAGENT_FORCE_KERNEL_FALLBACK"

// LocalKernelSymbolizer resolves kernel-mode addresses via blazesym,
// with a transparent pure-Go /proc/kallsyms fallback for hosts where
// blazesym can't read its required kernel images (lockdown=integrity,
// Secure Boot, missing CAP_SYS_RAWIO/CAP_DAC_READ_SEARCH).
//
// blazesym path: gives function name + offset + inline expansion +
// source file:line when the host kernel exposes vmlinux DWARF and
// /proc/kcore. Used by default on permissive hosts.
//
// Pure-Go kallsyms path (see kallsyms.go): gives function name +
// offset + module marker only. No DWARF, no inline frames. Sufficient
// for flame graphs and operator decoding — and works under
// lockdown=integrity, which is the common production case.
//
// The fallback decision is sticky: once we've seen
// BLAZE_ERR_PERMISSION_DENIED on this host, every subsequent batch
// goes straight to the pure-Go path. Re-probing blazesym on every
// batch would waste a CGO call per sample.
type LocalKernelSymbolizer struct {
	csym   *C.blaze_symbolizer
	closed atomic.Bool
	mu     sync.Mutex

	// callBlazesym is the seam under SymbolizeKernel. In production
	// it points to invoke (which routes to cgoSymbolize or to the
	// pure-Go kallsymsSymbolizer based on useFallback). Tests swap
	// it for a stub so the Go-level fallback ladder can be exercised
	// without a real blazesym handle and without a real
	// /proc/kallsyms read.
	callBlazesym func(ips []uint64, useFallback bool) ([]Frame, error)

	// fallback is set once blazesym reports permission-denied on the
	// CGO path, or at construction time when forceFallbackEnv is set.
	// Subsequent batches skip the failing CGO path and go straight to
	// the pure-Go /proc/kallsyms symbolizer.
	fallback atomic.Bool

	// kallsymsOnce + kallsymsCache + kallsymsErr lazily build the
	// pure-Go /proc/kallsyms index on the first fallback batch and
	// reuse it for the symbolizer's lifetime. Parsing is ~3M lines
	// of /proc/kallsyms on a typical x86_64 — one-time cost.
	kallsymsOnce  sync.Once
	kallsymsCache *kallsymsSymbolizer
	kallsymsErr   error
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
	s := &LocalKernelSymbolizer{csym: csym}
	s.callBlazesym = s.invoke
	if os.Getenv(forceFallbackEnv) == "1" {
		s.fallback.Store(true)
	}
	return s, nil
}

// SymbolizeKernel resolves kernel addresses to frames. On
// BLAZE_ERR_PERMISSION_DENIED from the CGO path, transparently
// switches to the pure-Go /proc/kallsyms symbolizer for the
// symbolizer's remaining lifetime. If even that fails, returns
// raw-address frames (Name="0x<hex>", Reason=FailureMissingSymbols)
// so kernel context survives into the pprof.
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

	// Sticky fallback: once we've seen permission-denied on the CGO
	// path, this host won't recover within the symbolizer's lifetime.
	// Skip blazesym on every subsequent batch.
	if s.fallback.Load() {
		frames, err := s.callBlazesym(ips, true)
		if err != nil {
			return rawKernelAddrFrames(ips), nil
		}
		return frames, nil
	}

	frames, err := s.callBlazesym(ips, false)
	if err == nil {
		return frames, nil
	}
	if errors.Is(err, errBlazePermissionDenied) {
		s.fallback.Store(true)
		frames, err = s.callBlazesym(ips, true)
		if err == nil {
			return frames, nil
		}
	}
	// Both paths failed — preserve raw kernel addresses so the
	// kernel side of the stack survives into the pprof.
	return rawKernelAddrFrames(ips), nil
}

// invoke is the production callBlazesym. useFallback=false routes to
// the CGO blazesym path (full inline + DWARF); useFallback=true
// routes to the pure-Go /proc/kallsyms symbolizer (name+offset only,
// but lockdown-safe).
func (s *LocalKernelSymbolizer) invoke(ips []uint64, useFallback bool) ([]Frame, error) {
	if useFallback {
		ks, err := s.getKallsymsFallback()
		if err != nil {
			return nil, err
		}
		return ks.Resolve(ips), nil
	}
	return s.cgoSymbolize(ips)
}

// getKallsymsFallback returns the lazily-built pure-Go /proc/kallsyms
// symbolizer. Built exactly once per LocalKernelSymbolizer lifetime;
// subsequent calls return the cached instance.
func (s *LocalKernelSymbolizer) getKallsymsFallback() (*kallsymsSymbolizer, error) {
	s.kallsymsOnce.Do(func() {
		s.kallsymsCache, s.kallsymsErr = newKallsymsSymbolizer()
	})
	return s.kallsymsCache, s.kallsymsErr
}

// cgoSymbolize invokes blazesym's kernel source. Returns
// errBlazePermissionDenied on BLAZE_ERR_PERMISSION_DENIED so the
// fallback ladder can switch to the pure-Go path; other blazesym
// errors propagate as wrapped errors.
func (s *LocalKernelSymbolizer) cgoSymbolize(ips []uint64) ([]Frame, error) {
	src := C.make_kernel_src()
	caddr := (*C.uint64_t)(unsafe.Pointer(&ips[0]))
	syms := C.blaze_symbolize_kernel_abs_addrs(s.csym, &src, caddr, C.size_t(len(ips)))
	if syms == nil {
		errc := C.blaze_err_last()
		if errc == C.BLAZE_ERR_PERMISSION_DENIED {
			return nil, errBlazePermissionDenied
		}
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

// rawKernelAddrFrames synthesizes Frames carrying just the raw IPs
// when no symbolizer could resolve them. Module is set so pprof's
// kernelSentinel mapping picks these up; Reason=FailureMissingSymbols
// matches the NoopKernelSymbolizer posture. Address is preserved so
// distinct pprof Locations stay distinct.
func rawKernelAddrFrames(ips []uint64) []Frame {
	out := make([]Frame, len(ips))
	for i, ip := range ips {
		out[i] = Frame{
			Address: ip,
			Name:    fmt.Sprintf("0x%x", ip),
			Module:  "[kernel.kallsyms]",
			Reason:  FailureMissingSymbols,
		}
	}
	return out
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
