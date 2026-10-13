//go:build linux && amd64

package pyunwind

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readThreadPointer returns the thread's FS base.
//
// On x86-64 glibc uses TLS variant II: the thread pointer IS the address
// `struct pthread` starts at, so there is nothing to correct and
// tsdOffsets.PthreadSize is 0 for this architecture.
//
// PTRACE_GETREGS carries the FS base directly in user_regs_struct, so no
// register-set plumbing is needed here the way it is on arm64.
func readThreadPointer(tid int) (uint64, error) {
	var regs unix.PtraceRegs
	if err := unix.PtraceGetRegs(tid, &regs); err != nil {
		return 0, fmt.Errorf("pyunwind: ptrace getregs tid %d: %w", tid, err)
	}
	return regs.Fs_base, nil
}
