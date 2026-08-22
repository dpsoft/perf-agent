package test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

// ---------------------------------------------------------------------
// Collect-until-condition harness (issue #42)
// ---------------------------------------------------------------------
//
// The integration suite used to sample for a fixed wall-clock window
// (3-10s at 99 Hz) and then assert on whatever happened to land. On a
// contended shared runner that is a coin flip: a sampled task only
// yields samples while it is ON CPU, so an I/O-bound workload that the
// scheduler keeps off-CPU produces few samples, all of them taken deep
// inside kernel I/O. Issue #42 recorded three shapes of the same
// condition — zero samples at all, samples whose every frame is
// kernel-side, and samples with kernel symbols but no user mapping —
// and the decisive datum that one commit failed twice with two
// different shapes and passed on the third attempt.
//
// The fix is to stop asserting on a fixed window. A collection here is
// cheap and repeatable (spawn the agent again against the same live
// workload), so we collect until the condition the test actually cares
// about is observed, bounded by a deadline. If the deadline expires the
// test still fails — the assertion is not weakened, it is given a
// bounded budget — and it fails with a message that says what was being
// waited for and what was actually captured.
//
// Two invariants keep this from becoming a "retry until green" loop:
//
//  1. The loop condition IS the assertion under test (or a precondition
//     strictly implied by it). A profiler that genuinely stopped
//     producing user frames never satisfies it, and the test fails at
//     the deadline.
//  2. The budget is finite and small, and every attempt costs at least
//     one collection window, so the loop cannot spin and cannot run
//     longer than `budget + one window`.

// collectBudgetFor returns the total wall-clock budget a collect-until
// loop may spend for a per-attempt collection window of `window`.
//
// Generously above the fixed window it replaces (so a run that lost the
// scheduling coin flip gets several more tosses) but bounded: 3s -> 15s
// (about 5 attempts), 5s -> 15s (3), 6s -> 18s (3), 10s -> 25s (2).
//
// This bounds ONE degraded test. It does not bound a comprehensively
// degraded run: the per-loop budgets sum to ~562s of re-collection
// across the converted sites, plus ~172s of overshoot (a loop may run
// to `budget + one window`), which is ~12 minutes on top of the suite's
// own work — over the `-timeout 15m` in .github/workflows/tests.yml.
// suiteRetryBudget below is what actually bounds that case.
func collectBudgetFor(window time.Duration) time.Duration {
	const (
		minBudget = 15 * time.Second
		maxBudget = 25 * time.Second
	)
	return min(max(3*window, minBudget), maxBudget)
}

// suiteRetryBudget bounds the TOTAL wall-clock time this package may
// spend on RE-collections — attempts 2..N of any collect-until loop —
// summed across every test in the run.
//
// Per-loop budgets (collectBudgetFor) bound one degraded test; they do
// not bound a run in which everything degrades at once. Summed, they
// exceed the CI `-timeout 15m`, and blowing that timeout kills the run
// with a goroutine dump — destroying exactly the diagnostics this
// harness exists to produce. A clean assertion failure is strictly more
// useful than a timeout.
//
// So the package shares one retry allowance. The FIRST attempt of every
// loop is free: it is the collection the test would have made before
// this change, and it is charged to the suite's baseline, not here.
// Once the allowance is spent, later loops still run that first attempt
// and let their own assertions decide — the suite degrades to its
// pre-#42 behaviour rather than timing out. Worst case for the package
// therefore becomes `baseline + suiteRetryBudget + one window`, which
// is ~9 minutes against the 15m timeout.
//
// Sized for the case it is meant to absorb, not the pathological one: a
// normal run has zero or one flaking tests, costing at most one per-loop
// budget (<= 25s) between them.
const suiteRetryBudget = 3 * time.Minute

// suiteRetrySpentNs accumulates nanoseconds spent on re-collections.
// Tests in this package do not run in parallel, but the counter is
// atomic so that a future t.Parallel cannot corrupt it.
var suiteRetrySpentNs atomic.Int64

// claimSuiteRetry reports whether a re-collection of up to `window` may
// start, along with how much of the shared allowance remains.
func claimSuiteRetry(window time.Duration) (bool, time.Duration) {
	left := max(suiteRetryBudget-time.Duration(suiteRetrySpentNs.Load()), 0)
	return window <= left, left
}

// chargeSuiteRetry books time spent on a re-collection.
func chargeSuiteRetry(d time.Duration) { suiteRetrySpentNs.Add(int64(d)) }

// workloadRuntime is how long test workloads are asked to run for.
//
// It must comfortably exceed the largest collect-until budget plus
// per-test setup: a retrying test that outlives its own workload would
// be asserting against a dead process, and the deadline message would
// blame the profiler for it. requireWorkloadAlive is the backstop that
// catches it anyway.
const workloadRuntime = 150 * time.Second

// workloadRuntimeFlag is `-duration=...` for the Go workloads.
var workloadRuntimeFlag = fmt.Sprintf("-duration=%s", workloadRuntime)

// workloadRuntimeSecs is the bare seconds argument the Rust and Python
// workloads take as argv[1].
var workloadRuntimeSecs = fmt.Sprintf("%d", int(workloadRuntime.Seconds()))

// collectUntil runs `attempt` repeatedly until it reports success or the
// budget is exhausted.
//
// `what` names the condition in the first person of the test ("samples
// containing at least one user-space frame"); it is quoted verbatim in
// the timeout message. `window` is how long a single attempt spends
// collecting — used both to decide whether another attempt fits in the
// budget and to bound the iteration count.
//
// Returns (true, "") on success, and (false, report) when the budget
// ran out; `report` names the condition, the attempts spent, and the
// diagnostic string of the last attempt.
//
// Termination: `maxAttempts` bounds the iteration count independently of
// the clock, so an `attempt` that returns instantly (a collection that
// failed to start, say) cannot spin.
func collectUntil(t *testing.T, what string, window, budget time.Duration, attempt func(n int) (ok bool, detail string)) (bool, string) {
	t.Helper()
	if window <= 0 {
		window = time.Second
	}
	if budget < window {
		budget = window
	}
	maxAttempts := int(budget/window) + 1

	start := time.Now()
	detail := "no attempt completed"
	attempts := 0
	starved := ""
	for n := 1; n <= maxAttempts; n++ {
		attemptStart := time.Now()
		var ok bool
		ok, detail = attempt(n)
		attempts = n
		if n > 1 {
			// Attempt 1 is the collection the test would have made
			// anyway; only re-collections draw on the shared allowance.
			chargeSuiteRetry(time.Since(attemptStart))
		}
		elapsed := time.Since(start)
		if ok {
			if n > 1 {
				t.Logf("collect-until: %s satisfied on attempt %d after %s; %s",
					what, n, elapsed.Round(time.Millisecond), detail)
			}
			return true, ""
		}
		t.Logf("collect-until: attempt %d did not satisfy %s after %s; %s",
			n, what, elapsed.Round(time.Millisecond), detail)
		if elapsed+window > budget {
			break
		}
		if allowed, left := claimSuiteRetry(window); !allowed {
			starved = fmt.Sprintf(
				". NOTE: the package-wide re-collection allowance (%s) is spent (%s left), "+
					"so this test stopped at %d attempt(s) rather than the %d its own budget allows — "+
					"earlier tests in this run were degraded too",
				suiteRetryBudget, left.Round(time.Second), n, maxAttempts)
			break
		}
	}

	// Deliberately not "exhausted the budget": when the shared allowance
	// stopped the loop early that would be false, and `starved` says so.
	return false, fmt.Sprintf(
		"timed out waiting for %s: %d attempt(s) over %s (per-test budget %s, "+
			"collection window %s per attempt); last attempt captured: %s%s",
		what, attempts, time.Since(start).Round(time.Millisecond), budget, window, detail, starved)
}

// collectProfileUntil repeats one full collection per attempt until
// `cond` holds on the resulting pprof or the budget expires.
//
// `collect` performs a single collection and returns the parsed profile;
// an error from it (empty output, unparseable output, agent produced
// nothing) is NOT fatal — it is exactly one of the failure shapes issue
// #42 is about, so it is reported as an unsatisfied attempt and retried.
// Hard environmental failures (the agent refusing to run at all) should
// t.Fatal from inside `collect`.
//
// Returns the profile from the last attempt (nil when the last attempt
// produced none), whether `cond` was satisfied, and the timeout report
// to hand to t.Fatal when it was not.
func collectProfileUntil(
	t *testing.T,
	what string,
	window time.Duration,
	collect func(n int) (*profile.Profile, error),
	cond func(*profile.Profile) bool,
) (*profile.Profile, bool, string) {
	t.Helper()
	var last *profile.Profile
	ok, report := collectUntil(t, what, window, collectBudgetFor(window), func(n int) (bool, string) {
		p, err := collect(n)
		last = p
		if err != nil {
			return false, fmt.Sprintf("no usable profile: %v", err)
		}
		return cond(p), describeProfile(p)
	})
	return last, ok, report
}

// ---------------------------------------------------------------------
// Profile diagnostics
// ---------------------------------------------------------------------

const (
	kernelMappingFile = "[kernel]"
	jitMappingFile    = "[jit]"
)

// profileSummary is the "what actually landed" snapshot printed by every
// collect-until timeout and by assertPprofFidelity on failure.
//
// The split that matters is user vs kernel frames: issue #42's amd64
// failure ("expected >=1 real mapping, got 0") could not distinguish "the
// profiler produced bad mappings" from "the workload was never on-CPU in
// user space", and that distinction is what decides whether the failure
// is a real regression.
type profileSummary struct {
	Samples   int
	Locations int
	Functions int
	// NamedFunctions counts functions with a resolved (non-empty,
	// non-"??") name — symbolization actually worked for them.
	NamedFunctions int

	// Frame counts are per sample-location occurrence, not per distinct
	// location, so they describe where the sampled time went.
	UserFrames     int
	KernelFrames   int
	JitFrames      int
	UnmappedFrames int

	RealMappings   int
	KernelMappings int
	JitMappings    int
	AnonMappings   int

	HasBuildID bool
	TotalValue int64
}

// mappingClass buckets a pprof mapping into the four kinds this suite
// cares about. A nil mapping and the builder's default zero-file mapping
// are both "anon": a PC that resolved to no binary at all.
func mappingClass(m *profile.Mapping) string {
	switch {
	case m == nil || m.File == "":
		return "anon"
	case m.File == kernelMappingFile:
		return "kernel"
	case m.File == jitMappingFile:
		return "jit"
	default:
		return "real"
	}
}

func summarizeProfile(p *profile.Profile) profileSummary {
	var s profileSummary
	if p == nil {
		return s
	}
	s.Samples = len(p.Sample)
	s.Locations = len(p.Location)
	s.Functions = len(p.Function)

	for _, fn := range p.Function {
		if fn.Name != "" && fn.Name != "??" {
			s.NamedFunctions++
		}
	}
	for _, m := range p.Mapping {
		switch mappingClass(m) {
		case "kernel":
			s.KernelMappings++
		case "jit":
			s.JitMappings++
		case "anon":
			s.AnonMappings++
		default:
			s.RealMappings++
		}
		if m.BuildID != "" {
			s.HasBuildID = true
		}
	}
	for _, sample := range p.Sample {
		for _, v := range sample.Value {
			s.TotalValue += v
		}
		for _, loc := range sample.Location {
			var m *profile.Mapping
			if loc != nil {
				m = loc.Mapping
			}
			switch mappingClass(m) {
			case "kernel":
				s.KernelFrames++
			case "jit":
				s.JitFrames++
			case "anon":
				s.UnmappedFrames++
			default:
				s.UserFrames++
			}
		}
	}
	return s
}

func (s profileSummary) String() string {
	return fmt.Sprintf(
		"samples=%d value_total=%d locations=%d functions=%d(named=%d) "+
			"frames{user=%d kernel=%d jit=%d unmapped=%d} "+
			"mappings{real=%d kernel=%d jit=%d anon=%d} build_id=%v",
		s.Samples, s.TotalValue, s.Locations, s.Functions, s.NamedFunctions,
		s.UserFrames, s.KernelFrames, s.JitFrames, s.UnmappedFrames,
		s.RealMappings, s.KernelMappings, s.JitMappings, s.AnonMappings,
		s.HasBuildID)
}

// describeProfile renders a one-line diagnostic for a profile that may be
// nil or empty. "0 samples" is stated in words, because an empty profile
// used to surface as a bare `EOF` from the pprof parser.
func describeProfile(p *profile.Profile) string {
	if p == nil {
		return "no profile at all (the collection produced no parseable output)"
	}
	if len(p.Sample) == 0 {
		return "an EMPTY profile: 0 samples (the collection window caught the workload off-CPU the whole time)"
	}
	return summarizeProfile(p).String()
}

// topFunctionNames returns up to n resolved function names, for failure
// messages that need to show what DID symbolize.
func topFunctionNames(p *profile.Profile, n int) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, n)
	for _, fn := range p.Function {
		if fn.Name == "" {
			continue
		}
		out = append(out, fn.Name)
		if len(out) == n {
			break
		}
	}
	return out
}

// hasFunctionContaining reports whether any function name in the profile
// contains any of the given substrings.
func hasFunctionContaining(p *profile.Profile, subs ...string) bool {
	if p == nil {
		return false
	}
	for _, fn := range p.Function {
		for _, sub := range subs {
			if strings.Contains(fn.Name, sub) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Reading profiles
// ---------------------------------------------------------------------

// readProfile parses a pprof file, distinguishing "the agent wrote
// nothing because it captured nothing" from "the profile is corrupt".
//
// The old path (gzip.NewReader on a zero-byte file) reported the first
// case as a bare `EOF`, which is what issue #42's arm64 run had to be
// diagnosed from. profile.Parse handles the gzip wrapper itself.
func readProfile(path string) (*profile.Profile, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("profile %s was never written: %w", path, err)
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf(
			"profile %s is EMPTY (0 bytes): the agent collected 0 samples in this window", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile %s: %w", path, err)
	}
	p, err := profile.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf(
			"profile %s (%d bytes) did not parse: %w — a truncated or empty profile means the collection captured no samples",
			path, len(body), err)
	}
	return p, nil
}

// ---------------------------------------------------------------------
// Workload liveness
// ---------------------------------------------------------------------

// processAlive reports whether pid names a live, non-zombie process.
// A killed-but-unreaped child still answers signal 0, so read the state
// field out of /proc/<pid>/stat instead.
func processAlive(pid int) bool {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// "<pid> (<comm>) <state> ..." — comm can contain spaces and
	// parentheses, so anchor on the LAST ')'.
	i := bytes.LastIndexByte(body, ')')
	if i < 0 || i+2 >= len(body) {
		return false
	}
	switch body[i+2] {
	case 'Z', 'X', 'x':
		return false
	}
	return true
}

// requireWorkloadAlive fails the test when the workload died before the
// collect-until loop finished with it.
//
// Without this, a workload whose own duration is shorter than the
// collection budget turns every later attempt into a guaranteed miss,
// and the deadline message would blame the profiler for a dead target.
func requireWorkloadAlive(t *testing.T, cmd *exec.Cmd, name string) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		t.Fatalf("workload %s was never started", name)
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatalf("workload %s (pid %d) exited before the collect-until budget was spent — "+
			"its run duration is shorter than the collection budget, so the condition under test is unreachable",
			name, cmd.Process.Pid)
	}
}

// profileHasStacks reports whether any sample carries at least one
// location — i.e. stack capture produced something.
func profileHasStacks(p *profile.Profile) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Sample {
		if len(s.Location) > 0 {
			return true
		}
	}
	return false
}

// profileHasSymbols reports whether at least one function carries a
// resolved name.
func profileHasSymbols(p *profile.Profile) bool {
	if p == nil {
		return false
	}
	for _, fn := range p.Function {
		if fn.Name != "" && fn.Name != "??" {
			return true
		}
	}
	return false
}

// userSpaceFrames counts sampled frames that came from user space,
// whether or not they resolved to a binary: file-backed, JIT, and PCs
// that resolved to no mapping at all. Kernel frames are excluded.
//
// This is the PRECONDITION half of a fidelity check, deliberately kept
// distinct from the assertion half. "Did the capture catch the target
// on-CPU in user space?" is an environmental question and worth
// retrying; "did those user-space PCs resolve to a real mapping?" is a
// correctness question about the profiler and must NOT be retried — a
// loop that waits for a real mapping can never fail the assertion that
// a real mapping exists. See fidelityDiagnosis, which splits the same
// two cases for the reader.
//
// Note that `UserFrames > 0` alone would NOT do: a location's mapping
// must appear in the profile's mapping table, so UserFrames > 0 implies
// RealMappings >= 1 just as surely. Including the unmapped frames is
// what makes this a precondition rather than a restatement.
func userSpaceFrames(s profileSummary) int {
	return s.UserFrames + s.JitFrames + s.UnmappedFrames
}

// profileHasUserSpaceFrames reports whether the capture caught the
// target on-CPU in user space at all.
func profileHasUserSpaceFrames(p *profile.Profile) bool {
	return userSpaceFrames(summarizeProfile(p)) > 0
}

// degenerateSampleFloor is the sample-count threshold below which the
// pprof fidelity / symbolization assertions are considered too noisy
// to be meaningful. A healthy 10s @ 99Hz CPU-bound run produces
// hundreds of user-space samples; the IO-bound subtests on slow CI
// runners can drop into the low tens or single digits.
//
// Lives here rather than beside isDegenerateProfile because the
// collect-until preconditions need it too, and a non-test file cannot
// see a constant declared in a _test.go one — `go build ./...` catches
// that even though `go vet` and `go test -c` do not.
const degenerateSampleFloor = 20

// profileFidelityJudgeable reports whether a capture carries enough
// signal for assertPprofFidelity's verdict to mean anything: at least
// degenerateSampleFloor samples, and at least one of them from user
// space.
//
// The sample floor is not a number invented here. isDegenerateProfile
// already uses degenerateSampleFloor to decide when a zero-real-mapping
// profile is tolerated and when it is "a genuine bug worth a loud
// failure" — its words. Reusing the same constant keeps the retry
// policy and the tolerance policy from disagreeing: we re-collect
// exactly while the capture is below the bar the suite already set for
// judging it, and hand it to the assertion the moment it clears it.
//
// Neither conjunct implies `RealMappings >= 1`, which is what keeps
// assertPprofFidelity able to fail — see userSpaceFrames.
func profileFidelityJudgeable(p *profile.Profile) bool {
	s := summarizeProfile(p)
	return s.Samples >= degenerateSampleFloor && userSpaceFrames(s) > 0
}

// describeMappings renders a profile's mapping table.
//
// Deliberately NOT `%+v` on p.Mapping. fmt prints a slice of struct
// POINTERS as bare heap addresses — `[0x1311ff8c230]` — because it only
// uses the `&{...}` form at depth 0. That is exactly what the message
// behind issue #42's symptom 3 said:
//
//	expected >=1 real mapping, got 0: [0x1311ff8c230]
//
// which is a Go pointer to one profile.Mapping, not an address from the
// capture and not any mapping's contents. The message carried a
// mapping COUNT and nothing else, so the reading recorded in the issue
// ("every user PC landed in one anonymous region") could not have been
// derived from it. This renders the fields, so the next occurrence is
// diagnosable from the log alone.
func describeMappings(p *profile.Profile) string {
	if p == nil || len(p.Mapping) == 0 {
		return "no mappings"
	}
	const maxShown = 12
	var b strings.Builder
	fmt.Fprintf(&b, "%d mapping(s):", len(p.Mapping))
	for i, m := range p.Mapping {
		if i == maxShown {
			fmt.Fprintf(&b, " ... and %d more", len(p.Mapping)-maxShown)
			break
		}
		if m == nil {
			b.WriteString(" <nil>")
			continue
		}
		fmt.Fprintf(&b, " [#%d file=%q start=%#x limit=%#x off=%#x build_id=%q]",
			m.ID, m.File, m.Start, m.Limit, m.Offset, m.BuildID)
	}
	return b.String()
}

// profileTotalValue sums every value of every sample — the blocking-ns
// total for an off-CPU profile.
func profileTotalValue(p *profile.Profile) int64 {
	if p == nil {
		return 0
	}
	var total int64
	for _, s := range p.Sample {
		for _, v := range s.Value {
			total += v
		}
	}
	return total
}
