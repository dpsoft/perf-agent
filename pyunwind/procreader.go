package pyunwind

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcReader is the FrameReader (and TLSBaseReader) Attach uses against a
// live process: memory through process_vm_readv, and one named thread's TLS
// base through ptrace.
//
// The two halves have different scopes on purpose. Memory reads are
// process-wide -- every address Attach validates (the code object, the
// frame chain, _PyRuntime) is the same for every thread -- while the TLS
// base belongs to ONE thread, because that is what the pthread TSD lookup
// indexes into. A ProcReader therefore names both a pid and the tid whose
// PyThreadState the attach-time validation will find.
//
// A thread that has never run Python has no PyThreadState in its TSD slot,
// which is a legitimate state and not an error: Attach refuses with
// ErrOffsetsUnreadable, and the caller moves to the next tid. See
// AttachProcess.
type ProcReader struct {
	pid int
	tid int
}

// NewProcReader returns a reader over pid's address space that will report
// tid's TLS base. tid must be a thread of pid (pid itself is the main
// thread's tid).
func NewProcReader(pid, tid int) *ProcReader {
	return &ProcReader{pid: pid, tid: tid}
}

// TID is the thread whose TLS base this reader reports. Exposed so a caller
// iterating threads can name the one that worked in a log line.
func (r *ProcReader) TID() int { return r.tid }

// read pulls n bytes from the target's address space in one
// process_vm_readv. It needs PTRACE_MODE_ATTACH_REALCREDS on the target --
// same-uid, or CAP_SYS_PTRACE, which the agent holds.
func (r *ProcReader) read(addr uint64, n int) ([]byte, error) {
	buf := make([]byte, n)
	local := []unix.Iovec{{Base: &buf[0], Len: uint64(n)}}
	remote := []unix.RemoteIovec{{Base: uintptr(addr), Len: n}}
	got, err := unix.ProcessVMReadv(r.pid, local, remote, 0)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: read %d bytes at %#x from pid %d: %w", n, addr, r.pid, err)
	}
	if got != n {
		return nil, fmt.Errorf("pyunwind: short read at %#x from pid %d: %d of %d bytes", addr, r.pid, got, n)
	}
	return buf, nil
}

// ReadU64 implements FrameReader.
func (r *ProcReader) ReadU64(addr uint64) (uint64, error) {
	b, err := r.read(addr, 8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// ReadU8 implements FrameReader.
func (r *ProcReader) ReadU8(addr uint64) (uint8, error) {
	b, err := r.read(addr, 1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// TLSBase implements TLSBaseReader. Its body is architecture-specific: see
// tlsbase_amd64.go, and tlsbase_other.go for why every other architecture
// refuses by name instead.
func (r *ProcReader) TLSBase() (uint64, error) { return r.tlsBase() }
