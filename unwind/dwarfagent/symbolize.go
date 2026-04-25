package dwarfagent

import (
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
)

// blazeSymToFrames converts one address's resolution (including
// inlined frames) into pprof.Frames in leaf-first order. addr is
// copied onto every frame so pprof Locations stay distinguishable.
func blazeSymToFrames(s blazesym.Sym, addr uint64) []pprof.Frame {
	out := make([]pprof.Frame, 0, 1+len(s.Inlined))
	for i := len(s.Inlined) - 1; i >= 0; i-- {
		in := s.Inlined[i]
		f := pprof.Frame{Name: in.Name, Module: s.Module, Address: addr}
		if in.CodeInfo != nil {
			f.File = in.CodeInfo.File
			f.Line = in.CodeInfo.Line
		}
		out = append(out, f)
	}
	outer := pprof.Frame{Name: s.Name, Module: s.Module, Address: addr}
	if s.CodeInfo != nil {
		outer.File = s.CodeInfo.File
		outer.Line = s.CodeInfo.Line
	}
	out = append(out, outer)
	return out
}

// symbolizePID resolves ips for pid and returns pprof frames in the
// same order as ips. Failed IPs contribute a single synthetic
// "[unknown]" frame carrying the original PC as Address.
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
		out := make([]pprof.Frame, len(ips))
		for i := range out {
			out[i] = pprof.Frame{Name: "[unknown]", Address: ips[i]}
		}
		return out
	}
	var out []pprof.Frame
	for i, s := range syms {
		var addr uint64
		if i < len(ips) {
			addr = ips[i]
		}
		out = append(out, blazeSymToFrames(s, addr)...)
	}
	return out
}
