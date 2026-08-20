package symbolize

import (
	"errors"
	"fmt"
	"sync/atomic"

	blazesym "github.com/libbpf/blazesym/go"
)

// ErrClosed is returned from operations on a closed Symbolizer.
var ErrClosed = errors.New("symbolize: closed")

// errSkippedMapFiles stands in for the first attempt's error once the
// map_files path has been latched off. It never escapes SymbolizeProcess.
var errSkippedMapFiles = errors.New("symbolize: map_files disabled")

// LocalSymbolizer wraps blazesym's Process source with no off-box hooks —
// preserves perf-agent's pre-debuginfod behavior. Used when no debuginfod
// URL is configured.
type LocalSymbolizer struct {
	bz     *blazesym.Symbolizer
	closed atomic.Bool
	// noMapFiles latches once /proc/<pid>/map_files/ has been proven
	// unusable for this process (see SymbolizeProcess). It is a
	// performance latch, not a correctness one: while false, every
	// symbolization pays a doomed first attempt.
	noMapFiles atomic.Bool
}

// NewLocalSymbolizer constructs a LocalSymbolizer with code-info and
// inlined-fns enabled (matches today's behavior at the three call sites).
func NewLocalSymbolizer() (*LocalSymbolizer, error) {
	bz, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		return nil, err
	}
	return &LocalSymbolizer{bz: bz}, nil
}

// SymbolizeProcess returns one Frame per IP. blazesym's Inlined chain is
// expanded into the Frame.Inlined slice in caller-most-to-callee order.
//
// blazesym's process source resolves each mapping through
// /proc/<pid>/map_files/<range>, which is inode-accurate: it cannot be
// fooled by a binary that was replaced or deleted since it was mapped.
// Following those magic symlinks is privileged — the kernel's
// proc_map_files_get_link() rejects the open with EPERM unless the caller
// holds CAP_CHECKPOINT_RESTORE (or CAP_SYS_ADMIN) — so on a perf-agent that
// was setcap'd with only cap_bpf,cap_perfmon the ENTIRE batch fails with
// "permission denied" and every frame degrades to a bare address. Nothing
// about that failure is visible in the returned error, because the fallback
// below deliberately does not propagate it.
//
// So: on any error, retry once with no_map_files, which reads the symbolic
// paths out of /proc/<pid>/maps instead. That is what every other profiler
// does and needs no capability at all; it is second choice only because a
// symbolic path can be re-pointed at different contents between the mmap and
// the read. Once the retry has actually rescued a batch we know map_files is
// closed to us for good, so the doomed first attempt is latched off.
//
// If the retry fails too — the usual reason being that the process exited and
// /proc/<pid>/maps is gone, "entity not found" — returns raw hex-named Frames
// instead of dropping the batch. That preserves stack shape and addresses so
// operators can decode with addr2line and the pprof's user mapping still has
// somewhere to attach. Those frames carry Reason == FailureMissingSymbols;
// callers that need to know symbolization resolved nothing must inspect
// Frame.Reason, because err is nil on this path (see
// gpuprobe.Stats.StacksUnresolved for a consumer that does).
func (s *LocalSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}
	opts := []blazesym.ProcessSourceOption{
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	}
	var syms []blazesym.Sym
	var err error
	if !s.noMapFiles.Load() {
		syms, err = s.bz.SymbolizeProcessAbsAddrs(ips, pid, opts...)
	} else {
		err = errSkippedMapFiles
	}
	if err != nil {
		// A fresh slice, not append(opts, ...): opts must not gain the
		// no_map_files option as a side effect if this function ever grows a
		// second use of it.
		retryOpts := make([]blazesym.ProcessSourceOption, 0, len(opts)+1)
		retryOpts = append(retryOpts, opts...)
		retryOpts = append(retryOpts, blazesym.ProcessSourceWithoutMapFiles(true))
		var retryErr error
		syms, retryErr = s.bz.SymbolizeProcessAbsAddrs(ips, pid, retryOpts...)
		if retryErr != nil {
			return rawUserAddrFrames(ips), nil
		}
		s.noMapFiles.Store(true)
	}
	out := make([]Frame, 0, len(syms))
	for i, sym := range syms {
		var addr uint64
		if i < len(ips) {
			addr = ips[i]
		}
		out = append(out, fromBlazesymSym(sym, addr))
	}
	return out, nil
}

// Close releases the underlying blazesym Symbolizer. Idempotent.
func (s *LocalSymbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.bz.Close()
	return nil
}

// fromBlazesymSym translates one blazesym.Sym into a Frame, populating
// Inlined in caller-most-to-callee order. addr is the abs IP this frame
// was resolved from.
//
// On per-IP miss (s.Name == "" — blazesym opened the binary but
// couldn't map this address to a symbol), Name is filled with the
// hex IP so the pprof Location renders as "0x<addr>" instead of
// "<unknown>". Symmetric to the kernel-side rawKernelAddrFrames /
// frameFromKernelCSym behavior — operators can decode with
// addr2line; <unknown> just hides the structure.
func fromBlazesymSym(s blazesym.Sym, addr uint64) Frame {
	name := s.Name
	reason := FailureNone
	if name == "" {
		name = fmt.Sprintf("0x%x", addr)
		reason = FailureMissingSymbols
	}
	f := Frame{
		Address: addr,
		Name:    name,
		Module:  s.Module,
		Offset:  s.Offset,
		Reason:  reason,
	}
	if s.CodeInfo != nil {
		f.File = s.CodeInfo.File
		f.Line = int(s.CodeInfo.Line)
		f.Column = int(s.CodeInfo.Column)
	}
	for _, in := range s.Inlined {
		inFrame := Frame{
			Address: addr,
			Name:    in.Name,
			Module:  s.Module,
		}
		if in.CodeInfo != nil {
			inFrame.File = in.CodeInfo.File
			inFrame.Line = int(in.CodeInfo.Line)
			inFrame.Column = int(in.CodeInfo.Column)
		}
		f.Inlined = append(f.Inlined, inFrame)
	}
	return f
}
