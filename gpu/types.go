package gpu

type GPUBackendID string

type GPUCapability string

const (
	CapabilityLaunchTrace    GPUCapability = "launch-trace"
	CapabilityExecTimeline   GPUCapability = "exec-timeline"
	CapabilityDeviceCounters GPUCapability = "device-counters"
	CapabilityPCSampling     GPUCapability = "gpu-pc-sampling"
	CapabilityStallReasons   GPUCapability = "stall-reasons"
	CapabilitySourceMap      GPUCapability = "gpu-source-correlation"
)

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
	CPUStack []any             `json:"cpu_stack"`
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
