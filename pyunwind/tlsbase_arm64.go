//go:build linux && arm64

package pyunwind

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ntARMTLS is NT_ARM_TLS, the regset holding TPIDR_EL0. x/sys/unix exports
// no constant for it (it exports only elfNT_PRSTATUS, unexported, for the
// general-purpose set), so it is named here from the kernel's
// include/uapi/linux/elf.h.
const ntARMTLS = 0x401

// readThreadPointer returns TPIDR_EL0 for the thread.
//
// On aarch64 glibc uses TLS variant I: the thread pointer does NOT point at
// `struct pthread`. The struct sits BEFORE it, and glibc's own THREAD_SELF
// is `__builtin_thread_pointer() - TLS_PRE_TCB_SIZE`. That distance is what
// tsdOffsets.PthreadSize carries, and subtracting it is the caller's job --
// hostTSSGet and bpf/interp/python/python_walk.h both do it. This returns
// the raw register.
//
// There is no PTRACE_GETREGS payload carrying it the way amd64's
// user_regs_struct carries the FS base, so it comes out of PTRACE_GETREGSET
// with NT_ARM_TLS, whose payload is a single u64. x/sys/unix's
// PtraceGetRegSetArm64 is hardcoded to the general-purpose struct, so the
// ptrace call is made directly.
//
// iov.Len is read back rather than assumed: PTRACE_GETREGSET reports how
// much it actually wrote, and a kernel that wrote less than 8 bytes has not
// given us a thread pointer. Accepting a short write would hand the TSD walk
// a half-initialised address, which is the silent-wrong-offset failure this
// package exists to refuse.
func readThreadPointer(tid int) (uint64, error) {
	var tp uint64
	iov := unix.Iovec{
		Base: (*byte)(unsafe.Pointer(&tp)),
		Len:  uint64(unsafe.Sizeof(tp)),
	}
	if _, _, errno := unix.Syscall6(
		unix.SYS_PTRACE,
		uintptr(unix.PTRACE_GETREGSET),
		uintptr(tid),
		uintptr(ntARMTLS),
		uintptr(unsafe.Pointer(&iov)),
		0, 0,
	); errno != 0 {
		return 0, fmt.Errorf("pyunwind: ptrace getregset NT_ARM_TLS tid %d: %w", tid, errno)
	}
	if iov.Len < uint64(unsafe.Sizeof(tp)) {
		return 0, fmt.Errorf("pyunwind: ptrace getregset NT_ARM_TLS tid %d returned %d bytes, want %d",
			tid, iov.Len, unsafe.Sizeof(tp))
	}
	return tp, nil
}
