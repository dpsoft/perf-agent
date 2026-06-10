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
// /boot/vmlinux-* and /usr/lib/debug/boot/ for DWARF. Since blazesym
// v0.2.4 (commit 987d36c) the KASLR offset — the only thing that
// needed /proc/kcore — is queried lazily and only when a vmlinux DWARF
// resolver is actually present. On the common lockdown=integrity host
// (no /boot/vmlinux DWARF installed) blazesym resolves kallsyms-only
// without ever touching /proc/kcore, so the BLAZE_ERR_PERMISSION_DENIED
// that the old pure-Go fallback existed for no longer occurs.
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
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// LocalKernelSymbolizer resolves kernel-mode addresses via blazesym.
//
// blazesym path: gives function name + offset + inline expansion +
// source file:line when the host kernel exposes vmlinux DWARF; falls
// back internally to kallsyms-only resolution (name + offset) when no
// vmlinux DWARF is present — including on lockdown=integrity hosts,
// where it no longer needs /proc/kcore (blazesym >= v0.2.4).
//
// If blazesym fails for any reason, SymbolizeKernel preserves the raw
// kernel addresses (Name="0x<hex>") so the kernel side of the stack
// still survives into the pprof.
type LocalKernelSymbolizer struct {
	csym   *C.blaze_symbolizer
	closed atomic.Bool
	mu     sync.Mutex

	// symbolize is the seam under SymbolizeKernel; in production it
	// points to cgoSymbolize. Tests swap it for a stub so the
	// raw-address backstop path can be exercised without a real
	// blazesym handle.
	symbolize func(ips []uint64) ([]Frame, error)

	// stats counts observability events (batch counts, raw-address
	// synthesis, blazesym error buckets). Exposed via Stats() for
	// end-of-run logging and the /metrics scrape.
	stats Counters
}

// Stats returns a snapshot of the symbolizer's observability
// counters. Safe to call concurrently with SymbolizeKernel.
func (s *LocalKernelSymbolizer) Stats() CountersSnapshot {
	return s.stats.Snapshot()
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
	s.symbolize = s.cgoSymbolize
	return s, nil
}

// SymbolizeKernel resolves kernel addresses to frames via blazesym.
// If blazesym fails, returns raw-address frames (Name="0x<hex>",
// Reason=FailureMissingSymbols) so kernel context survives into the
// pprof.
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

	s.stats.KernelBatches.Add(1)
	s.stats.KernelInputIPs.Add(uint64(len(ips)))
	// Record per-batch wall-clock duration so p50/p99 land in the
	// /metrics scrape and end-of-run log. Microseconds keep the
	// numbers human-readable across both warm-cache (sub-ms) and
	// cold-CGO (millisecond+) paths.
	t0 := time.Now()
	defer func() {
		s.stats.KernelBatchHist.Record(uint64(time.Since(t0).Microseconds()))
	}()

	frames, err := s.symbolize(ips)
	if err == nil {
		return frames, nil
	}
	// blazesym failed — preserve raw kernel addresses so the kernel
	// side of the stack survives into the pprof.
	s.stats.KernelBatchFailures.Add(1)
	s.stats.KernelRawAddrFrames.Add(uint64(len(ips)))
	return rawKernelAddrFrames(ips), nil
}

// cgoSymbolize invokes blazesym's kernel source.
//
// Bumps reason-bucketed counters at the error site (roadmap #4) so
// end-of-run logs / the /metrics scrape can distinguish a lockdown
// host that still EPERMs (KernelLockdownEPERM — only the narrow
// vmlinux-DWARF-installed case post-v0.2.4) from a buggy blazesym
// (any KernelOtherErr at all).
func (s *LocalKernelSymbolizer) cgoSymbolize(ips []uint64) ([]Frame, error) {
	src := C.make_kernel_src()
	caddr := (*C.uint64_t)(unsafe.Pointer(&ips[0]))
	syms := C.blaze_symbolize_kernel_abs_addrs(s.csym, &src, caddr, C.size_t(len(ips)))
	if syms == nil {
		errc := C.blaze_err_last()
		if errc == C.BLAZE_ERR_PERMISSION_DENIED {
			s.stats.KernelLockdownEPERM.Add(1)
		} else {
			s.stats.KernelOtherErr.Add(1)
		}
		errStr := C.GoString(C.blaze_err_str(errc))
		return nil, fmt.Errorf("blaze_symbolize_kernel_abs_addrs: %s (code %d)", errStr, int(errc))
	}
	defer C.blaze_syms_free(syms)

	out := make([]Frame, 0, int(syms.cnt))
	for i := range int(syms.cnt) {
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
