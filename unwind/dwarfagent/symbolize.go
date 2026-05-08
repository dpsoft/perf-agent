package dwarfagent

import (
	"log"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
)

// symbolizePID resolves ips for pid and returns pprof frames in the same
// order as ips. Failed IPs contribute a single synthetic "[unknown]"
// frame carrying the original PC as Address.
func symbolizePID(sym symbolize.Symbolizer, pid uint32, ips []uint64) []pprof.Frame {
	if len(ips) == 0 {
		return nil
	}
	frames, err := sym.SymbolizeProcess(pid, ips)
	if err != nil || len(frames) == 0 {
		if err != nil {
			log.Printf("dwarfagent: symbolize: %v", err)
		}
		out := make([]pprof.Frame, len(ips))
		for i := range out {
			out[i] = pprof.Frame{Name: "[unknown]", Address: ips[i]}
		}
		return out
	}
	return symbolize.ToProfFrames(frames)
}
