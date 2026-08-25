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
	anomalies := joinAnomalies(snap)
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
	}

	// Its own clause rather than folded into the one above, for the same
	// reason it has its own counter: these samples carried no correlation, so
	// they are pending on a different index under different bounds, and a
	// reader who sees one number cannot tell which. Continuous-mode
	// collection is optional and off by default, so the clause is absent
	// entirely when nothing produced such samples.
	if snap.PendingModuleSamples > 0 {
		fmt.Fprintf(&b, "; %d correlation-less pc samples pending over %d %s",
			snap.PendingModuleSamples, snap.PendingModuleGroups,
			plural(uint64(snap.PendingModuleGroups), "kernel group", "kernel groups"))
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
func joinAnomalies(snap Snapshot) []string {
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
