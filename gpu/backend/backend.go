package backend

import "context"

type GPUBackendID string

type GPUCapability string

type GPUDeviceRef struct {
	Backend  GPUBackendID
	DeviceID string
	Name     string
}

type GPUQueueRef struct {
	Backend GPUBackendID
	Device  GPUDeviceRef
	QueueID string
}

type GPUExecutionRef struct {
	Backend   GPUBackendID
	DeviceID  string
	QueueID   string
	ContextID string
	ExecID    string
}

type CorrelationID struct {
	Backend GPUBackendID
	Value   string
}

type LaunchContext struct {
	PID      uint32
	TID      uint32
	TimeNs   uint64
	CPUStack []any
	Tags     map[string]string
}

type GPUKernelLaunch struct {
	Correlation CorrelationID
	Queue       GPUQueueRef
	KernelName  string
	TimeNs      uint64
	Launch      LaunchContext
}

type GPUKernelExec struct {
	Execution   GPUExecutionRef
	Correlation CorrelationID
	Queue       GPUQueueRef
	KernelName  string
	StartNs     uint64
	EndNs       uint64
}

type GPUCounterSample struct {
	Device GPUDeviceRef
	TimeNs uint64
	Name   string
	Value  float64
	Unit   string
}

type GPUSample struct {
	Correlation CorrelationID
	Device      GPUDeviceRef
	TimeNs      uint64
	KernelName  string
	PC          uint64
	Function    string
	File        string
	Line        uint32
	StallReason string
	Weight      uint64
}

type EventSink interface {
	EmitLaunch(GPUKernelLaunch)
	EmitExec(GPUKernelExec)
	EmitCounter(GPUCounterSample)
	EmitSample(GPUSample)
}

type Backend interface {
	ID() GPUBackendID
	Capabilities() []GPUCapability
	Start(ctx context.Context, sink EventSink) error
	Stop(ctx context.Context) error
	Close() error
}
