// Package gpuprobe consumes the perfagent GPU USDT ABI from a shim library
// and feeds the normalized events into a gpu.EventSink.
//
// The shim emits fixed-size records (shim/core/usdt_abi.h, mirrored by
// internal/gpuabi) in batches. Each probe fire hands the BPF program a
// pointer, a record count and a per-probe monotonic sequence number; the
// program copies the batch out of user memory into a ringbuf, and this
// package decodes it and normalizes it into gpu events.
//
// Nothing is lost silently (spec §6.1). Batches the kernel could not deliver
// are counted in the BPF-side `dropped` map and surfaced as
// Stats.KernelDropped; gaps in a probe's sequence numbers are counted as
// Stats.SequenceGaps; record kinds this phase carries but does not yet
// normalize are counted as Stats.Undecoded; records whose wire correlation is
// zero — the ABI's "no correlation", which demotes them to the timeline's
// heuristic join — are counted as Stats.ZeroCorrelation, and separately in
// Stats.ZeroCorrelationExecs and Stats.PCSamplesWithoutCorrelation, because
// those two populations have opposite healthy readings and one counter
// cannot be read for both. Samples that did not
// decode at all are counted as Stats.Malformed, and *why* they did not decode
// is kept in Stats.DecodeFailures: a count of malformed samples with no
// reason attached is loss that is visible but not diagnosable.
package gpuprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/dpsoft/perf-agent/internal/kernelver"
	"github.com/dpsoft/perf-agent/internal/usdt"
	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/interp"
)

const (
	kindLaunch         = 1
	kindExec           = 2
	kindModule         = 3
	kindPC             = 4
	kindLaunchSampled  = 5
	kindKernelName     = 6
	kindStallMap       = 7
	kindSamplingWindow = 8
	kindConfig         = 9
	// kindDropped carries producer-side loss by class. The probe has existed
	// in the ABI header since Phase 3 with no kind and no cookie, so it was
	// never attached and never fired; without it every drop class the shim
	// could define would be a counter that could not go non-zero, which is
	// the failure mode this project has hit twelve times.
	kindDropped = 10

	// kindMax mirrors KIND_MAX in bpf/gpu_usdt.bpf.c: the number of slots in
	// the BPF-side `dropped` and `stacks_missing` arrays.
	//
	// It is larger than the highest kind on purpose. Resizing those arrays is
	// a map-layout change on both sides at once, so it is done once with
	// headroom rather than on every probe added. The pair is NOT free to
	// drift: a Go value larger than the C one reads slots the map does not
	// have and every lookup fails silently, so a drop storm reports zero
	// drops; a Go value smaller never reads the top kinds at all, with the
	// same symptom. Neither end errors. TestKindMaxPinsTheBPFDropAccountingArrays
	// pins this against the embedded object, which is the only artefact that
	// can settle the question.
	kindMax = 16

	// maxWalkFrames mirrors MAX_FRAMES in bpf/unwind_common.h: the walker's
	// bpf_loop bound, and therefore the length of a struct gpu_stack's pcs
	// array. A walk that produced exactly this many frames AND did not reach
	// a natural terminator (no walkerFlagsTerminated bit) AND did not stop
	// for a named reason of its own (no walkerFlagFPExhausted) hit the bound
	// and is truncated (Stats.StackWalkTruncated); one that reached the
	// bound and also terminated happens to be a complete stack exactly this
	// deep, not a truncated one.
	maxWalkFrames = 127

	// frameTagNative mirrors FRAME_TAG_NATIVE in bpf/unwind_record.h: the
	// kind of each pcs[] slot. A native slot holds one instruction pointer;
	// any other tag value is an UNWINDER ID and marks the first of TWO
	// consecutive slots written by that unwinder, neither of which is an
	// instruction pointer. Symbolizing either as a native PC invents a frame,
	// so resolveStackLocked folds them out (interp.SplitSlots) before the
	// symbolizer ever sees the addresses.
	frameTagNative = interp.FrameTagNative

	// The walkerFlag* constants mirror WALKER_FLAG_* in
	// bpf/unwind_common.h. They ride in struct gpu_stack's walker_flags and
	// say how the walk went, not just how far it got:
	//
	//   - walkerFlagDWARFUsed set means at least one frame was unwound via
	//     CFI. Clear means every frame the walk kept was reached by frame
	//     pointer alone — the shape that produced a flame graph rooted in
	//     the profiler's own callback on real hardware, because an FP walk
	//     cannot survive the first frame-pointer-omitting vendor frame.
	//   - walkerFlagFPTerminated set means the FP chain reached its natural
	//     end: a frame whose saved-FP slot held zero, which is the x86-64
	//     psABI's outermost-frame marker (_start zeroes %rbp before calling
	//     __libc_start_main; the clone child does the same). Since issue #45
	//     walk_step does not stop there - the return address stored beside
	//     that zero is a real caller PC, so it takes ONE further step with
	//     fp == 0 to let the unwind tables confirm the root. So this bit and
	//     walkerFlagRAUndefined are no longer mutually exclusive: a hybrid
	//     walk on glibc normally ends with BOTH, which is the strongest
	//     ending there is.
	//   - walkerFlagRAUndefined set means a frame's CFI gives the return
	//     address as UNDEFINED — the DWARF marker for an outermost frame,
	//     which glibc emits for _start and for thread entry points. The
	//     unwind information itself says the chain ends here, so this is the
	//     DWARF-side counterpart of walkerFlagFPTerminated and must be read
	//     together with it: a hybrid walk that crossed a frame-pointer-less
	//     frame CANNOT end via saved_fp == 0, because the DWARF step that
	//     carried it there set the frame pointer to zero.
	//   - walkerFlagFPExhausted set means the walk arrived at an FP_SAFE
	//     frame with no frame pointer to follow (ctx->fp == 0) and stopped.
	//     This is a FAILURE, not an end of chain, and the two used to share
	//     one bit (issue #44). Nothing said that frame was outermost; the
	//     frame pointer was lost one step earlier, when the DWARF rules of
	//     the FP_LESS frame below gave no location for it. Whatever called
	//     the FP_SAFE frame is real and is missing from the stack. See
	//     Stats.StackWalkFPExhausted for why it gets its own counter.
	//     walk_step does NOT set it when the frame pointer is zero because
	//     the chain ended legitimately one step earlier - that case already
	//     carries walkerFlagFPTerminated and is a success.
	//   - walkerFlagFPNonMonotonic set means a frame's saved-FP slot held a
	//     value that is neither zero nor above the current frame pointer, so
	//     the chain could not be followed. Also a FAILURE, and also a subset
	//     of Stats.StackWalkAbandoned, but pointing at a corrupt or
	//     hand-rolled frame rather than at unwind tables that dropped the
	//     register. The return address read out of the same frame IS still
	//     recorded before the walk stops.
	//   - walkerFlagRootDisagreement set means the step past the end of the
	//     frame-pointer chain landed on a frame whose CFI says a caller
	//     EXISTS instead of declaring it outermost: the two sources disagree
	//     about where the stack ends. Always arrives with
	//     walkerFlagFPTerminated, and its whole job is to stop that bit from
	//     reading as an unqualified success. See
	//     Stats.StackWalkRootDisagreement.
	//
	//     With walkerFlagFPTerminated and walkerFlagRAUndefined both clear
	//     the walk stopped for a reason it could do nothing about — a
	//     user-memory read fault at a live address, a lost frame pointer
	//     (walkerFlagFPExhausted), a non-monotonic frame pointer, a CFI
	//     lookup miss, an RA/FP location this walker does not track, or
	//     bpf_loop exhausting MAX_FRAMES — and did NOT reach the root, even
	//     though it may have produced usable frames.
	//   - walkerFlagCFIMiss set means the walker classified a frame as
	//     frame-pointer-less, went looking for the CFI entry covering it,
	//     and found none — so it stopped. Before Task 3 this could not
	//     happen in gpuprobe at all (no PID had tables, so no frame ever
	//     reached the lookup); now that registration installs them, it
	//     separates "the tables are missing" from "the tables are there and
	//     do not cover this PC". See Stats.StacksWalkedCFIMiss.
	walkerFlagFPTerminated     = 0x01
	walkerFlagDWARFUsed        = 0x02
	walkerFlagCFIMiss          = 0x04
	walkerFlagRAUndefined      = 0x08
	walkerFlagFPExhausted      = 0x10
	walkerFlagFPNonMonotonic   = 0x20
	walkerFlagRootDisagreement = 0x40
	// walkerFlagFramePushRefused mirrors WALKER_FLAG_FRAME_PUSH_REFUSED
	// (issue #83): frame_push_native or frame_push_python ran out of
	// pcs[]/tags[] room and dropped a frame rather than write it.
	// Deliberately NOT folded into the classification switch below: the
	// switch partitions how a walk ENDED, and a refused push is a frame
	// missing from the middle of a walk that ended some other way. Which
	// pusher hit it is recorded on the BPF side instead, by whichever
	// unwinder was pushing, in its own counters.
	walkerFlagFramePushRefused = 0x80

	// walkerFlagsTerminated is the set of bits that mean "the walk reached
	// the end of the chain". Neither StackWalkTruncated nor
	// StackWalkAbandoned may count a capture with any of them set.
	//
	// walkerFlagFPExhausted and walkerFlagFPNonMonotonic are deliberately NOT
	// in it. They mark walks that could not continue, which is exactly what
	// StackWalkAbandoned counts; including the first is the bug issue #44
	// describes, where a walk stopped mid-stack by a lost frame pointer read
	// as a clean termination and no counter moved.
	//
	// walkerFlagRootDisagreement is not in it either, and it OVERRIDES the
	// bits that are: it can only arrive alongside walkerFlagFPTerminated,
	// and it says that termination was contradicted by the unwind tables.
	// See the classification switch, which tests it first.
	walkerFlagsTerminated = walkerFlagFPTerminated | walkerFlagRAUndefined

	// gpuStackHdrSize, gpuStackTagsOff and gpuStackSize mirror struct
	// gpu_stack in bpf/gpu_usdt.bpf.c:
	//
	//	0 n_pcs  4 walker_flags  8 pcs[MAX_FRAMES]  1024 tags[MAX_FRAMES]  1151 pad
	//
	// The value is fixed-size because a BPF map value is; n_pcs says how
	// much of it is real. TestEmbeddedProgramCarriesTheStackMap pins
	// gpuStackSize against the embedded object's own value size, so a change
	// on either side fails a unit test rather than decoding garbage.
	//
	// tags[] arrived with issue #83: one FRAME_TAG_* byte per pcs[] slot,
	// because an interpreter frame occupies two consecutive slots and,
	// without the tag, reaches this consumer indistinguishable from two
	// native PCs.
	// gpuStackPadBytes is the compiler's own alignment pad after the u8
	// array - spelled out rather than folded into a magic number, the same
	// way unwind/dwarfagent's SampleRecordBytes had to be after getting this
	// exact arithmetic wrong once.
	gpuStackHdrSize  = 8
	gpuStackTagsOff  = gpuStackHdrSize + maxWalkFrames*8
	gpuStackPadBytes = 1
	gpuStackSize     = gpuStackTagsOff + maxWalkFrames + gpuStackPadBytes

	// gpuStackCapacity mirrors GPU_STACKS_SIZE: how many captures gpu_stacks
	// can hold at once, i.e. how many may be in flight between the probe and
	// the consumer's drain before the BPF side starts refusing them.
	gpuStackCapacity = 4096

	// The walk_errors slots, mirroring WALK_ERR_* in bpf/gpu_usdt.bpf.c.
	// Every way capture_stack can fail lands in exactly one of them.
	walkErrNoScratch = 0
	walkErrEmpty     = 1
	walkErrMapFull   = 2
	walkErrUpdate    = 3
	walkErrMax       = 4

	// batchHdrSize mirrors struct batch_hdr in bpf/gpu_usdt.bpf.c, which is
	// 40 bytes since Phase 4a appended stack_id and its padding:
	//
	//   0 kind  4 count  8 seq  16 pid  20 tid  24 bytes  32 stack_id  36 _pad
	//
	// The C side carries a _Static_assert on the same number. The two must
	// move together in one commit: a mismatch does not error anywhere, it
	// just decodes every field from the wrong offset.
	batchHdrSize = 40

	// usdtProvider is the only provider whose probes this consumer attaches.
	usdtProvider = "perfagent"
)

// cookieFor maps a USDT probe name to the BPF attach cookie that tells the
// program which record kind fired. The values are the KIND_* constants in
// bpf/gpu_usdt.bpf.c; a mismatch would decode garbage silently, so the pair
// is pinned by a unit test. Zero means "not a probe we know", and such
// probes are not attached at all.
func cookieFor(probeName string) uint64 {
	switch probeName {
	case "gpu_launch_v1":
		return kindLaunch
	case "gpu_exec_v1":
		return kindExec
	case "gpu_module_load_v1":
		return kindModule
	case "gpu_pc_sample_batch_v1":
		return kindPC
	case "gpu_launch_sampled_v1":
		return kindLaunchSampled
	case "gpu_kernel_name_v1":
		return kindKernelName
	case "gpu_stall_reason_map_v1":
		return kindStallMap
	case "gpu_sampling_window_v1":
		return kindSamplingWindow
	case "gpu_config_v1":
		return kindConfig
	case "gpu_dropped_v1":
		return kindDropped
	}
	return 0
}

// Config describes what to attach to and where the normalized events go.
type Config struct {
	// ShimPath is the ELF carrying the perfagent USDT probes.
	ShimPath string
	// PID restricts the attachment to one process; zero is system-wide.
	PID int
	// Backend labels the correlation IDs the consumer produces.
	Backend gpu.GPUBackendID
	// Sink receives the normalized events.
	Sink gpu.EventSink

	// Symbolizer resolves the instruction pointers of a sampled launch's
	// captured stack into frames. Nil disables resolution entirely: the
	// captures still arrive and are still accounted for (each one counts in
	// Stats.SymbolizeFailed), and every launch still reaches the sink -
	// without a stack, which projects as unattributed GPU time rather than
	// as a guess.
	Symbolizer symbolize.Symbolizer

	// SampledStackCapacity bounds the correlation -> stack side table that
	// holds a resolved capture until its batched twin arrives. Zero means
	// defaultSampledStackCapacity. See pendingStacks for why this table
	// exists and why it must be bounded.
	SampledStackCapacity int

	// DeferredLaunchCapacity bounds how many launches may be held at once
	// waiting for a sampled twin that has not arrived yet. Zero means
	// defaultDeferredLaunchCapacity. See deferredLaunches.
	DeferredLaunchCapacity int

	// KernelNameCapacity bounds the kernel_id -> name table. Zero means
	// defaultKernelNameCapacity. A ceiling, not a dial: a workload has one
	// entry per distinct kernel, but nothing in the ABI promises that.
	KernelNameCapacity int

	// PendingNamedEventCapacity bounds how many launches and executions may
	// be held at once waiting for a kernel name that has not arrived yet.
	// Zero means defaultPendingNamedEventCapacity. See pendingNames.
	PendingNamedEventCapacity int

	// StallNameCapacity bounds the stall_index -> name table. Zero means
	// defaultStallNameCapacity. A ceiling, not a dial: a device's stall
	// reasons are a fixed enum, but nothing on the wire promises that.
	StallNameCapacity int

	// PendingStallSampleCapacity bounds how many PC samples may be held at
	// once waiting for a stall name that has not arrived yet. Zero means
	// defaultPendingStallSampleCapacity. See pendingStallSamples.
	PendingStallSampleCapacity int

	// UnwindPIDCapacity bounds how many processes hold CFI tables for the
	// stack walker at once. Zero means defaultUnwindPIDCapacity. A
	// system-wide attach learns PIDs from the records that arrive, so the
	// set is fed by a profiled machine and has to be bounded; see
	// pidRegistry for the bound and Stats.UnwindPIDsEvicted for what it
	// costs when it bites.
	UnwindPIDCapacity int

	// CubinMaxBytes bounds ONE cubin offered over the cubin transport
	// (cubin.go). Zero means defaultCubinMaxBytes. An offer over the ceiling
	// is rejected whole and counted in CubinsRejectedTooLarge; it is never
	// truncated to fit, because a truncated cubin parses into a wrong line
	// table, which is the one failure worse than no line table.
	CubinMaxBytes int

	// Modules is the store every cubin that arrives over the cubin transport
	// (cubin.go) is written into. It is the ONE hop between the bytes
	// crossing the socket and gpu_src_status meaning anything: without it the
	// cubins are received, sealed, verified, size-checked and stored where
	// nothing reads them, and every PC sample in the profile reads
	// "no-module" (issue #93).
	//
	// It is INJECTED rather than owned, and the caller must hand the SAME
	// store to gpu.TimelineConfig.Modules (which resolves a pending PC group's
	// (crc, functionIndex) to a device function name) and to
	// gpu.ProjectionConfig.Modules (which resolves the source location). One
	// store, three references, no second copy of the bytes - a store this
	// package constructed for itself would be one the join and the projection
	// cannot see, which is exactly the shape of the bug this field closes.
	//
	// Nil is supported and is what every backend that does not resolve source
	// lines passes. With no store the channel still binds and offers are still
	// admitted, authenticated and counted; they land in a bounded placeholder
	// nothing reads, and every PC sample carries gpu_src_status="no-module".
	// That is the same fact for the reader as a cubin that never arrived (see
	// gpu.SrcNoModule), and it is deliberately not silent: a profile that says
	// "no-module" on every sample points straight at the missing store, while
	// one with the source labels absent is indistinguishable from a profile
	// taken before this phase existed.
	//
	// The store's own bounds (gpu.ModuleStoreConfig.Capacity and MaxBytes) are
	// the caller's, and they are enforced by eviction: a module the store drops
	// answers "no-module" from then on, never a stale line. CubinMaxBytes and
	// CubinTotalBytes below bound the TRANSPORT and are separate from them.
	Modules *gpu.ModuleStore

	// CubinTotalBytes bounds every cubin this consumer will hold. Zero means
	// defaultCubinTotalBytes. This is the memory a JIT- or template-explosion
	// workload can make the agent hold, so it is a ceiling rather than a dial;
	// an offer that would pass it is refused and counted, again in
	// CubinsRejectedTooLarge.
	CubinTotalBytes int64
}

const (
	// defaultSampledStackCapacity bounds the parked-stack side table. A
	// parked stack is one whose launch has not arrived (or never will,
	// because the batch carrying it was dropped), so the table has to be
	// bounded or a profiled application feeds it without limit. 1024 is
	// generous next to the real occupancy - a capture normally waits less
	// than one batch - and still trivially small in memory.
	defaultSampledStackCapacity = 1024

	// defaultDeferredLaunchCapacity bounds the launches held for a possible
	// sampled twin. Only launches from the most recent batch are ever held
	// (see Consumer.applyBatch), and a shim batch is 32 records, so this is
	// several times the working set; it exists as a hard ceiling for a
	// producer that batches differently, not as a tuning dial.
	defaultDeferredLaunchCapacity = 256

	// defaultKernelNameCapacity bounds the interned-name table. Real
	// workloads have tens to low hundreds of distinct kernels; 4096 leaves
	// room for a generated or JIT-heavy one while keeping the table's worst
	// case at a few hundred KB.
	defaultKernelNameCapacity = 4096

	// defaultPendingNamedEventCapacity bounds the events held for a name
	// that has not arrived. The window it has to cover is the producer's
	// drain interval (100ms in the shim), so it is sized for a burst of
	// launches and execs rather than for steady state - in steady state the
	// name is already interned and nothing waits at all.
	defaultPendingNamedEventCapacity = 512

	// defaultStallNameCapacity bounds the interned stall-reason table. A
	// device's stall reasons are a fixed enum - 38 on GA102 - so 256 is an
	// order of magnitude of headroom over anything a CUPTI device can
	// report, and a run that evicts from this table is a run whose producer
	// is not describing one device.
	defaultStallNameCapacity = 256

	// defaultPendingStallSampleCapacity bounds the PC samples held for a
	// stall name that has not arrived. The window it has to cover is the
	// gap between the consumer attaching and the producer's late-attach
	// replay of the stall map - up to one drain interval (100ms in the
	// shim) - during which PC batches arrive at their full rate. It is
	// sized like Timeline's own pendingSampleCap for that reason. Nothing
	// is dropped when it bites: the oldest sample is RELEASED, unresolved
	// and counted in StallNamesMissing.
	defaultPendingStallSampleCapacity = 4096
)

// Stats is the consumer's loss record. Every discard has a counter here or
// in KernelDropped; none of them is silent.
type Stats struct {
	// Batches and Records count what arrived and was normalized.
	Batches uint64
	Records uint64
	// SequenceGaps counts *batches* the consumer never saw, implied by a jump
	// in a probe's monotonic sequence number. The shim increments seq_ once
	// per batch (shim/core/batch.h), so one gap is one whole batch of up to
	// MAX_RECORDS_PER_BATCH records, not one record. It is therefore not
	// additive with KernelDropped or Records, which are both in records:
	// a gap is loss the consumer cannot size, only detect.
	SequenceGaps uint64
	// SinkRejected counts events the sink refused (full, or invalid).
	SinkRejected uint64
	// Undecoded counts records of a kind this phase carries on the wire but
	// does not decode at all. EVERY kind the ABI defines now has an applyBatch
	// arm: launches, executions, sampled launches, kernel names, module loads,
	// PC samples, the stall-reason map, sampling windows, the config record
	// and - since issue #94 gave the CUDA-graph refusal something to consume -
	// gpu_dropped_v1. So anything that reaches here is a kind the producer
	// invented, or a KIND_* added on one side of the wire and not the other,
	// both of which are silent loss unless counted.
	//
	// The successor for the one kind that used to land here is
	// DropsUnconsumed: a drop class this side decodes but does not yet act on
	// is counted there rather than left to look like an unknown kind, because
	// the two call for completely different responses.
	//
	// Healthy: zero, in every tier and on every producer, and gate_test.go
	// asserts it as an equality. Worst: equal to the record count of every
	// batch of some kind the consumer cannot read, which is the ABI having
	// drifted.
	Undecoded uint64
	// Malformed counts ringbuf samples that did not decode: a short header,
	// a payload shorter than the header claims, or a truncated record.
	// DecodeFailures says *why*; a bare count of malformed samples is a
	// symptom nobody can act on.
	Malformed uint64
	// DecodeFailures is why the Malformed samples were malformed: the first
	// maxDecodeFailureReasons distinct reasons, deduplicated by error text,
	// each with how many samples failed that way. It is the operator-facing
	// half of Malformed — an ABI drift, a producer emitting a record the
	// decoder rejects, or a truncated batch all read as "Malformed++"
	// otherwise, and the three call for completely different responses.
	//
	// Bounded on purpose: a producer that fails every sample the same way
	// must cost one table entry, not one entry per sample. The counts
	// therefore sum to Malformed only when nothing was crowded out; see
	// DecodeReasonsUnrecorded for the shortfall.
	DecodeFailures []DecodeFailure
	// DecodeReasonsUnrecorded counts decode failures whose reason did not
	// fit in the bounded table because maxDecodeFailureReasons distinct
	// reasons had already been seen. Those samples are still in Malformed;
	// only their reason was dropped, and that drop is not silent either.
	//
	//	Malformed = sum(DecodeFailures[i].Count) + DecodeReasonsUnrecorded
	DecodeReasonsUnrecorded uint64
	// ZeroCorrelation counts records that arrived carrying a wire correlation
	// of zero, which the ABI defines as "no correlation" (shim/core/usdt_abi.h;
	// spec §6.3 finding 3 makes it the normal case for PC samples in the
	// continuous collection mode this project ships). Those records are NOT
	// lost — they are normalized with the zero gpu.CorrelationID, which routes
	// them to the timeline's heuristic join instead of the exact one. The
	// counter exists because that demotion changes how confidently the join
	// can be read, and a silent demotion is as bad as silent loss.
	//
	// It is an aggregate over populations whose healthy values are
	// OPPOSITE, so it must never be read alone. ZeroCorrelationExecs and
	// PCSamplesWithoutCorrelation are the two subsets that carry the
	// meaning; see both.
	ZeroCorrelation uint64
	// PCSamplesWithoutCorrelation is the subset of ZeroCorrelation that
	// arrived on a gpu_pc_sample_batch_v1 record. It is the PC-sample twin
	// of ZeroCorrelationExecs and exists for the same reason: one counter
	// cannot serve two populations whose healthy readings are opposites.
	//
	// In Tier B (CONTINUOUS) CUPTI populates no correlation at all, so
	// EVERY PC record carries zero by design and this counter equals
	// PCSamplesDecoded on a perfectly healthy run. In Tier A
	// (KERNEL_SERIALIZED) CUPTI populates it on every record - the spike
	// measured 1,828 of 1,828 - so a single non-zero here is the shim
	// breaking Tier A's whole claim, that a PC sample joins to a launch
	// exactly.
	//
	// That is why it is not folded into ZeroCorrelation: with PC samples
	// dominating the traffic, ZeroCorrelation is enormous and healthy in
	// Tier B, and a Tier A contract violation would be invisible inside it
	// - the same defect issue #52 found for executions. Nothing is dropped
	// either way; the sample is normalized with the zero CorrelationID,
	// which routes it to the module-granularity attribution Tier B is
	// built on.
	//
	// Healthy: zero in Tier A, PCSamplesDecoded in Tier B. Worst: non-zero
	// in Tier A, where every such sample can only ever be attributed by
	// inference.
	PCSamplesWithoutCorrelation uint64
	// ZeroCorrelationExecs is the subset of ZeroCorrelation that arrived on a
	// gpu_exec_v1 record, and it means something completely different from
	// the rest of that counter — issue #52.
	//
	// A PC sample with no correlation is normal and expected (spec §6.3
	// finding 3: continuous collection never populates it), and PC samples
	// dominate the traffic, so ZeroCorrelation is large on a healthy run and
	// a handful of correlation-less EXECUTIONS is invisible inside it. But an
	// execution is exactly the record spec §6 says must always carry one: it
	// is the anchor the exact join runs on, and without it the execution can
	// only be guessed at (gpu.JoinStats.CorrelationlessExecutionCount is the
	// consumer-side witness of the same event).
	//
	// This counter is therefore the shim contract, made checkable. It must
	// read zero, and gate_test.go asserts that. Nothing is dropped when it
	// does not: the execution is normalized and emitted with the zero
	// correlation, which routes it to the timeline's heuristic join — where,
	// since #52, it is refused rather than joined across processes, because
	// correlationOf's zero value carries no pid either. Losing the record
	// would lose measured GPU time, which is a worse failure than losing its
	// stack.
	ZeroCorrelationExecs uint64
	// KernelDropped counts records the BPF program itself could not deliver:
	// a batch bigger than one ringbuf reservation, a full ringbuf, or a
	// faulting read of the producer's buffer. Read from the BPF `dropped`
	// map on each Stats call.
	KernelDropped uint64
	// SampledLaunches counts gpu_launch_sampled_v1 records decoded. It is the
	// denominator the sampling period is checked against: the producer prints
	// how many launches it sampled, and the two must agree.
	SampledLaunches uint64
	// StacksMissing counts sampled launches that arrived with a negative
	// batch_hdr.stack_id — the BPF-side walk produced nothing to hand over.
	// StackWalkEmpty, StackMapFull, StackMapUpdateFailed and
	// StackWalkScratchFailed say which of the ways it can fail happened.
	//
	// This is NOT loss: the launch record is delivered and normalized
	// anyway, it simply carries no CPU stack. Folding it into KernelDropped
	// would report a record loss that did not happen, which is its own kind
	// of dishonesty.
	StacksMissing uint64
	// StacksResolved counts captures that made it all the way to frames:
	// the gpu_stacks entry was there, it held instruction pointers, and the
	// symbolizer turned them into at least one frame. Every sampled launch
	// ends in exactly one of the counters below or in StacksMissing, so
	// they reconcile against SampledLaunches:
	//
	//	SampledLaunches = StacksMissing + StacksUncorrelated +
	//	                  StackLookupFailed + SymbolizeFailed + StacksResolved
	//	StacksResolved  = StacksAttached + StacksEvicted +
	//	                  StacksProfilerOnly + PendingStacks
	//
	// (the second identity holds at rest; PendingStacks is a gauge). Both
	// are asserted by TestSampledStackAccountingReconciles.
	StacksResolved uint64
	// StackLookupFailed counts captures whose gpu_stacks entry could not be
	// read back: the map lookup failed, the value was too short to be a
	// struct gpu_stack, or it decoded to no instruction pointers.
	//
	// Phase 4a's version of this counter had a routine cause that this one
	// does not. bpf_get_stackid was content-addressed, so two launches from
	// the same call site in flight at once shared one stack id and the
	// second found nothing after the first had deleted it. The walker mints
	// its own ids (see next_stack_id in bpf/gpu_usdt.bpf.c) and two captures
	// never share one, so that whole race - both its loss half and its
	// silent aliasing half - is gone. What is left here is a genuinely
	// broken read, which is why StackWalkEmpty exists to say when the entry
	// was there but the walk inside it was empty.
	StackLookupFailed uint64
	// StackWalkEmpty counts walks that produced no frames at all. It is a
	// SUBSET of StackLookupFailed on the consumer side plus the BPF side's
	// own count of empty walks, and the two are disjoint by construction:
	// an empty walk never reaches gpu_stacks (capture_stack counts it and
	// returns -1), so a consumer-side empty can only come from a value some
	// other writer put there.
	//
	// Being a subset keeps the SampledLaunches identity above a partition:
	// an empty walk still lands in exactly one bucket.
	StackWalkEmpty uint64
	// StackWalkTruncated counts resolved captures whose walk hit MAX_FRAMES
	// without also reaching a natural terminator (no walkerFlagsTerminated
	// bit at n_pcs == maxWalkFrames). NOT a failure and not part of the
	// identity: the frames are real and are attributed. It is here because a
	// truncated stack is missing its outermost frames - the ones nearest
	// main, which is where a flame graph is read from - so a rising count
	// means the profile's roots are being cut off even though nothing
	// failed. This is the "ran out of budget" failure; StackWalkAbandoned
	// is the other one, "could not proceed" - they are counted apart on
	// purpose, see StackWalkAbandoned.
	StackWalkTruncated uint64
	// StackWalkAbandoned counts resolved captures whose walk stopped before
	// reaching a natural terminator (no walkerFlagsTerminated bit) AND
	// before running out of budget (n_pcs < maxWalkFrames). This is the
	// counter Task 1 was missing: n_pcs == maxWalkFrames is exactly the
	// wrong test for "the walk failed to make progress", because a walk
	// that dies at the first frame-pointer-omitting vendor frame produces
	// n_pcs of roughly 1-3 and used to read as a complete, untruncated
	// stack - the case that produced a flame graph rooted entirely in the
	// profiler's own callback, silently, because nothing was counting it.
	//
	// The underlying causes are a bpf_probe_read_user fault at a live
	// address while walking the FP chain, a frame pointer that did not
	// increase, a CFI lookup miss (walkerFlagCFIMiss, 0x04), or DWARF
	// giving a return address in a register this walker does not track
	// (bpf/unwind_common.h walk_step). They stop the walk the same way:
	// fewer frames than requested, no end of chain.
	//
	// It deliberately does NOT count a walk whose CFI said the chain ends
	// there (walkerFlagRAUndefined, StackWalkReachedRoot) even though such a
	// walk also has walkerFlagFPTerminated clear. A hybrid walk that crossed
	// a frame-pointer-less frame can only ever end that way, so counting it
	// here made the counter fire on every SUCCESSFUL DWARF walk - measured
	// at abandoned == StacksWalkedDWARF == 62 on the Phase 4b gate - which
	// is both useless as a signal and reads as a fleet of failures to anyone
	// looking at Stats in production.
	//
	// It DOES count a walk that stopped because the frame pointer was lost
	// (walkerFlagFPExhausted, StackWalkFPExhausted), which until issue #44
	// shared a bit with the case above and was therefore invisible.
	//
	// NOT a failure in the record-loss sense - the frames captured are
	// still symbolized and attributed - but it is missing every frame above
	// where the walk gave up, and unlike StackWalkTruncated that loss has
	// no relationship to MAX_FRAMES at all, so it needs its own counter to
	// be visible rather than reading as zero.
	StackWalkAbandoned uint64
	// StackWalkReachedRoot and StackWalkFPExhausted are the two ways a walk
	// can end without the frame-pointer chain running out (which is
	// walkerFlagFPTerminated, and is not counted here). They were ONE
	// counter's worth of information - a single WALKER_FLAG_UNWIND_TERMINATED
	// bit - until issue #44 split them, and the split is the point: one is
	// the good outcome and the other is the bad one.
	//
	// StackWalkReachedRoot counts walks that ended at a frame whose CFI
	// gives the return address as UNDEFINED (walkerFlagRAUndefined): the
	// DWARF marker for an outermost frame, which glibc emits for _start and
	// for thread entry points. The walk saw the whole stack. This is the
	// GOOD outcome, and it is a subset of the walks StackWalkAbandoned does
	// not count.
	StackWalkReachedRoot uint64
	// StackWalkFPExhausted counts walks that stopped at an FP_SAFE frame
	// with no frame pointer to follow (walkerFlagFPExhausted): the DWARF
	// step out of the FP_LESS frame below it gave no location for %rbp and
	// zeroed it. This is the BAD outcome - the caller of that FP_SAFE frame
	// is real and is missing - and it is a SUBSET of StackWalkAbandoned,
	// the subset with this specific named cause, the same way
	// StacksWalkedCFIMiss is.
	//
	// It is also the instrument for issue #45, the root cause: ehcompile
	// emits fpType UNDEFINED wherever the CFI carries no rule for %rbp,
	// where the x86-64 psABI says a callee-saved register with no rule is
	// unchanged. Every walk crossing such a frame lands here. When #45 is
	// fixed, walks migrate from this counter to StackWalkReachedRoot, and
	// that migration is how the fix is measured rather than asserted.
	//
	// On the RTX 3090 validation run for #43 this was, in effect, all 452
	// DWARF walks: every one stopped at main, losing __libc_start_main_impl
	// and _start, and every one read as a complete walk because the two
	// outcomes shared a bit.
	StackWalkFPExhausted uint64
	// StackWalkFPNonMonotonic counts walks that stopped because a frame's
	// saved-FP slot held a value that is neither zero nor above the current
	// frame pointer (walkerFlagFPNonMonotonic): a corrupt frame, a
	// hand-rolled one, or a %rbp holding something that is not a frame base.
	// Like StackWalkFPExhausted it is a named-cause SUBSET of
	// StackWalkAbandoned.
	//
	// Before issue #45 this case was indistinguishable from a read fault:
	// walk_step tested it with a bare `return 1`, which also threw away the
	// return address it had already read out of a different slot of the same
	// frame. That address is now recorded before the walk stops, so the
	// outermost frame survives, and the stop is counted here.
	StackWalkFPNonMonotonic uint64
	// StackWalkRootDisagreement counts walks whose two sources disagreed
	// about where the stack ends (walkerFlagRootDisagreement): the
	// frame-pointer chain reached the psABI's outermost-frame marker, the
	// walk took its one step past it, and the CFI of the frame it landed on
	// says a caller exists rather than declaring it outermost. The walker
	// believes the frame pointer and stops.
	//
	// It exists because without it that walk is INDISTINGUISHABLE from a
	// clean one: walkerFlagFPTerminated is already set, so the walk would be
	// classified terminated and no counter would move - a counter reading
	// green in a case that may well be a truncation. That is the defect
	// class issue #44 exists to remove, and the step past the frame-pointer
	// root added in issue #45 is what created this instance of it.
	//
	// Counted in StackWalkAbandoned as well, like StackWalkFPExhausted and
	// StackWalkFPNonMonotonic: the ending is UNCONFIRMED, and an unconfirmed
	// ending must not be filed as a success. On the Phase 4b gate's producer
	// it is zero, because _start's CFI declares the root and the two sources
	// agree.
	StackWalkRootDisagreement uint64
	// StacksWalkedDWARF counts non-empty captures where at least one frame
	// was unwound via CFI (walkerFlagDWARFUsed set). This is the case that
	// can reach through a vendor library into the application beneath it.
	//
	// Like StackWalkTruncated and StackWalkAbandoned, this is counted as
	// soon as the walk's own flags are known - before symbolization, and
	// regardless of whether symbolization goes on to succeed - because the
	// question it answers ("how did the walk get these PCs") is a property
	// of the walk, not of what the symbolizer later does with them. It is
	// therefore not part of the SampledLaunches/StacksResolved identities;
	// it is a diagnostic overlay the same way the other two are.
	StacksWalkedDWARF uint64
	// StacksWalkedFPOnly counts non-empty captures where no frame used CFI
	// (walkerFlagDWARFUsed clear): every frame the walk kept came from the
	// frame-pointer chain alone. This is exactly the walk shape that
	// produced a flame graph attributing GPU time to the profiler's own
	// callback on real hardware - an FP walk cannot survive the first
	// frame-pointer-omitting vendor frame, so a stack landing here either
	// never left the profiler (see StacksProfilerOnly) or got lucky and
	// stayed inside frame-pointer-preserving code the whole way. Every
	// non-empty capture lands in exactly one of this counter or
	// StacksWalkedDWARF, whatever happens to it downstream (StacksResolved,
	// SymbolizeFailed, ...) - see StacksWalkedDWARF for why the split is
	// made this early rather than gated on symbolization succeeding.
	StacksWalkedFPOnly uint64
	// StacksWalkedNoTables counts captures walked for a process the walker
	// had no CFI tables for. It is the SUBSET of StacksWalkedFPOnly that is
	// explained rather than merely observed: with no entry in pid_mappings
	// every frame classifies as MODE_FP_SAFE and the hybrid walker has
	// nothing to be hybrid about, so an FP-only walk was the only possible
	// outcome. Deliberately not additive with StacksWalkedFPOnly, so the
	// two ways of getting a frame-pointer stack stay apart:
	//
	//   - StacksWalkedNoTables rising means registration has not caught up
	//     (or failed) - see UnwindPIDsRegistered / UnwindPIDsFailed /
	//     UnwindPIDsEvicted for which. It is expected to be non-zero and
	//     small on a system-wide attach: the walk that carries the first
	//     sighting of a process necessarily ran before the tables for it
	//     existed. It is a bug if it keeps rising for a long-lived process.
	//   - StacksWalkedFPOnly rising while this stays flat means the tables
	//     were there and the walker still never needed CFI, which is what a
	//     fully frame-pointer-preserving call path looks like.
	//
	// It is a LOWER BOUND, by construction: a PID's tables can land between
	// the walk and this count, and the error is always in the direction of
	// under-reporting. See pidRegistry.ready.
	StacksWalkedNoTables uint64
	// StacksWalkedCFIMiss counts captures whose walk consulted a CFI table
	// and found no entry covering the frame's PC (walkerFlagCFIMiss set).
	// The walker stops there, so these captures are also counted in
	// StackWalkAbandoned; this is the SUBSET of that with a named cause.
	//
	// It is the counterpart to StacksWalkedNoTables, and separating them is
	// the whole point: "no tables for this process" is fixed by registering
	// the process, "tables that do not cover this PC" is a gap in what
	// unwind/ehcompile produced for that binary. Conflating them would hide
	// the second behind the first, which is the more common of the two.
	StacksWalkedCFIMiss uint64
	// UnwindPIDsRegistered counts processes whose CFI tables were installed
	// for the walker, and UnwindBinariesAttached the distinct binaries
	// across them - the CFI compiles actually paid for. See pidRegistry.
	UnwindPIDsRegistered   uint64
	UnwindBinariesAttached uint64
	// UnwindPIDsFailed counts processes whose registration installed no
	// tables at all: the process exited before /proc/<pid>/maps could be
	// read, /proc was unreadable, or nothing it mapped carried usable
	// .eh_frame. Every walk for such a process is FP-only and counts in
	// StacksWalkedNoTables; this says why. UnwindLastError carries the most
	// recent reason, because a bare count cannot distinguish "the process
	// exited" from "the profiler cannot read /proc".
	UnwindPIDsFailed uint64
	UnwindLastError  string
	// UnwindPIDsEvicted counts processes pushed out of the bounded
	// registration set (Config.UnwindPIDCapacity) because
	// less-recently-seen entries had to make room. Their tables are
	// released and their later walks degrade to frame pointers, counted in
	// StacksWalkedNoTables. Attribution loss, never record loss.
	UnwindPIDsEvicted uint64
	// UnwindEnrolledPIDsEvicted is the subset of the above that had completed
	// the startup rendezvous: producers that were released on the promise
	// that their tables were installed, and then had them taken back to make
	// room. Non-zero means the capacity bound is biting on exactly the
	// processes the rendezvous exists to protect, and is the benign
	// explanation to check first when StacksNoTablesAfterEnroll is non-zero.
	UnwindEnrolledPIDsEvicted uint64
	// UnwindEnrolledMarksDropped counts rendezvous marks that aged out of the
	// bounded set pidRegistry keeps so the mark can outlive an eviction. Each
	// one is a PID whose later tableless walks will NOT be counted in
	// StacksNoTablesAfterEnroll even though they should be - an under-report,
	// which is the direction this counter exists to keep visible. Non-zero
	// means the set is small relative to how many distinct producers enrolled;
	// Config.UnwindPIDCapacity sizes both.
	UnwindEnrolledMarksDropped uint64
	// UnwindRequestsDropped counts registration or release requests the
	// worker queue could not accept. A dropped registration means the PID
	// is forgotten and retried on its next batch; a dropped release means
	// one process's tables stay installed until the BPF maps are closed.
	// Neither loses a record; both are counted rather than absorbed.
	UnwindRequestsDropped uint64
	// UnwindReleaseFailed counts table teardowns that returned an error, so
	// a slow leak inside the walker's maps is visible rather than inferred
	// from a rising StacksWalkedNoTables.
	UnwindReleaseFailed uint64
	// UnwindPIDsTracked is a gauge: how many processes the registry is
	// holding right now, registered or still being compiled. Bounded by
	// Config.UnwindPIDCapacity.
	UnwindPIDsTracked int
	// UnwindEnrollListening says whether the startup rendezvous (enroll.go)
	// is open for this consumer. False means the address could not be bound
	// - another consumer already holds it for this shim inode, or abstract
	// sockets are unavailable - and every producer therefore falls back to
	// lazy registration, i.e. exactly the pre-#49 behaviour, with the
	// startup window it implies. UnwindEnrollLastError says why.
	//
	// It is a flag rather than a silent fallback because a run that lost the
	// rendezvous and a run that never needed it are otherwise
	// indistinguishable from the counters, and the first is the one that
	// loses ~38% of its stacks.
	UnwindEnrollListening bool
	// UnwindEnrollAddress is the abstract socket name this consumer bound, or
	// "" when it bound none.
	//
	// It is here because every enrolment counter reading zero is ambiguous in
	// the one way that matters: "listening, and nobody came" and "the producer
	// computed a different name and connected to nothing" look identical.
	// That ambiguity cost a full CUDA round trip - a run where the producer
	// logged enroll=no-listener while the consumer was listening perfectly, on
	// an address derived from a different device number (see
	// enrollShimIdentity). The producer logs the name it derived; this is the
	// name the consumer derived; comparing two strings settles it.
	UnwindEnrollAddress string
	// UnwindEnrollRequests counts producers that reached the rendezvous and
	// passed both identity checks (SO_PEERCRED, and actually mapping this
	// consumer's shim inode), so registration was attempted for them.
	UnwindEnrollRequests uint64
	// UnwindEnrollConfirmed counts producers released with "your tables are
	// installed". Each one is a process whose FIRST sampled launch was
	// walked with CFI, which is the entire point of issue #49. Compare it
	// against the number of distinct producers a run had: a shortfall means
	// some producer took the lazy path.
	UnwindEnrollConfirmed uint64
	// UnwindEnrollRefused counts connections turned away without any
	// registration: no peer credentials, a PID other than Config.PID on a
	// per-PID attach, or a peer that does not map the shim. The last is the
	// interesting one - it is a process on this machine reaching the
	// rendezvous address for a shim it has not loaded.
	UnwindEnrollRefused uint64
	// UnwindEnrollThrottled counts connections refused by the rendezvous rate
	// limiter before any check or /proc read. The listener serves one
	// producer at a time, so a fork loop of shim-mapping processes could
	// otherwise monopolise it and expire genuine producers' budgets; see
	// enrollAdmission. A throttled producer is released instantly and runs on
	// the lazy path, so this is attribution loss, never a stalled
	// application. Non-zero on a machine that is not under attack means the
	// burst is genuinely larger than enrollUIDBurst per second.
	UnwindEnrollThrottled uint64
	// UnwindEnrollFailed counts producers whose registration was attempted
	// and installed nothing. They are released rather than parked, so they
	// run with frame-pointer stacks; every stack they go on to take is
	// counted in both StacksWalkedNoTables and StacksNoTablesAfterEnroll.
	UnwindEnrollFailed uint64
	// UnwindEnrollLastError is the most recent reason a rendezvous did not
	// end in a confirmation, including a failure to bind the address at all.
	UnwindEnrollLastError string
	// StacksNoTablesAfterEnroll is the counter that says the fix stopped
	// working.
	//
	// It is the subset of StacksWalkedNoTables whose process DID complete the
	// startup rendezvous. The rendezvous is meant to make that impossible:
	// the producer is blocked in its own initialisation until the tables are
	// installed, so a walk from it should never find them missing.
	//
	// There are exactly three ways it can be non-zero, and they are not
	// equally alarming - read it together with the two counters that name the
	// benign ones:
	//
	//   - the rendezvous ran and registration installed nothing. The producer
	//     was released anyway (a degraded profile beats a stalled process)
	//     and its walks are honestly tableless. Also counted in
	//     UnwindEnrollFailed.
	//   - the PID was evicted from the bounded set afterwards, so the tables
	//     were taken back. Also counted in UnwindEnrolledPIDsEvicted.
	//   - neither of those, in which case the ordering argument the whole fix
	//     rests on is false: a walk happened before the reply went out, or
	//     registration reported success it had not delivered. THAT has no
	//     benign reading.
	//
	// The enrolled mark deliberately outlives eviction (pidRegistry's
	// wasEnrolled set): if it did not, the second case above would leave this
	// counter reading zero while StacksWalkedNoTables climbed - green exactly
	// when things were worst, in the counter added to prevent that. The set
	// is bounded, so a PID evicted long ago can be forgotten (a false
	// negative, the safe direction) and a recycled PID can be miscredited (a
	// false positive, which UnwindEnrolledPIDsEvicted cross-checks).
	//
	// Deliberately NOT a way to make StacksWalkedNoTables read zero: that
	// counter still counts every tableless walk, enrolled or not.
	StacksNoTablesAfterEnroll uint64
	// StackMapFull counts captures the BPF side could not park because
	// gpu_stacks was full (gpuStackCapacity live entries). Read from the
	// `walk_errors` map.
	//
	// The map only fills if the consumer has stopped draining the ringbuf,
	// because an entry lives from the probe to resolveStackLocked and
	// nothing else holds one. It is counted rather than absorbed by
	// overwriting a live entry: an overwrite would hand one launch another
	// launch's call path, and no counter downstream could tell.
	StackMapFull uint64
	// StackMapUpdateFailed counts captures whose gpu_stacks insert failed
	// for any reason other than a full map — including the one that means
	// the id space wrapped onto an entry nobody consumed, which BPF_NOEXIST
	// turns into a refused insert rather than a silent overwrite. Read from
	// `walk_errors`.
	StackMapUpdateFailed uint64
	// StackWalkScratchFailed counts captures that never got as far as
	// walking: a per-CPU scratch slot (walker_scratch, gpu_stack_scratch or
	// stack_id_seq) could not be looked up. Read from `walk_errors`.
	//
	// It should be identically zero — a PERCPU_ARRAY lookup at key 0 cannot
	// fail on a loaded program — and exists so that if it ever is not, the
	// cause is named rather than showing up as unexplained StacksMissing.
	StackWalkScratchFailed uint64
	// SymbolizeFailed counts captures that were read back but produced no
	// frames: the symbolizer returned an error, returned nothing, or no
	// Symbolizer was configured at all. The launch is never dropped for
	// this - it degrades to no stack, which projects as unattributed GPU
	// time, and the degradation is visible here.
	SymbolizeFailed uint64
	// StacksUnresolved counts captures the symbolizer accepted — no error,
	// one frame per instruction pointer — in which NOT ONE frame came back
	// with a real symbol name. Every frame is an address rendered as
	// "0x<addr>", which pprof will happily display and no human can read.
	//
	// This is deliberately NOT folded into SymbolizeFailed, and it does not
	// take part in the SampledLaunches reconciliation above: the stack is
	// delivered and attached, so it is a resolution failure, not a loss. It
	// gets its own counter because the alternative is what actually happened
	// on this branch — the Phase 4a gate symbolized 63 stacks into 252
	// hex addresses while SymbolizeFailed sat at zero, and the failure was
	// only noticed by a human reading the frame names. A profiler that
	// resolves no names while reporting no failures is lying by omission.
	//
	// The usual causes, in the order worth checking:
	//
	//   - The target process exited before its stack was symbolized.
	//     /proc/<pid>/maps is gone, so there is nothing to resolve against.
	//   - No capability to follow /proc/<pid>/map_files/ and no usable
	//     fallback (see symbolize.LocalSymbolizer.SymbolizeProcess).
	//   - The binary genuinely has no symbol table.
	StacksUnresolved uint64
	// StackFramesUnresolved counts individual frames — one per captured
	// instruction pointer — that came back named after their own address.
	// Unlike StacksUnresolved this is expected to be non-zero in normal
	// operation: a stripped vendor library or a vDSO frame in an otherwise
	// well-resolved stack lands here. It is the ratio against
	// StacksResolved's frame count that is diagnostic, not the raw number.
	StackFramesUnresolved uint64
	// StackFramesModuleOnly is the subset of StackFramesUnresolved that at
	// least knows which file the address fell in, and therefore renders as
	// "libcuda.so.1+0x1b71c6" instead of "0x7f2c945b2c2b". NVIDIA ships no
	// symbols for the libcuda/libcupti internals a launch stack is full of,
	// so the name is genuinely unrecoverable while the module is not, and
	// knowing a frame is seven deep inside libcuda is most of the answer.
	//
	// Healthy: close to StackFramesUnresolved. Worst: zero while
	// StackFramesUnresolved is large - no ModuleIndex is wired into the
	// symbolizer, or the target exits before its stacks are drained, and
	// every unresolved frame is an ASLR'd address that means nothing across
	// runs. The two are counted apart precisely so that case cannot hide
	// inside the total.
	StackFramesModuleOnly uint64
	// StackDeleteFailed counts gpu_stacks entries the consumer read but could
	// not delete. See freeStackLocked: deletion is what stops the map
	// filling, so a rising count here is the early warning for capture
	// failures (StacksMissing) that follow.
	StackDeleteFailed uint64
	// StackInterpFrames counts interpreter frames spliced into resolved
	// launch stacks - one per two pcs[] slots the walker tagged with an
	// unwinder id rather than FRAME_TAG_NATIVE (bpf/unwind_record.h). They
	// are placed at their own position in the call path and named by the
	// unwinder that wrote them (for CPython, "python:0x..."); what this
	// counter answers is whether the handoff is reaching the GPU path at all,
	// which no other number here can say.
	//
	// One counter across every language, deliberately. It answers a question
	// about the HANDOFF, which is one mechanism; per-language totals belong to
	// the module's own counters, where the units and the failure modes are
	// the module's to define.
	StackInterpFrames uint64
	// StackInterpPairsTruncated counts walks that ended between the two slots
	// of an interpreter frame - MAX_FRAMES landed mid-pair, so the first word
	// arrived and its partner did not. The half frame is dropped rather than
	// half-read, and this is the counter that stops that drop being silent.
	// Expect it to be small and to move together with
	// walkerFlagFramePushRefused.
	StackInterpPairsTruncated uint64
	// StacksUncorrelated counts captures whose sampled launch carried a
	// wire correlation of zero. The stack is real, but the ABI's "no
	// correlation" leaves nothing to pair it with: the batched twin cannot
	// be identified, so the capture cannot be attached to any launch
	// without guessing which one it belongs to.
	StacksUncorrelated uint64
	// StacksAttached counts captures actually attached to a launch that
	// went to the sink - the number that ends up carrying attribution in
	// the profile, from both arrival orders.
	StacksAttached uint64
	// StacksEvicted counts resolved captures pushed out of the bounded
	// side table before their batched twin ever arrived, which happens when
	// the batch carrying that twin was dropped or when the table is too
	// small for the in-flight window. It is attribution loss, not record
	// loss: the launches themselves are unaffected.
	StacksEvicted uint64
	// StacksProfilerOnly counts resolved captures the consumer refused to
	// attach because the walk never left the profiler's own injected shim:
	// every frame it produced lies inside the .so carrying the probes, so
	// the stack says where the profiler was, not where the application was.
	//
	// This is attribution loss, not record loss - the launch is delivered
	// without a stack and its execution's measured GPU time projects as
	// [gpu:launch unsampled], the same bucket as a launch the sampler never
	// picked. The counter exists because the alternative is what a real
	// CUPTI run produced: 100% of the attributed GPU time nested under the
	// adapter's own callback frame, stated as confidently as a true
	// attribution. A profiler that cannot see the application must say so.
	//
	// Zero by construction for a self-contained producer such as shim/stub,
	// where the shim IS the program and there is no boundary to cross. See
	// shimScope for how the two deployment shapes are told apart.
	StacksProfilerOnly uint64
	// StacksProfilerOnlyUncertain is the SUBSET of StacksProfilerOnly
	// rejected without proof: no frame was provably outside the shim, but at
	// least one frame's module was unknown (the symbolizer named the frame
	// after its own address, or resolved it without a module), so the stack
	// might have reached the application and been unnameable rather than
	// never having got there.
	//
	// It is a subset, deliberately not additive with StacksProfilerOnly, so
	// the reconciliation identity above stays a partition. It separates the
	// two ways this guard can be wrong: the certain rejections are the bug
	// it was built for, while a rising uncertain count means symbolization
	// is failing to name modules and the guard is paying for it in lost
	// attribution.
	StacksProfilerOnlyUncertain uint64
	// KernelNamesLearned counts gpu_kernel_name_v1 records interned. The
	// producer replays its whole table on late attach, so a name may be
	// learned more than once for the same kernel; this counts records, not
	// distinct kernels (PendingNames is the gauge for the latter).
	KernelNamesLearned uint64
	// KernelNamesTruncated counts names the producer had to cut off at
	// GPU_KERNEL_NAME_MAX. Those names still resolve - marked, so a
	// truncated name is never presented as complete. See
	// truncatedNameSuffix.
	KernelNamesTruncated uint64
	// KernelNamesEvicted counts names pushed out of the bounded table. Not
	// record loss: events referring to that kernel still flow, unnamed, and
	// count in KernelNamesUnresolved.
	KernelNamesEvicted uint64
	// KernelNamesUnresolved counts launches and executions emitted with no
	// kernel name: the producer never named that kernel, the name was
	// evicted, or the event was released (by the queue's bound, or by a
	// Flush) before its name arrived. Without this counter an unnamed
	// execution in the profile is a mystery rather than a measurement.
	KernelNamesUnresolved uint64
	// PendingStacks, PendingLaunches, PendingNamedEvents and KnownKernelNames
	// are gauges, not counters: what the side tables hold right now.
	// PendingLaunches and PendingNamedEvents are also how much event
	// delivery the consumer is currently holding back - see Consumer.Flush.
	PendingStacks      int
	PendingLaunches    int
	PendingNamedEvents int
	KnownKernelNames   int

	// ----- PC sampling: the five kinds applyBatch decodes for Tier A and
	// Tier B. None of these resolves a source line or attributes a sample
	// to an execution; that is the module store's and the timeline's work.
	// These say what arrived and what could not be read.

	// PCSamplesDecoded counts gpu_pc_sample_batch_v1 records normalized
	// into gpu.GPUPCSample and handed to the sink. It is the denominator
	// the counters below are read against, and the answer to "did PC
	// sampling produce anything at all", which no other counter gives:
	// Records aggregates every kind.
	//
	// Healthy: non-zero whenever a tier is enabled. Worst: zero with a
	// tier enabled, which means the shim never drained a PC buffer - a
	// silent, total absence that looks exactly like an idle GPU.
	PCSamplesDecoded uint64
	// ModulesDecoded counts gpu_module_load_v1 records normalized into
	// gpu.GPUModule. It records that a module loaded, with its CRC and
	// size; it does NOT mean the module's BYTES arrived. Those travel the
	// cubin channel and are counted in CubinsReceived, and the two
	// disagreeing is the ordinary Tier B failure - a module announced but
	// unresolvable, every one of whose PC samples reads gpu_src_status
	// "no-module".
	//
	// ModuleLoad.BytesPtr is decoded and deliberately unused: it is a
	// pointer into the PRODUCER's address space, and following it would
	// need CAP_SYS_PTRACE, which this agent does not have and will not
	// take. See cubin.go.
	//
	// Healthy: one per distinct module the process loaded. Worst: zero
	// while executions flow, meaning no module ever reached the agent.
	ModulesDecoded uint64
	// StallNamesLearned counts gpu_stall_reason_map_v1 records interned.
	// The producer replays its whole table on late attach, so an index may
	// be learned more than once; this counts records, not distinct indices
	// (KnownStallNames is the gauge for the latter).
	//
	// Healthy: at least one full table - 38 entries on GA102 - per
	// producer. Worst: zero while PC samples flow, in which case every
	// sample's stall reason is "" and StallNamesMissing equals
	// PCSamplesDecoded.
	StallNamesLearned uint64
	// StallNamesTruncated counts stall names the producer had to cut off
	// at GPU_STALL_NAME_MAX. Those names still resolve, marked with
	// truncatedStallSuffix, so a truncated name is never presented as
	// complete.
	//
	// Healthy: zero. The ABI's buffer is exactly CUPTI's own
	// CUPTI_STALL_REASON_STRING_SIZE, so a CUPTI producer cannot overflow
	// it. Worst: non-zero, meaning stall names are being cut and two
	// distinct reasons sharing a prefix would otherwise have aggregated
	// into one label value.
	StallNamesTruncated uint64
	// StallNamesEvicted counts stall names pushed out of the bounded
	// table. Not record loss: the PC samples still flow, with an empty
	// stall reason, and count in StallNamesMissing.
	//
	// Healthy: zero - a device's stall reasons are a fixed enum far below
	// the bound. Worst: non-zero, meaning the table is churning and the
	// stall labels in the profile describe whichever indices happened to
	// be resident.
	StallNamesEvicted uint64
	// StallNamesMissing counts PC samples emitted with an EMPTY stall
	// reason: the producer never mapped that index, the name was evicted,
	// or the sample was released (by the pending queue's bound, or by a
	// Flush) before its name arrived.
	//
	// The sample is never dropped for this - it reaches the sink and its
	// GPU time is still measured - and the stall reason is never faked. An
	// index with no name becomes "", never "stall#17": the index is the
	// vendor's own and is not stable across devices or driver versions, so
	// rendering it would leak an unstable internal number into a label
	// value where a consumer would aggregate on it. This counter is what
	// makes the resulting blank visible instead of mysterious.
	//
	// Healthy: zero. Worst: equal to PCSamplesDecoded, which is a profile
	// full of PC samples with no stall reason at all - the whole point of
	// PC sampling, silently absent, if it were not counted.
	StallNamesMissing uint64
	// SamplingWindowsDecoded counts gpu_sampling_window_v1 records read.
	// A burst reaches the wire TWICE - an open record the instant it starts
	// and a closed record with the same start_ns when it stops - so this is
	// about twice the number of bursts on a clean Tier A run. Tier B does
	// not burst and emits none.
	//
	// The windows are normalized and handed to the sink, where the
	// serialization disclosure consumes them (gpu/serialization.go). This
	// counter is the wire-side half of the reconciliation:
	// Snapshot.SamplingWindowsReceived is what actually reached the store.
	//
	// Healthy: non-zero in Tier A, zero in Tier B. Worst: zero in Tier A,
	// where nothing would then say which executions ran perturbed and every
	// one of them reads serialized="unknown".
	SamplingWindowsDecoded uint64
	// SamplingWindowsOpen is the subset of SamplingWindowsDecoded that
	// arrived with end_ns == 0, which the ABI defines as "still open when
	// the producer stopped reporting". It is NOT a zero-length window, and
	// the two must never be conflated: an open window means every execution
	// at or after its start_ns is serialized="unknown", never "false".
	//
	// It is NOT an anomaly by itself, and this is the counter's one
	// subtlety. The producer emits an open record at every burst START
	// precisely so that a hard exit leaves the burst visible instead of
	// losing it, so a healthy Tier A run produces one of these per burst and
	// this counter tracks SamplingWindowsDecoded / 2. What IS an anomaly is
	// a window still open once its close should have arrived, which is
	// Snapshot.SamplingWindowsOpen - a gauge of the store, not a count of
	// records.
	//
	// Healthy: about half of SamplingWindowsDecoded in Tier A, zero in Tier
	// B. Worst: zero in Tier A, meaning the burst-start records are being
	// lost and a hard exit would take its whole perturbed tail with it.
	SamplingWindowsOpen uint64
	// ConfigsDecoded counts gpu_config_v1 records read. The producer emits
	// one per process and replays it on late attach, so this counts
	// records rather than producers.
	//
	// Healthy: at least one per producer. Worst: zero, in which case the
	// three gauges below are unset and the sampling period behind every PC
	// sample is unknown.
	ConfigsDecoded uint64
	// ConfigsDisagreed counts config records whose values differ from the
	// ones already held. The three gauges below are last-writer-wins, so
	// with a system-wide attach they describe whichever producer reported
	// most recently; this counter is what says the answer is ambiguous
	// instead of letting one process's configuration stand for the
	// machine's.
	//
	// Healthy: zero - one producer, one configuration, replayed
	// identically. Worst: non-zero, meaning ConfigSamplingFactor,
	// ConfigSMCount and ConfigClockHz describe an arbitrary one of several
	// producers and must not be used to scale anything.
	ConfigsDisagreed uint64
	// ConfigSamplingFactor, ConfigSMCount and ConfigClockHz are the
	// producer's own sampling configuration, from the most recent
	// gpu_config_v1. Gauges, not counters, and meaningful only while
	// ConfigsDisagreed is zero. Zero means no config record has arrived.
	ConfigSamplingFactor uint32
	ConfigSMCount        uint32
	ConfigClockHz        uint64
	// DropsDecoded counts gpu_dropped_v1 records read off the wire, of every
	// class. It is the denominator for the two counters below: they partition
	// it, so DropsDecoded == GraphExecReports + DropsUnconsumed exactly, and a
	// class that reached the decoder and then vanished shows up as a
	// shortfall rather than as nothing at all.
	DropsDecoded uint64
	// GraphExecReports counts gpu_dropped_v1 records carrying
	// GPU_DROP_CLASS_GRAPH_EXEC, and GraphExecutions sums their counts: how
	// many kernel executions the producer has reported as launched from a
	// CUDA graph.
	//
	// GraphExecutions is the one to read, and it is the counter that makes
	// Tier A's refusal auditable. A CUDA graph launch fires ONE runtime
	// callback for N kernels and gpu_exec_v1 carries no graph id, so those N
	// executions share one correlation — and Tier A's entire claim is exact
	// launch attribution. Left unconsumed (which it was, until issue #94) the
	// result is attribution that is exact-LOOKING and many-to-one, with every
	// counter on this struct reading green.
	//
	// Healthy: EXACTLY ZERO, on any run of a workload that launches no CUDA
	// graph. Non-zero the instant one is reported, which is the property that
	// makes it assertable in both directions. Both counters are cumulative and
	// neither is ever reset: the condition is a property of the process and
	// does not stop holding.
	//
	// The records are also forwarded to the sink as gpu.GPUGraphExecutions,
	// where Timeline withdraws Tier A's exact attribution for that process.
	// Counting without forwarding would be a number in a struct nobody reads;
	// forwarding without counting would make loss between here and the
	// Timeline invisible. Both, so the two can be compared.
	GraphExecReports uint64
	GraphExecutions  uint64
	// DropsUnconsumed counts gpu_dropped_v1 records whose drop class this
	// phase decodes but does not yet act on: pc-dropped-hw, pc-buffer-full and
	// pc-non-user-kernel (see internal/gpuabi's DropClass* and
	// DropClassName). They are Tier B completeness and loss figures, and
	// turning them into operator-visible numbers is a separate task; this
	// counter is what keeps them from being silent in the meantime, exactly
	// as Undecoded did before gpu_dropped_v1 had an applyBatch arm at all.
	//
	// It counts RECORDS, not the losses they describe. A record saying
	// "17 samples were dropped" adds one here, not seventeen: the sum of the
	// counts is a per-class figure and belongs with the per-class consumers
	// that do not exist yet, while what this says is "three statements about
	// loss arrived and nothing on this side read them".
	//
	// Healthy: zero from the CUPTI adapter on a clean run. Non-zero from the
	// stub, which emits one record per class so that every class is reachable
	// from a test.
	DropsUnconsumed uint64
	// PendingStallSamples and KnownStallNames are gauges: what the two
	// stall-reason side tables hold right now. PendingStallSamples is also
	// how many PC samples the consumer is currently holding back - see
	// Consumer.Flush.
	PendingStallSamples int
	KnownStallNames     int
	// KernelStacksMissing is the same event counted on the BPF side, read
	// from the `stacks_missing` map. It is kept separate from StacksMissing
	// rather than replacing it because the two disagree exactly when a batch
	// carrying a failed capture was itself lost: the kernel counted it, the
	// consumer never saw it. KernelStacksMissing > StacksMissing therefore
	// localizes loss that SequenceGaps can only detect.
	KernelStacksMissing uint64

	// The cubin transport (cubin.go): the dedicated AF_UNIX channel that
	// carries a module's BYTES, beside and deliberately not shared with the
	// enrolment rendezvous. Everything Tier B attribution can say about a PC
	// sample comes through here, so every way an offer can fail is counted
	// and none of them is silent.
	//
	// CubinsListening says whether the channel bound at all. A run with it
	// false resolves nothing: every PC sample reads gpu_src_status
	// "no-module". CubinsAddress is the abstract name that was bound, printed
	// for the same reason UnwindEnrollAddress is - when the two ends derive
	// different names every counter on both sides reads zero and nothing says
	// why.
	CubinsListening bool
	CubinsAddress   string
	// CubinsReceived counts offers accepted and stored. CubinBytesReceived is
	// their total size, so the total ceiling can be reasoned about from the
	// outside rather than trusted.
	CubinsReceived     uint64
	CubinBytesReceived uint64
	// CubinsDuplicate counts offers for a CRC already held. A counted no-op:
	// cubin_crc is content-addressed, so the same CRC is the same bytes and
	// re-reading them costs an mmap and a parse to reach the same answer. The
	// payload is never mapped for one of these.
	CubinsDuplicate uint64
	// CubinsRejectedTooLarge counts offers refused for size - past the
	// per-cubin ceiling (Config.CubinMaxBytes) or past the total
	// (Config.CubinTotalBytes). Nothing partial is ever stored for either.
	CubinsRejectedTooLarge uint64
	// CubinsRejectedMalformed counts offers with a bad header, a bad magic, an
	// unknown version or flag, no descriptor or more than one, or a payload
	// whose real size disagrees with the size the header declared.
	CubinsRejectedMalformed uint64
	// CubinsRejectedUnsealed counts offers whose memfd was missing at least
	// one of F_SEAL_SEAL|F_SEAL_SHRINK|F_SEAL_GROW|F_SEAL_WRITE, verified
	// with F_GET_SEALS BEFORE anything is mapped. Without F_SEAL_SHRINK a peer
	// can ftruncate under our mmap and SIGBUS this process; without
	// F_SEAL_WRITE the ELF mutates under the parser. There is no fallback that
	// reads one anyway.
	CubinsRejectedUnsealed uint64
	// CubinsRejectedUnauthorized counts peers refused by the same identity
	// rules the enrolment rendezvous uses, reused verbatim: no SO_PEERCRED, a
	// uid an unprivileged consumer will not serve, a PID other than the
	// attached one, or a peer that does not map the shim inode.
	CubinsRejectedUnauthorized uint64
	// CubinsThrottled counts offers refused by the CUBIN admission bucket.
	//
	// It reads as its own number because the bucket is its own bucket. That
	// is the whole reason this channel is separate: a module-heavy workload
	// can exhaust cubin admission all it likes and UnwindEnrollThrottled will
	// not move, so a throttled offer costs one module's source resolution and
	// cannot cost an enrolment - which is issue #49's ~38% stack loss.
	CubinsThrottled uint64
	// CubinsLastError is the most recent reason an offer did not land.
	CubinsLastError string
}

// maxDecodeFailureReasons bounds the decode-failure table. Four is enough to
// tell apart the ways decodeBatch can fail — a short header, a payload
// shorter than the header claims, a record the ABI rejects, a sampled batch
// that is not singular — while keeping the table a fixed-size array that
// costs nothing at all when nothing fails.
const maxDecodeFailureReasons = 4

// DecodeFailure is one reason ringbuf samples failed to decode, and how many
// samples failed for that reason. Reason is the error text as decodeBatch
// rendered it, which is what an operator reads.
type DecodeFailure struct {
	Reason string
	Count  uint64
}

// String renders a failure the way a log line or a test message wants it.
func (d DecodeFailure) String() string {
	return fmt.Sprintf("%dx %s", d.Count, d.Reason)
}

// decodeFailureTable remembers why decoding failed, bounded to the first
// maxDecodeFailureReasons distinct reasons.
//
// The state it has to survive is not a rare one-off: it is a producer whose
// every sample fails identically, which is exactly the shape of an ABI drift
// — thousands of failures, one reason. So the repeat path must not allocate,
// and it does not: errors.Is compares sentinels by identity without
// formatting anything, and only a reason never seen before is rendered with
// Error(). At most maxDecodeFailureReasons strings are ever retained.
//
// Nothing here is on the success path, which touches this type not at all.
type decodeFailureTable struct {
	errs   [maxDecodeFailureReasons]error
	reason [maxDecodeFailureReasons]string
	count  [maxDecodeFailureReasons]uint64
	n      int
	// unrecorded counts failures that arrived after the table filled up.
	unrecorded uint64
}

// note records one decode failure. Caller holds mu.
func (t *decodeFailureTable) note(err error) {
	// Identity fast path, allocation-free. It is deliberately symmetric:
	// errors.Is(err, stored) alone would fold a *wrapped* error into the
	// bare sentinel it wraps and report it under the bare sentinel's text,
	// losing the detail the wrapping exists to carry. Requiring the match
	// both ways means only genuinely identical errors merge here; anything
	// else falls through to the text comparison below, which is exact.
	for i := 0; i < t.n; i++ {
		if errors.Is(err, t.errs[i]) && errors.Is(t.errs[i], err) {
			t.count[i]++
			return
		}
	}
	// A wrapped error carrying formatted detail ("...: count=3") reaches
	// here every time; its text is what "distinct reason" means.
	reason := err.Error()
	for i := 0; i < t.n; i++ {
		if t.reason[i] == reason {
			t.count[i]++
			return
		}
	}
	if t.n == len(t.reason) {
		t.unrecorded++
		return
	}
	t.errs[t.n] = err
	t.reason[t.n] = reason
	t.count[t.n] = 1
	t.n++
}

// snapshot copies the table out for Stats. Nil when nothing failed, so a
// healthy consumer's Stats call allocates nothing here either.
func (t *decodeFailureTable) snapshot() []DecodeFailure {
	if t.n == 0 {
		return nil
	}
	out := make([]DecodeFailure, t.n)
	for i := 0; i < t.n; i++ {
		out[i] = DecodeFailure{Reason: t.reason[i], Count: t.count[i]}
	}
	return out
}

// batchReader is the slice of *ringbuf.Reader that Run and Close use. It
// exists so the Run/Close lifecycle — in particular Close racing a blocked
// Read — is testable without CAP_BPF, which creating a real ringbuf map
// requires. *ringbuf.Reader satisfies it.
//
// SetDeadline is part of the surface but is deliberately never used to
// interrupt a read: see Run for why it cannot work. It stays here so a fake
// can model its real locking and prove that Run does not reach for it.
type batchReader interface {
	Read() (ringbuf.Record, error)
	SetDeadline(t time.Time)
	Close() error
}

// stackStore is the slice of *ebpf.Map that sampled-stack resolution uses:
// read one gpu_stacks entry, then delete it. It is a seam for the same
// reason batchReader is - creating a real BPF map needs CAP_BPF, so without
// it the whole resolve/delete/attach path would be untestable - and
// *ebpf.Map satisfies it as-is.
type stackStore interface {
	LookupBytes(key any) ([]byte, error)
	Delete(key any) error
}

// Consumer owns the loaded BPF objects, the uprobe_multi link and the
// ringbuf reader for one shim.
type Consumer struct {
	cfg    Config
	objs   gpuusdtObjects
	links  []link.Link
	reader batchReader
	// stacks is c.objs.GpuStacks behind the stackStore seam. Nil when no BPF
	// objects were loaded (every unit test), which makes every resolution
	// count as StackLookupFailed rather than panic.
	stacks stackStore
	// unwind installs the per-PID CFI tables the BPF walker consults, and
	// bounds the set of PIDs that hold them. Nil when no BPF objects were
	// loaded (every unit test that does not inject a fake registrar), in
	// which case every walk reports no tables — which is the truth.
	//
	// It has its own lock and is safe to use under c.mu; none of the methods
	// c.mu-holding code calls does I/O or blocks. See pidRegistry.
	unwind *pidRegistry
	// enroll serves the startup rendezvous that makes those tables exist
	// before the first probe fires. Nil when the address could not be bound,
	// which is not fatal: producers then register lazily on their first
	// batch, exactly as before issue #49. See enroll.go.
	enroll *enrollListener
	// enrollErr is why there is no listener, surfaced as
	// Stats.UnwindEnrollLastError. Written once in Attach, read under mu.
	enrollErr string
	// cubin serves the cubin offer channel: a second abstract socket, a
	// second goroutine and a second admission bucket, beside and never
	// shared with the rendezvous above. Nil when the address could not be
	// bound, which is not fatal - every PC sample then reads gpu_src_status
	// "no-module", which is the truth. See cubin.go for why sharing the
	// enrolment socket would reintroduce issue #49.
	cubin *cubinListener
	// cubinErr is why there is no cubin listener, surfaced as
	// Stats.CubinsLastError. Written once in Attach, read under mu.
	cubinErr string

	mu          sync.Mutex
	seqByStream map[seqKey]uint64
	pending     *pendingStacks
	deferred    *deferredLaunches
	names       *kernelNameTable
	unnamed     *pendingNames
	// stalls and unresolvedStalls are the stall-reason twins of names and
	// unnamed: the bounded stall_index -> name table, and the bounded FIFO
	// of PC samples held until their index resolves. See stallnames.go.
	stalls          *stallNameTable
	unresolvedStall *pendingStallSamples
	// config is the most recent gpu_config_v1, and configSeen says whether
	// one has arrived at all - which the zero value cannot, because a
	// producer may legitimately report a zero sampling factor.
	config     gpuabi.Config
	configSeen bool
	// shim is the attribution guard's view of what the consumer attached
	// to: whether the shim is an injected library or the program itself,
	// and which module paths are the shim's own. Immutable after
	// newConsumer; see shimScope.
	shim shimScope

	// sawKernelName records whether this producer emits kernel names at
	// all. Holding an event for a name that is never coming would delay
	// every event a producer without the name probe ever sends, so nothing
	// waits until the producer has demonstrated, once, that names exist.
	sawKernelName bool
	stats         Stats
	// decodeFailures is the bounded reason table behind Stats.Malformed.
	// A zero value is a usable empty table, so newConsumer says nothing
	// about it.
	decodeFailures decodeFailureTable
}

// newConsumer builds a Consumer with its side tables sized from cfg. Attach
// and the unit tests share it so a store can never be forgotten in one of
// the two paths.
func newConsumer(cfg Config) *Consumer {
	return &Consumer{
		cfg:         cfg,
		shim:        newShimScope(cfg.ShimPath),
		seqByStream: map[seqKey]uint64{},
		pending:     newPendingStacks(cfg.SampledStackCapacity),
		deferred:    newDeferredLaunches(cfg.DeferredLaunchCapacity),
		names:       newKernelNameTable(cfg.KernelNameCapacity),
		unnamed:     newPendingNames(cfg.PendingNamedEventCapacity),

		stalls:          newStallNameTable(cfg.StallNameCapacity),
		unresolvedStall: newPendingStallSamples(cfg.PendingStallSampleCapacity),
	}
}

// seqKey identifies one sequence-number stream. The shim's seq_ counter is
// per-process (shim/core/batch.h), so a system-wide attach (Config.PID == 0)
// interleaves one independent stream per profiled process. Keying on kind
// alone would read those interleavings as enormous gaps — manufacturing
// exactly the loss the counter exists to detect.
type seqKey struct {
	kind uint32
	pid  uint32
}

// Attach discovers the shim's probes and attaches them all in one
// uprobe_multi link.
//
// It must be uprobe_multi, not link.Uprobe: the perf_uprobe PMU path requires
// CAP_SYS_ADMIN, measured, while the BPF-link path works with CAP_BPF +
// CAP_PERFMON alone (spec §11). Using the obvious API silently undoes the
// capability reduction Phase 1 delivered. Requires Linux 6.6+.
func Attach(cfg Config) (c *Consumer, err error) {
	if cfg.Sink == nil {
		return nil, errors.New("gpuprobe: Config.Sink is required")
	}

	probes, err := usdt.ParseFile(cfg.ShimPath)
	if err != nil {
		return nil, fmt.Errorf("parse usdt notes: %w", err)
	}

	var addrs, refCtrs, cookies []uint64
	for _, p := range probes {
		cookie := cookieFor(p.Name)
		if cookie == 0 || p.Provider != usdtProvider {
			continue
		}
		if !p.HasSemaphore {
			return nil, fmt.Errorf("probe %s has no semaphore; the shim cannot tell when to emit", p.Name)
		}
		addrs = append(addrs, p.Offset)
		refCtrs = append(refCtrs, p.SemaphoreOffset)
		cookies = append(cookies, cookie)
	}
	if len(addrs) == 0 {
		return nil, errors.New("no perfagent probes found in shim")
	}

	spec, err := loadGpuusdt()
	if err != nil {
		return nil, err
	}
	// Without this cilium/ebpf discovers the version through the vDSO, which a
	// setcap'd (non-dumpable) process cannot read. See internal/kernelver.
	kernelver.Apply(spec)

	c = newConsumer(cfg)
	// Every failure below this point must leave through a *bare* return that
	// only assigns err. `return nil, err` would set the named result c to nil
	// before the deferred cleanup runs, so the defer would call Close on a nil
	// receiver — a panic in the caller instead of an error, and a leak of the
	// programs, maps and links Close never got to release.
	defer func() {
		if err != nil {
			// Cleanup path: the caller sees the original failure, not
			// whatever the teardown of a half-built consumer reports.
			_ = c.Close()
			c = nil
		}
	}()

	if err = spec.LoadAndAssign(&c.objs, nil); err != nil {
		err = fmt.Errorf("load gpu usdt objects: %w", err)
		return
	}

	// Register PIDs with the stack walker BEFORE the link exists.
	//
	// walk_step (bpf/unwind_common.h) unwinds by CFI only for frames it can
	// find in pid_mappings; with nothing there it silently takes the
	// frame-pointer path, which is the bug this phase exists to fix. So the
	// tables have to be installed before the first probe can fire — and
	// creating the uprobe_multi link is precisely what makes it fire, since
	// the shim only emits once the link bumps its semaphore. Doing this
	// after the attach would leave a window whose width is a CFI compile.
	//
	// Best-effort, never fatal: a consumer with no CFI tables still profiles
	// with frame-pointer stacks, and says so in Stats.StacksWalkedNoTables.
	// Failing the attach instead would turn a degraded profile into no
	// profile at all.
	c.unwind = newPIDRegistry(newEhmapsRegistrar(&c.objs), cfg.UnwindPIDCapacity)
	if cfg.PID != 0 {
		// Eager, and synchronous: one known process, and the cost is paid
		// once at startup rather than against a live profiled application.
		// This is what unwind/dwarfagent does for a per-PID profiler, where
		// ModeLazy is forced back to ModeEager for the same reason.
		_, _ = c.unwind.registerNow(uint32(cfg.PID))
	}
	// For a system-wide attach (cfg.PID == 0) there is nothing to register
	// yet — the target may not even be running, which is exactly the gate's
	// shape. What closes that window is the startup rendezvous below: the
	// producer blocks in its own initialisation until its tables are in.
	// Registration on first sight of a PID (applyBatch -> note) remains as
	// the fallback for a producer that never reached it.

	// Bound BEFORE the link, for the same reason registration is: creating
	// the uprobe_multi link is what arms the shim's semaphores, and an armed
	// semaphore is what makes a producer try the rendezvous at all. A
	// listener bound after the link would miss a producer that started in
	// between.
	//
	// Best-effort, never fatal, for the same reason as everything else here:
	// a consumer with no rendezvous still profiles, registers lazily, and
	// says so in Stats.UnwindEnrollListening.
	if el, eerr := newEnrollListener(cfg, c.unwind); eerr != nil {
		c.enrollErr = eerr.Error()
	} else {
		c.enroll = el
	}

	// The cubin channel. Bound here beside the rendezvous and NOT as part of
	// it: its own address, its own listener, its own goroutine, its own
	// admission bucket. Best-effort in exactly the same way - a consumer
	// that cannot bind it still profiles, still walks stacks, and simply
	// resolves no source lines.
	//
	// The nil sink means "the one cfg asks for": cubinSinkFor writes accepted
	// cubins into Config.Modules when the caller supplied a store, and into
	// the bounded placeholder nothing reads when it did not. An explicit sink
	// is a test seam and nothing else.
	if cl, cerr := newCubinListener(cfg, nil); cerr != nil {
		c.cubinErr = cerr.Error()
	} else {
		c.cubin = cl
	}

	var ex *link.Executable
	if ex, err = link.OpenExecutable(cfg.ShimPath); err != nil {
		return
	}
	var l link.Link
	l, err = ex.UprobeMulti(nil, c.objs.GpuUsdtBatch, &link.UprobeMultiOptions{
		Addresses:     addrs,
		RefCtrOffsets: refCtrs,
		Cookies:       cookies,
		PID:           uint32(cfg.PID),
	})
	if err != nil {
		err = fmt.Errorf("uprobe_multi attach (needs Linux 6.6+): %w", err)
		return
	}
	c.links = append(c.links, l)

	// gpu_stacks is read (and each resolved entry deleted) by the sampled
	// stack path; see Consumer.resolveStackLocked.
	c.stacks = c.objs.GpuStacks

	if c.reader, err = ringbuf.NewReader(c.objs.Events); err != nil {
		return
	}
	return c, nil
}

// errSampledBatchNotSingular means a gpu_launch_sampled_v1 batch arrived
// carrying anything other than exactly one record.
//
// The stack id lives in the batch header, one per batch. A batch of N sampled
// launches would therefore attribute one captured stack to N unrelated
// launches — silently, since every record still decodes perfectly. The BPF
// program caps this kind at one record per batch (max_records in
// bpf/gpu_usdt.bpf.c) and the decoder refuses anything else, so neither end
// relies on the other, or on the producer, to hold the invariant.
var errSampledBatchNotSingular = errors.New("gpuprobe: sampled-launch batch must carry exactly one record")

// batch is one decoded ringbuf sample: a header plus the records it carried.
type batch struct {
	Kind     uint32
	Seq      uint64
	PID, TID uint32
	RawCount uint32
	// StackID is the BPF program's own gpu_stacks key for the launching
	// thread's user stack, or negative when the capture failed. It is a
	// handle the program mints, not a kernel stackmap id: the hybrid walker
	// that produces these frames cannot populate a BPF_MAP_TYPE_STACK_TRACE.
	// The wire field is unchanged in size and position. It is a property of the whole
	// batch because the BPF program stores it in the header — sound only
	// because the one kind that carries a stack, kindLaunchSampled, always
	// arrives with count == 1. That is enforced at both ends (the BPF
	// program's per-kind cap and decodeBatch), not assumed of the producer;
	// see errSampledBatchNotSingular. It is -1 on every other kind and must
	// only be interpreted for kindLaunchSampled.
	StackID         int32
	Launches        []gpuabi.Launch
	Execs           []gpuabi.Exec
	SampledLaunches []gpuabi.LaunchSampled
	KernelNames     []gpuabi.KernelName
	Modules         []gpuabi.ModuleLoad
	PCSamples       []gpuabi.PCSample
	StallReasons    []gpuabi.StallReason
	SamplingWindows []gpuabi.SamplingWindow
	Configs         []gpuabi.Config
	// Drops are decoded here and dispatched by class in noteDropLocked.
	// GPU_DROP_CLASS_GRAPH_EXEC is acted on - it arms Tier A's CUDA-graph
	// refusal, issue #94 - and the three Tier B loss classes are counted in
	// Stats.DropsUnconsumed until the task that turns each into an
	// operator-visible number lands. Nothing here is silent either way.
	Drops []gpuabi.Dropped
}

func decodeBatch(b []byte) (batch, error) {
	if len(b) < batchHdrSize {
		return batch{}, gpuabi.ErrShortRecord
	}
	le := binary.LittleEndian
	out := batch{
		Kind:    le.Uint32(b[0:]),
		Seq:     le.Uint64(b[8:]),
		PID:     le.Uint32(b[16:]),
		TID:     le.Uint32(b[20:]),
		StackID: int32(le.Uint32(b[32:])),
	}
	count := int(le.Uint32(b[4:]))
	out.RawCount = uint32(count)
	nbytes := int(le.Uint64(b[24:]))
	payload := b[batchHdrSize:]
	if nbytes < 0 || nbytes > len(payload) {
		return batch{}, fmt.Errorf("batch claims %d payload bytes, has %d", nbytes, len(payload))
	}
	payload = payload[:nbytes]

	switch out.Kind {
	case kindLaunch:
		// Division, not multiplication: count comes from a uint32 field and
		// count*SizeLaunch could overflow int on a 32-bit build.
		if count > len(payload)/gpuabi.SizeLaunch {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.Launches = make([]gpuabi.Launch, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeLaunch(payload[i*gpuabi.SizeLaunch:])
			if err != nil {
				return batch{}, err
			}
			out.Launches = append(out.Launches, rec)
		}
	case kindExec:
		if count > len(payload)/gpuabi.SizeExec {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.Execs = make([]gpuabi.Exec, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeExec(payload[i*gpuabi.SizeExec:])
			if err != nil {
				return batch{}, err
			}
			out.Execs = append(out.Execs, rec)
		}
	case kindModule:
		if count > len(payload)/gpuabi.SizeModuleLoad {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.Modules = make([]gpuabi.ModuleLoad, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeModuleLoad(payload[i*gpuabi.SizeModuleLoad:])
			if err != nil {
				return batch{}, err
			}
			out.Modules = append(out.Modules, rec)
		}
	case kindPC:
		if count > len(payload)/gpuabi.SizePCSample {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.PCSamples = make([]gpuabi.PCSample, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodePCSample(payload[i*gpuabi.SizePCSample:])
			if err != nil {
				return batch{}, err
			}
			out.PCSamples = append(out.PCSamples, rec)
		}
	case kindLaunchSampled:
		// One header, one stack id, one launch. See errSampledBatchNotSingular.
		if count != 1 {
			return batch{}, fmt.Errorf("%w: count=%d", errSampledBatchNotSingular, count)
		}
		if count > len(payload)/gpuabi.SizeLaunchSampled {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.SampledLaunches = make([]gpuabi.LaunchSampled, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeLaunchSampled(payload[i*gpuabi.SizeLaunchSampled:])
			if err != nil {
				return batch{}, err
			}
			out.SampledLaunches = append(out.SampledLaunches, rec)
		}
	case kindKernelName:
		if count > len(payload)/gpuabi.SizeKernelName {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.KernelNames = make([]gpuabi.KernelName, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeKernelName(payload[i*gpuabi.SizeKernelName:])
			if err != nil {
				return batch{}, err
			}
			out.KernelNames = append(out.KernelNames, rec)
		}
	case kindStallMap:
		if count > len(payload)/gpuabi.SizeStallReason {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.StallReasons = make([]gpuabi.StallReason, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeStallReason(payload[i*gpuabi.SizeStallReason:])
			if err != nil {
				return batch{}, err
			}
			out.StallReasons = append(out.StallReasons, rec)
		}
	case kindSamplingWindow:
		if count > len(payload)/gpuabi.SizeSamplingWindow {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.SamplingWindows = make([]gpuabi.SamplingWindow, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeSamplingWindow(payload[i*gpuabi.SizeSamplingWindow:])
			if err != nil {
				return batch{}, err
			}
			out.SamplingWindows = append(out.SamplingWindows, rec)
		}
	case kindConfig:
		if count > len(payload)/gpuabi.SizeConfig {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.Configs = make([]gpuabi.Config, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeConfig(payload[i*gpuabi.SizeConfig:])
			if err != nil {
				return batch{}, err
			}
			out.Configs = append(out.Configs, rec)
		}
	case kindDropped:
		if count > len(payload)/gpuabi.SizeDropped {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.Drops = make([]gpuabi.Dropped, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeDropped(payload[i*gpuabi.SizeDropped:])
			if err != nil {
				return batch{}, err
			}
			out.Drops = append(out.Drops, rec)
		}
	}
	return out, nil
}

// noteSeq counts batches lost between the ones that arrived. A gap is loss
// the consumer did not observe and must never be silent (spec §6.1). The
// stream is identified by (kind, pid): see seqKey. Caller holds mu.
// noteSeq records a batch's producer sequence number and returns how many
// records of this kind, from this process, are known to have been lost since
// the previous one.
//
// The return value is not decoration. The serialization disclosure below needs
// it: a hole in the sampling-window history means the intervals either side of
// it cannot be shown to be gaps, so the window store has to restart its
// coverage rather than span across the hole and report an unproven gap as a
// proven one. That is the difference between gpu_serialized="unknown" and
// gpu_serialized="false", and it is the one distinction this tier exists to
// keep.
func (c *Consumer) noteSeq(kind, pid uint32, seq uint64) uint64 {
	key := seqKey{kind: kind, pid: pid}
	prev, seen := c.seqByStream[key]
	var lost uint64
	if seen && seq > prev+1 {
		lost = seq - prev - 1
		c.stats.SequenceGaps += lost
	}
	c.seqByStream[key] = seq
	return lost
}

// correlationOf converts a wire correlation into the core's CorrelationID.
//
// pid is the batch header's — the process that fired the probe — and it is
// part of the id, not decoration. Vendor correlation counters restart from a
// low value in every process and the probes fire for every process that maps
// the shim, so a correlation without its pid collides across processes within
// the first handful of launches; see gpu.CorrelationID. Taking it as a
// parameter is what makes it impossible to build one of these without
// deciding whose it is.
//
// A wire value of zero means "this record carries no correlation" — the ABI
// says so explicitly for PC samples in continuous collection, the mode this
// project ships (shim/core/usdt_abi.h, spec §6.3 finding 3). It must map to a
// correlation that is not Present(), because that is what gpu/timeline.go
// tests to decide a record needs the heuristic join. Formatting it as the
// string "0" would produce a perfectly valid-looking ID that every
// uncorrelated record in one process shares, collapsing them into one
// exact-join bucket and yielding confident, wrong joins with nothing counted.
// The whole zero value is returned rather than one carrying only the pid, so
// that the older `== gpu.CorrelationID{}` reading and the Present() reading
// agree on these records.
//
// Caller holds mu: the zero case bumps a counter so the demotion is visible.
func (c *Consumer) correlationOf(pid uint32, v uint64) gpu.CorrelationID {
	if v == 0 {
		c.stats.ZeroCorrelation++
		return gpu.CorrelationID{}
	}
	return gpu.CorrelationID{Backend: c.cfg.Backend, PID: pid, Value: strconv.FormatUint(v, 10)}
}

// Run reads batches until the context is cancelled or the consumer is
// closed. It is not safe to call Run more than once concurrently.
func (c *Consumer) Run(ctx context.Context) error {
	// Cancellation closes the reader. It cannot use SetDeadline:
	// ringbuf.(*Reader).ReadInto takes the reader's own mutex and holds it
	// for the entire blocking loop, epoll wait included, while SetDeadline
	// wants that same mutex — so a deadline set on a *blocked* reader parks
	// forever behind the read it was supposed to interrupt, and nothing is
	// ever woken. Close is the documented interrupt ("It interrupts calls to
	// Read"), and it works precisely because it closes the poller BEFORE
	// acquiring the mutex. It is also idempotent: once the poller is closed a
	// second Close returns nil, so Consumer.Close remains safe afterwards.
	// The read then fails with ringbuf.ErrClosed, which the loop below treats
	// as a clean exit.
	stop := context.AfterFunc(ctx, func() { _ = c.reader.Close() })
	defer stop()
	// Nothing more will arrive once this loop ends, so no held launch can
	// still gain a stack. Releasing here (and not only in Close) is what
	// lets a caller cancel, wait for Run, and take a complete Snapshot
	// without closing the consumer.
	defer c.Flush()

	for {
		rec, err := c.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		b, err := decodeBatch(rec.RawSample)
		if err != nil {
			c.mu.Lock()
			c.stats.Malformed++
			// Why, not just how many: the error is the only evidence
			// that this sample was rejected rather than lost, and
			// throwing it away here is what makes a malformed-sample
			// count unactionable. See decodeFailureTable.
			c.decodeFailures.note(err)
			c.mu.Unlock()
			continue
		}
		c.applyBatch(b)
	}
}

func (c *Consumer) applyBatch(b batch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stats.Batches++
	lost := c.noteSeq(b.Kind, b.PID, b.Seq)
	// First sight of a process is what makes it interesting to the walker,
	// and "first sight" is any batch, not the first sampled launch.
	//
	// Sampled launches are one in SamplePeriod, and launch/exec/name batches
	// from the same process arrive earlier and far more often, so registering
	// on the sampled record alone would wait for a rare event before starting
	// work that takes tens to hundreds of milliseconds — and every sampled
	// launch in that window is an FP-only walk. Registering on the first
	// batch of any kind makes the un-tabled window as narrow as the transport
	// allows. Every process reaching here mapped the shim and fired a probe,
	// so nothing uninteresting is registered.
	//
	// O(1) and non-blocking; the compiling happens on the registry's own
	// goroutine, never on this one. See pidRegistry.
	c.unwind.note(b.PID)

	// A held launch is only ever waiting for a sampled record, and the
	// sampled record it is waiting for would be the very next thing off the
	// ringbuf (see sampledstacks.go). Anything else arriving means the wait
	// is over, so the queue is released here rather than left to age: the
	// timeline's join wants launches promptly, and a launch held past the
	// window buys nothing.
	if b.Kind != kindLaunchSampled {
		c.releaseDeferredLocked()
	}

	switch b.Kind {
	case kindLaunch:
		for _, l := range b.Launches {
			c.stats.Records++
			ev := gpu.GPUKernelLaunch{
				Correlation: c.correlationOf(b.PID, l.Correlation),
				// The shim stamps CLOCK_MONOTONIC, which is the only domain
				// the core accepts; say so rather than leaning on
				// NormalizeClockDomain's zero-value default.
				ClockDomain: gpu.ClockDomainCPUMonotonic,
				TimeNs:      l.TimeNs,
				Launch: gpu.LaunchContext{
					PID:    b.PID,
					TID:    l.TID,
					TimeNs: l.TimeNs,
					// CPUStack is filled in below if this launch was the one
					// in SamplePeriod the shim captured a stack for.
				},
			}
			c.admitLaunchLocked(ev, l.KernelID)
		}
	case kindExec:
		for _, e := range b.Execs {
			c.stats.Records++
			if e.Correlation == 0 {
				// Counted here rather than inside correlationOf, which is
				// shared with the launch, PC-sample and sampled-stack paths
				// and cannot tell them apart. See Stats.ZeroCorrelationExecs
				// for why an execution's zero is a contract violation while a
				// PC sample's is routine.
				c.stats.ZeroCorrelationExecs++
			}
			ev := gpu.GPUKernelExec{
				Correlation: c.correlationOf(b.PID, e.Correlation),
				ClockDomain: gpu.ClockDomainCPUMonotonic,
				StartNs:     e.StartNs,
				EndNs:       e.EndNs,
			}
			c.emitExecLocked(ev, e.KernelID)
		}
	case kindLaunchSampled:
		// One capture per batch, counted once per batch: decodeBatch has
		// already refused any count but 1, so the loop below runs exactly
		// once, but counting the capture failure outside it means a future
		// change to that rule degrades into an obvious undercount rather
		// than multiplying one failure by the record count.
		if b.StackID < 0 {
			// A launch whose stack capture failed is still a launch: it is
			// counted here and carried on, never discarded.
			c.stats.StacksMissing++
		}
		for _, sl := range b.SampledLaunches {
			c.stats.Records++
			c.stats.SampledLaunches++
			// Never emitted as a launch of its own: it is the same launch as
			// the batched gpu_launch_v1 record with the same correlation, and
			// the timeline has no dedup that would catch the double. Only its
			// stack travels on, onto that batched twin.
			c.attachSampledStackLocked(b.PID, b.StackID, sl)
		}
	case kindKernelName:
		for _, n := range b.KernelNames {
			c.stats.Records++
			c.learnKernelNameLocked(n)
		}
	case kindModule:
		for _, m := range b.Modules {
			c.stats.Records++
			c.emitModuleLocked(m)
		}
	case kindPC:
		for _, p := range b.PCSamples {
			c.stats.Records++
			c.emitPCSampleLocked(b.PID, p)
		}
	case kindStallMap:
		for _, s := range b.StallReasons {
			c.stats.Records++
			c.learnStallNameLocked(s)
		}
	case kindSamplingWindow:
		for i, w := range b.SamplingWindows {
			c.stats.Records++
			// The loss is attributed to the FIRST window in the batch and to
			// no other: `lost` counts records missing before this batch, so
			// charging it to every record in it would restart the store's
			// coverage once per window and throw away the rest of the batch's
			// evidence.
			var recLost uint64
			if i == 0 {
				recLost = lost
			}
			c.noteSamplingWindowLocked(b.PID, w, recLost)
		}
	case kindConfig:
		for _, cfg := range b.Configs {
			c.stats.Records++
			c.noteConfigLocked(cfg)
		}
	case kindDropped:
		for _, d := range b.Drops {
			c.stats.Records++
			c.noteDropLocked(b.PID, d)
		}
	default:
		// A kind neither side of the wire defines: carried, not normalized,
		// counted, never silent. Every kind the ABI DOES define has an arm
		// above, so reaching here is the ABI having drifted. See
		// Stats.Undecoded.
		c.stats.Undecoded += uint64(b.RawCount)
	}
}

// emitModuleLocked normalizes one gpu_module_load_v1 record and hands it to
// the sink. Caller holds mu.
//
// ModuleLoad.BytesPtr is decoded and deliberately NOT used. It is a pointer
// into the producer's address space; reading it would need /proc/<pid>/mem
// or process_vm_readv, both of which need CAP_SYS_PTRACE, which this agent
// does not have and does not take. The bytes travel the cubin channel
// instead (cubin.go). This record says that a module loaded, with its CRC
// and size, and nothing more.
func (c *Consumer) emitModuleLocked(rec gpuabi.ModuleLoad) {
	c.stats.ModulesDecoded++
	ev := gpu.GPUModule{
		Ref:       gpu.ModuleRef{Backend: c.cfg.Backend, CRC: rec.CubinCRC},
		SizeBytes: rec.SizeBytes,
		LoadedNs:  rec.LoadNs,
	}
	if err := c.cfg.Sink.EmitModule(ev); err != nil {
		c.stats.SinkRejected++
	}
}

// emitPCSampleLocked normalizes one gpu_pc_sample_batch_v1 record, resolving
// its stall index, and either hands it to the sink or holds it until the
// stall map arrives. Caller holds mu.
//
// The identity that always arrives is the MODULE, not the correlation:
// {Backend, CRC}, content-addressed, which is what Tier B attribution runs
// through. The correlation is taken from the batch header's pid through
// correlationOf, so a wire value of zero yields the zero CorrelationID (not
// one carrying only a pid) and a non-zero one is qualified by the process
// that produced it - the same rule launches and executions follow, for the
// same reason: vendor correlation counters restart in every process.
func (c *Consumer) emitPCSampleLocked(pid uint32, rec gpuabi.PCSample) {
	c.stats.PCSamplesDecoded++
	if rec.Correlation == 0 {
		// Counted here rather than inside correlationOf, which is shared
		// with the launch, exec and sampled-stack paths and cannot tell
		// them apart. See Stats.PCSamplesWithoutCorrelation for why a PC
		// sample's zero is routine in Tier B and a contract violation in
		// Tier A - two opposite healthy values that one counter cannot
		// carry.
		c.stats.PCSamplesWithoutCorrelation++
	}
	ev := gpu.GPUPCSample{
		Correlation: c.correlationOf(pid, rec.Correlation),
		Module:      gpu.ModuleRef{Backend: c.cfg.Backend, CRC: rec.CubinCRC},
		// The shim stamps CLOCK_MONOTONIC, which is the only domain the
		// core accepts; say so rather than leaning on
		// NormalizeClockDomain's zero-value default.
		//
		// TimeNs stays zero: gpu_pc_sample_batch_v1 carries no timestamp
		// and neither does the batch header, so there is nothing to put
		// there. Stamping the arrival time would present a consumer-side
		// clock reading as a measurement of when the instruction stalled,
		// which is exactly the inference-as-measurement this project
		// forbids. The timeline's pending horizon therefore does not age
		// these out by time; bounding them is Task 8a's second pending
		// index.
		ClockDomain: gpu.ClockDomainCPUMonotonic,
		PCOffset:    rec.PCOffset,
		// Carried, not dropped: FunctionIndex is half of the key
		// Timeline.pendingModule groups Tier B samples by. Leaving it zero
		// would put every sample of a process into one group per cubin -
		// a partial re-run of the collapse Task 8a exists to fix, and one
		// that reads as perfectly healthy because the counters only see
		// samples arriving and being grouped, not which group.
		FunctionIndex: rec.FunctionIndex,
		Count:         uint64(rec.Count),
	}
	if name, ok := c.resolveStallNameLocked(rec.StallIndex); ok {
		ev.StallReason = name
		c.sinkPCSampleLocked(ev, true)
		return
	}
	c.holdForStallNameLocked(unresolvedSample{stallIndex: rec.StallIndex, sample: ev})
}

// sinkPCSampleLocked is the only place a PC sample reaches the sink.
// resolved reports whether a stall name was found; an unresolved sample is
// delivered anyway - a missing name must never cost a record - with an
// EMPTY stall reason and a counter, never with the raw index rendered into
// it. Caller holds mu.
func (c *Consumer) sinkPCSampleLocked(ev gpu.GPUPCSample, resolved bool) {
	if !resolved {
		c.stats.StallNamesMissing++
	}
	if err := c.cfg.Sink.EmitPCSample(ev); err != nil {
		c.stats.SinkRejected++
	}
}

// resolveStallNameLocked looks a stall index up in the interned table.
//
// Deliberate divergence from resolveKernelNameLocked: there is no sentinel
// index. A kernel id of zero is the ABI's "no kernel", so that path can
// short-circuit; CUPTI's stall reason indices are opaque vendor numbers and
// zero is a perfectly ordinary one, so treating it as "none" would silently
// blank one real stall reason on every device where it happens to be index
// 0. Caller holds mu.
func (c *Consumer) resolveStallNameLocked(index uint32) (string, bool) {
	s, ok := c.stalls.get(index)
	if !ok {
		return "", false
	}
	return s.resolved(), true
}

// holdForStallNameLocked queues a PC sample until its stall name arrives,
// releasing the oldest held sample if the queue is full. Caller holds mu.
//
// The second deliberate divergence from the kernel-name path: there is no
// waitsForNameLocked gate here, so a sample with an unknown index is held
// whether or not a stall map has ever been seen. A launch is held only once
// the producer has demonstrated that names exist, because holding one for a
// name that is never coming delays the join; a PC sample is not on that
// path, and the case the gate would break is the one the ABI makes normal -
// the stall map is one-shot plus replay, so the FIRST PC batch of a run
// routinely precedes it, and a gate keyed on "have we seen a map yet" would
// send exactly those samples out permanently unresolved. A producer that
// never maps its indices costs a fixed lag of PendingStallSampleCapacity
// samples and nothing else: the queue releases its oldest on every push, so
// every sample is still delivered, counted in StallNamesMissing.
func (c *Consumer) holdForStallNameLocked(s unresolvedSample) {
	if released, ok := c.unresolvedStall.push(s); ok {
		c.releaseUnresolvedStallLocked(released, "")
	}
}

// releaseUnresolvedStallLocked sends one held PC sample on, with the stall
// name if one was found and without it otherwise. Caller holds mu.
func (c *Consumer) releaseUnresolvedStallLocked(s unresolvedSample, name string) {
	s.sample.StallReason = name
	c.sinkPCSampleLocked(s.sample, name != "")
}

// learnStallNameLocked interns one stall reason name and releases every PC
// sample waiting on it. The twin of learnKernelNameLocked. Caller holds mu.
func (c *Consumer) learnStallNameLocked(rec gpuabi.StallReason) {
	c.stats.StallNamesLearned++
	if rec.Truncated {
		c.stats.StallNamesTruncated++
	}
	s := stallName{name: rec.Name, truncated: rec.Truncated}
	c.stats.StallNamesEvicted += uint64(c.stalls.put(rec.Index, s))
	// A name that arrives empty resolves nothing, so held samples would be
	// released unresolved anyway; releasing them here keeps that decision
	// in one place.
	name := s.resolved()
	for _, waiting := range c.unresolvedStall.takeByIndex(rec.Index) {
		c.releaseUnresolvedStallLocked(waiting, name)
	}
}

// noteSamplingWindowLocked normalizes one PC-sampling burst and hands it to
// the sink. Caller holds mu.
//
// The PID comes from the batch header - the process that fired the probe - and
// travels IN the event rather than being checked somewhere else, the same
// discipline CorrelationID carries. Two processes both running Tier A produce
// interleaved windows on one system-wide attach, and a window store that
// mixed them would mark one process's executions perturbed because the other
// one was bursting.
//
// lost is how many window records from this process are known to have been
// dropped just before this one. It is the difference between "we hold an
// unbroken history and can prove this interval was a gap" and "we cannot", so
// it is carried on the event rather than left as a global counter nobody can
// attribute.
func (c *Consumer) noteSamplingWindowLocked(pid uint32, w gpuabi.SamplingWindow, lost uint64) {
	c.stats.SamplingWindowsDecoded++
	if w.Open() {
		// end_ns == 0 is the ABI's "still open when the producer stopped
		// reporting", not a zero-length window. DecodeSamplingWindow has
		// already refused a genuinely inverted one.
		//
		// This is NOT an anomaly by itself, and its doc comment on Stats says
		// why: the producer emits an open record at every burst START so that
		// a hard exit leaves the burst visible rather than losing it, so a
		// healthy Tier A run produces one of these per burst. What is an
		// anomaly is a window still open at Snapshot time, which
		// Snapshot.SamplingWindowsOpen reports.
		c.stats.SamplingWindowsOpen++
	}
	ev := gpu.GPUSamplingWindow{
		Backend:     c.cfg.Backend,
		PID:         pid,
		ClockDomain: gpu.ClockDomainCPUMonotonic,
		StartNs:     w.StartNs,
		EndNs:       w.EndNs,
		Mode:        gpu.SamplingMode(w.Mode),
		Lost:        lost,
		// Carried through verbatim. DecodeSamplingWindow has already zeroed it
		// on an open window, so it cannot arrive claiming a quiet interval
		// after a burst whose end is unknown.
		NextStartDeltaMs: w.NextStartDeltaMs,
	}
	if err := c.cfg.Sink.EmitSamplingWindow(ev); err != nil {
		c.stats.SinkRejected++
	}
}

// noteDropLocked dispatches one gpu_dropped_v1 record by its class. Caller
// holds mu.
//
// One class is ACTED ON here and the rest are counted: GPU_DROP_CLASS_GRAPH_EXEC
// arms Tier A's CUDA-graph refusal (issue #94) and is forwarded to the sink,
// while the three Tier B loss classes land in Stats.DropsUnconsumed until the
// task that gives each an operator-visible number lands.
//
// That asymmetry is not arbitrary. The other three describe LOSS - samples the
// hardware discarded, a buffer that filled, device time this mechanism
// structurally cannot see - and loss under-reports a profile, which is bad and
// is visible as a shortfall. The graph class describes something worse and
// entirely different: a claim that is FALSE while looking like the strongest
// answer the pipeline can give. A graph launch fires one runtime callback for
// N kernels and gpu_exec_v1 carries no graph id, so N executions share one
// correlation and Tier A bills N kernels to one call site with
// gpu_join="exact", gpu_pc_attrib="exact" and every counter green. It cannot
// wait for the general treatment of drop classes, because the whole point of
// the class is to stop a tier from running.
//
// pid is the batch header's - the process that fired the probe - which is what
// scopes the refusal. A graph-using process in a system-wide profile must not
// withdraw the exact attribution of every other process being profiled beside
// it.
func (c *Consumer) noteDropLocked(pid uint32, d gpuabi.Dropped) {
	c.stats.DropsDecoded++
	if d.Class != gpuabi.DropClassGraphExec {
		c.stats.DropsUnconsumed++
		return
	}
	c.stats.GraphExecReports++
	c.stats.GraphExecutions += d.Count
	ev := gpu.GPUGraphExecutions{
		Backend: c.cfg.Backend,
		PID:     pid,
		Count:   d.Count,
	}
	if err := c.cfg.Sink.EmitGraphExecutions(ev); err != nil {
		c.stats.SinkRejected++
	}
}

// noteConfigLocked records the producer's sampling configuration. Caller
// holds mu.
//
// Last writer wins, and ConfigsDisagreed is what stops that being a lie: a
// system-wide attach sees one of these per producer, so without the counter
// one process's sampling factor would silently stand for the machine's.
func (c *Consumer) noteConfigLocked(cfg gpuabi.Config) {
	c.stats.ConfigsDecoded++
	if c.configSeen && cfg != c.config {
		c.stats.ConfigsDisagreed++
	}
	c.config = cfg
	c.configSeen = true
	c.stats.ConfigSamplingFactor = cfg.SamplingFactor
	c.stats.ConfigSMCount = cfg.SMCount
	c.stats.ConfigClockHz = cfg.ClockHz
}

// admitLaunchLocked decides whether a normalized launch goes to the sink now
// or waits briefly for a sampled stack. Caller holds mu.
//
// Three cases, in the order they are cheapest to decide:
//
//   - No correlation. Pairing is by correlation and nothing else - matching
//     on pid/tid/time would be a guess dressed as a fact - so an
//     uncorrelated launch can never gain a stack and goes straight out.
//   - Its stack is already parked (the sampled record arrived first, the
//     common order). Attach and emit.
//   - Otherwise hold it: its sampled record may be the next ringbuf sample.
//     The queue releases on the next batch of any other kind, on Flush, when
//     Run returns and on Close, so "hold" is bounded by arrivals, not by
//     time, and never means "drop".
//
// The first two cases can therefore overtake a launch already held from the
// same batch. That reordering is bounded by one batch and is harmless to the
// consumers that exist: gpu.LaunchCache keys on the process-qualified
// correlation, orders eviction
// by insertion rather than by timestamp, and ignores a timestamp older than
// the newest it has seen (observeTimestampLocked), so an out-of-order Put
// neither displaces the wrong entry nor moves the horizon.
func (c *Consumer) admitLaunchLocked(ev gpu.GPUKernelLaunch, kernelID uint64) {
	if !ev.Correlation.Present() {
		c.emitLaunchLocked(ev, kernelID)
		return
	}
	// The correlation already names the process (correlationOf), so it is the
	// whole key: nothing here has to remember to add a pid.
	key := ev.Correlation
	if st, ok := c.pending.take(key); ok {
		c.attachLocked(&ev, st.frames, st.period)
		c.emitLaunchLocked(ev, kernelID)
		return
	}
	if released, ok := c.deferred.push(deferredLaunch{launch: ev, kernelID: kernelID}); ok {
		c.emitLaunchLocked(released.launch, released.kernelID)
	}
}

// attachSampledStackLocked resolves one sampled launch's captured stack and
// gets it onto the launch it belongs to - which is the batched
// gpu_launch_v1 record with the same correlation, not this record. Caller
// holds mu.
func (c *Consumer) attachSampledStackLocked(pid uint32, stackID int32, sl gpuabi.LaunchSampled) {
	if stackID < 0 {
		// The capture failed; already counted as StacksMissing by the
		// caller (and by kind in walk_errors), and there is no entry to
		// read or free.
		return
	}
	if sl.Correlation == 0 {
		// Unpairable: the ABI's "no correlation" leaves no way to say which
		// launch this stack belongs to, and attaching it to a plausible
		// neighbour would be a fabricated call path. The gpu_stacks slot is
		// still ours to release.
		c.stats.StacksUncorrelated++
		c.freeStackLocked(stackID)
		return
	}
	frames, ok := c.resolveStackLocked(pid, stackID)
	if !ok {
		return // counted inside resolveStackLocked
	}
	c.stats.StacksResolved++

	// A stack that never left the profiler's own injected module is not an
	// attribution, and attaching it would hand this launch's measured GPU
	// time to a call path inside the profiler. Withhold it: the launch ships
	// stackless and projects as unattributed, exactly as if the sampler had
	// never picked it. See shimScope for the rule and its failure modes.
	switch c.shim.verdict(frames) {
	case stackProfilerOnlyUncertain:
		c.stats.StacksProfilerOnlyUncertain++
		c.stats.StacksProfilerOnly++
		return
	case stackProfilerOnly:
		c.stats.StacksProfilerOnly++
		return
	case stackAttributable:
	}

	// Built through the same correlationOf as the batched twin's, from the
	// same batch-header pid, so the two keys are equal by construction and
	// neither can be a bare correlation value. sl.Correlation is non-zero
	// here (checked above), so correlationOf cannot take its ZeroCorrelation
	// branch and this does not double-count that demotion.
	key := c.correlationOf(pid, sl.Correlation)
	// The batched twin arrived first and is still being held: attach and let
	// it go now, since nothing more can arrive for it.
	if held, ok := c.deferred.take(key); ok {
		c.attachLocked(&held.launch, frames, sl.SamplePeriod)
		c.emitLaunchLocked(held.launch, held.kernelID)
		return
	}
	// Otherwise the twin is still to come; park the stack for it.
	c.stats.StacksEvicted += uint64(c.pending.park(key, frames, sl.SamplePeriod))
}

// attachLocked puts a resolved stack on a launch. SamplePeriod travels with
// the stack so a consumer can see how much GPU time one sampled call path
// stands for - it is NOT applied to the launch's own time anywhere in this
// pipeline. Caller holds mu.
func (c *Consumer) attachLocked(ev *gpu.GPUKernelLaunch, frames []pp.Frame, period uint32) {
	ev.Launch.CPUStack = frames
	ev.Launch.SamplePeriod = period
	c.stats.StacksAttached++
}

// decodeGPUStack reads one struct gpu_stack (bpf/gpu_usdt.bpf.c) out of a map
// value: n_pcs, walker_flags, then that many little-endian u64 PCs, leaf
// first. It returns ok=false only when the value is too short to be a
// gpu_stack at all.
//
// internal/bpfstack.ExtractIPs is deliberately NOT used here, even though it
// decodes the same little-endian u64s. It stops at the first zero slot,
// which is the right rule for a BPF_MAP_TYPE_STACK_TRACE value (the kernel
// zero-fills the tail) and the wrong one here: this value is copied out of a
// per-CPU scratch record whose tail still holds the PREVIOUS capture's
// frames, so a zero-terminated scan would splice two unrelated stacks into
// one call path and present it as fact. n_pcs is the only authority, and it
// is also what makes truncation detectable at all — a full-length walk is
// indistinguishable from a complete one once the length is thrown away.
//
// n_pcs comes off the wire, so it is clamped rather than trusted.
func decodeGPUStack(raw []byte) (pcs []uint64, tags []uint8, flags uint32, ok bool) {
	if len(raw) < gpuStackSize {
		return nil, nil, 0, false
	}
	le := binary.LittleEndian
	n := int(le.Uint32(raw[0:]))
	flags = le.Uint32(raw[4:])
	if n > maxWalkFrames {
		n = maxWalkFrames
	}
	pcs = make([]uint64, n)
	tags = make([]uint8, n)
	for i := range pcs {
		pcs[i] = le.Uint64(raw[gpuStackHdrSize+i*8:])
		// Only the first n tags are read, for the same reason only the first
		// n PCs are: the BPF side copies both arrays whole out of a per-CPU
		// scratch buffer, so the slots past n_pcs still hold the previous
		// capture's bytes.
		tags[i] = raw[gpuStackTagsOff+i]
	}
	return pcs, tags, flags, true
}

// resolveStackLocked turns a gpu_stacks handle into symbolized frames: read
// the entry, take the PCs the walk actually produced, symbolize them against
// the launching process, flatten to pprof frames. Caller holds mu.
//
// Two details differ from profile/:
//
//   - Order. ToProfFrames returns leaf-first; the gpu projection nests the
//     [gpu:launch] boundary and the kernel frame *under* the call path, so
//     the frames are reversed to root-first here, the same way profile/
//     reverses before handing frames to the pprof builder.
//   - Deletion. See freeStackLocked.
//
// Every failure path returns ok=false with a counter bumped, and the caller
// then ships the launch without a stack: a launch is never dropped because
// its stack could not be resolved.
func (c *Consumer) resolveStackLocked(pid uint32, stackID int32) ([]pp.Frame, bool) {
	if c.stacks == nil {
		c.stats.StackLookupFailed++
		return nil, false
	}
	raw, err := c.stacks.LookupBytes(uint32(stackID))
	if err != nil || len(raw) == 0 {
		c.stats.StackLookupFailed++
		// A failed lookup still leaves the slot occupied, and nothing else
		// ever removes it, so free it here the same as on the success path -
		// the entry is useless to us either way and gpu_stacks is finite.
		// ErrKeyNotExist is the one case with nothing to free, and calling
		// Delete for it would only inflate StackDeleteFailed.
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			c.freeStackLocked(stackID)
		}
		return nil, false
	}
	ips, tags, flags, ok := decodeGPUStack(raw)
	// Free the slot as soon as its contents are in hand, before anything
	// that can fail: the entry is useless to us from here on either way.
	c.freeStackLocked(stackID)
	if !ok {
		// A value too short to be a struct gpu_stack. Decoding whatever
		// prefix arrived would invent a call path out of a layout mismatch.
		c.stats.StackLookupFailed++
		return nil, false
	}
	if len(ips) == 0 {
		// The walk produced nothing. Counted twice on purpose: once as the
		// bucket this capture lands in, once as the reason.
		c.stats.StackLookupFailed++
		c.stats.StackWalkEmpty++
		return nil, false
	}
	// walker_flags says how the walk got these frames, independent of what
	// happens to them next - see Stats.StacksWalkedDWARF for why this is
	// counted here rather than gated on symbolization succeeding.
	if flags&walkerFlagDWARFUsed != 0 {
		c.stats.StacksWalkedDWARF++
	} else {
		c.stats.StacksWalkedFPOnly++
		// Why it was FP-only. A walk for a process the walker has no tables
		// for could not have been anything else, and that is a different
		// problem from a walk that had tables and never needed them - the
		// first is fixed by registering the process, the second is what a
		// frame-pointer-preserving call path legitimately looks like.
		// Counting only the FP-only case keeps the two disjoint by
		// construction: a DWARF walk proves tables existed.
		if !c.unwind.ready(pid) {
			c.stats.StacksWalkedNoTables++
			if c.unwind.enrolled(pid) {
				// The producer blocked in its own initialisation waiting for
				// exactly these tables and was told to go. Finding them
				// missing now is a contradiction, not a startup transient -
				// see Stats.StacksNoTablesAfterEnroll.
				c.stats.StacksNoTablesAfterEnroll++
			}
		}
	}
	if flags&walkerFlagCFIMiss != 0 {
		// A table was consulted and did not cover this PC. Not the same
		// failure as having no table at all, and the walk stopped there, so
		// this capture is also counted in StackWalkAbandoned below.
		c.stats.StacksWalkedCFIMiss++
	}
	// How the walk ENDED. The three FAILURE bits are mutually exclusive -
	// each of those arms in walk_step returns 1 the moment it sets its flag.
	// walkerFlagRAUndefined is not exclusive with walkerFlagFPTerminated:
	// since issue #45 a walk that runs off the end of the frame-pointer
	// chain takes one further step and the CFI of that last frame may also
	// declare it outermost, in which case both are set and the walk is
	// counted here once.
	if flags&walkerFlagRAUndefined != 0 {
		c.stats.StackWalkReachedRoot++
	}
	if flags&walkerFlagFPExhausted != 0 {
		// A lost frame pointer, not an end of chain - see
		// Stats.StackWalkFPExhausted. Also counted in StackWalkAbandoned
		// below, of which this is the named-cause subset.
		c.stats.StackWalkFPExhausted++
	}
	if flags&walkerFlagFPNonMonotonic != 0 {
		// A saved frame pointer that does not name a caller frame - see
		// Stats.StackWalkFPNonMonotonic. Also a named-cause subset of
		// StackWalkAbandoned.
		c.stats.StackWalkFPNonMonotonic++
	}
	if flags&walkerFlagRootDisagreement != 0 {
		// The FP chain said root and the CFI said otherwise - see
		// Stats.StackWalkRootDisagreement. Also a named-cause subset of
		// StackWalkAbandoned.
		c.stats.StackWalkRootDisagreement++
	}
	switch {
	case flags&walkerFlagRootDisagreement != 0:
		// Checked BEFORE walkerFlagsTerminated, and that order is the whole
		// point: this bit only ever arrives WITH walkerFlagFPTerminated, so
		// testing termination first would swallow it and the walk would read
		// as a clean success with nothing to show otherwise. An ending the
		// two sources disagree about is not an ending this consumer will
		// vouch for.
		c.stats.StackWalkAbandoned++
	case flags&walkerFlagsTerminated != 0:
		// The walk reached the end of the chain, not the end of a budget
		// and not a failure: either the FP chain's natural end
		// (saved_fp == 0, walkerFlagFPTerminated) or the point the unwind
		// information itself calls outermost (walkerFlagRAUndefined).
		// Both must be honoured here - a hybrid walk that crossed a
		// frame-pointer-less frame cannot end the first way, because the
		// DWARF step that carried it there zeroed the frame pointer, so
		// testing only walkerFlagFPTerminated counted every successful
		// DWARF walk as abandoned. Not truncated either, even in the
		// coincidental case where the end is the maxWalkFrames'th frame.
	case flags&(walkerFlagFPExhausted|walkerFlagFPNonMonotonic) != 0:
		// The frame-pointer chain gave out and the walk could not continue,
		// either because there was no frame pointer left to follow or
		// because the one it found did not name a caller frame. This case is
		// checked BEFORE the maxWalkFrames one on purpose: a walk that ran
		// out of frame pointer on its 127th frame did not stop because of
		// the budget, and calling it truncated would file a known failure
		// under "ran out of room".
		c.stats.StackWalkAbandoned++
	case len(ips) == maxWalkFrames:
		// No natural terminator, and bpf_loop ran out of iterations. The
		// frames are real and are used; the missing outermost ones - the
		// ones nearest main - are what this records.
		c.stats.StackWalkTruncated++
	default:
		// No end of chain, and the walk did not run out of budget either:
		// it stopped because it could not continue - a
		// bpf_probe_read_user fault at a live address, a frame pointer
		// that did not increase, a CFI lookup miss, or a return address
		// in a register this walker does not track
		// (bpf/unwind_common.h walk_step). This is the case
		// "len(ips) == maxWalkFrames" alone
		// could never catch: a walk that dies at the first
		// frame-pointer-omitting vendor frame produces n_pcs of roughly
		// 1-3 and used to read as a complete, untruncated stack.
		c.stats.StackWalkAbandoned++
	}
	if c.cfg.Symbolizer == nil {
		c.stats.SymbolizeFailed++
		return nil, false
	}
	// Kernel-range IPs are not split out the way profile/ does it. This
	// capture is taken at a uprobe on a USDT probe in the application's own
	// code, so the walked stack is user context by construction, unlike a
	// perf_event sample that can land mid-syscall. If that assumption ever
	// breaks, the symptom is unresolved frames, not misattributed ones.
	// The stack is symbolized against a LIVE process: blazesym resolves it
	// through /proc/<pid>/maps, which the kernel removes the moment the
	// process exits. If the launching process is gone by the time its record
	// is drained from the ringbuf, every frame comes back as a bare address
	// and lands in StacksUnresolved below.
	//
	// For a long-running GPU process that is a non-issue — the consumer
	// drains continuously and the producer outlives its own records by
	// orders of magnitude. For a SHORT-LIVED one it is not: a process that
	// launches a few kernels and exits can beat the drain, and the real
	// CUPTI adapter will meet exactly that workload. Fixing it properly
	// means capturing the maps eagerly (at first sight of a pid, or at
	// process-exit via a sched_process_exit probe) and symbolizing against
	// that snapshot rather than against /proc — which is Phase 4b work, not
	// this phase's. Until then the degradation is at least COUNTED, which is
	// the part that was missing.
	// Split the tagged slots before anything is symbolized. An interpreter
	// frame occupies two consecutive slots holding whatever pair of words its
	// unwinder chose; handing either to the symbolizer asks it to name an
	// address that is not code, and blazesym will happily place it in
	// whatever mapping it falls in. Two plausible, wrong native frames on the
	// one path this whole feature exists to serve - issue #83.
	//
	// The fold lives in unwind/interp, shared with the DWARF profilers'
	// consumer: one implementation of "which words are instruction pointers"
	// rather than two that can drift.
	slots, truncatedPair := interp.SplitSlots(ips, tags)
	if truncatedPair {
		c.stats.StackInterpPairsTruncated++
	}
	native := interp.NativeIPs(slots)

	var frames []symbolize.Frame
	if len(native) > 0 {
		var err error
		frames, err = c.cfg.Symbolizer.SymbolizeProcess(pid, native)
		if err != nil {
			c.stats.SymbolizeFailed++
			return nil, false
		}
	}
	// Symbolization that returns a frame per IP and resolves not one name is
	// not success. Counted here, before ToProfFrames flattens the inline
	// chains, because Reason survives only on symbolize.Frame - pprof frames
	// carry no failure field, so after the conversion an address-only name is
	// indistinguishable from a function genuinely called "0x4017c2".
	//
	// Bounded and allocation-free: one pass over the frames already in hand,
	// a few integer increments, nothing retained.
	var resolved, moduleOnly int
	for i := range frames {
		if frames[i].Reason == symbolize.FailureNone {
			resolved++
			continue
		}
		// Unresolved, but the symbolizer placed it in a mapping - see
		// symbolize.attachModules. Counted apart from the plain total so a
		// run that recovers no modules at all cannot read the same as one
		// that recovers them for every frame.
		if _, ok := frames[i].ModuleOffset(); ok {
			moduleOnly++
		}
	}
	c.stats.StackFramesUnresolved += uint64(len(frames) - resolved)
	c.stats.StackFramesModuleOnly += uint64(moduleOnly)
	if resolved == 0 && len(frames) > 0 {
		c.stats.StacksUnresolved++
	}
	// Splice the interpreter frames back in at their own positions, at the
	// symbolize.Frame level where the correspondence with `native` is still
	// one-to-one: ToProfFrames expands inline chains, so after it runs there
	// is no index to splice against any more. Counted after the
	// resolved/moduleOnly loop above so an unsymbolized interpreter frame does
	// not inflate StackFramesUnresolved, which measures the NATIVE
	// symbolizer's reach.
	// symbolize.LocalSymbolizer documents one Frame per IP (symbolize/local.go),
	// and the splice below relies on it: `frames` is indexed positionally
	// against the native slots. A shorter return would silently drop the tail
	// natives from the call path -- placed frames would stay in order, but the
	// stack would be short with nothing saying so. Refuse rather than emit a
	// truncated call path, and count it as the symbolization failure it is.
	if len(frames) != len(native) {
		c.stats.SymbolizeFailed++
		return nil, false
	}

	merged := frames
	if len(native) != len(slots) {
		merged = make([]symbolize.Frame, 0, len(slots))
		next := 0
		for _, sl := range slots {
			if sl.IsInterp() {
				c.stats.StackInterpFrames++
				merged = append(merged, symbolize.Frame{
					Address: sl.PC,
					Name:    sl.Name(),
					// Not FailureNone: the frame is placed correctly and its
					// address is real, but nothing named it, and pprof has no
					// unsymbolized bit of its own to infer that from later.
					Reason: symbolize.FailureMissingSymbols,
				})
				continue
			}
			merged = append(merged, frames[next])
			next++
		}
	}

	out := symbolize.ToProfFrames(merged)
	if len(out) == 0 {
		c.stats.SymbolizeFailed++
		return nil, false
	}
	pp.Reverse(out)
	return out, true
}

// freeStackLocked deletes a gpu_stacks entry once its contents have been
// read.
//
// This is not housekeeping, it is what keeps capture working. gpu_stacks
// holds gpuStackCapacity entries and nothing else ever removes one: the BPF
// side inserts with BPF_NOEXIST, so a full map (or an id that wrapped onto a
// live entry) refuses the NEW capture rather than overwriting the old one.
// Leak entries and occupancy grows with the length of the run instead of
// with what is in flight, until every sampled launch reports StacksMissing -
// which reads exactly like a broken capture path rather than a full map.
// Deleting each entry as it is consumed is what keeps the two proportional.
//
// (profile/ does not need this and does not do it: it drains its stackmap
// once at the end of a fixed-length profiling run. The continuous consumer
// here is the case that does.)
//
// A delete failure is counted, not ignored: it is the leading indicator of
// the map filling, and Stats.StackMapFull is what it leads to.
//
// Phase 4a's version of this comment had to document a race it could not
// fix: bpf_get_stackid is content-addressed, so freeing a bucket let a
// different stack land in it, and a batch still sitting in the ringbuf that
// referred to the old id would silently read the new occupant and attach a
// call path that did not produce that launch. The walker mints its own ids
// per CPU (next_stack_id in bpf/gpu_usdt.bpf.c) and never reuses one until
// 2^18 captures on that CPU have gone by, so an id in the ringbuf cannot
// name a different stack than the one it was minted for. The race is closed,
// not merely documented. Caller holds mu.
func (c *Consumer) freeStackLocked(stackID int32) {
	if c.stacks == nil {
		return
	}
	if err := c.stacks.Delete(uint32(stackID)); err != nil {
		c.stats.StackDeleteFailed++
	}
}

// emitLaunchLocked names a launch and hands it to the sink, or holds it if
// its name has not arrived yet. Caller holds mu.
func (c *Consumer) emitLaunchLocked(ev gpu.GPUKernelLaunch, kernelID uint64) {
	name, ok := c.resolveKernelNameLocked(kernelID)
	if !ok && c.waitsForNameLocked(kernelID) {
		held := ev
		c.holdForNameLocked(unnamedEvent{kernelID: kernelID, launch: &held})
		return
	}
	ev.KernelName = name
	c.sinkLaunchLocked(ev, ok)
}

// emitExecLocked is emitLaunchLocked for an execution. Executions need the
// name at least as much as launches do: the projection's
// [gpu:kernel:<name>] frame is built from the *execution's* KernelName, and
// the timeline's heuristic join matches on it. Caller holds mu.
func (c *Consumer) emitExecLocked(ev gpu.GPUKernelExec, kernelID uint64) {
	name, ok := c.resolveKernelNameLocked(kernelID)
	if !ok && c.waitsForNameLocked(kernelID) {
		held := ev
		c.holdForNameLocked(unnamedEvent{kernelID: kernelID, exec: &held})
		return
	}
	ev.KernelName = name
	c.sinkExecLocked(ev, ok)
}

// sinkLaunchLocked and sinkExecLocked are the only two places an event
// reaches the sink. named reports whether a kernel name was resolved; an
// unnamed event is delivered anyway - a missing name must never cost a
// record - and counted so it is visible rather than mysterious. Caller
// holds mu.
func (c *Consumer) sinkLaunchLocked(ev gpu.GPUKernelLaunch, named bool) {
	if !named {
		c.stats.KernelNamesUnresolved++
	}
	if err := c.cfg.Sink.EmitLaunch(ev); err != nil {
		c.stats.SinkRejected++
	}
}

func (c *Consumer) sinkExecLocked(ev gpu.GPUKernelExec, named bool) {
	if !named {
		c.stats.KernelNamesUnresolved++
	}
	if err := c.cfg.Sink.EmitExec(ev); err != nil {
		c.stats.SinkRejected++
	}
}

// resolveKernelNameLocked looks a kernel id up in the interned table. A
// kernel id of zero is the ABI's "no kernel", so it never resolves and
// never waits. Caller holds mu.
func (c *Consumer) resolveKernelNameLocked(kernelID uint64) (string, bool) {
	if kernelID == 0 {
		return "", false
	}
	k, ok := c.names.get(kernelID)
	if !ok {
		return "", false
	}
	return k.resolved(), true
}

// waitsForNameLocked decides whether an event with an unknown kernel id is
// worth holding. Only if this producer emits names at all: otherwise every
// event from a producer without the name probe would be held to its bound
// and released late, buying nothing. Caller holds mu.
func (c *Consumer) waitsForNameLocked(kernelID uint64) bool {
	return kernelID != 0 && c.sawKernelName
}

// holdForNameLocked queues an event until its name arrives, releasing the
// oldest held event if the queue is full. Caller holds mu.
func (c *Consumer) holdForNameLocked(ev unnamedEvent) {
	if released, ok := c.unnamed.push(ev); ok {
		c.releaseUnnamedLocked(released, "")
	}
}

// releaseUnnamedLocked sends one held event on, with the name if one was
// found and without it otherwise. Caller holds mu.
func (c *Consumer) releaseUnnamedLocked(ev unnamedEvent, name string) {
	switch {
	case ev.launch != nil:
		ev.launch.KernelName = name
		c.sinkLaunchLocked(*ev.launch, name != "")
	case ev.exec != nil:
		ev.exec.KernelName = name
		c.sinkExecLocked(*ev.exec, name != "")
	}
}

// learnKernelNameLocked interns one name and releases everything waiting on
// it. Caller holds mu.
func (c *Consumer) learnKernelNameLocked(rec gpuabi.KernelName) {
	c.stats.KernelNamesLearned++
	c.sawKernelName = true
	if rec.Truncated {
		c.stats.KernelNamesTruncated++
	}
	k := kernelName{name: rec.Name, truncated: rec.Truncated}
	c.stats.KernelNamesEvicted += uint64(c.names.put(rec.KernelID, k))
	// A name that arrives empty resolves nothing, so held events would be
	// released as unnamed anyway; releasing them here keeps that decision
	// in one place.
	name := k.resolved()
	for _, waiting := range c.unnamed.takeByKernel(rec.KernelID) {
		c.releaseUnnamedLocked(waiting, name)
	}
}

// releaseDeferredLocked emits every launch being held for a sampled stack,
// oldest first. They may then be held again for a kernel name, which
// releaseUnnamedAllLocked resolves - Flush runs the two in that order.
// Caller holds mu.
func (c *Consumer) releaseDeferredLocked() {
	for _, held := range c.deferred.drain() {
		c.emitLaunchLocked(held.launch, held.kernelID)
	}
}

// releaseUnnamedAllLocked sends on every event still waiting for a name,
// unnamed and counted. Caller holds mu.
func (c *Consumer) releaseUnnamedAllLocked() {
	for _, ev := range c.unnamed.drain() {
		c.releaseUnnamedLocked(ev, "")
	}
}

// releaseUnresolvedStallAllLocked sends on every PC sample still waiting for
// a stall name, unresolved and counted. Caller holds mu.
func (c *Consumer) releaseUnresolvedStallAllLocked() {
	for _, s := range c.unresolvedStall.drain() {
		c.releaseUnresolvedStallLocked(s, "")
	}
}

// Flush releases everything the consumer is holding back, in arrival order:
// launches waiting for a sampled stack, launches and executions waiting for
// a kernel name, and PC samples waiting for a stall name.
//
// Run calls it on the way out and Close calls it too, so a consumer that is
// finished never leaves a launch behind. It is exported for the other case:
// a caller taking a Snapshot while the consumer is still running would
// otherwise miss up to DeferredLaunchCapacity of the most recent launches -
// held, not lost, but absent from that snapshot and therefore able to make
// their executions look unmatched. Call Flush immediately before such a
// snapshot.
func (c *Consumer) Flush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Order matters: releasing a held launch can put it straight into the
	// name queue, so the launches go first and the name queue is drained
	// after, leaving nothing behind in either.
	c.releaseDeferredLocked()
	c.releaseUnnamedAllLocked()
	// PC samples held for a stall name are on their own queue and are
	// independent of the two above; a snapshot must not miss them either.
	c.releaseUnresolvedStallAllLocked()
}

// Stats returns the loss record, including the BPF-side drop counters read
// fresh from the `dropped` map.
// The `dropped` map is read under mu, not outside it: Close tears c.objs down
// under the same lock. Reading the map while another goroutine closes the map
// fd does not crash — the lookup just fails — so the symptom would have been a
// silent KernelDropped == 0, which is precisely the silent loss §6.1 forbids.
func (c *Consumer) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stats
	out.DecodeFailures = c.decodeFailures.snapshot()
	out.DecodeReasonsUnrecorded = c.decodeFailures.unrecorded
	out.KernelDropped = c.sumPerKind(c.objs.Dropped)
	out.KernelStacksMissing = c.sumPerKind(c.objs.StacksMissing)
	// Why each of those captures failed. StackWalkEmpty already carries the
	// consumer-side count; the BPF side's is added to it rather than kept
	// apart because the two are disjoint - an empty walk that the BPF side
	// counted never reached gpu_stacks for the consumer to see.
	out.StackWalkEmpty += c.walkError(walkErrEmpty)
	out.StackMapFull = c.walkError(walkErrMapFull)
	out.StackMapUpdateFailed = c.walkError(walkErrUpdate)
	out.StackWalkScratchFailed = c.walkError(walkErrNoScratch)
	// The registration side of the walker: which processes got CFI tables,
	// which did not, and what the bound pushed out. Read from the registry's
	// own lock rather than mirrored into c.stats, because the worker
	// goroutine — not this one — is what advances them.
	uw, tracked := c.unwind.snapshot()
	out.UnwindPIDsRegistered = uw.registered
	out.UnwindBinariesAttached = uw.binariesAttached
	out.UnwindPIDsFailed = uw.failed
	out.UnwindLastError = uw.lastErr
	out.UnwindPIDsEvicted = uw.evicted
	out.UnwindEnrolledPIDsEvicted = uw.enrolledEvicted
	out.UnwindEnrolledMarksDropped = uw.enrolledMarksDropped
	out.UnwindRequestsDropped = uw.requestsDropped
	out.UnwindReleaseFailed = uw.releaseFailed
	out.UnwindPIDsTracked = tracked
	en := c.enroll.snapshot()
	out.UnwindEnrollListening = c.enroll != nil
	out.UnwindEnrollAddress = c.enroll.address()
	out.UnwindEnrollRequests = en.requests
	out.UnwindEnrollConfirmed = en.confirmed
	out.UnwindEnrollRefused = en.refused
	out.UnwindEnrollThrottled = en.throttled
	out.UnwindEnrollFailed = en.failed
	out.UnwindEnrollLastError = en.lastErr
	if out.UnwindEnrollLastError == "" {
		out.UnwindEnrollLastError = c.enrollErr
	}
	cu := c.cubin.snapshot()
	out.CubinsListening = c.cubin != nil
	out.CubinsAddress = c.cubin.address()
	out.CubinsReceived = cu.received
	out.CubinBytesReceived = cu.bytes
	out.CubinsDuplicate = cu.duplicate
	out.CubinsRejectedTooLarge = cu.tooLarge
	out.CubinsRejectedMalformed = cu.malformed
	out.CubinsRejectedUnsealed = cu.unsealed
	out.CubinsRejectedUnauthorized = cu.unauthorized
	out.CubinsThrottled = cu.throttled
	out.CubinsLastError = cu.lastErr
	if out.CubinsLastError == "" {
		out.CubinsLastError = c.cubinErr
	}
	// Gauges, read fresh: what the two side tables are holding right now.
	out.PendingStacks = c.pending.len()
	out.PendingLaunches = c.deferred.len()
	out.PendingNamedEvents = c.unnamed.len()
	out.KnownKernelNames = c.names.len()
	out.PendingStallSamples = c.unresolvedStall.len()
	out.KnownStallNames = c.stalls.len()
	return out
}

// walkError reads one slot of the BPF `walk_errors` array: why a capture
// produced no stack. A read failure, or the nil map every unit test has,
// reads as zero rather than panicking. Caller holds mu.
func (c *Consumer) walkError(slot uint32) uint64 {
	if c.objs.WalkErrors == nil || slot >= walkErrMax {
		return 0
	}
	var v uint64
	if err := c.objs.WalkErrors.Lookup(&slot, &v); err != nil {
		return 0
	}
	return v
}

// sumPerKind totals one of the BPF program's per-kind counter arrays
// (`dropped`, `stacks_missing`). A read failure leaves the total at zero
// rather than panicking; the map is nil in tests that construct a Consumer
// directly. Caller holds mu.
func (c *Consumer) sumPerKind(m *ebpf.Map) uint64 {
	if m == nil {
		return 0
	}
	var total uint64
	for key := uint32(0); key < kindMax; key++ {
		var v uint64
		if err := m.Lookup(&key, &v); err != nil {
			continue
		}
		total += v
	}
	return total
}

// Close releases the ringbuf reader, the links and the BPF objects. It is
// safe to call on a partially constructed Consumer, on a nil one, and more
// than once.
func (c *Consumer) Close() error {
	// Nil-receiver safe: Attach's cleanup defer runs on whatever the named
	// result holds, and a caller that kept a nil *Consumer from a failed
	// Attach must get an error path, not a panic.
	if c == nil {
		return nil
	}
	var errs []error
	// c.reader is closed first and outside mu, because closing it is what
	// wakes a Run blocked in Read, and Run takes mu to apply each batch. It is
	// deliberately NOT set to nil: Run reads the field without holding mu.
	// ringbuf.Reader's Close, Read and SetDeadline are safe to call
	// concurrently and a second Close is a no-op, so leaving the pointer in
	// place is both race-free and idempotent.
	if c.reader != nil {
		errs = append(errs, c.reader.Close())
	}
	// Any launch still being held for a sampled stack goes to the sink
	// before the objects come down: the wait is over, and a held launch
	// dropped at teardown would be silent record loss - the one thing this
	// consumer must never do. Flush takes mu itself, so it runs before the
	// lock below rather than inside it.
	c.Flush()
	// Stop the registration worker before anything is torn down, and outside
	// mu. It writes CFI tables into c.objs' maps, so letting it run past the
	// close below would mean a compile finishing into closed file
	// descriptors. Outside mu because Stats takes mu and the wait can last a
	// whole compile. Idempotent, and a no-op on a nil registry, so a
	// half-built consumer from a failed Attach is safe.
	// The rendezvous goes down before the registry: it is the only other
	// thing that calls into the registrar, and closing it first means the
	// wait below is the last compile there can be. A producer blocked on an
	// accepted connection is released by the close (it reads EOF and takes
	// the lazy path), never left parked on a consumer that has gone.
	if err := c.enroll.close(); err != nil {
		errs = append(errs, err)
	}
	// The cubin channel comes down beside it. Nothing blocks on this one, so
	// the ordering carries no requirement; it is closed here so a peer
	// mid-offer is released rather than left writing into a consumer that
	// has gone.
	if err := c.cubin.close(); err != nil {
		errs = append(errs, err)
	}
	c.unwind.close()
	// The links and objects come down under mu so Stats, which reads the
	// `dropped` map, cannot be looking at c.objs while it is being closed.
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.links {
		errs = append(errs, l.Close())
	}
	c.links = nil
	errs = append(errs, c.objs.Close())
	return errors.Join(errs...)
}
