package symbolize

import (
	"errors"
	"sync/atomic"

	blazesym "github.com/libbpf/blazesym/go"
)

// ErrClosed is returned from operations on a closed Symbolizer.
var ErrClosed = errors.New("symbolize: closed")

// LocalSymbolizer wraps blazesym's Process source with no off-box hooks —
// preserves perf-agent's pre-debuginfod behavior. Used when no debuginfod
// URL is configured.
type LocalSymbolizer struct {
	bz     *blazesym.Symbolizer
	closed atomic.Bool
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
func (s *LocalSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}
	syms, err := s.bz.SymbolizeProcessAbsAddrs(
		ips,
		pid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	if err != nil {
		return nil, err
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
func fromBlazesymSym(s blazesym.Sym, addr uint64) Frame {
	f := Frame{
		Address: addr,
		Name:    s.Name,
		Module:  s.Module,
		Offset:  s.Offset,
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
