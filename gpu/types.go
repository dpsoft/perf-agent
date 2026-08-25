// Package gpu defines the canonical, vendor-neutral GPU event model that
// backends (CUPTI, ROCm/HIP, DRM, replay fixtures) normalize into and that
// downstream joins/exporters consume.
package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	pp "github.com/dpsoft/perf-agent/pprof"
)

// GPUBackendID identifies the producer of a GPU event.
type GPUBackendID string

const (
	BackendLinuxDRM GPUBackendID = "linuxdrm"
	BackendReplay   GPUBackendID = "replay"
	BackendHIP      GPUBackendID = "hip"
	BackendCUPTI    GPUBackendID = "cupti"
)

// GPUCapability is a feature a Backend advertises it can produce.
type GPUCapability uint8

const (
	CapabilityInvalid GPUCapability = iota
	CapabilityLaunchTrace
	CapabilityExecTimeline
	CapabilityPCSampling
	CapabilityLifecycleTimeline
)

var capabilityNames = []GPUCapability{
	CapabilityLaunchTrace,
	CapabilityExecTimeline,
	CapabilityPCSampling,
	CapabilityLifecycleTimeline,
}

var capabilityToName = map[GPUCapability]string{
	CapabilityLaunchTrace:       "launch-trace",
	CapabilityExecTimeline:      "exec-timeline",
	CapabilityPCSampling:        "gpu-pc-sampling",
	CapabilityLifecycleTimeline: "lifecycle-timeline",
}

var nameToCapability = map[string]GPUCapability{
	"launch-trace":       CapabilityLaunchTrace,
	"exec-timeline":      CapabilityExecTimeline,
	"gpu-pc-sampling":    CapabilityPCSampling,
	"lifecycle-timeline": CapabilityLifecycleTimeline,
}

// CapabilityNames returns the known capabilities in stable order.
func CapabilityNames() []GPUCapability {
	return slices.Clone(capabilityNames)
}

func (c GPUCapability) String() string {
	if name, ok := capabilityToName[c]; ok {
		return name
	}
	return fmt.Sprintf("unknown-gpu-capability-%d", uint8(c))
}

func (c GPUCapability) MarshalJSON() ([]byte, error) {
	name, ok := capabilityToName[c]
	if !ok {
		return nil, fmt.Errorf("unknown gpu capability %d", c)
	}
	return json.Marshal(name)
}

func (c *GPUCapability) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("decode gpu capability: %w", err)
	}
	value, ok := nameToCapability[name]
	if !ok {
		return fmt.Errorf("unknown gpu capability %q", name)
	}
	*c = value
	return nil
}

// GPUDeviceRef identifies a physical or logical GPU device.
type GPUDeviceRef struct {
	Backend  GPUBackendID `json:"backend"`
	DeviceID string       `json:"device_id"`
	Name     string       `json:"name"`
}

// GPUQueueRef identifies a submission queue on a device.
type GPUQueueRef struct {
	Backend GPUBackendID `json:"backend"`
	Device  GPUDeviceRef `json:"device"`
	QueueID string       `json:"queue_id"`
}

// GPUExecutionRef identifies one kernel execution on a device/queue/context.
type GPUExecutionRef struct {
	Backend   GPUBackendID `json:"backend"`
	DeviceID  string       `json:"device_id"`
	QueueID   string       `json:"queue_id"`
	ContextID string       `json:"context_id"`
	ExecID    string       `json:"exec_id"`
}

// CorrelationID ties a launch to its later execution/sample events. It is
// comparable and safe to use directly as a map key.
//
// A vendor correlation value is unique only WITHIN one process. CUPTI's
// correlationId is a process-wide counter (§6.3 finding 4) and ROCm's
// correlation_id.internal is no different: every process starts its sequence
// from a low value. The probes, meanwhile, are attached with uprobe_multi
// against the shim *file*, so every process that maps it feeds one consumer
// and one Timeline, and system-wide (Config.PID == 0) is the documented
// default. Two profiled processes therefore collide on correlation within the
// first handful of launches.
//
// PID is what makes the identity whole, and it lives here - rather than as a
// separate argument each join site is trusted to remember - so the compiler
// enforces it at every construction site instead. That matters because a
// launch carries a symbolized CPU stack resolved against /proc/<pid>/maps of
// the process that produced it: a join across processes does not merely swap
// metadata, it attributes one process's measured GPU time to a call path in a
// different address space. A fabricated flame graph is worse than no flame
// graph (spec §4).
//
// PID is the process that produced the event, as observed by the probe (the
// batch header's pid), not a thread id; LaunchContext.TID carries the thread.
// A backend with no process to name (a device-global lifecycle producer)
// leaves it zero, and all such events then share one process namespace, which
// is exactly what they mean.
//
// Present, not the zero value, is the test for "this record carried a
// correlation at all" - see that method.
type CorrelationID struct {
	Backend GPUBackendID `json:"backend"`
	PID     uint32       `json:"pid,omitempty"`
	Value   string       `json:"value"`
}

// Present reports whether a correlation was actually supplied for this
// record. Value is the whole of that answer: PID and Backend are context the
// producer knows regardless, so a record that carries no vendor correlation
// may still arrive with both filled in, and comparing against the zero
// CorrelationID would then read it as a real, joinable id.
//
// This distinction is load-bearing. Timeline.Snapshot routes a correlation-
// less execution to the heuristic join and an execution whose correlation
// merely MISSED the cache to unattributed (spec §13, review Critical 2);
// mistaking the first for the second, or the reverse, is how an execution
// gets guess-attached to a launch it has no relationship with.
func (c CorrelationID) Present() bool { return c.Value != "" }

// ClockDomain identifies which clock a *_ns timestamp was measured against.
//
// Timestamp contract: every *_ns field emitted into the normalized GPU event
// model is in the CPU monotonic clock domain. Backends that observe
// GPU/device-local or host-synced clocks must convert them before emitting
// launches, executions, PC samples, modules, or timeline events. The core
// never converts; see ValidateSupportedClockDomain.
type ClockDomain uint8

const (
	ClockDomainInvalid ClockDomain = iota
	ClockDomainCPUMonotonic
	ClockDomainSynced
	ClockDomainGPUDevice
)

var clockDomainToName = map[ClockDomain]string{
	ClockDomainCPUMonotonic: "cpu-monotonic",
	ClockDomainSynced:       "synced",
	ClockDomainGPUDevice:    "gpu-device",
}

var nameToClockDomain = map[string]ClockDomain{
	"cpu-monotonic": ClockDomainCPUMonotonic,
	"synced":        ClockDomainSynced,
	"gpu-device":    ClockDomainGPUDevice,
}

func (d ClockDomain) String() string {
	if name, ok := clockDomainToName[d]; ok {
		return name
	}
	return fmt.Sprintf("unknown-clock-domain-%d", uint8(d))
}

func (d ClockDomain) MarshalJSON() ([]byte, error) {
	name, ok := clockDomainToName[d]
	if !ok {
		return nil, fmt.Errorf("unknown clock domain %d", d)
	}
	return json.Marshal(name)
}

func (d *ClockDomain) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("decode clock domain: %w", err)
	}
	value, ok := nameToClockDomain[name]
	if !ok {
		return fmt.Errorf("unknown clock domain %q", name)
	}
	*d = value
	return nil
}

// NormalizeClockDomain maps the zero value to ClockDomainCPUMonotonic so
// producers that omit the field default to the only supported domain.
func NormalizeClockDomain(domain ClockDomain) ClockDomain {
	if domain == ClockDomainInvalid {
		return ClockDomainCPUMonotonic
	}
	return domain
}

// ValidateSupportedClockDomain rejects any domain other than CPU-monotonic.
// The core never converts timestamps; producers must convert device or
// synced clocks to CPU-monotonic before emitting.
func ValidateSupportedClockDomain(domain ClockDomain) error {
	switch NormalizeClockDomain(domain) {
	case ClockDomainCPUMonotonic:
		return nil
	case ClockDomainSynced, ClockDomainGPUDevice:
		return fmt.Errorf("unsupported clock domain %q", NormalizeClockDomain(domain))
	default:
		return fmt.Errorf("unknown clock domain %d", domain)
	}
}

// LaunchContext carries the host-side context (CPU stack, thread, tags)
// captured at kernel launch time.
type LaunchContext struct {
	PID      uint32     `json:"pid"`
	TID      uint32     `json:"tid"`
	TimeNs   uint64     `json:"time_ns"`
	CPUStack []pp.Frame `json:"cpu_stack"`
	// SamplePeriod is the launch sampler's denominator at the moment this
	// launch's stack was captured: one launch in SamplePeriod carries a
	// CPU stack, the rest carry none (shim/core/sampler.h). It travels
	// with the stack rather than being reconstructed downstream, because
	// the period can change between runs and even mid-run.
	//
	// It is the MEAN stride, not an exact one. The shim jitters each gap
	// around SamplePeriod so the sampler cannot lock phase against a
	// periodic launch pattern -- a fixed stride at an even period gave one
	// kernel of an alternating pair every stack and the other none (issue
	// #50). The rate is unchanged: gaps are drawn from a set of consecutive
	// integers centred on SamplePeriod, so the long-run rate is exactly
	// 1/SamplePeriod. The exact schedule is replayable from the shim's seed
	// via internal/gpuabi.SampleSchedule.
	//
	// It is NOT a scale factor to multiply this launch's GPU time by.
	// Sampling applies to stack *capture* only: every execution is still
	// measured and joined, so durations stay exact and the sampled and
	// unsampled populations sum to the true GPU total. The period is
	// carried through to a per-sample label so a consumer that wants an
	// extrapolated estimate computes one deliberately, instead of being
	// handed an estimate dressed up as a measurement.
	SamplePeriod uint32            `json:"sample_period,omitempty"`
	Tags         map[string]string `json:"tags"`
}

// GPUKernelLaunch is emitted when a kernel is submitted for execution.
type GPUKernelLaunch struct {
	Correlation CorrelationID `json:"correlation"`
	Queue       GPUQueueRef   `json:"queue"`
	KernelName  string        `json:"kernel_name"`
	ClockDomain ClockDomain   `json:"clock_domain,omitempty"`
	TimeNs      uint64        `json:"time_ns"`
	Launch      LaunchContext `json:"launch"`
}

// GPUKernelExec is emitted when a launched kernel's execution window is
// known (start/end on-device), joined back to its launch by Correlation.
//
// There is no separate PID field: the process that produced this execution is
// carried by Correlation (see CorrelationID), which is the only identity the
// join uses and therefore the only place it cannot be forgotten.
//
// An execution that carried no correlation VALUE takes the heuristic path,
// but that does not make it process-less: Correlation.PID and
// Correlation.Value are independent (Present() tests Value alone), so a
// producer that knows which process fired the probe can - and must - say so
// even when the vendor gave it nothing to correlate on. Issue #52: the
// heuristic join uses exactly that field to refuse a match across processes,
// so a record that leaves it zero cannot be joined heuristically at all and
// degrades to unattributed instead.
type GPUKernelExec struct {
	Execution   GPUExecutionRef `json:"execution"`
	Correlation CorrelationID   `json:"correlation"`
	Queue       GPUQueueRef     `json:"queue"`
	KernelName  string          `json:"kernel_name"`
	ClockDomain ClockDomain     `json:"clock_domain,omitempty"`
	StartNs     uint64          `json:"start_ns"`
	EndNs       uint64          `json:"end_ns"`
}

// ModuleRef identifies a GPU binary (a cubin for CUPTI) by content hash. It is
// symbolization metadata, not an event: the bytes travel out of band and are
// resolved centrally.
type ModuleRef struct {
	Backend GPUBackendID `json:"backend"`
	CRC     uint64       `json:"crc"`
}

// GPUModule records that a GPU binary was loaded. Emitted on its own lifecycle,
// replayed to consumers that attach late.
type GPUModule struct {
	Ref       ModuleRef `json:"ref"`
	SizeBytes uint64    `json:"size_bytes"`
	LoadedNs  uint64    `json:"loaded_ns"`
}

// GPUPCSample is one program-counter sample with its stall reason.
// Capability-gated: only backends advertising CapabilityPCSampling emit these,
// so no backend pays for CUPTI's richness.
//
// PCOffset is an offset within Module, not an absolute address — it means
// nothing without the module, which is why the two are separate types.
//
// Module, not Correlation, is the identity that always arrives. CUPTI populates
// a PC sample's correlation ID only in kernel-serialized collection, which
// serializes execution and so is not what we run; in continuous mode it is
// always zero. Correlation is therefore optional here (Present() == false
// means unknown) and attribution goes through the module and PC offset.
//
// When a correlation IS supplied, it carries the producing process with it
// (see CorrelationID), so a sample can only ever be handed to an execution
// from the same process.
// FunctionIndex is the sampled instruction's device function within Module —
// CUPTI's per-module functionIndex, carried on gpu_pc_sample_batch_v1 since
// the ABI froze and populated by the consumer straight off the wire. It is
// meaningless without Module, exactly as PCOffset is: the pair
// (Module.CRC, FunctionIndex) is what names a device function, and
// (Module.CRC, FunctionIndex, PCOffset) is what names an instruction.
//
// It is load-bearing for continuous-mode collection specifically. With no
// correlation, {PID, Module.CRC, FunctionIndex} is the only key a sample can
// be grouped under, and without this field every sample a process produces
// keys identically — see Timeline.pendingModule.
type GPUPCSample struct {
	Correlation   CorrelationID `json:"correlation"`
	Module        ModuleRef     `json:"module"`
	FunctionIndex uint32        `json:"function_index,omitempty"`
	ClockDomain   ClockDomain   `json:"clock_domain,omitempty"`
	TimeNs        uint64        `json:"time_ns"`
	PCOffset      uint64        `json:"pc_offset"`
	StallReason   string        `json:"stall_reason,omitempty"`
	Count         uint64        `json:"count"`
}

// TimelineEventKind classifies a GPUTimelineEvent.
type TimelineEventKind string

const (
	TimelineEventRuntime TimelineEventKind = "runtime"
	TimelineEventSyscall TimelineEventKind = "syscall"
	TimelineEventIOCtl   TimelineEventKind = "ioctl"
	TimelineEventSubmit  TimelineEventKind = "submit"
	TimelineEventWait    TimelineEventKind = "wait"
	TimelineEventContext TimelineEventKind = "context"
	TimelineEventQueue   TimelineEventKind = "queue"
	TimelineEventMemory  TimelineEventKind = "memory"
	TimelineEventDevice  TimelineEventKind = "device"
)

// GPUTimelineEvent is a generic, low-level lifecycle event (syscall, ioctl,
// submit/wait, context/queue/memory/device activity) used to reconstruct
// GPU activity when richer launch/exec events aren't available.
type GPUTimelineEvent struct {
	Backend     GPUBackendID      `json:"backend"`
	Kind        TimelineEventKind `json:"kind"`
	Family      string            `json:"family,omitempty"`
	Name        string            `json:"name,omitempty"`
	ClockDomain ClockDomain       `json:"clock_domain,omitempty"`
	TimeNs      uint64            `json:"time_ns"`
	DurationNs  uint64            `json:"duration_ns,omitempty"`
	PID         uint32            `json:"pid,omitempty"`
	TID         uint32            `json:"tid,omitempty"`
	Device      *GPUDeviceRef     `json:"device,omitempty"`
	Queue       *GPUQueueRef      `json:"queue,omitempty"`
	ContextID   string            `json:"context_id,omitempty"`
	FD          int32             `json:"fd,omitempty"`
	ResultCode  int64             `json:"result_code,omitempty"`
	Driver      string            `json:"driver,omitempty"`
	Source      string            `json:"source,omitempty"`
	Confidence  string            `json:"confidence,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// JoinStats reports how well launches, executions and events correlated
// during a join pass, for diagnosing lossy or ambiguous joins.
//
// Minor cleanup (final whole-branch review): this used to also declare
// HeuristicEventJoinCount and UnmatchedCandidateEventCount, plus a separate
// WorkloadAttribution type with 11 more fields - all `omitempty`, none ever
// written anywhere in this package, so a serialized Snapshot with real drops
// elsewhere would still read as "zero event-join activity" for these,
// indistinguishable from a producer that genuinely had none. Removed rather
// than kept as documented-inert: they belong to the launch/event join
// (Timeline does not join GPUTimelineEvent to launches at all yet - see
// TimelineConfig.LaunchEventJoinWindowNs, which this phase repurposed for
// the launch/exec heuristic instead) and to attribution aggregation, neither
// of which exists yet. Re-add them, with a real writer in the same change,
// when that work lands.
type JoinStats struct {
	LaunchCount                  uint64 `json:"launch_count,omitempty"`
	MatchedLaunchCount           uint64 `json:"matched_launch_count,omitempty"`
	UnmatchedLaunchCount         uint64 `json:"unmatched_launch_count,omitempty"`
	ExactExecutionJoinCount      uint64 `json:"exact_execution_join_count,omitempty"`
	HeuristicExecutionJoinCount  uint64 `json:"heuristic_execution_join_count,omitempty"`
	AmbiguousHeuristicMatchCount uint64 `json:"ambiguous_heuristic_match_count,omitempty"`
	UnmatchedExecutionCount      uint64 `json:"unmatched_execution_count,omitempty"`
	// CorrelationlessExecutionCount counts executions that arrived carrying
	// no vendor correlation at all (CorrelationID.Present() == false) and
	// were therefore routed to the heuristic join instead of the exact one.
	//
	// It is the witness that issue #52's path was entered. The heuristic is
	// meant to be dead code on every backend shipping today: spec §6 makes a
	// correlation mandatory on every launch and execution, and gpuprobe
	// supplies one, so this counter reads zero on a healthy run. A path that
	// is unreachable *by convention* and counts nothing is indistinguishable
	// from a path nobody exercised; this makes the difference observable, so
	// a future backend, a malformed record or an adapter written to a
	// different assumption announces itself instead of quietly starting to
	// guess.
	//
	// It deliberately OVERLAPS the three mutually-exclusive outcome
	// counters: every execution counted here also lands in exactly one of
	// HeuristicExecutionJoinCount or UnmatchedExecutionCount (never in
	// ExactExecutionJoinCount, which by definition requires a correlation).
	// It is not part of the sum joinAnomalies checks.
	CorrelationlessExecutionCount uint64 `json:"correlationless_execution_count,omitempty"`
	// CrossProcessHeuristicBlockedCount counts heuristic joins that were
	// refused because the execution and the candidate launch could not be
	// shown to come from the same process - issue #52.
	//
	// A candidate DID qualify on queue, kernel name and the time window: had
	// the join been process-blind, as it was before this counter existed,
	// this execution would have been handed that launch, along with its
	// symbolized CPU stack and its Tags (pod_uid, container_id). Every
	// increment is therefore one cross-container attribution that did not
	// happen. "Cross-process" includes the case where the process is merely
	// unknown on one side rather than known-and-different: an unproven
	// match and a disproven one are refused identically, because a heuristic
	// join is already a guess and a guess about *which process* is the one
	// spec §4 forbids.
	//
	// The executions are not dropped. They degrade to unattributed - their
	// measured GPU time stays in the profile under FrameLaunchUnsampled,
	// carrying no stack and no tags - and are counted in
	// UnmatchedExecutionCount like any other miss. This counter says why.
	//
	// The remedy for a backend that trips it is to populate
	// CorrelationID.PID even on records that carry no vendor correlation
	// value: PID and Value are independent there (see CorrelationID.Present),
	// so "no correlation" and "no process" are separate statements and a
	// producer that knows the pid can always say so.
	CrossProcessHeuristicBlockedCount uint64 `json:"cross_process_heuristic_blocked_count,omitempty"`
	// OutOfWindowDropCount counts heuristic-join misses caused specifically
	// by LaunchEventJoinWindowNs excluding every candidate that would
	// otherwise have qualified (i.e. at least one candidate preceded the
	// exec, but none within the window) - distinct from a miss with no
	// preceding candidate at all. Became writable once review Important 3
	// wired the window into findLaunchHeuristic; every occurrence is also
	// counted in UnmatchedExecutionCount, the same way every heuristic hit
	// is counted in both HeuristicExecutionJoinCount and MatchedLaunchCount.
	OutOfWindowDropCount uint64 `json:"out_of_window_drop_count,omitempty"`
}

// EventSink receives normalized events from a backend. Every method returns an
// error so a backend can be pushed back on: a sink at capacity returns
// ErrSinkFull, and the backend is expected to drop and count rather than block.
type EventSink interface {
	EmitLaunch(GPUKernelLaunch) error
	EmitExec(GPUKernelExec) error
	EmitPCSample(GPUPCSample) error
	EmitModule(GPUModule) error
	EmitEvent(GPUTimelineEvent) error
}

// Backend produces normalized GPU events into an EventSink.
type Backend interface {
	ID() GPUBackendID
	Capabilities() []GPUCapability
	Start(ctx context.Context, sink EventSink) error
	Stop(ctx context.Context) error
	Close() error
}
