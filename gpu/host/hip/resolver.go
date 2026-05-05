package hip

import (
	"fmt"

	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/gpu/host/hip/internal/stackmap"
	hipsymbolize "github.com/dpsoft/perf-agent/gpu/host/hip/internal/symbolize"
	pp "github.com/dpsoft/perf-agent/pprof"
)

type processSymbolizer interface {
	SymbolizeProcessAbsAddrs(addrs []uint64, pid uint32, options ...blazesym.ProcessSourceOption) ([]blazesym.Sym, error)
	Close()
}

type stackBytesLookup interface {
	LookupBytes(key interface{}) ([]byte, error)
}

func resolveKernelName(sym processSymbolizer, pid uint32, addr uint64) string {
	if sym != nil {
		syms, err := sym.SymbolizeProcessAbsAddrs(
			[]uint64{addr},
			pid,
			blazesym.ProcessSourceWithPerfMap(true),
			blazesym.ProcessSourceWithDebugSyms(true),
		)
		if err == nil && len(syms) > 0 && syms[0].Name != "" {
			return syms[0].Name
		}
	}
	return fmt.Sprintf("hip_kernel@%#x", addr)
}

func resolveStackFrames(sym processSymbolizer, stacks stackBytesLookup, pid uint32, stackID int32) []pp.Frame {
	if sym == nil || stacks == nil || stackID < 0 {
		return nil
	}

	raw, err := stacks.LookupBytes(uint32(stackID))
	if err != nil || len(raw) == 0 {
		return nil
	}

	ips := stackmap.ExtractIPs(raw)
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
		frames := make([]pp.Frame, 0, len(ips))
		for _, ip := range ips {
			frames = append(frames, pp.Frame{Name: "[unknown]", Address: ip})
		}
		return frames
	}

	frames := make([]pp.Frame, 0, len(ips))
	for i, sym := range syms {
		if i >= len(ips) {
			break
		}
		frames = append(frames, hipsymbolize.BlazeSymToFrames(sym, ips[i])...)
	}
	return frames
}
