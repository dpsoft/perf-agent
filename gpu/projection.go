package gpu

import (
	"fmt"
	"maps"
	"math/bits"

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
				SampleType:  pp.SampleTypeGpu,
				Aggregation: pp.SampleAggregated,
				Stack:       frames,
				Value:       executionWeight(view.Exec),
				Labels:      common,
			})
			continue
		}

		weights := distributeExecutionWeight(executionWeight(view.Exec), view.PCSamples)
		for i, pcs := range view.PCSamples {
			// common is projectionLabels' return value, which always starts
			// from `make(map[string]string)` - never nil - so maps.Clone(common)
			// is never nil either; no separate nil-guard is needed here.
			labels := maps.Clone(common)
			if pcs.StallReason != "" {
				labels["gpu_stall"] = pcs.StallReason
			}
			labels["gpu_pc"] = fmt.Sprintf("%#x", pcs.PCOffset)

			samples = append(samples, pp.ProfileSample{
				Pid:         pid,
				SampleType:  pp.SampleTypeGpu,
				Aggregation: pp.SampleAggregated,
				Stack:       frames,
				Value:       weights[i],
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
// one execution: the launch's tags (pod/container/cgroup) plus queue, device
// and correlation. Per-PC-sample labels (gpu_stall, gpu_pc) are layered on
// top of a clone of this map by the caller, after this function returns -
// see the reserved-name note below for why that ordering matters there too.
//
// Tags are copied in FIRST, and every gpu_* reserved label is set AFTER that
// copy, deliberately: Tags are producer-supplied (a launch's Tags come from
// CLI --tag flags and cgroup/k8s attribution, both attacker/operator
// controlled) while the gpu_* names carry facts this package itself derived
// from the joined execution and PC sample. A tag literally named "gpu_queue"
// or "gpu_correlation" must not be able to overwrite - and thereby forge -
// the real profiler-derived value; reserved names always win. gpu_stall and
// gpu_pc get the same protection by virtue of being set in ProjectExecutions
// after this function's map is cloned, i.e. also strictly after Tags.
//
// gpu_correlation is built from view.Exec.Correlation - the execution's own,
// vendor-reported correlation - not from the joined launch's Correlation.
// For an exact join the two are identical by construction (that's how the
// join found the launch), so it makes no observable difference. For a
// heuristic join, though, Exec.Correlation may be empty or may not equal the
// joined launch's Correlation: the exec's own correlation, if any, simply
// didn't match anything in the cache, which is why the heuristic path ran at
// all. This label reports what was actually observed on the execution, not
// what was inferred by the join; a heuristic match has no vendor-provided
// correlation for this execution to report, so the label reflects that
// honestly (present-if-nonzero, exactly as for an exact join) rather than
// borrowing the launch's Correlation and presenting an inference as an
// observation.
//
// gpu_join and gpu_ambiguous are the review Critical 1 fix: ExecutionView
// already carried Join/Heuristic/Ambiguous, but nothing ever read them on
// the way to a pprof sample, so a heuristic join (a guess) and an exact join
// (vendor-provided truth) were indistinguishable in the output - including
// the launch's Tags (pod_uid/container_id), which a heuristic join can
// attach to the wrong container. gpu_join is set unconditionally (not
// omitempty, no "only if non-default" branch) to exactly "exact",
// "heuristic" or "unmatched": an ABSENT label must never be readable as
// "exact" by a consumer that doesn't know to check for its absence.
// gpu_ambiguous is set (to "true") only when view.Ambiguous, since false is
// the overwhelmingly common case and the presence check on the exact/
// unmatched paths is unambiguous either way. Like every other gpu_* label,
// both are set after the Tags copy so a producer-supplied tag can never
// forge them.
func projectionLabels(view ExecutionView) map[string]string {
	labels := make(map[string]string)
	if view.Launch != nil {
		maps.Copy(labels, view.Launch.Launch.Tags)
	}
	if view.Exec.Queue.QueueID != "" {
		labels["gpu_queue"] = view.Exec.Queue.QueueID
	}
	if view.Exec.Queue.Device.DeviceID != "" {
		labels["gpu_device"] = view.Exec.Queue.Device.DeviceID
	}
	if view.Exec.Correlation.Value != "" {
		labels["gpu_correlation"] = fmt.Sprintf("%s:%s", view.Exec.Correlation.Backend, view.Exec.Correlation.Value)
	}
	switch view.Join {
	case JoinExact:
		labels["gpu_join"] = "exact"
	case JoinHeuristic:
		labels["gpu_join"] = "heuristic"
	default:
		labels["gpu_join"] = "unmatched"
	}
	if view.Ambiguous {
		labels["gpu_ambiguous"] = "true"
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

// distributeExecutionWeight is the review Critical 4 fix, part 2: a PC
// sample's weight used to be its own aggregated Count, fed into the same
// value dimension (SampleTypeCpu, nanoseconds-by-convention-but-not-really)
// as executionWeight's nanosecond duration. That mixed counts and
// nanoseconds in one field, and - independent of that - meant a kernel's
// total attributed time was its sample count, not its actual duration: a
// kernel sampled once for a count of 1 attributed 1ns; the same kernel
// running for 70us but never sampled attributed 70000ns via the no-PC-
// samples branch above. Two unrelated numbers, both mislabeled as the same
// unit.
//
// The fix: PC samples no longer get their own independent weight. Instead
// execWeight - the execution's real duration in nanoseconds, exactly what
// executionWeight returns - is split across pcs proportionally by each
// sample's Count, so the parts sum to the whole: a kernel's total attributed
// time equals its actual duration regardless of how many PC samples it
// received or how their counts are distributed. Integer division leaves a
// remainder (up to len(pcs)-1 nanoseconds); rather than let it vanish, it is
// added to the last sample's weight so sum(weights) == execWeight exactly.
func distributeExecutionWeight(execWeight uint64, pcs []GPUPCSample) []uint64 {
	weights := make([]uint64, len(pcs))
	if len(pcs) == 0 {
		return weights
	}

	counts := make([]uint64, len(pcs))
	var totalCount uint64
	for i, s := range pcs {
		c := s.Count
		if c < 1 {
			c = 1
		}
		counts[i] = c
		totalCount += c
	}

	var distributed uint64
	for i, c := range counts {
		// execWeight*c overflows uint64 for a large enough interval (nothing
		// validates EndNs-StartNs against a malformed producer), and a wrapped
		// product silently destroys proportionality: every share computes as 0
		// and the residue below hands the whole duration to the last sample.
		// bits.Mul64/Div64 carry the full 128-bit product, so the result is
		// exact for every input. Div64 cannot panic here: c <= totalCount, so
		// the quotient is at most execWeight and hi is therefore < totalCount.
		hi, lo := bits.Mul64(execWeight, c)
		w, _ := bits.Div64(hi, lo, totalCount)
		weights[i] = w
		distributed += w
	}
	if distributed < execWeight {
		weights[len(weights)-1] += execWeight - distributed
	}
	return weights
}
