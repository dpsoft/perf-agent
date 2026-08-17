package gpu

import (
	"fmt"
	"maps"

	pp "github.com/dpsoft/perf-agent/pprof"
)

// ProjectExecutions turns a joined Snapshot into pprof samples.
//
// The split between frames and labels is deliberate: frames are stack
// identity, so only what should nest in a flame graph goes there - the
// launch's real CPU stack, the [gpu:launch] boundary marker, then
// [gpu:kernel:<name>]. Everything that would otherwise fragment that
// identity (the per-sample PC, stall reason, queue/device/correlation, and
// the launch's tags) goes into per-sample labels instead. Two PC samples
// from the same kernel therefore share one stack and differ only by label.
func ProjectExecutions(snap Snapshot) []pp.ProfileSample {
	samples := make([]pp.ProfileSample, 0, len(snap.Executions))
	for _, view := range snap.Executions {
		frames := projectionFrames(view)
		pid := projectionPID(view)
		common := projectionLabels(view)

		if len(view.PCSamples) == 0 {
			samples = append(samples, pp.ProfileSample{
				Pid:         pid,
				SampleType:  pp.SampleTypeCpu,
				Aggregation: pp.SampleAggregated,
				Stack:       frames,
				Value:       executionWeight(view.Exec),
				Labels:      common,
			})
			continue
		}

		for _, pcs := range view.PCSamples {
			labels := maps.Clone(common)
			if labels == nil {
				labels = make(map[string]string, 2)
			}
			if pcs.StallReason != "" {
				labels["gpu_stall"] = pcs.StallReason
			}
			labels["gpu_pc"] = fmt.Sprintf("%#x", pcs.PCOffset)

			samples = append(samples, pp.ProfileSample{
				Pid:         pid,
				SampleType:  pp.SampleTypeCpu,
				Aggregation: pp.SampleAggregated,
				Stack:       frames,
				Value:       pcSampleWeight(pcs),
				Labels:      labels,
			})
		}
	}
	return samples
}

// projectionFrames builds the frame slice: the launch's real CPU stack (if
// any), the [gpu:launch] boundary, then the kernel frame. Nothing else - not
// the PC, not the stall reason, not the queue/device/correlation - belongs
// here; those go through projectionLabels instead.
func projectionFrames(view ExecutionView) []pp.Frame {
	var frames []pp.Frame
	if view.Launch != nil {
		frames = append(frames, view.Launch.Launch.CPUStack...)
	}
	frames = append(frames, pp.FrameFromName("[gpu:launch]"))
	if view.Exec.KernelName != "" {
		frames = append(frames, pp.FrameFromName(fmt.Sprintf("[gpu:kernel:%s]", view.Exec.KernelName)))
	}
	return frames
}

// projectionPID resolves the pid to attribute the sample to. An unmatched
// execution has no launch to take it from, so it projects with pid 0 rather
// than a fabricated one.
func projectionPID(view ExecutionView) uint32 {
	if view.Launch != nil {
		return view.Launch.Launch.PID
	}
	return 0
}

// projectionLabels builds the labels shared by every sample projected from
// one execution: queue, device, correlation, and the launch's tags (pod/
// container/cgroup). Per-PC-sample labels (gpu_stall, gpu_pc) are layered on
// top of a clone of this map by the caller.
func projectionLabels(view ExecutionView) map[string]string {
	labels := make(map[string]string)
	if view.Exec.Queue.QueueID != "" {
		labels["gpu_queue"] = view.Exec.Queue.QueueID
	}
	if view.Exec.Queue.Device.DeviceID != "" {
		labels["gpu_device"] = view.Exec.Queue.Device.DeviceID
	}
	if view.Exec.Correlation.Value != "" {
		labels["gpu_correlation"] = fmt.Sprintf("%s:%s", view.Exec.Correlation.Backend, view.Exec.Correlation.Value)
	}
	if view.Launch != nil {
		maps.Copy(labels, view.Launch.Launch.Tags)
	}
	return labels
}

// executionWeight is the fallback sample weight when an execution has no PC
// samples: the execution's own interval, floored at 1 so a zero-or-negative
// window still counts as one occurrence rather than vanishing.
func executionWeight(exec GPUKernelExec) uint64 {
	if exec.EndNs <= exec.StartNs {
		return 1
	}
	return exec.EndNs - exec.StartNs
}

// pcSampleWeight is a PC sample's own weight: its aggregated Count, floored
// at 1 for the same reason as executionWeight.
func pcSampleWeight(pcs GPUPCSample) uint64 {
	if pcs.Count < 1 {
		return 1
	}
	return pcs.Count
}
