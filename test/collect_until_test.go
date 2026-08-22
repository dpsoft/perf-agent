package test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

// These tests cover the collect-until harness itself. They load no BPF
// and spawn no workload, so they run without root or capabilities —
// the harness is the one part of the integration suite whose logic can
// be checked on an unprivileged machine.

func TestCollectBudgetForClampsBothEnds(t *testing.T) {
	if got := collectBudgetFor(1 * time.Second); got != 15*time.Second {
		t.Errorf("collectBudgetFor(1s) = %s, want the 15s floor", got)
	}
	if got := collectBudgetFor(6 * time.Second); got != 18*time.Second {
		t.Errorf("collectBudgetFor(6s) = %s, want 3x = 18s", got)
	}
	if got := collectBudgetFor(60 * time.Second); got != 25*time.Second {
		t.Errorf("collectBudgetFor(60s) = %s, want the 25s ceiling", got)
	}
}

func TestCollectUntilReturnsOnFirstSatisfyingAttempt(t *testing.T) {
	calls := 0
	ok, report := collectUntil(t, "the condition", 10*time.Millisecond, time.Second,
		func(int) (bool, string) {
			calls++
			return calls == 2, "detail"
		})
	if !ok {
		t.Fatalf("expected success, got report: %s", report)
	}
	if calls != 2 {
		t.Errorf("attempt called %d times, want exactly 2 (stop as soon as satisfied)", calls)
	}
	if report != "" {
		t.Errorf("report on success = %q, want empty", report)
	}
}

// TestCollectUntilCannotSpin pins the property that matters most for a
// loop with a deadline: an attempt that returns instantly and never
// succeeds is bounded by the attempt count, not just by the clock.
func TestCollectUntilCannotSpin(t *testing.T) {
	calls := 0
	window := 10 * time.Millisecond
	budget := 50 * time.Millisecond
	ok, report := collectUntil(t, "an impossible condition", window, budget,
		func(int) (bool, string) {
			calls++
			return false, "nothing landed"
		})
	if ok {
		t.Fatal("expected failure for an impossible condition")
	}
	maxAttempts := int(budget/window) + 1
	if calls > maxAttempts {
		t.Errorf("attempt called %d times, want <= %d", calls, maxAttempts)
	}
	for _, want := range []string{"an impossible condition", "nothing landed", "attempt(s)"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q does not mention %q", report, want)
		}
	}
}

// TestCollectUntilStopsWhenNoAttemptFits covers the clock-side bound:
// an attempt that consumes the whole budget is not retried.
func TestCollectUntilStopsWhenNoAttemptFits(t *testing.T) {
	calls := 0
	ok, _ := collectUntil(t, "a condition", 20*time.Millisecond, 30*time.Millisecond,
		func(int) (bool, string) {
			calls++
			time.Sleep(25 * time.Millisecond)
			return false, "still nothing"
		})
	if ok {
		t.Fatal("expected failure")
	}
	if calls != 1 {
		t.Errorf("attempt called %d times, want 1 (a second attempt does not fit the budget)", calls)
	}
}

func TestCollectProfileUntilRetriesUnreadableProfiles(t *testing.T) {
	calls := 0
	p, ok, report := collectProfileUntil(t, "a profile with samples", 10*time.Millisecond,
		func(int) (*profile.Profile, error) {
			calls++
			if calls == 1 {
				return nil, os.ErrNotExist
			}
			return &profile.Profile{Sample: []*profile.Sample{{}}}, nil
		},
		func(p *profile.Profile) bool { return len(p.Sample) > 0 })
	if !ok {
		t.Fatalf("expected success on the second attempt; report: %s", report)
	}
	if calls != 2 {
		t.Errorf("collect called %d times, want 2", calls)
	}
	if p == nil || len(p.Sample) != 1 {
		t.Errorf("returned profile = %v, want the one from the last attempt", p)
	}
}

// buildSyntheticProfile returns a profile with one sample carrying one
// frame of each mapping class.
func buildSyntheticProfile() *profile.Profile {
	realMapping := &profile.Mapping{ID: 1, File: "/usr/bin/workload", BuildID: "abcd"}
	kernelMapping := &profile.Mapping{ID: 2, File: kernelMappingFile}
	jitMapping := &profile.Mapping{ID: 3, File: jitMappingFile}
	anonMapping := &profile.Mapping{ID: 4}

	locs := []*profile.Location{
		{ID: 1, Mapping: realMapping, Address: 0x10},
		{ID: 2, Mapping: kernelMapping, Address: 0x20},
		{ID: 3, Mapping: jitMapping},
		{ID: 4, Mapping: anonMapping, Address: 0x40},
	}
	return &profile.Profile{
		Mapping:  []*profile.Mapping{realMapping, kernelMapping, jitMapping, anonMapping},
		Location: locs,
		Function: []*profile.Function{{ID: 1, Name: "main.work"}, {ID: 2, Name: ""}},
		Sample:   []*profile.Sample{{Location: locs, Value: []int64{7}}},
	}
}

func TestSummarizeProfileSplitsFramesAndMappings(t *testing.T) {
	s := summarizeProfile(buildSyntheticProfile())
	if s.Samples != 1 || s.TotalValue != 7 {
		t.Errorf("samples=%d total=%d, want 1 and 7", s.Samples, s.TotalValue)
	}
	if s.UserFrames != 1 || s.KernelFrames != 1 || s.JitFrames != 1 || s.UnmappedFrames != 1 {
		t.Errorf("frame split = user %d kernel %d jit %d unmapped %d, want 1 each",
			s.UserFrames, s.KernelFrames, s.JitFrames, s.UnmappedFrames)
	}
	if s.RealMappings != 1 || s.KernelMappings != 1 || s.JitMappings != 1 || s.AnonMappings != 1 {
		t.Errorf("mapping split = real %d kernel %d jit %d anon %d, want 1 each",
			s.RealMappings, s.KernelMappings, s.JitMappings, s.AnonMappings)
	}
	if s.NamedFunctions != 1 || !s.HasBuildID {
		t.Errorf("named=%d build_id=%v, want 1 and true", s.NamedFunctions, s.HasBuildID)
	}
	for _, want := range []string{"samples=1", "user=1", "kernel=1", "real=1"} {
		if !strings.Contains(s.String(), want) {
			t.Errorf("summary %q does not contain %q", s, want)
		}
	}
}

func TestDescribeProfileNamesTheEmptyCases(t *testing.T) {
	if got := describeProfile(nil); !strings.Contains(got, "no profile") {
		t.Errorf("describeProfile(nil) = %q", got)
	}
	got := describeProfile(&profile.Profile{})
	if !strings.Contains(got, "EMPTY") || !strings.Contains(got, "0 samples") {
		t.Errorf("describeProfile(empty) = %q, want it to say EMPTY and 0 samples", got)
	}
}

func TestReadProfileReportsEmptyFileAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.pb.gz")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readProfile(path)
	if err == nil {
		t.Fatal("expected an error for a zero-byte profile")
	}
	if !strings.Contains(err.Error(), "EMPTY") || !strings.Contains(err.Error(), "0 samples") {
		t.Errorf("error %q does not say the profile was empty", err)
	}
}

func TestReadProfileReportsGarbageAsUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.pb.gz")
	if err := os.WriteFile(path, []byte("not a profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfile(path); err == nil || !strings.Contains(err.Error(), "did not parse") {
		t.Errorf("error = %v, want a 'did not parse' diagnosis", err)
	}
}

func TestFidelityDiagnosisDistinguishesTheCases(t *testing.T) {
	cases := []struct {
		name string
		sum  profileSummary
		want string
	}{
		{"nothing sampled", profileSummary{}, "nothing was sampled"},
		{"user PCs with no mapping", profileSummary{Samples: 3, UnmappedFrames: 9}, "mapping resolution"},
		{"all kernel", profileSummary{Samples: 3, KernelFrames: 9}, "never on-CPU in user space"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fidelityDiagnosis(tc.sum); !strings.Contains(got, tc.want) {
				t.Errorf("fidelityDiagnosis = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

func TestProcessAliveTracksSelfAndNonexistent(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("processAlive(self) = false")
	}
	if processAlive(1 << 30) {
		t.Error("processAlive(bogus pid) = true")
	}
}

func TestHasFunctionContaining(t *testing.T) {
	p := buildSyntheticProfile()
	if !hasFunctionContaining(p, "runtime.", "main.") {
		t.Error("expected main.work to match")
	}
	if hasFunctionContaining(p, "nope") {
		t.Error("unexpected match")
	}
	if hasFunctionContaining(nil, "main.") {
		t.Error("nil profile should not match")
	}
}

// TestUserSpacePreconditionDoesNotImplyRealMapping is the regression
// guard for the defect this harness must not have: if the loop waits on
// something that implies the assertion, the assertion can never fail
// after a successful loop, and an intermittent regression is retried
// into a pass.
//
// The precondition must hold for a capture whose user-space PCs
// resolved to NO binary — the exact shape issue #42 saw on amd64 —
// while summarizeProfile still reports zero real mappings, so
// assertPprofFidelity can fail on it.
func TestUserSpacePreconditionDoesNotImplyRealMapping(t *testing.T) {
	anon := &profile.Mapping{ID: 1, Start: 0x1311ff8c000}
	locs := []*profile.Location{{ID: 1, Mapping: anon, Address: 0x1311ff8c230}}
	p := &profile.Profile{
		Mapping:  []*profile.Mapping{anon},
		Location: locs,
		Sample:   []*profile.Sample{{Location: locs, Value: []int64{1}}},
	}
	if !profileHasUserSpaceFrames(p) {
		t.Error("a capture with an unmapped user PC must satisfy the precondition")
	}
	if got := summarizeProfile(p).RealMappings; got != 0 {
		t.Errorf("RealMappings = %d, want 0 — the fidelity assertion must still be able to fail", got)
	}

	// The other half: a kernel-only capture must NOT satisfy the
	// precondition, so it is retried rather than judged.
	kern := &profile.Mapping{ID: 2, File: kernelMappingFile}
	klocs := []*profile.Location{{ID: 2, Mapping: kern, Address: 0xffffffff8f6a831c}}
	kp := &profile.Profile{
		Mapping:  []*profile.Mapping{kern},
		Location: klocs,
		Sample:   []*profile.Sample{{Location: klocs, Value: []int64{1}}},
	}
	if profileHasUserSpaceFrames(kp) {
		t.Error("a kernel-only capture must not satisfy the user-space precondition")
	}
}

// TestCollectUntilRespectsTheSuiteWideAllowance pins the global bound:
// once the package's shared re-collection allowance is spent, a loop
// runs its (free) first attempt and stops, so a comprehensively
// degraded run degrades to pre-#42 behaviour instead of overrunning the
// CI timeout.
func TestCollectUntilRespectsTheSuiteWideAllowance(t *testing.T) {
	saved := suiteRetrySpentNs.Swap(int64(suiteRetryBudget))
	defer suiteRetrySpentNs.Store(saved)

	calls := 0
	ok, report := collectUntil(t, "a condition", 10*time.Millisecond, time.Second,
		func(int) (bool, string) {
			calls++
			return false, "nothing landed"
		})
	if ok {
		t.Fatal("expected failure")
	}
	if calls != 1 {
		t.Errorf("attempt called %d times, want 1 (the shared allowance is spent)", calls)
	}
	if !strings.Contains(report, "package-wide re-collection allowance") {
		t.Errorf("report %q does not explain that the shared allowance ran out", report)
	}
}

// TestCollectUntilChargesOnlyReCollections pins the accounting: the
// first attempt of a loop is the collection the test would have made
// anyway and must not draw on the shared allowance; attempts after it
// must.
func TestCollectUntilChargesOnlyReCollections(t *testing.T) {
	saved := suiteRetrySpentNs.Swap(0)
	defer suiteRetrySpentNs.Store(saved)

	collectUntil(t, "a condition met immediately", 10*time.Millisecond, 15*time.Millisecond,
		func(int) (bool, string) {
			time.Sleep(12 * time.Millisecond)
			return true, "satisfied"
		})
	if spent := suiteRetrySpentNs.Load(); spent != 0 {
		t.Errorf("charged %dns for a single-attempt loop, want 0", spent)
	}

	n := 0
	collectUntil(t, "a condition met on the second attempt", 5*time.Millisecond, 500*time.Millisecond,
		func(int) (bool, string) {
			n++
			time.Sleep(6 * time.Millisecond)
			return n == 2, "detail"
		})
	if spent := suiteRetrySpentNs.Load(); spent <= 0 {
		t.Errorf("charged %dns for a loop that re-collected, want > 0", spent)
	}
}

// symptomThreeProfile reconstructs the capture behind issue #42's third
// comment, from what the code guarantees rather than from the log text:
//
//   - pprof/pprof.go:182 always pre-creates Mapping[0] = {ID:1} with an
//     empty File, referenced or not;
//   - every kernel frame appends a "[kernel]" sentinel mapping, every
//     JIT frame a "[jit]" one, every resolver hit a file-backed one;
//   - the write path is WriteUncompressed with no Compact/Merge, and a
//     round trip preserves unreferenced mappings
//     (TestProfileRoundTripKeepsUnreferencedMappings);
//   - the failure printed exactly ONE mapping pointer, and
//     isDegenerateProfile returned false with real=0 and jit=0, which
//     given its own logic can only mean len(Sample) >= the floor.
//
// So: >= 20 samples, every frame on the default anon mapping, and NO
// kernel frames at all.
func symptomThreeProfile(samples int) *profile.Profile {
	def := &profile.Mapping{ID: 1}
	locs := []*profile.Location{{ID: 1, Mapping: def, Address: 0x2a10}}
	p := &profile.Profile{
		Mapping:  []*profile.Mapping{def},
		Location: locs,
		Function: []*profile.Function{{ID: 1, Name: "rust_workload::cpu_intensive_work"}},
	}
	for range samples {
		p.Sample = append(p.Sample, &profile.Sample{Location: locs, Value: []int64{1}})
	}
	return p
}

// TestProfileRoundTripKeepsUnreferencedMappings pins the fact the
// symptom-3 reconstruction rests on: nothing in the write/parse path
// prunes a mapping just because no location points at it. If this ever
// stops holding, "exactly one mapping" no longer implies "the default
// one", and the reasoning in symptomThreeProfile has to be redone.
func TestProfileRoundTripKeepsUnreferencedMappings(t *testing.T) {
	def := &profile.Mapping{ID: 1}
	kern := &profile.Mapping{ID: 2, File: kernelMappingFile}
	loc := &profile.Location{ID: 1, Mapping: kern, Address: 0xffffffff8f6a831c}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
		Mapping:    []*profile.Mapping{def, kern},
		Location:   []*profile.Location{loc},
		Sample:     []*profile.Sample{{Location: []*profile.Location{loc}, Value: []int64{1}}},
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := profile.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(back.Mapping) != 2 {
		t.Fatalf("round trip kept %d mapping(s), want 2 (the unreferenced default must survive)", len(back.Mapping))
	}
	if back.Mapping[0].File != "" {
		t.Errorf("mapping[0].File = %q, want the empty default", back.Mapping[0].File)
	}
}

// TestSymptomThreeIsJudgedNotRetried states the outcome plainly: the
// capture behind symptom 3 clears the fidelity precondition, so it is
// handed to assertPprofFidelity and fails on the first attempt. This
// change does not make that failure green, and the test exists so that
// nobody "fixes" it later by widening the loop condition.
func TestSymptomThreeIsJudgedNotRetried(t *testing.T) {
	p := symptomThreeProfile(40)

	if !profileFidelityJudgeable(p) {
		t.Fatal("symptom 3's capture must clear the precondition — it is judged, not retried")
	}
	if got := summarizeProfile(p).RealMappings; got != 0 {
		t.Fatalf("RealMappings = %d, want 0 — assertPprofFidelity must still fail on it", got)
	}
	if diag := fidelityDiagnosis(summarizeProfile(p)); !strings.Contains(diag, "mapping resolution") {
		t.Errorf("diagnosis = %q, want it to name mapping resolution as the suspect", diag)
	}

	// The starved shape — same mappings, too few samples — is retried
	// instead, and if the budget runs out isDegenerateProfile tolerates
	// it exactly as it did before this change.
	starved := symptomThreeProfile(degenerateSampleFloor - 1)
	if profileFidelityJudgeable(starved) {
		t.Error("a capture below the sample floor must be re-collected, not judged")
	}
	if !isDegenerateProfile(starved) {
		t.Error("the starved shape must still hit the pre-existing degenerate tolerance")
	}
}

// TestDescribeMappingsRendersContentsNotPointers guards the message
// defect that made symptom 3 undiagnosable: `%+v` on a slice of struct
// pointers prints heap addresses, not fields.
func TestDescribeMappingsRendersContentsNotPointers(t *testing.T) {
	p := symptomThreeProfile(40)

	got := describeMappings(p)
	for _, want := range []string{"1 mapping(s)", `file=""`, "#1"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeMappings = %q, want it to contain %q", got, want)
		}
	}
	// The old form carried none of that — a count and a heap address.
	if old := fmt.Sprintf("%+v", p.Mapping); strings.Contains(old, "file=") || strings.Contains(old, "File:") {
		t.Errorf("premise check failed: %%+v on []*Mapping now renders fields (%q); "+
			"the reasoning about issue #42's message needs revisiting", old)
	}
}
