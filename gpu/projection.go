package gpu

import (
	"fmt"
	"maps"
	"math/bits"
	"path"
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
//
// # The PC-sample label set
//
// Every sample projected from a PC sample carries, on top of the execution's
// shared labels:
//
//	gpu_stall       the instruction's stall reason, when the producer named one
//	gpu_pc          the instruction's offset within its module, subject to the
//	                cardinality budget - see pcLabelBudget
//	gpu_pc_attrib   how the sample reached this execution - unconditional,
//	                and "graph-refused" where a CUDA graph made "exact" false
//	gpu_src_status  why the sample does or does not have a source location -
//	                unconditional
//	gpu_src_file    the source file's BASENAME, only under "resolved"
//	gpu_src_line    the source line, only under "resolved"
//	gpu_src_func    the device function, only under "resolved"
//
// The two unconditional ones are unconditional for gpu_join's reason: an
// absent label must never be readable as a positive answer by a consumer who
// does not know to check for its absence.
//
// One label the design lists is deliberately NOT here: gpu_serialized, whose
// three values come from the Tier A sampling windows that do not exist in this
// tree yet. A gpu_serialized="false" that means "we have no windows to check"
// is precisely the answer the design forbids, so until the windows arrive the
// label is absent rather than meaningless.
func ProjectExecutions(snap Snapshot) []pp.ProfileSample {
	samples, _ := ProjectExecutionsWith(snap, ProjectionConfig{})
	return samples
}

// ProjectExecutionsWith is ProjectExecutions with a source resolver and a
// cardinality budget, returning the projection's own counters beside the
// samples.
//
// The split mirrors CountingSink.Snapshot / SnapshotWith: the plain form stays
// the whole API for a caller with no module store and no interest in the
// counters, and this form is what a driver calls when it has either. The stats
// are RETURNED rather than accumulated on a field somewhere, because they
// describe one projection of one snapshot - a second call over the same
// snapshot suppresses the same labels again, and a running total of that would
// count the same loss twice.
//
// See ProjectionConfig for why a nil Modules is a supported, accounted-for
// state rather than a skipped one.
func ProjectExecutionsWith(snap Snapshot, cfg ProjectionConfig) ([]pp.ProfileSample, ProjectionStats) {
	// A nil Modules is answered by an EMPTY STORE, not by this function
	// deciding a status for itself. ModuleStore is the single place
	// gpu_src_status is decided (see SrcStatus), and an empty store answers
	// every Resolve with no-module - which is exactly the truth when no store
	// was configured: no usable module bytes exist for that CRC. Synthesizing
	// the same answer here would put a second decision site in the codebase
	// and would be the first place a fifth value could ever appear.
	modules := cfg.Modules
	if modules == nil {
		modules = NewModuleStore(ModuleStoreConfig{})
	}
	budget := newPCLabelBudget(cfg.MaxDistinctPCLabels)

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

		// One value per execution, read once: every PC sample on this view
		// reached it the same way, so resolving it per sample would be the
		// same answer computed len(PCSamples) times.
		attrib := projectionPCAttrib(view)

		weights := distributeExecutionWeight(executionWeight(view.Exec), view.PCSamples)
		for i, pcs := range view.PCSamples {
			// common is projectionLabels' return value, which always starts
			// from `make(map[string]string)` - never nil - so maps.Clone(common)
			// is never nil either; no separate nil-guard is needed here.
			//
			// Every label set from here down is set AFTER this clone, which is
			// itself taken after projectionLabels copied the producer-supplied
			// Tags. That ordering is the whole reserved-name defence: a launch
			// tagged "gpu_src_file" or "gpu_pc_attrib" is overwritten by the
			// value this package derived, never the other way round. See
			// projectionLabels' note, and TestProjectionReservedLabelsWinOverTags.
			labels := maps.Clone(common)
			if pcs.StallReason != "" {
				labels["gpu_stall"] = pcs.StallReason
			}
			if budget.admit(pcs.PCOffset) {
				labels["gpu_pc"] = fmt.Sprintf("%#x", pcs.PCOffset)
			}
			labels["gpu_pc_attrib"] = attrib
			setSourceLabels(labels, modules.Resolve(pcs.Module.CRC, pcs.FunctionIndex, pcs.PCOffset))

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
	return samples, budget.stats()
}

// ProjectionConfig configures ProjectExecutionsWith.
type ProjectionConfig struct {
	// Modules is the store the source labels are resolved against.
	//
	// Nil is supported and is ACCOUNTED FOR rather than skipped: with no
	// store, every PC sample carries gpu_src_status="no-module" and no
	// location, which is the same fact for the reader as a cubin that never
	// reached the agent (see SrcNoModule). The labels do not disappear,
	// because a profile whose source labels are absent is indistinguishable
	// from one taken before this phase existed, while a profile that says
	// "no-module" on every sample points straight at the missing store.
	// This is the same accounting Timeline's module join uses for a nil
	// store - see PCJoinStats.GroupsUnresolvedName.
	Modules *ModuleStore

	// MaxDistinctPCLabels caps how many DISTINCT gpu_pc values one
	// projection may emit. Zero (and anything negative) means
	// defaultMaxDistinctPCLabels. See pcLabelBudget for what happens past
	// the cap and why gpu_pc is the label that gives way.
	MaxDistinctPCLabels int
}

// ProjectionStats is what one ProjectExecutionsWith call did to the labels it
// was asked to emit. It is per-call, not cumulative - see ProjectExecutionsWith.
type ProjectionStats struct {
	// DistinctPCLabels is how many distinct gpu_pc values this projection
	// emitted, and PCLabelCap the ceiling it was allowed. Both are reported
	// so that "we were nowhere near the cap" and "we sat exactly on it" are
	// distinguishable without recomputing anything from the profile.
	DistinctPCLabels uint64 `json:"distinct_pc_labels,omitempty"`
	PCLabelCap       uint64 `json:"pc_label_cap,omitempty"`

	// PCLabelsSuppressed is the design's ProjectionPCLabelsSuppressed: PC
	// samples that were projected WITHOUT a gpu_pc label because admitting
	// their offset would have pushed the profile past PCLabelCap distinct
	// values. Every one of those samples still carries its gpu_stall,
	// gpu_pc_attrib and gpu_src_* labels and still carries its full share of
	// the execution's duration - only the instruction offset is missing.
	//
	// Zero is the ordinary reading. Non-zero is surfaced by JoinHealthWith,
	// because a profile that silently lost its PC labels looks exactly like
	// one that never had any.
	PCLabelsSuppressed uint64 `json:"projection_pc_labels_suppressed,omitempty"`
}

// defaultMaxDistinctPCLabels is the ceiling on distinct gpu_pc values in one
// projection.
//
// It is REASONED, NOT MEASURED, and the design says so: the real distinct-PC
// count on a genuine profile is one of the things deferred to hardware. The
// number comes from the design's own pathological estimate - 20,000 distinct
// PCs costing roughly 400 KB of string table before gzip and ~140 KB after,
// which it calls tolerable. Setting the ceiling at the top of the range the
// design already accepted means it cannot fire on any workload that design
// considered reasonable, and fires only past it. Once the count is measured on
// real cubins this becomes a number rather than a bound on an estimate.
//
// gpu_pc saturates - once every hot instruction has been sampled at least
// once, a longer run adds no new values - so this bounds the profile's string
// table, not its length.
const defaultMaxDistinctPCLabels = 20_000

// pcLabelBudget bounds the distinct gpu_pc values one projection emits.
//
// # Why gpu_pc is the label that gives way
//
// gpu_pc is the most numerous label in the set (one value per distinct sampled
// instruction) and the least actionable on its own: a bare instruction offset
// tells a reader nothing that gpu_stall (what the instruction was waiting for)
// and gpu_src_file/_line/_func (where it is in their source) do not tell them
// better. So under cardinality pressure the numerous label is dropped and the
// useful ones are kept, and the drop is counted rather than silent.
//
// # Why an already-seen offset is always admitted
//
// The cap exists to bound the pprof STRING TABLE, which stores one entry per
// distinct label value. A repeat of an offset already emitted costs nothing
// there, so refusing it would suppress information that has already been paid
// for while leaving the bound exactly where it was. The rule is therefore
// "admit no NEW value past the cap", not "emit nothing past the cap": distinct
// values are bounded at exactly the ceiling either way, and the second reading
// would throw away strictly more of the profile for no saving. Suppression is
// counted per SAMPLE, not per distinct offset, because the sample is the thing
// that went out incomplete.
type pcLabelBudget struct {
	seen       map[uint64]struct{}
	ceiling    int
	suppressed uint64
}

func newPCLabelBudget(max int) *pcLabelBudget {
	if max <= 0 {
		max = defaultMaxDistinctPCLabels
	}
	return &pcLabelBudget{seen: make(map[uint64]struct{}), ceiling: max}
}

// admit reports whether this PC sample may carry a gpu_pc label, counting the
// refusal when it may not. %#x is injective over uint64, so the set of
// admitted offsets is exactly the set of distinct label values.
func (b *pcLabelBudget) admit(pcOffset uint64) bool {
	if _, ok := b.seen[pcOffset]; ok {
		return true
	}
	if len(b.seen) >= b.ceiling {
		b.suppressed++
		return false
	}
	b.seen[pcOffset] = struct{}{}
	return true
}

func (b *pcLabelBudget) stats() ProjectionStats {
	return ProjectionStats{
		DistinctPCLabels:   uint64(len(b.seen)),
		PCLabelCap:         uint64(b.ceiling), //nolint:gosec // newPCLabelBudget forces a positive ceiling.
		PCLabelsSuppressed: b.suppressed,
	}
}

// projectionPCAttrib renders gpu_pc_attrib for an execution that carries PC
// samples: HOW those samples reached this execution, and therefore how far the
// attribution can be trusted. One of PCAttribs() - exact, kernel,
// kernel-ambiguous or kernel-multidevice - decided entirely by the join (see
// PCAttrib) and only rendered here.
//
// It is emitted UNCONDITIONALLY on every PC-derived sample, for gpu_join's
// reason: an absent label must never be readable as "exact" by a consumer that
// does not know to check for its absence. Absence is the answer this label can
// least afford, because the value it would be mistaken for is the only one of
// the four that is not an inference.
//
// A view carrying PC samples and no attribution is a bug in the join - the
// conformance suite's assertPCAttribAccompaniesSamples exists to catch it -
// and it renders as a value no consumer can read as one of the four, in the
// same shape and for the same reason as SrcStatus.String's "unset-src-status".
// It is not a fifth value of the label's domain; it is what a join bug looks
// like from the outside. Omitting the label instead would hide the bug behind
// the one reading that must never be reachable by accident.
func projectionPCAttrib(view ExecutionView) string {
	if pcAttribRank(view.PCAttrib) == 0 {
		return "unset-pc-attrib"
	}
	return string(view.PCAttrib)
}

// pcSampleReservedLabels is every label name ProjectExecutionsWith derives per
// PC sample. It is the list projectionLabels clears from the producer-supplied
// Tags; a name added to the projection and forgotten here is a name a producer
// can forge whenever the projection has no value for it.
var pcSampleReservedLabels = []string{
	"gpu_stall",
	"gpu_pc",
	"gpu_pc_attrib",
	"gpu_src_status",
	"gpu_src_file",
	"gpu_src_line",
	"gpu_src_func",
}

// setSourceLabels writes gpu_src_status and, only under a resolved status, the
// source location.
//
// gpu_src_status is unconditional. An ABSENT source label reads as "not
// sampled"; an explicit status reads as "sampled, and here is why there is no
// location", and those are different facts needing different actions from the
// reader - recompile with -lineinfo, ship the cubin to the agent, or accept
// that the compiler emitted no line for this instruction. The four values are
// decided by ModuleStore and nowhere else; this function only spells them.
//
// The location is taken through Resolution.Source, whose ok comes back in the
// same expression as the data, so a location can only be emitted under
// SrcResolved. There is no branch here that could pair a file with
// "no-lineinfo".
func setSourceLabels(labels map[string]string, res Resolution) {
	labels["gpu_src_status"] = res.Status().String()

	fn, file, line, ok := res.Source()
	if !ok {
		return
	}
	// An empty string is not a value: a label present and blank reads as "the
	// name is blank", while an absent one under gpu_src_status="resolved"
	// reads as "the line table had no name for this", which is what happened.
	// Neither is expected - the store's resolved path always has both - so
	// this is a guard, not a case.
	if base := srcFileBase(file); base != "" {
		labels["gpu_src_file"] = base
	}
	if fn != "" {
		labels["gpu_src_func"] = fn
	}
	labels["gpu_src_line"] = strconv.FormatUint(uint64(line), 10)
}

// srcFileBase reduces a line table's file name to its BASENAME. The directory
// goes nowhere at all.
//
// A cubin's DWARF file names are build-host absolute paths (the fixtures in
// internal/cubin/testdata carry /tmp/perf-agent-cubin-fixtures/single.cu).
// Three reasons not to carry them: they vary per build, so the same kernel
// built twice produces two distinct label values where the reader sees one
// file; they cost a long string in the pprof string table for information no
// reader acts on; and they leak the build environment's layout into a profile
// that may be shared outside the organisation that built it. The basename
// beside gpu_src_func is enough to find the line in the repository it came
// from, which is the only thing a reader does with it.
//
// path.Base, not filepath.Base: the separator in a cubin's line table is the
// build host's, and this must not depend on the agent's own OS. The agent is
// Linux-only and nvcc emits '/' there.
func srcFileBase(file string) string {
	if file == "" {
		return ""
	}
	base := path.Base(file)
	if base == "." || base == "/" {
		return ""
	}
	return base
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
//
// The same rule covers every label ProjectExecutionsWith layers on top of a
// clone of this map - gpu_stall, gpu_pc, gpu_pc_attrib and the gpu_src_*
// family - because the clone is taken after this function returns and they are
// written after the clone. A tag literally named "gpu_src_file" therefore
// loses to the file the module store resolved, which is the only ordering
// under which a producer-controlled string cannot claim to be a source
// location this package derived.
func projectionLabels(view ExecutionView) map[string]string {
	labels := make(map[string]string)
	if view.Launch != nil {
		maps.Copy(labels, view.Launch.Launch.Tags)
	}
	// Every per-PC-sample reserved name is cleared here, immediately after the
	// Tags copy and before anything is derived.
	//
	// Overwriting is not enough on its own, because those labels are
	// CONDITIONAL: gpu_src_file is emitted only under a resolved status,
	// gpu_stall only when the producer named a reason, gpu_pc only inside the
	// cardinality budget, and none of them at all on an execution that carries
	// no PC samples. A tag named "gpu_src_file" would therefore survive
	// untouched in exactly the cases where this package has no value of its
	// own - a forged source location standing beside
	// gpu_src_status="no-module", which is the strongest possible form of the
	// lie the reserved-name discipline exists to prevent. Reserved names win
	// by absence too.
	for _, k := range pcSampleReservedLabels {
		delete(labels, k)
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
	// gpu_graph_refused is the CUDA-graph refusal, and it rides on EVERY
	// execution of an affected process rather than only on the PC-bearing
	// ones — the same scope gpu_join has, because it says the same kind of
	// thing about the same join. One graph launch fires one runtime callback
	// for N kernels, so gpu_join="exact" on those executions is exact-looking
	// and many-to-one: N kernels' time and N kernels' samples are billed to
	// one CPU call site. gpu_pc_attrib="graph-refused" says it for the sampled
	// subset; this says it for all of them.
	//
	// It is deleted before it is set, which the other conditional labels here
	// are not, because it is the only one whose ABSENCE is what a reader acts
	// on: a producer-supplied tag literally named "gpu_graph_refused" would
	// otherwise survive on an execution this package did not refuse, and a
	// forged refusal is a way to make a healthy Tier A profile look
	// untrustworthy. It is set only when true because "no graph was reported"
	// is the ordinary state of every profile; the loud, counted, unconditional
	// form of this fact is the joinhealth anomaly and
	// Snapshot.ExecutionsGraphRefused, not a label on every sample ever
	// projected.
	//
	// It is never set in Tier B or with sampling off, however many graph
	// executions were reported — see ExecutionView.GraphRefused.
	delete(labels, "gpu_graph_refused")
	if view.GraphRefused {
		labels["gpu_graph_refused"] = "true"
	}
	// gpu_serialized is set UNCONDITIONALLY on every execution, exactly as
	// gpu_join is and for exactly the same reason: an absent label would read
	// as "not perturbed" to a consumer that does not know to check for its
	// absence, and "not perturbed" is the one answer that must never be
	// reachable by accident.
	//
	// It rides on every execution rather than only on PC-bearing ones because
	// serialization is a property of the INTERVAL, not of whether a sample
	// landed: every kernel that ran inside a burst ran serialized, sampled or
	// not (see gpu/serialization.go).
	//
	// The value comes from SerializationState.String(), whose zero value is
	// "unknown" — so a view that never reached the classifier degrades to
	// "unknown" here rather than to "false".
	//
	// LIMITATION, stated rather than left to be discovered: this is the GPU
	// projection. On-CPU and off-CPU samples taken during a burst are
	// distorted too — serialization inflates precisely the synchronization
	// wait that off-CPU profiling exists to measure — and they carry no
	// marking at all, because those profilers know nothing about GPUs.
	// joinhealth reports it whenever any window was recorded.
	labels["gpu_serialized"] = view.Serialized.String()
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
