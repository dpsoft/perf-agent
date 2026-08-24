package gpuabi

// The consumer-side replica of the shim's launch sampler
// (shim/core/sampler.h). It exists so that "the consumer can verify the
// sampler" survives the move from a fixed stride to a jittered one
// (issue #50).
//
// Under the old sampler a launch was sampled iff launch_seq%sample_period==0,
// which any consumer could check with one modulo and no shared state. The
// jittered sampler draws each gap from a pure function of (seed, period,
// sample point), so the schedule is still a deterministic chain -- it just
// takes this much arithmetic to replay instead of a modulo. Everything below
// mirrors Sampler::gap_at() exactly, and TestTheGoReplicaMatchesTheShimSampler
// pins it to the numbers shim/core/sampler_test.cc's schedule pin asserts on
// the C++ side, so the two cannot drift apart quietly.
//
// What this replays is the chain of sample POINTS, which is thread-
// independent, so SampledCount is exact for any launch stream. The launch
// ordinal that actually carries each stack can be later than its chain point
// when launches arrive concurrently on several threads (never earlier) --
// see the concurrency note in shim/core/sampler.h. For a single-threaded
// launch stream the two coincide exactly.
//
// None of this is on the decode path. It is for tools that need the exact
// expected sampled count before a run (cmd/gpu-cuda-profile) and for anyone
// auditing a profile's sampled population after one.

// DefaultSampleSeed mirrors perfagent::Sampler::kDefaultSeed. The shim's seed
// is a fixed constant so two runs of the same workload sample identically;
// PERFAGENT_GPU_SAMPLE_SEED overrides it in the CUPTI adapter.
const DefaultSampleSeed uint64 = 0x9E3779B97F4A7C15

// SampleGap returns the number of launches from the sample point at seq to
// the next one. Uniform over the 2k+1 integers centred on period (k =
// period/2), so the mean gap is exactly period and the long-run rate is
// exactly 1/period.
func SampleGap(seq uint64, period uint32, seed uint64) uint64 {
	if period <= 1 {
		return 1
	}
	k := uint64(period / 2)
	span := 2*k + 1
	r := sampleMix(seed ^ (seq * 0x9E3779B97F4A7C15))
	// Lemire's multiply-shift, matching the C++ side bit for bit: the top 32
	// bits of the hash scaled into [0, span).
	off := ((r >> 32) * span) >> 32
	return uint64(period) - k + off
}

// SampleSchedule returns the launch ordinals the shim samples over the first
// `launches` launches, in order. Ordinal 0 is always sampled.
func SampleSchedule(launches uint64, period uint32, seed uint64) []uint64 {
	if period == 0 {
		period = 1 // the shim coerces 0 to 1 rather than dividing by zero
	}
	out := make([]uint64, 0, launches/uint64(period)+2)
	for seq := uint64(0); seq < launches; seq += SampleGap(seq, period, seed) {
		out = append(out, seq)
	}
	return out
}

// SampledCount is the exact number of launches that carry a CPU stack over
// `launches` launches. Exact, not expected: the schedule is deterministic
// given the seed, which is what keeps the phase gate's assertion an equality
// rather than a tolerance band.
func SampledCount(launches uint64, period uint32, seed uint64) uint64 {
	if period == 0 {
		period = 1
	}
	var n uint64
	for seq := uint64(0); seq < launches; seq += SampleGap(seq, period, seed) {
		n++
	}
	return n
}

// sampleMix is splitmix64's finalizer, mirroring Sampler::mix().
func sampleMix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x
}
