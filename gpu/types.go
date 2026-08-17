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
type CorrelationID struct {
	Backend GPUBackendID `json:"backend"`
	Value   string       `json:"value"`
}

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
	PID      uint32            `json:"pid"`
	TID      uint32            `json:"tid"`
	TimeNs   uint64            `json:"time_ns"`
	CPUStack []pp.Frame        `json:"cpu_stack"`
	Tags     map[string]string `json:"tags"`
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

// GPUPCSample is one program-counter sample with its stall reason, attributed to
// a kernel launch by correlation ID. Capability-gated: only backends advertising
// CapabilityPCSampling emit these, so no backend pays for CUPTI's richness.
//
// PCOffset is an offset within Module, not an absolute address — it means
// nothing without the module, which is why the two are separate types.
type GPUPCSample struct {
	Correlation CorrelationID `json:"correlation"`
	Module      ModuleRef     `json:"module"`
	ClockDomain ClockDomain   `json:"clock_domain,omitempty"`
	TimeNs      uint64        `json:"time_ns"`
	PCOffset    uint64        `json:"pc_offset"`
	StallReason string        `json:"stall_reason,omitempty"`
	Count       uint64        `json:"count"`
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

// WorkloadAttribution aggregates GPU activity observed for one
// container/cgroup over a window, for cost/usage attribution.
type WorkloadAttribution struct {
	CgroupID            string         `json:"cgroup_id,omitempty"`
	PodUID              string         `json:"pod_uid,omitempty"`
	ContainerID         string         `json:"container_id,omitempty"`
	ContainerRuntime    string         `json:"container_runtime,omitempty"`
	FirstSeenNs         uint64         `json:"first_seen_ns,omitempty"`
	LastSeenNs          uint64         `json:"last_seen_ns,omitempty"`
	Backends            []GPUBackendID `json:"backends,omitempty"`
	EventFamilies       []string       `json:"event_families,omitempty"`
	KernelNames         []string       `json:"kernel_names,omitempty"`
	LaunchCount         uint64         `json:"launch_count,omitempty"`
	ExactJoinCount      uint64         `json:"exact_join_count,omitempty"`
	HeuristicJoinCount  uint64         `json:"heuristic_join_count,omitempty"`
	ExecutionCount      uint64         `json:"execution_count,omitempty"`
	ExecutionDurationNs uint64         `json:"execution_duration_ns,omitempty"`
	SampleWeight        uint64         `json:"sample_weight,omitempty"`
	EventCount          uint64         `json:"event_count,omitempty"`
	EventDurationNs     uint64         `json:"event_duration_ns,omitempty"`
}

// JoinStats reports how well launches, executions and events correlated
// during a join pass, for diagnosing lossy or ambiguous joins.
type JoinStats struct {
	LaunchCount                  uint64 `json:"launch_count,omitempty"`
	MatchedLaunchCount           uint64 `json:"matched_launch_count,omitempty"`
	UnmatchedLaunchCount         uint64 `json:"unmatched_launch_count,omitempty"`
	ExactExecutionJoinCount      uint64 `json:"exact_execution_join_count,omitempty"`
	HeuristicExecutionJoinCount  uint64 `json:"heuristic_execution_join_count,omitempty"`
	AmbiguousHeuristicMatchCount uint64 `json:"ambiguous_heuristic_match_count,omitempty"`
	UnmatchedExecutionCount      uint64 `json:"unmatched_execution_count,omitempty"`
	HeuristicEventJoinCount      uint64 `json:"heuristic_event_join_count,omitempty"`
	OutOfWindowDropCount         uint64 `json:"out_of_window_drop_count,omitempty"`
	UnmatchedCandidateEventCount uint64 `json:"unmatched_candidate_event_count,omitempty"`
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
