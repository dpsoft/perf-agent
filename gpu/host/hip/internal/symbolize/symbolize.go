package symbolize

import (
	blazesym "github.com/libbpf/blazesym/go"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func blazeSymToFrames(sym blazesym.Sym, addr uint64) []pp.Frame {
	out := make([]pp.Frame, 0, 1+len(sym.Inlined))
	for i := len(sym.Inlined) - 1; i >= 0; i-- {
		in := sym.Inlined[i]
		frame := pp.Frame{Name: in.Name, Module: sym.Module, Address: addr}
		if in.CodeInfo != nil {
			frame.File = in.CodeInfo.File
			frame.Line = in.CodeInfo.Line
		}
		out = append(out, frame)
	}
	frame := pp.Frame{Name: sym.Name, Module: sym.Module, Address: addr}
	if sym.CodeInfo != nil {
		frame.File = sym.CodeInfo.File
		frame.Line = sym.CodeInfo.Line
	}
	out = append(out, frame)
	return out
}
