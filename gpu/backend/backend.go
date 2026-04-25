package backend

import (
	"context"

	"github.com/dpsoft/perf-agent/gpu"
)

type GPUBackendID = gpu.GPUBackendID
type GPUCapability = gpu.GPUCapability

type EventSink interface {
	EmitLaunch(gpu.GPUKernelLaunch)
	EmitExec(gpu.GPUKernelExec)
	EmitCounter(gpu.GPUCounterSample)
	EmitSample(gpu.GPUSample)
}

type Backend interface {
	ID() GPUBackendID
	Capabilities() []GPUCapability
	Start(ctx context.Context, sink EventSink) error
	Stop(ctx context.Context) error
	Close() error
}
