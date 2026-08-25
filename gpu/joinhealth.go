package gpu

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	joinHealthPrefix  = "gpu join"
	joinAnomalyPrefix = "gpu join ANOMALY"
)

// plural renders "1 launch" / "2 launches". These lines are read by a person
// under time pressure, and "1 launches" reads like a formatting bug in a
// line whose entire job is to be believed.
func plural(n uint64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatUint(n, 10) + " " + many
}

// JoinHealth renders a Snapshot's join and loss counters as operator-facing
// lines: element 0 is always a one-line summary, and every element after it
// is one anomaly that the summary's trailing count agrees with. Callers log
// them one per line (see cmd/gpu-stub-profile, cmd/gpu-cuda-profile).
//
// The shape is deliberate. Printing the whole counter set on every run - the
// obvious `%+v` - is what makes a rising UnmatchedExecutionCount invisible:
// a line that is a wall of zeros on every healthy run trains the reader to
// skip it, and then it is skipped on the one run that mattered. So the
// healthy case collapses to a single short sentence with no zero-valued
// fields in it at all, and anything anomalous is spelled out on its own
// line, each with a few words of what the number means when it is bad. The
// anomalous case is *longer* than the healthy case, which is the property
// that makes it noticed.
//
// The summary's trailing "no anomalies" / "N anomalies" is the only derived
// figure here, and it is derived so that it cannot read green when things
// are worst: it is a count of raised conditions, so it reads 0 only when
// every condition is unraised, and it grows monotonically as more go wrong.
// The degenerate worst case - nothing arrived at all - is itself one of the
// conditions ("no executions in this snapshot"), so an empty snapshot
// reports an anomaly rather than the vacuous "all exact" that a ratio-based
// health figure would print for 0/0.
//
// Scope. This covers what Snapshot knows: the join, the launch cache, the
// timeline's own bounded stores, and (when the caller populated it via
// CountingSink.SnapshotWith) admission. It cannot see loss upstream of the
// sink - a full ringbuf, a batch the BPF program could not deliver - which
// is gpuprobe.Stats's business and is logged separately by both drivers.
// "no anomalies" therefore means the join is clean, not that nothing was
// lost on the way to it.
//
// Time scales differ between the fields quoted here and are labelled as
// such: JoinStats is per-snapshot (Timeline resets its launch counter on
// every Snapshot call), while LaunchCacheStats and TimelineDropStats are
// cumulative over the Timeline's life. Both drivers snapshot exactly once,
// at the end, so the two coincide there; a caller that snapshots
// periodically should read the eviction counters as running totals.
func JoinHealth(snap Snapshot) []string {
	return JoinHealthWith(snap, ProjectionStats{})
}

// JoinHealthWith is JoinHealth plus what the projection itself dropped.
//
// The split exists because ProjectionStats is not in the Snapshot and cannot
// be: it is produced by ProjectExecutionsWith, which runs after the snapshot
// is taken. It is the same shape as SinkStats, which Timeline also cannot see
// and which a caller supplies through CountingSink.SnapshotWith.
//
// The zero value is the "nothing was suppressed" reading, which is also what
// JoinHealth passes, so a caller that does not project through
// ProjectExecutionsWith reports no suppression - correctly, since without that
// call nothing suppressed anything.
func JoinHealthWith(snap Snapshot, proj ProjectionStats) []string {
	anomalies := joinAnomalies(snap, proj)
	return append([]string{joinSummary(snap, len(anomalies))}, anomalies...)
}

// joinSummary is the always-printed line. len(snap.Executions), not a
// JoinStats field, is the execution total: the three join outcomes are
// mutually exclusive and sum to it, so quoting the slice length keeps the
// parenthesised breakdown checkable against a figure that does not come
// from the same counters it is auditing. joinAnomalies performs that check
// rather than leaving it to the reader's arithmetic.
func joinSummary(snap Snapshot, anomalies int) string {
	js := snap.JoinStats
	execs := uint64(len(snap.Executions))

	var b strings.Builder
	b.WriteString(joinHealthPrefix + ": ")
	switch {
	case execs == 0:
		b.WriteString("no executions")
	case js.ExactExecutionJoinCount == execs:
		fmt.Fprintf(&b, "%s, all exact", plural(execs, "execution", "executions"))
	default:
		fmt.Fprintf(&b, "%s (%d exact, %d heuristic, %d unmatched)",
			plural(execs, "execution", "executions"), js.ExactExecutionJoinCount,
			js.HeuristicExecutionJoinCount, js.UnmatchedExecutionCount)
	}

	switch {
	case js.LaunchCount == 0:
		b.WriteString("; no launches")
	case js.UnmatchedLaunchCount == 0:
		fmt.Fprintf(&b, "; %s, all matched", plural(js.LaunchCount, "launch", "launches"))
	default:
		fmt.Fprintf(&b, "; %s (%d matched, %d unmatched)",
			plural(js.LaunchCount, "launch", "launches"),
			js.MatchedLaunchCount, js.UnmatchedLaunchCount)
	}

	fmt.Fprintf(&b, "; cache %d live", snap.LaunchCache.Live)

	// PC samples are optional on this pipeline (CUPTI's continuous mode
	// emits them, the stub does not), so the clause appears only when there
	// were any - a permanent "0 attributed, 0 pending" would be exactly the
	// zero-valued noise this format exists to avoid.
	if snap.AttributedPCSamples > 0 || snap.PendingSamples > 0 {
		fmt.Fprintf(&b, "; pc samples %d attributed, %d pending",
			snap.AttributedPCSamples, snap.PendingSamples)
		// The split by index is printed only when both are actually in play.
		// A pure Tier A run (every sample exact) and a pure Tier B run (every
		// sample through the module) each need one number, not two, and
		// printing "0 by kernel" on every CUPTI run is the zero-valued noise
		// this format exists to avoid.
		if snap.PCJoin.AttributedExact > 0 && snap.PCJoin.AttributedKernel > 0 {
			fmt.Fprintf(&b, " (%d exact, %d by kernel)",
				snap.PCJoin.AttributedExact, snap.PCJoin.AttributedKernel)
		} else if snap.PCJoin.AttributedKernel > 0 {
			b.WriteString(" by kernel")
		}
	}

	// Its own clause rather than folded into the one above, for the same
	// reason it has its own counter: these samples carried no correlation, so
	// they are pending on a different index under different bounds, and a
	// reader who sees one number cannot tell which. Continuous-mode
	// collection is optional and off by default, so the clause is absent
	// entirely when nothing produced such samples.
	if snap.PendingModuleSamples > 0 {
		fmt.Fprintf(&b, "; %s pending over %s",
			plural(uint64(snap.PendingModuleSamples), "correlation-less pc sample", "correlation-less pc samples"),
			plural(uint64(snap.PendingModuleGroups), "kernel group", "kernel groups"))
	}

	// The serialization disclosure, and only when there is one to make. With
	// PC sampling off or in continuous collection every execution is "false"
	// and nothing was ever serialized, so a permanent "0 serialized" clause
	// would be exactly the zero-valued noise this format avoids.
	if snap.ExecutionsSerialized > 0 || snap.ExecutionsSerializationUnknown > 0 ||
		snap.SamplingWindowsReceived > 0 {
		fmt.Fprintf(&b, "; serialization %d true, %d false, %d unknown over %s",
			snap.ExecutionsSerialized, snap.ExecutionsNotSerialized,
			snap.ExecutionsSerializationUnknown,
			plural(uint64(snap.SamplingWindowsHeld), "burst", "bursts"))
	}

	switch anomalies {
	case 0:
		b.WriteString("; no anomalies")
	case 1:
		b.WriteString("; 1 anomaly")
	default:
		fmt.Fprintf(&b, "; %d anomalies", anomalies)
	}
	return b.String()
}

// joinAnomalies returns one line per raised condition, in severity order:
// nothing arrived, then the counters disagreeing with the executions in
// hand, then attribution the profile got wrong or guessed, then the
// eviction paths that caused it, then admission loss underneath.
//
// UnmatchedLaunchCount is deliberately not a condition. A launch whose
// execution has not landed yet is the normal state at any snapshot boundary
// - it is in the summary line, where it is readable, but raising it would
// fire on every healthy periodic snapshot and devalue the word "anomaly"
// for the counters that do mean something is wrong.
func joinAnomalies(snap Snapshot, proj ProjectionStats) []string {
	js, lc, dr := snap.JoinStats, snap.LaunchCache, snap.Dropped
	execs := uint64(len(snap.Executions))

	var out []string
	add := func(format string, args ...any) {
		out = append(out, joinAnomalyPrefix+": "+fmt.Sprintf(format, args...))
	}

	if execs == 0 {
		add("no executions in this snapshot — nothing was attributed at all; " +
			"check the probe attachment and the sink before reading anything below")
	}
	// The three join outcomes are mutually exclusive and every execution
	// takes exactly one of them, so they must sum to the number of
	// executions actually in the snapshot. joinSummary deliberately takes
	// its denominator from len(snap.Executions) rather than from these
	// counters precisely so the two can be compared - but a breakdown that
	// silently fails to add up is not something a reader spots by eye, and
	// unchecked it would make every figure below look authoritative while
	// being wrong. Raised first, because it is the condition that decides
	// whether the rest of the line can be believed at all.
	if outcomes := js.ExactExecutionJoinCount + js.HeuristicExecutionJoinCount +
		js.UnmatchedExecutionCount; outcomes != execs {
		add("join outcomes sum to %d but the snapshot holds %d executions — the join counters "+
			"disagree with what is actually present; treat every figure below as unreliable",
			outcomes, execs)
	}
	// The serialization disclosure's own sum identity, and it is raised in the
	// same place and for the same reason as the join one above: three
	// mutually-exclusive outcomes, every execution takes exactly one, so they
	// must add up to what is actually in the snapshot. A shortfall means an
	// execution reached the profile with no disclosure at all.
	if serialization := snap.ExecutionsSerialized + snap.ExecutionsNotSerialized +
		snap.ExecutionsSerializationUnknown; serialization != execs {
		add("serialization outcomes sum to %d but the snapshot holds %d executions — some "+
			"execution carries no gpu_serialized disclosure at all; treat every duration "+
			"in this profile as unqualified",
			serialization, execs)
	}
	// The whole point of Tier A's disclosure. Raised BEFORE the join
	// anomalies below because it qualifies the durations themselves rather
	// than what they were attributed to: a perturbed measurement joined
	// perfectly is still a perturbed measurement.
	if snap.ExecutionsSerialized > 0 {
		add("%d of %d executions ran while GPU kernels were SERIALIZED by the profiler — "+
			"their durations are inflated by the measurement and are marked "+
			"gpu_serialized=\"true\". CPU and off-CPU samples taken during those bursts are "+
			"distorted too and carry no marking at all",
			snap.ExecutionsSerialized, execs)
	}
	if snap.ExecutionsSerializationUnknown > 0 && snap.SamplingWindowsReceived > 0 {
		add("%d of %d executions cannot be said to have run unperturbed — no sampling window "+
			"covers them (a dropped batch, a late attach, a sequence gap, or a burst that "+
			"never closed). They are marked gpu_serialized=\"unknown\" and MUST NOT be read "+
			"as \"false\"",
			snap.ExecutionsSerializationUnknown, execs)
	}
	if snap.SamplingWindowsOpen > 0 {
		add("%s still open — the producer stopped reporting mid-burst (a hard exit), so the "+
			"end of the burst is unknown and every execution from its start onward is "+
			"gpu_serialized=\"unknown\"",
			plural(uint64(snap.SamplingWindowsOpen), "sampling window", "sampling windows"))
	}
	if dr.EvictedSamplingWindows > 0 {
		add("%s evicted from the serialization disclosure store — executions that far back "+
			"degrade from \"false\" to \"unknown\"; raise "+
			"TimelineConfig.MaxSamplingWindowsPerPID or snapshot more often",
			plural(dr.EvictedSamplingWindows, "sampling window", "sampling windows"))
	}
	if js.UnmatchedExecutionCount > 0 {
		add("%d of %d executions unmatched — GPU time arrived with no launch to attach it to; "+
			"it is in the profile under %s carrying no CPU stack",
			js.UnmatchedExecutionCount, execs, FrameLaunchUnsampled)
	}
	if js.HeuristicExecutionJoinCount > 0 {
		add("%d of %d executions joined heuristically — matched on queue, kernel name and timing "+
			"rather than vendor correlation; the CPU stack may be another launch's from the same process",
			js.HeuristicExecutionJoinCount, execs)
	}
	// Issue #52's two witnesses. The first fires whenever the heuristic path
	// is entered at all, which on every backend shipping today means a
	// producer broke spec §6's "every launch and execution carries a
	// correlation" — the heuristic is meant to be dead code, and a dead path
	// that reports nothing when it wakes up is indistinguishable from one
	// nobody exercised. The second fires when the process guard actually
	// stopped a join, i.e. counts the cross-container attributions that would
	// have happened under the old, process-blind rule.
	if js.CorrelationlessExecutionCount > 0 {
		add("%d of %d executions arrived with no vendor correlation — spec §6 requires one on "+
			"every execution, so a producer is violating its own contract; these can only be "+
			"guessed at, never joined exactly",
			js.CorrelationlessExecutionCount, execs)
	}
	if js.CrossProcessHeuristicBlockedCount > 0 {
		add("%s refused because the candidate launch could not be shown to come from the same "+
			"process — those executions are unattributed rather than billed to another process's "+
			"call stack and container; have the producer set CorrelationID.PID",
			plural(js.CrossProcessHeuristicBlockedCount, "heuristic join", "heuristic joins"))
	}
	if js.AmbiguousHeuristicMatchCount > 0 {
		add("%s flagged ambiguous — more than one launch qualified and one was chosen; "+
			"these are the least trustworthy stacks in the profile",
			plural(js.AmbiguousHeuristicMatchCount, "heuristic join", "heuristic joins"))
	}
	if js.OutOfWindowDropCount > 0 {
		add("%d of the unmatched executions had a candidate launch just outside "+
			"LaunchEventJoinWindowNs — widen that window or snapshot more often",
			js.OutOfWindowDropCount)
	}
	if lc.EvictedCapacity > 0 {
		add("launch cache evicted %s at capacity (%d live) — too small for the launch rate, "+
			"so their executions cannot join; raise TimelineConfig.LaunchCache.Capacity",
			plural(lc.EvictedCapacity, "launch", "launches"), lc.Live)
	}
	if lc.EvictedHorizon > 0 {
		add("launch cache evicted %s past HorizonNs — they aged out before their execution "+
			"arrived; raise the horizon if kernels sit queued that long",
			plural(lc.EvictedHorizon, "launch", "launches"))
	}
	if lc.Replaced > 0 {
		add("launch cache replaced %s — the same correlation ID came back while still "+
			"live; the earlier launch and its stack are gone",
			plural(lc.Replaced, "live entry", "live entries"))
	}
	if lc.AnomalousTimestamp > 0 {
		add("%s carried an out-of-range timestamp — the producer's clock is suspect and "+
			"horizon eviction is running against a clamped anchor",
			plural(lc.AnomalousTimestamp, "launch", "launches"))
	}
	// PC-attribution quality, kept strictly apart from the heuristic-launch
	// clauses above. An execution can appear in both sets - joined to its
	// launch by vendor correlation and still carrying inferred PC samples -
	// and the two lines say different things about different joins.
	if pj := snap.PCJoin; pj.AmbiguousAttributions > 0 {
		add("%s carrying PC samples attributed by kernel with more than one execution of that "+
			"kernel in the snapshot — the stall detail is on the right kernel but not provably on "+
			"the right invocation; these samples are marked %s, which is NOT the same flag as an "+
			"ambiguous heuristic launch join",
			plural(pj.AmbiguousAttributions, "execution", "executions"), PCAttribKernelAmbiguous)
	}
	if pj := snap.PCJoin; pj.MultiDeviceProcesses > 0 {
		add("%s ran kernels on more than one device — PC samples carry no device id and one binary "+
			"has one cubin CRC on both, so their samples cannot be told apart; %s are marked %s. "+
			"PC sampling is single-GPU in this phase",
			plural(pj.MultiDeviceProcesses, "process", "processes"),
			plural(pj.MultiDeviceAttributions, "execution", "executions"),
			PCAttribKernelMultiDevice)
	}
	if pj := snap.PCJoin; pj.DeviceTrackingCapped > 0 {
		add("%s could not be admitted to the multi-device guard (it is full) and are being treated "+
			"as single-device — a second device in one of them would go unmarked",
			plural(pj.DeviceTrackingCapped, "process", "processes"))
	}
	if pj := snap.PCJoin; pj.GroupsUnresolvedName > 0 {
		add("%s could not be resolved to a device function — no cubin for that CRC reached the "+
			"agent, it was evicted, or the function index is not in its symbol table; those samples "+
			"stay pending and will age out unattributed rather than be attached to a nearby kernel",
			plural(pj.GroupsUnresolvedName, "correlation-less PC group", "correlation-less PC groups"))
	}
	if pj := snap.PCJoin; pj.GroupsNoProcess > 0 {
		add("%s arrived naming no process — every process that names none shares one key, so they "+
			"cannot be joined to any execution without risking another process's kernel; have the "+
			"producer set CorrelationID.PID",
			plural(pj.GroupsNoProcess, "correlation-less PC group", "correlation-less PC groups"))
	}
	if dr.EvictedPendingSamples > 0 {
		add("%s evicted while waiting for their execution — never attributed, so the "+
			"stall detail on those kernels is under-reported",
			plural(dr.EvictedPendingSamples, "PC sample", "PC samples"))
	}
	if dr.EvictedPendingModuleSamples > 0 {
		add("%s evicted while waiting for their kernel — these carried no correlation, so "+
			"they were grouped by module and function; the group index is too small, its "+
			"horizon too short, or their executions never arrived at all",
			plural(dr.EvictedPendingModuleSamples, "correlation-less PC sample", "correlation-less PC samples"))
	}
	if dr.EvictedExecutions > 0 {
		add("%s evicted from the timeline ring before this snapshot — that GPU time is "+
			"missing from the profile entirely; snapshot more often or raise the capacity",
			plural(dr.EvictedExecutions, "execution", "executions"))
	}
	if dr.EvictedEvents > 0 {
		add("%s evicted before this snapshot — raise TimelineConfig.EventCapacity",
			plural(dr.EvictedEvents, "timeline event", "timeline events"))
	}
	if dr.EvictedModules > 0 {
		add("%s evicted before this snapshot — kernels from evicted modules resolve to bare addresses",
			plural(dr.EvictedModules, "module", "modules"))
	}
	// Raised because the alternative is invisible. A profile whose gpu_pc
	// labels were suppressed looks exactly like a profile that never had any:
	// the samples are all there, carrying their weight, their stall reason and
	// their source line, and nothing in the pprof output says an instruction
	// offset was ever meant to be on them. This is the only place that fact is
	// reported at all, which is why it is an anomaly rather than a summary
	// clause - see ProjectionStats.PCLabelsSuppressed.
	if proj.PCLabelsSuppressed > 0 {
		add("%s projected without gpu_pc — this profile reached its ceiling of %d distinct "+
			"instruction offsets, so those samples keep their stall reason and source line but "+
			"carry no offset; the workload is loading far more distinct hot code than the "+
			"cardinality budget expects, or ProjectionConfig.MaxDistinctPCLabels is set too low",
			plural(proj.PCLabelsSuppressed, "PC sample", "PC samples"), proj.PCLabelCap)
	}

	for _, k := range []struct {
		one, many string
		stats     EventKindStats
	}{
		{"launch", "launches", snap.SinkStats.Launches},
		{"execution", "executions", snap.SinkStats.Execs},
		{"PC sample", "PC samples", snap.SinkStats.PCSamples},
		{"module", "modules", snap.SinkStats.Modules},
		{"timeline event", "timeline events", snap.SinkStats.Events},
	} {
		lost := k.stats.DroppedFull + k.stats.DroppedInvalid + k.stats.DroppedDownstream
		if lost == 0 {
			continue
		}
		add("sink dropped %s at admission (%d full, %d invalid, %d downstream) — they never "+
			"reached the timeline, so no join counter above accounts for them",
			plural(lost, k.one, k.many), k.stats.DroppedFull, k.stats.DroppedInvalid,
			k.stats.DroppedDownstream)
	}

	return out
}
