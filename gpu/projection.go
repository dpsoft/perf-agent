package gpu

import (
	"fmt"
	"maps"
	"math/bits"
	"strconv"

	pp "github.com/dpsoft/perf-agent/pprof"
)

// FrameLaunch is the boundary marker under a launch whose real CPU call
// path is known, i.e. one the launch sampler selected for stack capture.
// FrameLaunchUnsampled is the marker for an execution whose launch carried
// no stack - because the launch was not sampled, because the capture or its
// symbolization failed, or because no launch joined at all.
//
// They are deliberately two different frames rather than one marker plus a
// label: the frame is what a flame graph nests on, so the attributed and
// unattributed populations must be separable by *shape*, not only by a
// label a viewer may never render. An unsampled execution never borrows a
// sampled sibling's call path - see projectionFrames.
const (
	FrameLaunch          = "[gpu:launch]"
	FrameLaunchUnsampled = "[gpu:launch unsampled]"
)

// ProjectExecutions turns a joined Snapshot into pprof samples.
//
// The split between frames and labels is deliberate: frames are stack
// identity, so only what should nest in a flame graph goes there - the
// launch's real CPU stack, the [gpu:launch] boundary marker, then
// [gpu:kernel:<name>]. Everything that would otherwise fragment that
// identity (the per-sample PC, stall reason, queue/device/correlation, the
// producing process, and the launch's tags) goes into per-sample labels
// instead. Two PC samples
// from the same kernel therefore share one stack and differ only by label.
//
// Sampling and honesty. Launch stacks are sampled (one launch in
// LaunchContext.SamplePeriod carries one), but executions are not: every
// execution is recorded and joined, so its duration is measured, not
// estimated. What sampling costs is the *attribution* of that duration to a
// CPU call path. This function therefore projects two populations and never
// scales either one:
//
//   - an execution whose launch carried a sampled stack projects as
//     <real CPU stack> -> [gpu:launch] -> [gpu:kernel:<name>], with its own
//     duration as the value and gpu_sample_period as a label;
//   - an execution with no stack projects as
//     [gpu:launch unsampled] -> [gpu:kernel:<name>], again with its own
//     duration.
//
// The two populations sum to exactly the measured GPU total. Multiplying
// the sampled population by SamplePeriod would turn a measurement into an
// estimate and present it as fact - the same dishonesty this package
// already refuses for heuristic joins, which are labelled rather than
// silently promoted to exact. The period rides along as a label so a
// consumer that wants the extrapolation computes it deliberately.
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

// sampledStack returns the launch's captured CPU stack, and whether there is
// one. It is the single definition of "this execution's time is attributable
// to a call path": a launch that joined but carried no stack (not sampled,
// or a capture/symbolization failure the consumer counted) is exactly as
// unattributed as an execution that joined no launch at all, and both must
// project the same way. Deciding this from SamplePeriod instead would claim
// attribution for a sampled launch whose capture failed.
//
// "Carried no stack" also covers a capture the consumer refused
// to attach because it never left the profiler's own injected module - a
// frame-pointer walk that died inside the vendor libraries and came back
// holding nothing but the adapter's own callback (gpuprobe.shimScope,
// counted as gpuprobe.Stats.StacksProfilerOnly). That judgement needs to
// know which module is the profiler's and which deployment shape it is in,
// which is vendor- and deployment-specific knowledge this package
// deliberately does not hold. It is made where that knowledge lives, and
// arrives here as what it is: a launch with no stack.
func sampledStack(view ExecutionView) ([]pp.Frame, bool) {
	if view.Launch == nil || len(view.Launch.Launch.CPUStack) == 0 {
		return nil, false
	}
	return view.Launch.Launch.CPUStack, true
}

// projectionFrames builds the frame slice: the launch's real CPU stack when
// one was sampled, the boundary marker, then the kernel frame. Nothing else
// - not the PC, not the stall reason, not the queue/device/correlation -
// belongs here; those go through projectionLabels instead.
//
// An execution with no stack gets [gpu:launch unsampled] and nothing above
// it. It must never be given a stack from anywhere else: the sampled
// population's call paths belong to the specific launches that were
// sampled, and lending one to a sibling would put measured GPU time under a
// call path that provably did not produce it.
func projectionFrames(view ExecutionView) []pp.Frame {
	var frames []pp.Frame
	if stack, ok := sampledStack(view); ok {
		frames = append(frames, stack...)
		frames = append(frames, pp.FrameFromName(FrameLaunch))
	} else {
		frames = append(frames, pp.FrameFromName(FrameLaunchUnsampled))
	}
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

// labelPID resolves the process to name in the gpu_pid label.
//
// It is NOT always projectionPID. projectionPID answers "whose address space
// were these frames symbolized in", and an unmatched execution honestly has
// no answer: it carries no launch, so no stack, so no address space. gpu_pid
// answers a different question - "which process produced this GPU work" - and
// an unmatched execution usually can answer it, because since issue #36 the
// producing process rides on the execution's own CorrelationID. An execution
// whose correlation merely MISSED the cache (its launch aged out) is exactly
// that case, and it is the population issue #53 cares most about: it keeps
// gpu_correlation, so without this fallback it would keep the ambiguity too.
//
// The launch wins when there is one, so gpu_pid never contradicts the pid the
// frames were resolved against. For an exact join the two are equal by
// construction - (backend, pid, value) is the join key. For a heuristic join
// the execution supplied no correlation at all (that is the only way it
// reaches the heuristic), so the launch's pid is the only pid in play; it is
// an inference, and gpu_join="heuristic" already says so on the same sample.
//
// Zero means no process is known - a correlation-less execution that matched
// nothing, or a device-global producer that names no process (see
// CorrelationID.PID). The label is then omitted rather than emitted as
// "gpu_pid=0", which would name pid 0 - a real pid, the kernel's - as the
// producer. Absence here is readable, because gpu_join on the same sample
// says why.
func labelPID(view ExecutionView) uint32 {
	if pid := projectionPID(view); pid != 0 {
		return pid
	}
	return view.Exec.Correlation.PID
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
// gpu_pid names the process that produced the GPU work (issue #53). Before
// it, a system-wide profile carried no gpu_* label naming a process at all:
// the pid was load-bearing internally after issue #36 - it is what keeps one
// process's GPU time off another's call path - but invisible in the output.
// See labelPID for which pid it reports and when it is omitted.
//
// It is a label, not a frame, per spec §8: frames are exhaustively the real
// CPU stack, the [gpu:launch] boundary and the kernel name, and every piece
// of context ABOUT a sample - queue, device, correlation, cgroup, pod UID,
// container ID - is a label. Process identity is the same kind of thing as
// the cgroup/pod/container identity already listed there, and a per-process
// frame would fragment stack identity for a fact no flame graph nests on.
//
// It is emitted unconditionally, including in single-process mode
// (Config.PID != 0) where every sample carries the same value. This package
// has no access to the agent's mode and should not grow one for this; more to
// the point, a consumer reading a profile cannot tell "single-process, so the
// label was skipped" from "GPU labels absent entirely", so a label that
// disappears in one mode is harder to consume than one that is always there.
// Its cardinality is one string-table value per distinct process (one, in
// that mode) plus one for the key - bounded by the process count, not by the
// execution or PC-sample count.
//
// gpu_correlation keeps its "backend:value" format and does NOT gain the pid.
// The ambiguity issue #53 describes is real - two processes can emit the same
// gpu_correlation string - but qualifying it in place is a compatibility
// break with a silent failure mode: an existing
// `pprof -tagfocus gpu_correlation=cupti:4294967301` would stop matching
// anything and render an empty profile, which reads as "no such GPU work"
// rather than "your filter is stale". Adding gpu_pid alongside instead leaves
// every existing filter matching exactly what it matched before, and makes
// the over-grouping visible and resolvable in the output (`-tags` shows the
// process breakdown; `-tagignore gpu_pid=<other>` narrows it) rather than
// silently invisible. It also keeps one fact per label - the shape pod_uid
// and container_id already use - and keeps this label what its comment below
// says it is: a report of what the vendor actually observed on this
// execution, not a composite this package assembled. A third label carrying
// the composite was considered and rejected on cardinality: gpu_correlation
// is already the highest-cardinality label here (roughly one distinct string
// per execution), and a parallel fully-qualified copy would double that,
// where gpu_pid adds one string per process.
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
// honestly (CorrelationID.Present(), exactly as for an exact join) rather than
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
	if pid := labelPID(view); pid != 0 {
		labels["gpu_pid"] = strconv.FormatUint(uint64(pid), 10)
	}
	if view.Exec.Queue.QueueID != "" {
		labels["gpu_queue"] = view.Exec.Queue.QueueID
	}
	if view.Exec.Queue.Device.DeviceID != "" {
		labels["gpu_device"] = view.Exec.Queue.Device.DeviceID
	}
	if view.Exec.Correlation.Present() {
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
	// gpu_sample_period rides only on the population that actually carries a
	// sampled stack, and only when the producer declared a period. It is the
	// denominator a consumer needs to extrapolate "sampled GPU time" to "all
	// GPU time attributable to this call path" - an estimate this package
	// refuses to compute on the reader's behalf, because the value beside it
	// is a measured duration and silently mixing the two would make the
	// estimate indistinguishable from the measurement.
	if _, ok := sampledStack(view); ok && view.Launch.Launch.SamplePeriod > 0 {
		labels["gpu_sample_period"] = strconv.FormatUint(uint64(view.Launch.Launch.SamplePeriod), 10)
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
