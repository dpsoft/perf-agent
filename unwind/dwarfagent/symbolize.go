package dwarfagent

import (
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
)

// blazeSymToFrames converts a blazesym.Sym (one address's resolution,
// including any inlined frames) into one or more pprof.Frames in
// outermost-last order (innermost first, then outer real function last).
// Lifted from profile.Profiler's version to keep the two walkers
// byte-compatible at the frame layer.
//
// blazesym reports Inlined in outer→inner order; we walk it in
// reverse so the pprof stack has the innermost frame first.
func blazeSymToFrames(s blazesym.Sym) []pprof.Frame {
	out := make([]pprof.Frame, 0, 1+len(s.Inlined))
	for i := len(s.Inlined) - 1; i >= 0; i-- {
		in := s.Inlined[i]
		f := pprof.Frame{Name: in.Name, Module: s.Module}
		if in.CodeInfo != nil {
			f.File = in.CodeInfo.File
			f.Line = in.CodeInfo.Line
		}
		out = append(out, f)
	}
	outer := pprof.Frame{Name: s.Name, Module: s.Module}
	if s.CodeInfo != nil {
		outer.File = s.CodeInfo.File
		outer.Line = s.CodeInfo.Line
	}
	out = append(out, outer)
	return out
}

// symbolizePID resolves a slice of absolute user-space addresses for
// one PID and returns the corresponding pprof frames in the same order
// as ips. Missing IPs (unresolved by blazesym) contribute a single
// synthetic "[unknown]" frame each so chain depths stay meaningful.
func symbolizePID(sym *blazesym.Symbolizer, pid uint32, ips []uint64) []pprof.Frame {
	if len(ips) == 0 {
		return nil
	}
	syms, err := sym.SymbolizeProcessAbsAddrs(
		ips,
		pid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	if err != nil || len(syms) == 0 {
		// Blanket fallback: emit one [unknown] per IP.
		out := make([]pprof.Frame, len(ips))
		for i := range out {
			out[i] = pprof.FrameFromName("[unknown]")
		}
		return out
	}
	var out []pprof.Frame
	for _, s := range syms {
		out = append(out, blazeSymToFrames(s)...)
	}
	return out
}
