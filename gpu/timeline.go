package gpu

import (
	"cmp"
	"maps"
	"slices"
)

type ExecutionView struct {
	Launch    *GPUKernelLaunch `json:"launch,omitempty"`
	Exec      GPUKernelExec    `json:"exec"`
	Samples   []GPUSample      `json:"samples,omitempty"`
	Heuristic bool             `json:"heuristic"`
}

type EventView struct {
	Launch    *GPUKernelLaunch `json:"launch,omitempty"`
	Event     GPUTimelineEvent `json:"event"`
	Heuristic bool             `json:"heuristic"`
}

type Snapshot struct {
	Executions   []ExecutionView       `json:"executions"`
	EventViews   []EventView           `json:"event_views,omitempty"`
	Events       []GPUTimelineEvent    `json:"events,omitempty"`
	Counters     []GPUCounterSample    `json:"counters,omitempty"`
	Attributions []WorkloadAttribution `json:"attributions,omitempty"`
}

type Timeline struct {
	launches []GPUKernelLaunch
	execs    []GPUKernelExec
	counters []GPUCounterSample
	samples  []GPUSample
	events   []GPUTimelineEvent
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

func (t *Timeline) RecordEvent(event GPUTimelineEvent) {
	t.events = append(t.events, cloneTimelineEvent(event))
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
	eventViews := make([]EventView, 0, len(t.events))
	for _, event := range t.events {
		view := EventView{Event: cloneTimelineEvent(event)}
		if launch := t.findLaunchForEvent(event); launch != nil {
			view.Launch = launch
			view.Heuristic = true
		}
		eventViews = append(eventViews, view)
	}
	return Snapshot{
		Executions:   views,
		EventViews:   eventViews,
		Events:       cloneTimelineEvents(t.events),
		Counters:     slices.Clone(t.counters),
		Attributions: buildAttributions(views, eventViews),
	}
}

type workloadKey struct {
	cgroupID    string
	podUID      string
	containerID string
}

func buildAttributions(executions []ExecutionView, events []EventView) []WorkloadAttribution {
	byKey := make(map[workloadKey]*WorkloadAttribution)
	for _, exec := range executions {
		if exec.Launch == nil {
			continue
		}
		key, ok := attributionKey(exec.Launch.Launch.Tags)
		if !ok {
			continue
		}
		entry := ensureAttribution(byKey, key)
		entry.ExecutionCount++
		entry.ExecutionDurationNs += max(1, exec.Exec.EndNs-exec.Exec.StartNs)
		if len(exec.Samples) == 0 {
			continue
		}
		for _, sample := range exec.Samples {
			entry.SampleWeight += max(1, sample.Weight)
		}
	}
	for _, event := range events {
		if event.Launch == nil {
			continue
		}
		key, ok := attributionKey(event.Launch.Launch.Tags)
		if !ok {
			continue
		}
		entry := ensureAttribution(byKey, key)
		entry.EventCount++
		entry.EventDurationNs += max(1, event.Event.DurationNs)
	}
	out := make([]WorkloadAttribution, 0, len(byKey))
	for _, entry := range byKey {
		out = append(out, *entry)
	}
	slices.SortFunc(out, func(a, b WorkloadAttribution) int {
		if diff := cmp.Compare(a.CgroupID, b.CgroupID); diff != 0 {
			return diff
		}
		if diff := cmp.Compare(a.PodUID, b.PodUID); diff != 0 {
			return diff
		}
		return cmp.Compare(a.ContainerID, b.ContainerID)
	})
	return out
}

func attributionKey(tags map[string]string) (workloadKey, bool) {
	key := workloadKey{
		cgroupID:    tags["cgroup_id"],
		podUID:      tags["pod_uid"],
		containerID: tags["container_id"],
	}
	if key == (workloadKey{}) {
		return workloadKey{}, false
	}
	return key, true
}

func ensureAttribution(byKey map[workloadKey]*WorkloadAttribution, key workloadKey) *WorkloadAttribution {
	if entry, ok := byKey[key]; ok {
		return entry
	}
	entry := &WorkloadAttribution{
		CgroupID:    key.cgroupID,
		PodUID:      key.podUID,
		ContainerID: key.containerID,
	}
	byKey[key] = entry
	return entry
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

func (t *Timeline) findLaunchForEvent(event GPUTimelineEvent) *GPUKernelLaunch {
	switch event.Kind {
	case TimelineEventSubmit, TimelineEventWait:
	default:
		return nil
	}
	var best *GPUKernelLaunch
	for _, launch := range t.launches {
		if launch.Launch.PID == 0 || launch.Launch.TID == 0 {
			continue
		}
		if launch.Launch.PID != event.PID || launch.Launch.TID != event.TID {
			continue
		}
		if launch.TimeNs > event.TimeNs {
			continue
		}
		if best == nil || launch.TimeNs > best.TimeNs {
			copy := cloneLaunch(launch)
			best = &copy
		}
	}
	return best
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

func cloneTimelineEvents(in []GPUTimelineEvent) []GPUTimelineEvent {
	out := make([]GPUTimelineEvent, 0, len(in))
	for _, event := range in {
		out = append(out, cloneTimelineEvent(event))
	}
	return out
}

func cloneTimelineEvent(in GPUTimelineEvent) GPUTimelineEvent {
	out := in
	out.Attributes = maps.Clone(in.Attributes)
	if in.Device != nil {
		device := *in.Device
		out.Device = &device
	}
	if in.Queue != nil {
		queue := *in.Queue
		out.Queue = &queue
	}
	return out
}
