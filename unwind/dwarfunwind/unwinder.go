// Package dwarfunwind wraps libunwind for the DWARF leg of the --unwind auto
// hybrid. It takes captured (regs, stack) from a perf_event sample and
// produces a PC chain by evaluating DWARF CFI in userspace.
//
// Use this when fpwalker is unlikely to succeed — when the sampled PC is in
// FP-less code (libstd, glibc, stripped C++). For the FP-safe common case,
// fpwalker is faster; see unwind/fpwalker for that.
package dwarfunwind

/*
#cgo CFLAGS: -I/usr/include
#cgo LDFLAGS: -lunwind -lunwind-x86_64

#include "unwinder.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

// Unwinder owns a libunwind address space. Safe to use from one goroutine
// at a time. If concurrent unwinding is needed, create one Unwinder per
// worker.
type Unwinder struct {
	c *C.pa_unwinder_t
}

// New creates an Unwinder. Returns error if libunwind setup fails
// (typically out-of-memory; this does not fail per sample).
func New() (*Unwinder, error) {
	c := C.pa_unwinder_new()
	if c == nil {
		return nil, fmt.Errorf("pa_unwinder_new returned NULL")
	}
	u := &Unwinder{c: c}
	// Defensive finalizer in case Close is forgotten. Not a substitute for
	// Close — file descriptors and mmaps allocated per unwind are released
	// inside pa_unwind, but the address space lives until pa_unwinder_free.
	runtime.SetFinalizer(u, func(x *Unwinder) { x.Close() })
	return u, nil
}

// Close releases the libunwind address space and any cached fds.
// Idempotent.
func (u *Unwinder) Close() {
	if u == nil || u.c == nil {
		return
	}
	C.pa_unwinder_free(u.c)
	u.c = nil
	runtime.SetFinalizer(u, nil)
}

// Unwind produces a leaf-first PC chain by DWARF-unwinding the captured
// stack state. Returns the chain or an error. An empty result (and no
// error) means the captured IP resolved but libunwind couldn't step
// further — likely missing debug info or a frame the unwinder doesn't
// recognize.
func (u *Unwinder) Unwind(pid uint32, regs []uint64, stackAddr uint64, stack []byte) ([]uint64, error) {
	if u.c == nil {
		return nil, fmt.Errorf("unwinder closed")
	}
	if len(regs) == 0 {
		return nil, fmt.Errorf("no captured regs")
	}

	const maxPCs = 128
	out := make([]uint64, maxPCs)

	var regsPtr *C.uint64_t
	if len(regs) > 0 {
		regsPtr = (*C.uint64_t)(unsafe.Pointer(&regs[0]))
	}
	var stackPtr *C.uint8_t
	if len(stack) > 0 {
		stackPtr = (*C.uint8_t)(unsafe.Pointer(&stack[0]))
	}

	n := C.pa_unwind(
		u.c,
		C.int32_t(pid),
		regsPtr, C.size_t(len(regs)),
		C.uint64_t(stackAddr),
		stackPtr, C.size_t(len(stack)),
		(*C.uint64_t)(unsafe.Pointer(&out[0])), C.size_t(maxPCs),
	)
	if n < 0 {
		return nil, fmt.Errorf("pa_unwind returned %d", int(n))
	}
	return out[:int(n):int(n)], nil
}
