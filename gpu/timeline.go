package gpu

import (
	"maps"
	"slices"
)

type ExecutionView struct {
	Launch    *GPUKernelLaunch `json:"launch,omitempty"`
	Exec      GPUKernelExec    `json:"exec"`
	Samples   []GPUSample      `json:"samples,omitempty"`
	Heuristic bool             `json:"heuristic"`
}

type Snapshot struct {
	Executions []ExecutionView   `json:"executions"`
	Counters   []GPUCounterSample `json:"counters,omitempty"`
}

type Timeline struct {
	launches []GPUKernelLaunch
	execs    []GPUKernelExec
	counters []GPUCounterSample
	samples  []GPUSample
}

func NewTimeline() *Timeline {
	return &Timeline{}
}

func (t *Timeline) RecordLaunch(launch GPUKernelLaunch) {
	t.launches = append(t.launches, cloneLaunch(launch))
}

func (t *Timeline) RecordExec(exec GPUKernelExec) {
	t.execs = append(t.execs, exec)
}

func (t *Timeline) RecordCounter(counter GPUCounterSample) {
	t.counters = append(t.counters, counter)
}

func (t *Timeline) RecordSample(sample GPUSample) {
	t.samples = append(t.samples, sample)
}

func (t *Timeline) Snapshot() Snapshot {
	views := make([]ExecutionView, 0, len(t.execs))
	for _, exec := range t.execs {
		view := ExecutionView{
			Exec:    exec,
			Samples: samplesForExec(t.samples, exec),
		}
		if launch := t.findLaunchByCorrelation(exec); launch != nil {
			view.Launch = launch
		} else if launch := t.findLaunchHeuristic(exec); launch != nil {
			view.Launch = launch
			view.Heuristic = true
		}
		views = append(views, view)
	}
	return Snapshot{
		Executions: views,
		Counters:   slices.Clone(t.counters),
	}
}

func (t *Timeline) findLaunchByCorrelation(exec GPUKernelExec) *GPUKernelLaunch {
	if exec.Correlation == (CorrelationID{}) {
		return nil
	}
	for _, launch := range t.launches {
		if launch.Correlation == exec.Correlation {
			copy := cloneLaunch(launch)
			return &copy
		}
	}
	return nil
}

func (t *Timeline) findLaunchHeuristic(exec GPUKernelExec) *GPUKernelLaunch {
	for _, launch := range t.launches {
		if launch.Queue.Backend != exec.Queue.Backend || launch.Queue.QueueID != exec.Queue.QueueID {
			continue
		}
		if launch.KernelName != exec.KernelName {
			continue
		}
		if launch.TimeNs > exec.StartNs {
			continue
		}
		copy := cloneLaunch(launch)
		return &copy
	}
	return nil
}

func samplesForExec(all []GPUSample, exec GPUKernelExec) []GPUSample {
	var out []GPUSample
	for _, sample := range all {
		if sample.Correlation != (CorrelationID{}) && sample.Correlation == exec.Correlation {
			out = append(out, sample)
		}
	}
	return out
}

func cloneLaunch(in GPUKernelLaunch) GPUKernelLaunch {
	out := in
	out.Launch.Tags = maps.Clone(in.Launch.Tags)
	out.Launch.CPUStack = slices.Clone(in.Launch.CPUStack)
	return out
}
