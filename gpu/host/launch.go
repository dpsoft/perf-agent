package host

import (
	"fmt"
	"maps"
	"slices"

	"github.com/dpsoft/perf-agent/gpu/cgroupmeta"
	"github.com/dpsoft/perf-agent/gpu"
	pp "github.com/dpsoft/perf-agent/pprof"
)

type LaunchRecord struct {
	Backend       gpu.GPUBackendID   `json:"backend"`
	PID           uint32             `json:"pid"`
	TID           uint32             `json:"tid"`
	TimeNs        uint64             `json:"time_ns"`
	CPUStack      []pp.Frame         `json:"cpu_stack"`
	KernelName    string             `json:"kernel_name"`
	QueueID       string             `json:"queue_id"`
	ContextID     string             `json:"context_id"`
	CorrelationID string             `json:"correlation_id"`
	Tags          map[string]string  `json:"tags"`
	Source        string             `json:"source"`
}

type launchSink struct {
	sink    gpu.EventSink
	cgroups *cgroupmeta.PathCache
}

func NormalizeLaunch(record LaunchRecord) (gpu.GPUKernelLaunch, error) {
	if record.Backend == "" {
		return gpu.GPUKernelLaunch{}, fmt.Errorf("launch record missing backend")
	}
	if record.CorrelationID == "" {
		return gpu.GPUKernelLaunch{}, fmt.Errorf("launch record missing correlation_id")
	}
	if record.KernelName == "" {
		return gpu.GPUKernelLaunch{}, fmt.Errorf("launch record missing kernel_name")
	}

	return gpu.GPUKernelLaunch{
		Correlation: gpu.CorrelationID{
			Backend: record.Backend,
			Value:   record.CorrelationID,
		},
		Queue: gpu.GPUQueueRef{
			Backend: record.Backend,
			QueueID: record.QueueID,
		},
		KernelName: record.KernelName,
		ClockDomain: gpu.ClockDomainCPUMonotonic,
		TimeNs:     record.TimeNs,
		Launch: gpu.LaunchContext{
			PID:      record.PID,
			TID:      record.TID,
			TimeNs:   record.TimeNs,
			CPUStack: slices.Clone(record.CPUStack),
			Tags:     maps.Clone(record.Tags),
		},
	}, nil
}

func NewLaunchSink(sink gpu.EventSink) HostSink {
	return newLaunchSinkWithLookup(sink, cgroupmeta.LookupPath)
}

func (s launchSink) EmitLaunchRecord(record LaunchRecord) error {
	record.Tags = s.enrichTags(record.PID, record.Tags)
	launch, err := NormalizeLaunch(record)
	if err != nil {
		return err
	}
	s.sink.EmitLaunch(launch)
	return nil
}

func newLaunchSinkWithLookup(sink gpu.EventSink, lookup cgroupmeta.PathLookup) HostSink {
	return launchSink{
		sink:    sink,
		cgroups: cgroupmeta.NewPathCache(lookup),
	}
}

func (s launchSink) enrichTags(pid uint32, tags map[string]string) map[string]string {
	if s.cgroups == nil || pid == 0 {
		return maps.Clone(tags)
	}
	path, ok := s.cgroups.Lookup(pid)
	if !ok || path == "" {
		return maps.Clone(tags)
	}
	out := maps.Clone(tags)
	if out == nil {
		out = make(map[string]string)
	}
	if out["cgroup_path"] == "" {
		out["cgroup_path"] = path
	}
	meta := cgroupmeta.MetadataFromPath(path)
	if out["pod_uid"] == "" && meta.PodUID != "" {
		out["pod_uid"] = meta.PodUID
	}
	if out["container_id"] == "" && meta.ContainerID != "" {
		out["container_id"] = meta.ContainerID
	}
	if out["container_runtime"] == "" && meta.ContainerRuntime != "" {
		out["container_runtime"] = meta.ContainerRuntime
	}
	return out
}
