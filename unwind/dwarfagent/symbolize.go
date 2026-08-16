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

// symbolizePIDWithKernel resolves both user-mode and kernel-mode IPs for a
// single sample. Kernel frames are leaf-side and are prepended to the user
// frames so the resulting chain is leaf-first (kernel → user-leaf → … →
// user-root). pprof.Reverse() later flips this to outermost-first.
//
// When kernelIPs is empty (the typical case with --kernel-stacks off or
// stale BPF stack-IDs), behaves identically to symbolizePID. When user-mode
// symbolization fails we still emit synthetic "[unknown]" placeholders so
// the kernel frames don't appear hanging off an unrelated stack.
func symbolizePIDWithKernel(sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer, pid uint32, userIPs, kernelIPs []uint64) []pprof.Frame {
	userFrames := symbolizePID(sym, pid, userIPs)
	if len(kernelIPs) == 0 {
		return userFrames
	}
	kernelFrames, err := kernelSym.SymbolizeKernel(kernelIPs)
	if err != nil {
		log.Printf("dwarfagent: symbolize kernel: %v", err)
	}
	kf := symbolize.ToProfFramesKernel(kernelFrames)
	if len(kf) == 0 {
		return userFrames
	}
	out := make([]pprof.Frame, 0, len(kf)+len(userFrames))
	out = append(out, kf...)
	out = append(out, userFrames...)
	return out
}
