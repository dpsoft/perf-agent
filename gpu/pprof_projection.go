package gpu

import (
	"fmt"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func ProjectExecutionSamples(snap Snapshot) []pp.ProfileSample {
	out := projectExecutionOnlySamples(snap.Executions)
	for _, eventView := range snap.EventViews {
		if eventView.Launch == nil {
			continue
		}
		out = append(out, pp.ProfileSample{
			Pid:         eventView.Launch.Launch.PID,
			SampleType:  pp.SampleTypeCpu,
			Aggregation: pp.SampleAggregated,
			Stack:       buildEventSyntheticStack(eventView),
			Value:       max(1, eventView.Event.DurationNs),
		})
	}
	return out
}

func projectExecutionOnlySamples(views []ExecutionView) []pp.ProfileSample {
	var out []pp.ProfileSample
	for _, execView := range views {
		switch {
		case len(execView.Samples) > 0:
			for _, gpuSample := range execView.Samples {
				out = append(out, pp.ProfileSample{
					Pid:         execView.launchPID(),
					SampleType:  pp.SampleTypeCpu,
					Aggregation: pp.SampleAggregated,
					Stack:       buildSyntheticStack(execView, gpuSample),
					Value:       max(1, gpuSample.Weight),
				})
			}
		default:
			out = append(out, pp.ProfileSample{
				Pid:         execView.launchPID(),
				SampleType:  pp.SampleTypeCpu,
				Aggregation: pp.SampleAggregated,
				Stack:       buildSyntheticStack(execView, GPUSample{}),
				Value:       max(1, execView.Exec.EndNs-execView.Exec.StartNs),
			})
		}
	}
	return out
}

func buildSyntheticStack(execView ExecutionView, gpuSample GPUSample) []pp.Frame {
	var stack []pp.Frame
	if execView.Launch != nil {
		stack = append(stack, execView.Launch.Launch.CPUStack...)
		stack = append(stack, buildLaunchTagFrames(execView.Launch.Launch.Tags)...)
	}
	stack = append(stack, pp.FrameFromName("[gpu:launch]"))
	if queueID := execView.Exec.Queue.QueueID; queueID != "" {
		stack = append(stack, pp.FrameFromName(fmt.Sprintf("[gpu:queue:%s]", queueID)))
	}
	if kernelName := execView.Exec.KernelName; kernelName != "" {
		stack = append(stack, pp.FrameFromName(fmt.Sprintf("[gpu:kernel:%s]", kernelName)))
	}
	if gpuSample.StallReason != "" {
		stack = append(stack, pp.FrameFromName(fmt.Sprintf("[gpu:stall:%s]", gpuSample.StallReason)))
	}
	if gpuSample.Function != "" {
		stack = append(stack, pp.FrameFromName(fmt.Sprintf("[gpu:function:%s]", gpuSample.Function)))
	}
	if gpuSample.File != "" {
		sourceFrame := fmt.Sprintf("[gpu:source:%s]", gpuSample.File)
		if gpuSample.Line != 0 {
			sourceFrame = fmt.Sprintf("[gpu:source:%s:%d]", gpuSample.File, gpuSample.Line)
		}
		stack = append(stack, pp.FrameFromName(sourceFrame))
	}
	if gpuSample.PC != 0 {
		stack = append(stack, pp.FrameFromName(fmt.Sprintf("[gpu:pc:%#x]", gpuSample.PC)))
	}
	return stack
}

func buildLaunchTagFrames(tags map[string]string) []pp.Frame {
	if len(tags) == 0 {
		return nil
	}
	var frames []pp.Frame
	if value := tags["cgroup_id"]; value != "" {
		frames = append(frames, pp.FrameFromName(fmt.Sprintf("[gpu:cgroup:%s]", value)))
	}
	if value := tags["pod_uid"]; value != "" {
		frames = append(frames, pp.FrameFromName(fmt.Sprintf("[gpu:pod:%s]", value)))
	}
	if value := tags["container_id"]; value != "" {
		frames = append(frames, pp.FrameFromName(fmt.Sprintf("[gpu:container:%s]", value)))
	}
	return frames
}

func buildEventSyntheticStack(eventView EventView) []pp.Frame {
	var stack []pp.Frame
	if eventView.Launch != nil {
		stack = append(stack, eventView.Launch.Launch.CPUStack...)
		stack = append(stack, buildLaunchTagFrames(eventView.Launch.Launch.Tags)...)
	}
	stack = append(stack, pp.FrameFromName("[gpu:launch]"))
	stack = append(stack, pp.FrameFromName(fmt.Sprintf("[gpu:event:%s:%s]", eventView.Event.Kind, eventView.Event.Name)))
	return stack
}

func (e ExecutionView) launchPID() uint32 {
	if e.Launch == nil {
		return 0
	}
	return e.Launch.Launch.PID
}
