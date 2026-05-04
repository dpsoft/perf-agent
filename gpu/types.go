package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	pp "github.com/dpsoft/perf-agent/pprof"
)

type GPUBackendID string

const (
	BackendLinuxDRM   GPUBackendID = "linuxdrm"
	BackendLinuxKFD   GPUBackendID = "linuxkfd"
	BackendAMDSample  GPUBackendID = "amdsample"
	BackendStream     GPUBackendID = "stream"
	BackendReplay     GPUBackendID = "replay"
	BackendHIP        GPUBackendID = "hip"
	BackendHostReplay GPUBackendID = "host-replay"
)

type GPUCapability uint8

const (
	CapabilityInvalid GPUCapability = iota
	CapabilityLaunchTrace
	CapabilityExecTimeline
	CapabilityDeviceCounters
	CapabilityPCSampling
	CapabilityStallReasons
	CapabilitySourceMap
	CapabilityLifecycleTimeline
)

var capabilityNames = []GPUCapability{
	CapabilityLaunchTrace,
	CapabilityExecTimeline,
	CapabilityDeviceCounters,
	CapabilityPCSampling,
	CapabilityStallReasons,
	CapabilitySourceMap,
	CapabilityLifecycleTimeline,
}

var capabilityToName = map[GPUCapability]string{
	CapabilityLaunchTrace:       "launch-trace",
	CapabilityExecTimeline:      "exec-timeline",
	CapabilityDeviceCounters:    "device-counters",
	CapabilityPCSampling:        "gpu-pc-sampling",
	CapabilityStallReasons:      "stall-reasons",
	CapabilitySourceMap:         "gpu-source-correlation",
	CapabilityLifecycleTimeline: "lifecycle-timeline",
}

var nameToCapability = map[string]GPUCapability{
	"launch-trace":            CapabilityLaunchTrace,
	"exec-timeline":           CapabilityExecTimeline,
	"device-counters":         CapabilityDeviceCounters,
	"gpu-pc-sampling":         CapabilityPCSampling,
	"stall-reasons":           CapabilityStallReasons,
	"gpu-source-correlation":  CapabilitySourceMap,
	"lifecycle-timeline":      CapabilityLifecycleTimeline,
}

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

type GPUDeviceRef struct {
	Backend  GPUBackendID `json:"backend"`
	DeviceID string       `json:"device_id"`
	Name     string       `json:"name"`
}

type GPUQueueRef struct {
	Backend GPUBackendID `json:"backend"`
	Device  GPUDeviceRef `json:"device"`
	QueueID string       `json:"queue_id"`
}

type GPUExecutionRef struct {
	Backend   GPUBackendID `json:"backend"`
	DeviceID  string       `json:"device_id"`
	QueueID   string       `json:"queue_id"`
	ContextID string       `json:"context_id"`
	ExecID    string       `json:"exec_id"`
}

type CorrelationID struct {
	Backend GPUBackendID `json:"backend"`
	Value   string       `json:"value"`
}

// Timestamp contract:
//   - all *_ns fields emitted into the normalized GPU event model are in the
//     CPU monotonic clock domain
//   - backends that observe GPU/device-local clocks must convert them before
//     emitting launches, executions, samples, counters, or timeline events
//   - replay fixtures and stream NDJSON follow the same contract
type LaunchContext struct {
	PID      uint32            `json:"pid"`
	TID      uint32            `json:"tid"`
	TimeNs   uint64            `json:"time_ns"`
	CPUStack []pp.Frame        `json:"cpu_stack"`
	Tags     map[string]string `json:"tags"`
}

type GPUKernelLaunch struct {
	Correlation CorrelationID `json:"correlation"`
	Queue       GPUQueueRef   `json:"queue"`
	KernelName  string        `json:"kernel_name"`
	TimeNs      uint64        `json:"time_ns"`
	Launch      LaunchContext `json:"launch"`
}

type GPUKernelExec struct {
	Execution   GPUExecutionRef `json:"execution"`
	Correlation CorrelationID   `json:"correlation"`
	Queue       GPUQueueRef     `json:"queue"`
	KernelName  string          `json:"kernel_name"`
	StartNs     uint64          `json:"start_ns"`
	EndNs       uint64          `json:"end_ns"`
}

type GPUCounterSample struct {
	Device GPUDeviceRef `json:"device"`
	TimeNs uint64       `json:"time_ns"`
	Name   string       `json:"name"`
	Value  float64      `json:"value"`
	Unit   string       `json:"unit"`
}

type GPUSample struct {
	Correlation CorrelationID `json:"correlation"`
	Device      GPUDeviceRef  `json:"device"`
	TimeNs      uint64        `json:"time_ns"`
	KernelName  string        `json:"kernel_name"`
	PC          uint64        `json:"pc"`
	Function    string        `json:"function"`
	File        string        `json:"file"`
	Line        uint32        `json:"line"`
	StallReason string        `json:"stall_reason"`
	Weight      uint64        `json:"weight"`
}

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

type GPUTimelineEvent struct {
	Backend    GPUBackendID      `json:"backend"`
	Kind       TimelineEventKind `json:"kind"`
	Family     string            `json:"family,omitempty"`
	Name       string            `json:"name,omitempty"`
	TimeNs     uint64            `json:"time_ns"`
	DurationNs uint64            `json:"duration_ns,omitempty"`
	PID        uint32            `json:"pid,omitempty"`
	TID        uint32            `json:"tid,omitempty"`
	Device     *GPUDeviceRef     `json:"device,omitempty"`
	Queue      *GPUQueueRef      `json:"queue,omitempty"`
	ContextID  string            `json:"context_id,omitempty"`
	FD         int32             `json:"fd,omitempty"`
	ResultCode int64             `json:"result_code,omitempty"`
	Driver     string            `json:"driver,omitempty"`
	Source     string            `json:"source,omitempty"`
	Confidence string            `json:"confidence,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

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

type EventSink interface {
	EmitLaunch(GPUKernelLaunch)
	EmitExec(GPUKernelExec)
	EmitCounter(GPUCounterSample)
	EmitSample(GPUSample)
	EmitEvent(GPUTimelineEvent)
}

type Backend interface {
	ID() GPUBackendID
	EventBackends() []GPUBackendID
	Capabilities() []GPUCapability
	Start(ctx context.Context, sink EventSink) error
	Stop(ctx context.Context) error
	Close() error
}
