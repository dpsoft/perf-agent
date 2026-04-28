package gpu

import (
	"context"
	"slices"

	pp "github.com/dpsoft/perf-agent/pprof"
)

type GPUBackendID string

type GPUCapability string

const (
	CapabilityLaunchTrace       GPUCapability = "launch-trace"
	CapabilityExecTimeline      GPUCapability = "exec-timeline"
	CapabilityDeviceCounters    GPUCapability = "device-counters"
	CapabilityPCSampling        GPUCapability = "gpu-pc-sampling"
	CapabilityStallReasons      GPUCapability = "stall-reasons"
	CapabilitySourceMap         GPUCapability = "gpu-source-correlation"
	CapabilityLifecycleTimeline GPUCapability = "lifecycle-timeline"
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

func CapabilityNames() []GPUCapability {
	return slices.Clone(capabilityNames)
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
	LaunchCount         uint64         `json:"launch_count,omitempty"`
	ExactJoinCount      uint64         `json:"exact_join_count,omitempty"`
	HeuristicJoinCount  uint64         `json:"heuristic_join_count,omitempty"`
	ExecutionCount      uint64         `json:"execution_count,omitempty"`
	ExecutionDurationNs uint64         `json:"execution_duration_ns,omitempty"`
	SampleWeight        uint64         `json:"sample_weight,omitempty"`
	EventCount          uint64         `json:"event_count,omitempty"`
	EventDurationNs     uint64         `json:"event_duration_ns,omitempty"`
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
	Capabilities() []GPUCapability
	Start(ctx context.Context, sink EventSink) error
	Stop(ctx context.Context) error
	Close() error
}
