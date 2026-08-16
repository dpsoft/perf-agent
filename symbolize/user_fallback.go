package symbolize

import "fmt"

// rawUserAddrFrames synthesizes Frames carrying just the raw IPs when
// blazesym (or any user-side symbolizer) couldn't resolve them. The
// caller wraps the slice into pprof — stack shape and addresses are
// preserved so operators can post-process with addr2line, instead of
// the whole user side disappearing.
//
// Use case: profiling a process whose /proc/<pid>/exe is restricted
// by ptrace_scope (a setcap'd target, e.g., perf-agent itself), or
// when DebuginfodSymbolizer can't find an upstream debug file for a
// locally-built binary. Without this fallback, perf-agent #2's
// flamegraph of perf-agent #1 was 100% [unknown] — the discovery
// case that motivated this fix.
//
// Module is left empty: the pprof builder routes user-side Locations
// through their /proc/<pid>/maps-derived mapping (Bug 3 fix), so the
// mapping's filename still appears next to the raw-hex name in
// downstream tooling.
func rawUserAddrFrames(ips []uint64) []Frame {
	out := make([]Frame, len(ips))
	for i, ip := range ips {
		out[i] = Frame{
			Address: ip,
			Name:    fmt.Sprintf("0x%x", ip),
			Reason:  FailureMissingSymbols,
		}
	}
	return out
}
