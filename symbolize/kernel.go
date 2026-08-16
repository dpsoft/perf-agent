package symbolize

import (
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/pprof"
)

// ErrKernelSymbolsUnavailable indicates /proc/kallsyms is unreadable or
// kptr-restricted (kernel addresses come back as zeros). Callers SHOULD
// construct a NoopKernelSymbolizer and continue rather than fail.
var ErrKernelSymbolsUnavailable = errors.New("symbolize: kernel symbols unavailable (kptr_restrict?)")

// KernelSymbolizer resolves kernel-mode addresses to symbolic frames.
// Kernel-mode resolution has no PID — kernel + module symbols are
// global. Implementations are safe for concurrent use.
type KernelSymbolizer interface {
	SymbolizeKernel(ips []uint64) ([]Frame, error)
	Close() error
}

// NoopKernelSymbolizer returns a Frame per IP with Name = "0x<hex>" and
// Reason = FailureMissingSymbols. Used when --kernel-stacks is off, or
// when /proc/kallsyms is locked down.
type NoopKernelSymbolizer struct{}

// SymbolizeKernel returns one Frame per input IP with the address
// rendered as a hex string in Name. Address is preserved so pprof
// Locations stay distinguishable.
func (NoopKernelSymbolizer) SymbolizeKernel(ips []uint64) ([]Frame, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	out := make([]Frame, len(ips))
	for i, ip := range ips {
		out[i] = Frame{
			Address: ip,
			Name:    fmt.Sprintf("0x%x", ip),
			Reason:  FailureMissingSymbols,
		}
	}
	return out, nil
}

// Close is a no-op.
func (NoopKernelSymbolizer) Close() error { return nil }

// MergeKernelFirst returns a leaf-first frame chain by prepending kernel
// frames (already leaf-first per blazesym convention) onto user frames.
// Either slice may be nil.
func MergeKernelFirst(kernel, user []Frame) []Frame {
	if len(kernel) == 0 {
		return user
	}
	if len(user) == 0 {
		return kernel
	}
	out := make([]Frame, 0, len(kernel)+len(user))
	out = append(out, kernel...)
	out = append(out, user...)
	return out
}

// ToProfFramesKernel is ToProfFrames + IsKernel=true on every output frame.
// pprof.ProfileBuilder routes IsKernel frames through the existing
// kernelSentinel mapping at pprof/pprof.go:288 — no builder code changes
// needed. Used by every call site that converts symbolized kernel frames
// to pprof.Frame.
func ToProfFramesKernel(frames []Frame) []pprof.Frame {
	out := ToProfFrames(frames)
	for i := range out {
		out[i].IsKernel = true
	}
	return out
}
