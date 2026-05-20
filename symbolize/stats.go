package symbolize

import (
	"fmt"
	"sync/atomic"
)

// Counters tracks observability counters for the kernel symbolizer.
// All fields are safe for concurrent use; bump via the atomic Add /
// Store methods, read a consistent point-in-time view via Snapshot.
//
// Why: under kernel lockdown=integrity the v1.2.0 M1 symbolizer
// silently dropped every kernel frame for the lifetime of the agent
// — nothing surfaced the problem to operators. These counters make
// "blazesym broke, fallback engaged" / "frames dropped to raw-hex"
// observable so a self-profile lane or /metrics scrape can alert.
type Counters struct {
	// KernelBatches is the number of SymbolizeKernel calls that
	// reached the blazesym/fallback layer (after the empty-input
	// short-circuit).
	KernelBatches atomic.Uint64

	// KernelInputIPs is the total number of kernel IPs handed into
	// SymbolizeKernel across all batches.
	KernelInputIPs atomic.Uint64

	// KernelBatchFailures is the number of batches where every
	// symbolizer (blazesym + kallsyms fallback) returned an error,
	// forcing the raw-address synthesis path.
	KernelBatchFailures atomic.Uint64

	// KernelFallbackEngaged is 1 once the symbolizer has switched
	// from the blazesym path to the pure-Go /proc/kallsyms path
	// (sticky for the symbolizer's lifetime). Non-zero is the
	// canary for lockdown=integrity hosts.
	KernelFallbackEngaged atomic.Uint64

	// KernelRawAddrFrames is the cumulative count of kernel IPs
	// that fell to the raw-address synthesis path (Frame.Name =
	// "0x<hex>"). High values mean blazesym + kallsyms both failed
	// — likely a configuration problem on the host.
	KernelRawAddrFrames atomic.Uint64
}

// CountersSnapshot is a value-type point-in-time view of Counters.
// Returned by (*Counters).Snapshot so callers can read consistently
// without racing against in-flight Add calls (which only updates
// individual fields atomically, not the struct as a whole — fine for
// observability reads).
type CountersSnapshot struct {
	KernelBatches         uint64
	KernelInputIPs        uint64
	KernelBatchFailures   uint64
	KernelFallbackEngaged uint64
	KernelRawAddrFrames   uint64
}

// Snapshot returns the current counter values as a plain struct.
func (c *Counters) Snapshot() CountersSnapshot {
	return CountersSnapshot{
		KernelBatches:         c.KernelBatches.Load(),
		KernelInputIPs:        c.KernelInputIPs.Load(),
		KernelBatchFailures:   c.KernelBatchFailures.Load(),
		KernelFallbackEngaged: c.KernelFallbackEngaged.Load(),
		KernelRawAddrFrames:   c.KernelRawAddrFrames.Load(),
	}
}

// String formats the snapshot as a one-line log message — emitted at
// agent shutdown so operators see fallback engagement and frame
// drops without having to add a metrics scrape.
func (s CountersSnapshot) String() string {
	return fmt.Sprintf(
		"symbolize: batches=%d input_ips=%d batch_failures=%d fallback_engaged=%d raw_addr_frames=%d",
		s.KernelBatches, s.KernelInputIPs, s.KernelBatchFailures, s.KernelFallbackEngaged, s.KernelRawAddrFrames,
	)
}
