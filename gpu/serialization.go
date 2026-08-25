package gpu

import "sort"

// The serialization disclosure: which executions ran while GPU kernels were
// being serialized by the profiler, which provably did not, and which cannot
// be said either way.
//
// Why the window is the unit and the sampled kernel is not
// -------------------------------------------------------
// In CUPTI's kernel-serialized collection the device serializes kernels for as
// long as sampling is enabled. Every kernel that executed inside a burst
// therefore ran perturbed — SAMPLED OR NOT. Marking only the executions that
// received a PC sample would under-report the perturbation by exactly the
// fraction of kernels the sampler missed, which is most of them.
//
// Why "unknown" is a first-class answer
// -------------------------------------
// A profile that reports "not perturbed" when it means "cannot tell" is the
// failure spec §4 forbids, and it is the exact shape of the gpu_join
// precedent. So "false" is emitted ONLY from positive evidence: an execution
// interval that lies wholly inside a span of time the agent holds an unbroken
// window history for, and that intersects none of the bursts in it. Everything
// else — no windows at all, an execution outside the covered span, a hole in
// the window sequence, a burst that never closed — is "unknown".
//
// Stated as an invariant, because it is the one this file exists to hold:
// SerializationNotSerialized is returned by exactly one branch of exactly one
// function below, and that branch requires containment in a proven interval.
// Every other path falls through to SerializationUnknown, which is also the
// zero value of the type.

// defaultMaxSamplingWindowsPerPID bounds how many bursts one process's history
// may hold. At the shipped duty cycle (a 50 ms burst at most every 500 ms) a
// process produces at most two bursts a second, so this is roughly half an
// hour of Tier A before the oldest are dropped — and dropping the oldest moves
// the coverage start FORWARD, which turns old executions from "false" into
// "unknown". That is the safe direction and the only direction eviction here
// can move an answer.
const defaultMaxSamplingWindowsPerPID = 4096

// defaultMaxSamplingWindowPIDs bounds how many processes' histories are held
// at once. Tier A is opt-in and perturbing, so a machine running it in
// hundreds of processes at once is a misconfiguration rather than a workload;
// a process past the bound gets no windows, and its executions read "unknown".
const defaultMaxSamplingWindowPIDs = 256

// samplingWindow is one burst as the store holds it.
type samplingWindow struct {
	startNs uint64
	endNs   uint64 // 0 = still open when the producer stopped reporting
	mode    SamplingMode
}

func (w samplingWindow) open() bool { return w.endNs == 0 }

// serializes reports whether kernels running inside this window were
// serialized. Only the kernel-serialized mode does. An UNSET mode does not:
// it is a producer that did not say, which is not the same as a producer that
// said no — such a window is handled as opaque by classify below rather than
// being read either way.
func (w samplingWindow) serializes() bool { return w.mode == SamplingModeKernelSerialized }

// intersects reports whether [startNs, endNs] overlaps the window. An open
// window is [startNs, +inf).
func (w samplingWindow) intersects(startNs, endNs uint64) bool {
	if endNs < w.startNs {
		return false
	}
	if w.open() {
		return true
	}
	return startNs <= w.endNs
}

// windowSet is one process's burst history.
//
// coverageStartNs is the earliest instant this history can speak for. It is
// the first window's start, and it moves FORWARD on two events: a sequence gap
// (records were lost, so nothing before the gap can be shown to be contiguous)
// and an eviction (the oldest window left, so the same is true of it). It
// never moves backward, so an answer can only ever become less certain.
type windowSet struct {
	wins            []samplingWindow
	coverageStartNs uint64
	haveCoverage    bool
}

// windowStore holds every process's burst history and answers the one question
// the disclosure needs.
type windowStore struct {
	byPID     map[uint32]*windowSet
	maxPerPID int
	maxPIDs   int

	// received counts windows accepted into the store, superseded counts the
	// closed records that replaced their own burst's open record (so
	// received - superseded is the number of distinct bursts), and the last
	// three are the store's own bounded-storage losses. Every one of them is
	// assertable from a test.
	received    uint64
	superseded  uint64
	evicted     uint64
	refusedPIDs uint64
	unknownMode uint64
}

func newWindowStore(maxPerPID, maxPIDs int) *windowStore {
	if maxPerPID <= 0 {
		maxPerPID = defaultMaxSamplingWindowsPerPID
	}
	if maxPIDs <= 0 {
		maxPIDs = defaultMaxSamplingWindowPIDs
	}
	return &windowStore{
		byPID:     make(map[uint32]*windowSet),
		maxPerPID: maxPerPID,
		maxPIDs:   maxPIDs,
	}
}

// add records one window.
//
// A burst reaches the wire twice: an OPEN record the instant it starts and a
// CLOSED record with the same StartNs when it stops. The pairing is what makes
// a hard exit visible — the open record is already delivered when the process
// dies — so this must not count one burst twice, and it must not let the open
// record win. The rule is one-way: a closed record replaces an open one with
// the same start, and an open record never replaces a closed one, whichever
// order a lossy transport delivers them in.
func (s *windowStore) add(w GPUSamplingWindow) {
	if w.EndNs != 0 && w.EndNs < w.StartNs {
		// An inverted window is a producer contract violation, not a hole.
		// gpuabi.DecodeSamplingWindow already refuses these at the wire
		// boundary; refusing again here keeps the store's own invariant
		// (endNs == 0 or endNs >= startNs) true for callers that build one
		// directly, e.g. a replay fixture.
		s.unknownMode++
		return
	}
	set := s.byPID[w.PID]
	if set == nil {
		if len(s.byPID) >= s.maxPIDs {
			// Refused rather than evicting somebody else's history: dropping
			// a live process's windows would turn its executions from a
			// proven answer into "unknown", and doing that to an established
			// process to make room for a new one trades a good answer for no
			// answer. The new process's executions read "unknown", which is
			// what they honestly are.
			s.refusedPIDs++
			return
		}
		set = &windowSet{}
		s.byPID[w.PID] = set
	}
	s.received++
	if w.Mode != SamplingModeContinuous && w.Mode != SamplingModeKernelSerialized {
		s.unknownMode++
	}
	if w.Lost > 0 || !set.haveCoverage {
		// Records were lost between the previous window and this one, so the
		// history has a hole and nothing before this window can be shown to
		// be a gap. Coverage restarts here; the older windows STAY, because
		// they are still positive evidence that a burst was open then.
		set.coverageStartNs = w.StartNs
		set.haveCoverage = true
	}

	nw := samplingWindow{startNs: w.StartNs, endNs: w.EndNs, mode: w.Mode}
	// Windows arrive in start order from one producer, so the common case is
	// an append or a match against the tail. The scan is backwards for that
	// reason and is bounded by maxPerPID in the worst case.
	for i := len(set.wins) - 1; i >= 0; i-- {
		if set.wins[i].startNs != nw.startNs {
			continue
		}
		if set.wins[i].open() && !nw.open() {
			set.wins[i] = nw // the close, superseding its own open record
		}
		s.superseded++
		return
	}
	set.wins = append(set.wins, nw)
	if len(set.wins) > 1 && set.wins[len(set.wins)-2].startNs > nw.startNs {
		sort.Slice(set.wins, func(i, j int) bool { return set.wins[i].startNs < set.wins[j].startNs })
	}
	if len(set.wins) > s.maxPerPID {
		drop := len(set.wins) - s.maxPerPID
		s.evicted += uint64(drop)
		set.wins = append(set.wins[:0], set.wins[drop:]...)
		// The evicted windows were the earliest, so the history no longer
		// covers the interval before whatever is now oldest. Forward only.
		if len(set.wins) > 0 && set.wins[0].startNs > set.coverageStartNs {
			set.coverageStartNs = set.wins[0].startNs
		}
	}
}

// coverageEndNs is the last instant this history can speak for.
//
// An OPEN window ends coverage at its own start: the burst was running from
// there and nothing says when — or whether — it stopped. Otherwise coverage
// runs to the end of the last closed burst. It deliberately does NOT extend
// past that: the next burst's open record may simply not have been drained
// yet, so the interval after the last known window is not a proven gap.
func (set *windowSet) coverageEndNs() (uint64, bool) {
	if !set.haveCoverage || len(set.wins) == 0 {
		return 0, false
	}
	var end uint64
	var have bool
	for _, w := range set.wins {
		if w.startNs < set.coverageStartNs {
			continue
		}
		if w.open() {
			// The earliest open window at or after the coverage start caps
			// everything. wins is start-ordered, so this is the answer.
			return w.startNs, true
		}
		if !have || w.endNs > end {
			end, have = w.endNs, true
		}
	}
	return end, have
}

// classify answers the disclosure for one execution.
//
// The three branches, in the order that matters:
//
//  1. Intersects a closed kernel-serialized burst -> "true". Definite, and it
//     wins over everything else: an execution that provably overlapped a burst
//     is perturbed whatever else is unknown about the rest of its interval.
//  2. Wholly inside a proven span and intersecting no burst -> "false". This
//     is the ONLY branch that returns "false", and it needs positive evidence
//     on both endpoints.
//  3. Everything else -> "unknown".
func (s *windowStore) classify(pid uint32, startNs, endNs uint64) SerializationState {
	if endNs < startNs {
		// A backwards execution cannot be placed against anything.
		return SerializationUnknown
	}
	set := s.byPID[pid]
	if set == nil || !set.haveCoverage {
		// No window history for this process at all: a dropped batch, a late
		// attach, a producer that never fired the probe, or a PID the store
		// refused. All of them are "cannot tell".
		return SerializationUnknown
	}
	// "true" outranks "unknown", so the whole set is scanned before an
	// unknown is returned: an execution that provably overlapped a closed
	// burst is perturbed whatever is unknown about the rest of its interval,
	// and reporting "unknown" there would throw away a fact we hold.
	opaque := false
	for _, w := range set.wins {
		if !w.intersects(startNs, endNs) {
			continue
		}
		switch {
		case w.open():
			// The burst was running from its start and nothing says when — or
			// whether — it stopped. Cannot be placed.
			opaque = true
		case w.mode == SamplingModeUnset:
			// The producer did not say which mode. That is not evidence of
			// serialization and it is not evidence of its absence either.
			opaque = true
		case w.serializes():
			return SerializationSerialized
		default:
			// A closed continuous-mode burst. Nothing was serialized; keep
			// looking, and this interval still counts as covered.
		}
	}
	if opaque {
		return SerializationUnknown
	}
	end, ok := set.coverageEndNs()
	if !ok || startNs < set.coverageStartNs || endNs > end {
		return SerializationUnknown
	}
	return SerializationNotSerialized
}

// windows returns how many bursts are held and how many of them are still
// open. Both are gauges for the operator: a non-zero open count says a burst's
// end is unknown and an unbounded tail of executions cannot be said to have
// run unperturbed.
func (s *windowStore) windows() (held, open int) {
	for _, set := range s.byPID {
		held += len(set.wins)
		for _, w := range set.wins {
			if w.open() {
				open++
			}
		}
	}
	return held, open
}
