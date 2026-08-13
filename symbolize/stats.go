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
// "blazesym broke" / "frames dropped to raw-hex" observable so a
// self-profile lane or /metrics scrape can alert.
type Counters struct {
	// KernelBatches is the number of SymbolizeKernel calls that
	// reached the blazesym layer (after the empty-input
	// short-circuit).
	KernelBatches atomic.Uint64

	// KernelInputIPs is the total number of kernel IPs handed into
	// SymbolizeKernel across all batches.
	KernelInputIPs atomic.Uint64

	// KernelBatchFailures is the number of batches where blazesym
	// returned an error, forcing the raw-address synthesis path.
	KernelBatchFailures atomic.Uint64

	// KernelRawAddrFrames is the cumulative count of kernel IPs
	// that fell to the raw-address synthesis path (Frame.Name =
	// "0x<hex>"). High values mean blazesym failed — likely a
	// configuration problem on the host.
	KernelRawAddrFrames atomic.Uint64

	// Reason-bucketed counters for batch-level blazesym failures.
	// Roadmap #4: lets operators distinguish "lockdown-class host
	// (blazesym EPERMs, dropping kernel frames to raw addresses)"
	// from "blazesym is throwing some other error" without
	// re-instrumenting.
	//
	// KernelLockdownEPERM bumps each time blazesym returns
	// BLAZE_ERR_PERMISSION_DENIED. Post-v0.2.4 this should only fire
	// on the narrow lockdown + /boot/vmlinux-DWARF-installed host
	// class; a steady stream of these means kernel frames are
	// dropping to raw addresses and the host needs attention.
	KernelLockdownEPERM atomic.Uint64

	// KernelOtherErr bumps when blazesym returns a non-EPERM
	// failure (some other CGO-side error from the Rust library).
	// Should normally be ~0; non-zero in production deserves a
	// look at the log lines surrounding the failure.
	KernelOtherErr atomic.Uint64

	// KernelBatchHist records per-SymbolizeKernel-call wall-clock
	// duration (microseconds) for the recent window. Snapshots
	// expose p50/p99 so operators can alert on tail latency without
	// trace-level logging. Sized for sliding-window semantics
	// (latencyHistSize=1024).
	KernelBatchHist LatencyHist
}

// CountersSnapshot is a value-type point-in-time view of Counters.
// Returned by (*Counters).Snapshot so callers can read consistently
// without racing against in-flight Add calls (which only updates
// individual fields atomically, not the struct as a whole — fine for
// observability reads).
type CountersSnapshot struct {
	KernelBatches       uint64
	KernelInputIPs      uint64
	KernelBatchFailures uint64
	KernelRawAddrFrames uint64
	KernelLockdownEPERM uint64
	KernelOtherErr      uint64
	KernelBatchHist     LatencyHistSnapshot
}

// Snapshot returns the current counter values as a plain struct.
func (c *Counters) Snapshot() CountersSnapshot {
	return CountersSnapshot{
		KernelBatches:       c.KernelBatches.Load(),
		KernelInputIPs:      c.KernelInputIPs.Load(),
		KernelBatchFailures: c.KernelBatchFailures.Load(),
		KernelRawAddrFrames: c.KernelRawAddrFrames.Load(),
		KernelLockdownEPERM: c.KernelLockdownEPERM.Load(),
		KernelOtherErr:      c.KernelOtherErr.Load(),
		KernelBatchHist:     c.KernelBatchHist.Snapshot(),
	}
}

// String formats the snapshot as a one-line log message — emitted at
// agent shutdown so operators see frame drops without having to add a
// metrics scrape.
func (s CountersSnapshot) String() string {
	return fmt.Sprintf(
		"symbolize: batches=%d input_ips=%d batch_failures=%d raw_addr_frames=%d eperm=%d other_err=%d batch_p50_us=%d batch_p99_us=%d batch_max_us=%d",
		s.KernelBatches, s.KernelInputIPs, s.KernelBatchFailures, s.KernelRawAddrFrames,
		s.KernelLockdownEPERM, s.KernelOtherErr,
		s.KernelBatchHist.P50Us, s.KernelBatchHist.P99Us, s.KernelBatchHist.MaxUs,
	)
}
