package gpu

import (
	"fmt"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func ProjectExecutionSamples(snap Snapshot) []pp.ProfileSample {
	var out []pp.ProfileSample
	for _, execView := range snap.Executions {
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
	if gpuSample.PC != 0 {
		stack = append(stack, pp.FrameFromName(fmt.Sprintf("[gpu:pc:%#x]", gpuSample.PC)))
	}
	return stack
}

func (e ExecutionView) launchPID() uint32 {
	if e.Launch == nil {
		return 0
	}
	return e.Launch.Launch.PID
}
