package gpu

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
)

type ExecutionView struct {
	Launch    *GPUKernelLaunch `json:"launch,omitempty"`
	Exec      GPUKernelExec    `json:"exec"`
	Samples   []GPUSample      `json:"samples,omitempty"`
	Join      JoinKind         `json:"join,omitempty"`
	Heuristic bool             `json:"heuristic"`
}

type EventView struct {
	Launch    *GPUKernelLaunch `json:"launch,omitempty"`
	Event     GPUTimelineEvent `json:"event"`
	Join      JoinKind         `json:"join,omitempty"`
	Heuristic bool             `json:"heuristic"`
}

type JoinKind string

const (
	JoinExact     JoinKind = "exact"
	JoinHeuristic JoinKind = "heuristic"
)

type Snapshot struct {
	Launches     []GPUKernelLaunch     `json:"launches,omitempty"`
	Executions   []ExecutionView       `json:"executions"`
	EventViews   []EventView           `json:"event_views,omitempty"`
	Events       []GPUTimelineEvent    `json:"events,omitempty"`
	Counters     []GPUCounterSample    `json:"counters,omitempty"`
	JoinStats    JoinStats             `json:"join_stats,omitempty"`
	Attributions []WorkloadAttribution `json:"attributions,omitempty"`
}

type TimelineConfig struct {
	LaunchEventJoinWindowNs uint64
}

type Timeline struct {
	cfg      TimelineConfig
	launches []GPUKernelLaunch
	execs    []GPUKernelExec
	counters []GPUCounterSample
	samples  []GPUSample
	events   []GPUTimelineEvent
}

func NewTimeline(cfg ...TimelineConfig) *Timeline {
	if len(cfg) == 0 {
		return &Timeline{}
	}
	return &Timeline{cfg: cfg[0]}
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
			view.Join = JoinExact
		} else if launch := t.findLaunchHeuristic(exec); launch != nil {
			view.Launch = launch
			view.Join = JoinHeuristic
			view.Heuristic = true
		}
		views = append(views, view)
	}
	eventViews := make([]EventView, 0, len(t.events))
	for _, event := range t.events {
		view := EventView{Event: cloneTimelineEvent(event)}
		if launch := t.findLaunchForEvent(event); launch != nil {
			view.Launch = launch
			view.Join = JoinHeuristic
			view.Heuristic = true
		}
		eventViews = append(eventViews, view)
	}
	return Snapshot{
		Launches:     cloneLaunches(t.launches),
		Executions:   views,
		EventViews:   eventViews,
		Events:       cloneTimelineEvents(t.events),
		Counters:     slices.Clone(t.counters),
		JoinStats:    buildJoinStats(t.launches, views, eventViews),
		Attributions: buildAttributions(t.launches, views, eventViews),
	}
}

func buildJoinStats(launches []GPUKernelLaunch, executions []ExecutionView, events []EventView) JoinStats {
	stats := JoinStats{
		LaunchCount: uint64(len(launches)),
	}
	matched := make(map[string]struct{})
	for _, exec := range executions {
		switch exec.Join {
		case JoinExact:
			stats.ExactExecutionJoinCount++
		case JoinHeuristic:
			stats.HeuristicExecutionJoinCount++
		default:
			stats.UnmatchedExecutionCount++
		}
		if exec.Launch != nil {
			matched[launchJoinKey(*exec.Launch)] = struct{}{}
		}
	}
	for _, event := range events {
		if !isJoinCandidateEvent(event.Event) {
			continue
		}
		if event.Join == JoinHeuristic && event.Launch != nil {
			stats.HeuristicEventJoinCount++
			matched[launchJoinKey(*event.Launch)] = struct{}{}
			continue
		}
		stats.UnmatchedCandidateEventCount++
	}
	stats.MatchedLaunchCount = uint64(len(matched))
	if stats.LaunchCount >= stats.MatchedLaunchCount {
		stats.UnmatchedLaunchCount = stats.LaunchCount - stats.MatchedLaunchCount
	}
	return stats
}

type workloadKey struct {
	cgroupID    string
	podUID      string
	containerID string
	runtime     string
}

func buildAttributions(launches []GPUKernelLaunch, executions []ExecutionView, events []EventView) []WorkloadAttribution {
	byKey := make(map[workloadKey]*WorkloadAttribution)
	for _, launch := range launches {
		key, ok := attributionKey(launch.Launch.Tags)
		if !ok {
			continue
		}
		entry := ensureAttribution(byKey, key)
		entry.observe(launch.TimeNs, launch.TimeNs)
		entry.addBackend(launch.Correlation.Backend)
		entry.addKernelName(launch.KernelName)
		entry.LaunchCount++
	}
	for _, exec := range executions {
		if exec.Launch == nil {
			continue
		}
		key, ok := attributionKey(exec.Launch.Launch.Tags)
		if !ok {
			continue
		}
		entry := ensureAttribution(byKey, key)
		entry.observe(exec.Exec.StartNs, exec.Exec.EndNs)
		entry.addBackend(exec.Exec.Execution.Backend)
		entry.addKernelName(exec.Exec.KernelName)
		entry.observeJoin(exec.Join)
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
		entry.observe(event.Event.TimeNs, event.Event.TimeNs+max(1, event.Event.DurationNs))
		entry.addBackend(event.Event.Backend)
		entry.addEventFamily(eventFamily(event.Event))
		entry.observeJoin(event.Join)
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
		if diff := cmp.Compare(a.ContainerID, b.ContainerID); diff != 0 {
			return diff
		}
		return cmp.Compare(a.ContainerRuntime, b.ContainerRuntime)
	})
	return out
}

func attributionKey(tags map[string]string) (workloadKey, bool) {
	key := workloadKey{
		cgroupID:    tags["cgroup_id"],
		podUID:      tags["pod_uid"],
		containerID: tags["container_id"],
		runtime:     tags["container_runtime"],
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
		CgroupID:         key.cgroupID,
		PodUID:           key.podUID,
		ContainerID:      key.containerID,
		ContainerRuntime: key.runtime,
	}
	byKey[key] = entry
	return entry
}

func (w *WorkloadAttribution) observe(startNs, endNs uint64) {
	if startNs == 0 && endNs == 0 {
		return
	}
	if w.FirstSeenNs == 0 || startNs < w.FirstSeenNs {
		w.FirstSeenNs = startNs
	}
	if endNs > w.LastSeenNs {
		w.LastSeenNs = endNs
	}
}

func (w *WorkloadAttribution) addBackend(backend GPUBackendID) {
	if backend == "" {
		return
	}
	if slices.Contains(w.Backends, backend) {
		return
	}
	w.Backends = append(w.Backends, backend)
	slices.Sort(w.Backends)
}

func (w *WorkloadAttribution) addEventFamily(family string) {
	if family == "" {
		return
	}
	if slices.Contains(w.EventFamilies, family) {
		return
	}
	w.EventFamilies = append(w.EventFamilies, family)
	slices.Sort(w.EventFamilies)
}

func (w *WorkloadAttribution) addKernelName(name string) {
	if name == "" {
		return
	}
	if slices.Contains(w.KernelNames, name) {
		return
	}
	w.KernelNames = append(w.KernelNames, name)
	slices.Sort(w.KernelNames)
}

func (w *WorkloadAttribution) observeJoin(join JoinKind) {
	switch join {
	case JoinExact:
		w.ExactJoinCount++
	case JoinHeuristic:
		w.HeuristicJoinCount++
	}
}

func eventFamily(event GPUTimelineEvent) string {
	if event.Family != "" {
		return event.Family
	}
	if family := event.Attributes["command_family"]; family != "" {
		return family
	}
	return "unknown"
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
	if !isJoinCandidateEvent(event) {
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
		if t.cfg.LaunchEventJoinWindowNs > 0 && event.TimeNs-launch.TimeNs > t.cfg.LaunchEventJoinWindowNs {
			continue
		}
		if best == nil || launch.TimeNs > best.TimeNs {
			copy := cloneLaunch(launch)
			best = &copy
		}
	}
	return best
}

func isJoinCandidateEvent(event GPUTimelineEvent) bool {
	switch event.Kind {
	case TimelineEventSubmit, TimelineEventWait:
		return true
	case TimelineEventMemory:
		return event.Attributes["command_family"] == "kfd"
	default:
		return false
	}
}

func launchJoinKey(launch GPUKernelLaunch) string {
	return string(launch.Correlation.Backend) + "\x00" +
		launch.Correlation.Value + "\x00" +
		launch.KernelName + "\x00" +
		launch.Queue.QueueID + "\x00" +
		strconv.FormatUint(uint64(launch.Launch.PID), 10) + "\x00" +
		strconv.FormatUint(uint64(launch.Launch.TID), 10) + "\x00" +
		strconv.FormatUint(launch.TimeNs, 10)
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

func cloneLaunches(in []GPUKernelLaunch) []GPUKernelLaunch {
	out := make([]GPUKernelLaunch, 0, len(in))
	for _, launch := range in {
		out = append(out, cloneLaunch(launch))
	}
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
