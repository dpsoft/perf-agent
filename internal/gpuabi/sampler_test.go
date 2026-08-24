package gpuabi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shared pin. These are the same numbers shim/core/sampler_test.cc
// asserts on the C++ side; the two implementations of the schedule have to
// agree or "the consumer can verify the sampler" is a claim with nothing
// behind it. A change to the hash, the seed or the gap band fails both
// suites, which is the point.
func TestTheGoReplicaMatchesTheShimSampler(t *testing.T) {
	wantGaps8 := []uint64{11, 4, 9, 6, 4, 8, 4, 8, 9, 5}
	wantGaps3 := []uint64{4, 2, 3, 2, 2, 3, 2, 3, 3, 2}
	for seq := range uint64(10) {
		assert.Equal(t, wantGaps8[seq], SampleGap(seq, 8, DefaultSampleSeed),
			"gap at seq %d, period 8", seq)
		assert.Equal(t, wantGaps3[seq], SampleGap(seq, 3, DefaultSampleSeed),
			"gap at seq %d, period 3", seq)
	}

	sched := SampleSchedule(500, 8, DefaultSampleSeed)
	require.GreaterOrEqual(t, len(sched), 10)
	assert.Equal(t, []uint64{0, 11, 19, 31, 38, 43, 51, 62, 70, 77}, sched[:10],
		"the first ten sample points of the default schedule at period 8")

	// The two counts the phase gate and cmd/gpu-cuda-profile depend on. Exact,
	// because the schedule is a deterministic chain from the seed.
	assert.Equal(t, uint64(58), SampledCount(500, 8, DefaultSampleSeed),
		"the phase gate's 500 launches at period 8")
	assert.Equal(t, uint64(505), SampledCount(4000, 8, DefaultSampleSeed),
		"the CUDA validation run's 4000 launches at period 8")
	assert.Equal(t, uint64(len(sched)), SampledCount(500, 8, DefaultSampleSeed),
		"SampleSchedule and SampledCount must not disagree")
}

// The property the whole fix exists for, stated on the consumer side: the
// schedule reaches both residues of a 2-cycle. Under the fixed stride every
// sample point at an even period had the same parity, so a workload
// alternating two kernels gave one of them every stack and the other none
// (issue #50, measured on an RTX 3090: perfagent_scale held 48% of GPU time
// and appeared in zero sampled stacks).
func TestTheScheduleDoesNotLockPhase(t *testing.T) {
	for cycle := uint64(2); cycle <= 4; cycle++ {
		for period := uint32(2); period <= 12; period++ {
			hits := make([]int, cycle)
			for _, seq := range SampleSchedule(20000, period, DefaultSampleSeed) {
				hits[seq%cycle]++
			}
			for residue, n := range hits {
				assert.Positivef(t, n,
					"period %d never samples launch ordinals congruent to %d mod %d: "+
						"a workload cycling %d kernels loses that one entirely",
					period, residue, cycle, cycle)
			}
		}
	}
}

// sample_period still names the rate it always named: gaps are drawn from a
// set of consecutive integers centred on the period, so the mean gap is
// exactly the period. Asserted to 1% over 200k launches -- at period 8 the
// per-gap standard deviation is ~2.6 launches, so 25k gaps put a 1% band far
// outside any plausible deviation of a correct schedule.
func TestTheSampledRateStaysAtOneInPeriod(t *testing.T) {
	const launches = 200_000
	for _, period := range []uint32{2, 3, 4, 7, 8, 16, 64} {
		got := float64(SampledCount(launches, period, DefaultSampleSeed))
		want := float64(launches) / float64(period)
		assert.InEpsilonf(t, want, got, 0.01,
			"period %d sampled %.0f of %d launches, expected ~%.1f", period, got, launches, want)
	}
}

// Period 1 samples every launch and period 0 is coerced to 1, exactly as the
// fixed-stride sampler did. The gap band collapses to {1} at period 1, so
// this is not a special case in the code, but it is the one behaviour a
// caller can depend on by name.
func TestPeriodOneAndZeroAreUnchanged(t *testing.T) {
	assert.Equal(t, uint64(1), SampleGap(0, 1, DefaultSampleSeed))
	assert.Equal(t, uint64(1), SampleGap(12345, 1, DefaultSampleSeed))
	assert.Equal(t, uint64(100), SampledCount(100, 1, DefaultSampleSeed),
		"period 1 must sample every launch")
	assert.Equal(t, uint64(100), SampledCount(100, 0, DefaultSampleSeed),
		"period 0 is coerced to 1, never a division by zero and never silent capture loss")
}

// Every gap lies inside the band the ABI promises, which is the invariant a
// consumer can check from the sampled records' launch_seq values alone, with
// no seed. It replaces `launch_seq % sample_period == 0` as the producer-side
// sanity check.
func TestEveryGapLiesInTheDeclaredBand(t *testing.T) {
	for _, period := range []uint32{2, 3, 4, 8, 16, 64} {
		lo := uint64(period - period/2)
		hi := uint64(period + period/2)
		sched := SampleSchedule(50_000, period, DefaultSampleSeed)
		for i := 1; i < len(sched); i++ {
			gap := sched[i] - sched[i-1]
			assert.GreaterOrEqualf(t, gap, lo, "period %d gap %d below the band", period, gap)
			assert.LessOrEqualf(t, gap, hi, "period %d gap %d above the band", period, gap)
		}
	}
}

// A different seed gives a different schedule, so PERFAGENT_GPU_SAMPLE_SEED
// actually buys per-process variation rather than being decoration.
func TestADifferentSeedGivesADifferentSchedule(t *testing.T) {
	a := SampleSchedule(1000, 8, DefaultSampleSeed)
	b := SampleSchedule(1000, 8, 0x1234567890ABCDEF)
	assert.NotEqual(t, a, b)
}
