package gpu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------- the values

// The three values, in every spelling the setting accepts. The numerals are
// part of the contract, not legacy tolerated by accident:
// PERFAGENT_GPU_PC_SAMPLING has been 0/1/2 since Tier B shipped and container
// specs are already set that way, so a parser that ignored them would turn a
// configured Tier A run into a silent off one.
func TestParsePCSamplingTierAcceptsExactlyThreeValues(t *testing.T) {
	for _, c := range []struct {
		in   string
		want PCSamplingTier
	}{
		{"off", PCSamplingOff},
		{"0", PCSamplingOff},
		{"", PCSamplingOff},
		{"   ", PCSamplingOff},
		{"continuous", PCSamplingContinuous},
		{"1", PCSamplingContinuous},
		{"serialized", PCSamplingSerialized},
		{"2", PCSamplingSerialized},
		// Case and stray whitespace are an operator's typing, not a different
		// setting.
		{"SERIALIZED", PCSamplingSerialized},
		{"  Continuous  ", PCSamplingContinuous},
		// Naming one tier twice is redundant, not contradictory.
		{"continuous,continuous", PCSamplingContinuous},
	} {
		t.Run(strings.TrimSpace(c.in), func(t *testing.T) {
			got, err := ParsePCSamplingTier(c.in)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// The zero value is off. This is the same discipline SerializationUnknown's
// zero value carries and it exists for the same reason: a config nobody
// filled in, a struct a test built by hand and a field lost in a copy must all
// land on the tier that does not touch the workload. Turning PC sampling on
// has to be written somewhere, deliberately.
func TestPCSamplingTierZeroValueIsOff(t *testing.T) {
	var t0 PCSamplingTier
	assert.Equal(t, PCSamplingOff, t0)
	assert.Equal(t, "off", t0.String())
	assert.Equal(t, "off", t0.EnvValue())

	var cfg TimelineConfig
	assert.Equal(t, PCSamplingOff, cfg.PCSampling)
}

// An out-of-range tier must not render as "off". A value that fell out of a
// bad conversion reading as the safe default is the one answer nobody would
// investigate.
func TestPCSamplingTierOutOfRangeDoesNotReadAsOff(t *testing.T) {
	bogus := PCSamplingTier(9)
	assert.False(t, bogus.Valid())
	assert.Equal(t, "invalid(9)", bogus.String())
	assert.NotEqual(t, "off", bogus.String())
}

// ------------------------------------------------------------- the refusals

// The unknown-value error. Every near-miss an operator actually types is here
// rather than one synthetic "xyzzy", because "on" and "true" are what somebody
// reaches for when they remember this as a boolean and "serialised" is what a
// British operator types.
func TestParsePCSamplingTierRefusesUnknownValues(t *testing.T) {
	for _, in := range []string{
		"nonsense", "3", "true", "on", "yes", "enabled", "serialised", "tier-a",
		// A good token beside a bad one is still a refusal, never the good
		// token: the operator wrote something they did not mean, and running
		// half of it is running something they did not ask for.
		"continuous,nonsense", "nonsense,continuous",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := ParsePCSamplingTier(in)
			require.ErrorIs(t, err, ErrPCSamplingUnknownTier)
			assert.Equal(t, PCSamplingOff, got,
				"a refused setting must fall closed to off, never to a tier nobody chose")
			// The error names the three legal values, because a refusal that
			// does not say what IS legal makes the operator guess.
			for _, name := range PCSamplingTierNames {
				assert.Contains(t, err.Error(), name)
			}
		})
	}
}

// THE BOTH-SET ERROR, in one setting.
//
// Both orderings are asserted, and that is the assertion rather than a
// courtesy: a parser that quietly took the first token would answer
// "continuous" for one of these and "serialized" for the other — a silent pick
// that looks correct in half the runs and perturbs the workload in the other
// half, with nothing in the profile saying which happened.
func TestParsePCSamplingTierRefusesBothTiersInOneValue(t *testing.T) {
	for _, in := range []string{
		"continuous,serialized", "serialized,continuous", "1+2", "2 1",
		"continuous serialized", "off,serialized",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := ParsePCSamplingTier(in)
			require.ErrorIs(t, err, ErrPCSamplingTiersExclusive)
			assert.Equal(t, PCSamplingOff, got)
			// The REASON, not just the rule. A reader who knows only the rule
			// will eventually try to route around it — one context per tier
			// is the obvious idea and CUPTI does not forbid it.
			assert.Contains(t, err.Error(), "COLLECTION_MODE")
			assert.Contains(t, err.Error(), "application's choice")
		})
	}
}

// THE BOTH-SET ERROR, across the two sources. There genuinely are two: a
// driver takes a flag, and the producer's environment may already carry
// PERFAGENT_GPU_PC_SAMPLING from a shell export or a container spec.
// Resolving that by precedence would leave the profile's attribution quality
// decided by whichever source the operator forgot about.
func TestSelectRefusesWhenTheFlagAndTheEnvironmentDisagree(t *testing.T) {
	for _, c := range []struct{ flag, env string }{
		{"continuous", "serialized"},
		{"serialized", "continuous"},
		{"off", "serialized"},
		{"continuous", "0"},
	} {
		t.Run(c.flag+"/"+c.env, func(t *testing.T) {
			got, err := PCSamplingRequest{
				Flag: c.flag, Env: c.env, AcknowledgePerturbation: true,
			}.Select()
			require.ErrorIs(t, err, ErrPCSamplingTiersExclusive)
			assert.Equal(t, PCSamplingOff, got)
			assert.Contains(t, err.Error(), PCSamplingEnvVar)
			assert.Contains(t, err.Error(), "--gpu-pc-sampling")
		})
	}
}

// Agreement is not a disagreement, and an unspecified flag is not an "off"
// that contradicts the environment. Without this the exclusivity rule would
// make an exported PERFAGENT_GPU_PC_SAMPLING unusable with any driver that
// has the flag.
func TestSelectResolvesTheSourcesWhenTheyDoNotDisagree(t *testing.T) {
	for _, c := range []struct {
		name, flag, env string
		want            PCSamplingTier
	}{
		{"neither", "", "", PCSamplingOff},
		{"flag only", "continuous", "", PCSamplingContinuous},
		{"env only", "", "continuous", PCSamplingContinuous},
		{"both, same tier", "continuous", "continuous", PCSamplingContinuous},
		{"both, same tier, different spelling", "serialized", "2", PCSamplingSerialized},
		{"flag defers to env", "", "serialized", PCSamplingSerialized},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := PCSamplingRequest{
				Flag: c.flag, Env: c.env, AcknowledgePerturbation: true,
			}.Select()
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// An unknown value is attributed to the source that carried it, in both
// directions. An operator staring at a correct flag needs to be told the
// environment is what is wrong.
func TestSelectNamesTheSourceOfAnUnreadableValue(t *testing.T) {
	_, err := PCSamplingRequest{Flag: "nonsense"}.Select()
	require.ErrorIs(t, err, ErrPCSamplingUnknownTier)
	assert.Contains(t, err.Error(), "--gpu-pc-sampling")

	_, err = PCSamplingRequest{Env: "nonsense"}.Select()
	require.ErrorIs(t, err, ErrPCSamplingUnknownTier)
	assert.Contains(t, err.Error(), PCSamplingEnvVar)
}

// --------------------------------------------------- the acknowledgement gate

// Tier A is a destructive flag in the ordinary sense — it changes the thing
// being measured — so it takes the same shape as one: refused unless the
// operator said so deliberately, and the refusal explains what they would be
// consenting to rather than just naming a flag.
func TestSerializedIsRefusedWithoutAnExplicitAcknowledgement(t *testing.T) {
	for _, source := range []string{"flag", "env"} {
		t.Run(source, func(t *testing.T) {
			req := PCSamplingRequest{}
			if source == "flag" {
				req.Flag = "serialized"
			} else {
				req.Env = "serialized"
			}
			got, err := req.Select()
			require.ErrorIs(t, err, ErrPCSamplingNotAcknowledged)
			assert.Equal(t, PCSamplingOff, got,
				"an unacknowledged Tier A must not run; it must not run as Tier B either")

			// All three perturbations are named in the refusal itself, not
			// only in the standing warning: this text is the last thing an
			// operator sees before they decide whether to pass the flag.
			assert.Contains(t, err.Error(), "inflates the kernel durations")
			assert.Contains(t, err.Error(), "off-CPU profile taken alongside it with no marking")
			assert.Contains(t, err.Error(), "CUDA graphs")
			assert.Contains(t, err.Error(), "--gpu-pc-sampling-acknowledge-perturbation")

			req.AcknowledgePerturbation = true
			got, err = req.Select()
			require.NoError(t, err)
			assert.Equal(t, PCSamplingSerialized, got)
		})
	}
}

// The acknowledgement gates Tier A and nothing else. An operator who leaves it
// set in a script must not thereby change what "off" or "continuous" does.
func TestTheAcknowledgementGatesOnlySerialized(t *testing.T) {
	for _, tier := range []string{"off", "continuous"} {
		for _, ack := range []bool{false, true} {
			got, err := PCSamplingRequest{Flag: tier, AcknowledgePerturbation: ack}.Select()
			require.NoError(t, err)
			assert.Equal(t, tier, got.String())
		}
	}
}

// ------------------------------------------------------ "off" means OFF

// The producer's environment for an off run names off EXPLICITLY.
//
// This is the agent half of the "off means off" assertion; the wire half is
// shim/stub/pc_tier_test.cc, which traps the four PC-sampling probe sites with
// int3 and requires that not one of them fires with the tier off while the
// launch and exec probes still do. Here the claim is narrower and still
// load-bearing: whatever an operator exported into their shell last week must
// not reach the producer of a run this agent believes is off.
func TestOffIsHandedToTheProducerExplicitly(t *testing.T) {
	assert.Equal(t, "off", PCSamplingOff.EnvValue())
	assert.Equal(t, "continuous", PCSamplingContinuous.EnvValue())
	assert.Equal(t, "serialized", PCSamplingSerialized.EnvValue())

	// The value a driver writes is one the producer's own parser reads back as
	// the same tier. A driver that wrote "0" and a producer that read names
	// only would silently disagree about the safest possible setting.
	for _, tier := range []PCSamplingTier{PCSamplingOff, PCSamplingContinuous, PCSamplingSerialized} {
		back, err := ParsePCSamplingTier(tier.EnvValue())
		require.NoError(t, err)
		assert.Equal(t, tier, back)
	}
}

// With the tier off or continuous nothing is ever serialized, so every
// execution is "false" unconditionally and the window store is not consulted
// at all — even when a producer emits windows anyway (a leftover injection, a
// system-wide attach). Only PCSamplingSerialized routes through the evidence.
func TestOnlySerializedConsultsTheWindowStore(t *testing.T) {
	for _, tier := range []PCSamplingTier{PCSamplingOff, PCSamplingContinuous} {
		t.Run(tier.String(), func(t *testing.T) {
			tl := NewTimeline(TimelineConfig{PCSampling: tier})
			emitBurst(t, tl, uint64(tierAPID), 1000, 9000)
			require.NoError(t, tl.EmitExec(serializedExec("a", 2000, 3000)))

			snap := tl.Snapshot()
			require.Len(t, snap.Executions, 1)
			assert.Equal(t, tier, snap.PCSampling)
			// Ingested and counted — the two ends must not go silently out of
			// step — but the ANSWER is unconditional.
			assert.Equal(t, uint64(2), snap.SamplingWindowsReceived)
			assert.Equal(t, uint64(1), snap.ExecutionsNotSerialized)
			assert.Zero(t, snap.ExecutionsSerialized)
			assert.Zero(t, snap.ExecutionsSerializationUnknown)
			assert.Equal(t, []string{"false"}, states(snap))
			assertSumIdentity(t, snap)
		})
	}
}

// The tier rides on the Snapshot rather than being inferred from the window
// counters, and this is the case an inference gets backwards: Tier A selected,
// not one window received. Inferring "no windows, so nothing was serialized"
// would mark a wholly perturbed run "false".
func TestTheSnapshotCarriesTheTierEvenWhenNoWindowArrived(t *testing.T) {
	tl := tierATimeline()
	require.NoError(t, tl.EmitExec(serializedExec("a", 2000, 3000)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Equal(t, PCSamplingSerialized, snap.PCSampling)
	assert.Zero(t, snap.SamplingWindowsReceived)
	assert.Equal(t, []string{"unknown"}, states(snap))
	assert.Equal(t, uint64(1), snap.ExecutionsSerializationUnknown)
	assert.Zero(t, snap.ExecutionsNotSerialized,
		"Tier A with no evidence is \"unknown\", never \"false\"")
	assertSumIdentity(t, snap)
}

// The Snapshot is an operator-facing artifact, so a serialized one says
// "serialized" rather than "2", and a document naming two tiers fails to
// decode rather than decoding to one of them.
func TestTheTierRoundTripsThroughJSONByName(t *testing.T) {
	b, err := json.Marshal(Snapshot{PCSampling: PCSamplingSerialized})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"pc_sampling":"serialized"`)

	var back Snapshot
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, PCSamplingSerialized, back.PCSampling)

	var bad PCSamplingTier
	require.Error(t, bad.UnmarshalText([]byte("continuous,serialized")))
	require.Error(t, bad.UnmarshalText([]byte("nonsense")))
}

// -------------------------------------------------------- the standing warning

// The warning exists for Tier A and for nothing else. A warning printed on an
// off or continuous run is a warning readers learn to skip, and then it is
// skipped on the run that mattered.
func TestTheStandingWarningIsTierAOnly(t *testing.T) {
	assert.Nil(t, PCSamplingStandingWarning(PCSamplingOff))
	assert.Nil(t, PCSamplingStandingWarning(PCSamplingContinuous))
	assert.NotEmpty(t, PCSamplingStandingWarning(PCSamplingSerialized))
}

// ALL THREE perturbations, named. This is the requirement rather than a
// stylistic preference: an operator told only about the first will see
// gpu_serialized="true" on the GPU samples, conclude that the marked ones are
// the perturbed ones, and then trust an off-CPU profile whose synchronization
// waits are inflated by exactly this mechanism and carry no marking at all.
func TestTheStandingWarningNamesAllThreePerturbations(t *testing.T) {
	lines := PCSamplingStandingWarning(PCSamplingSerialized)
	require.NotEmpty(t, lines)
	for _, l := range lines {
		assert.True(t, strings.HasPrefix(l, PCSamplingWarningPrefix+": "), "line %q", l)
	}
	joined := strings.Join(lines, "\n")

	// 1. GPU kernel durations, and the label that marks them.
	assert.Contains(t, joined, "GPU KERNEL DURATIONS INSIDE A BURST ARE INFLATED")
	assert.Contains(t, joined, `gpu_serialized="true"`)
	assert.Contains(t, joined, `must never be read as "false"`)

	// 2. The one an operator will otherwise get backwards: the CPU and
	// off-CPU profiles are distorted and carry NO marking.
	assert.Contains(t, joined,
		"CPU AND OFF-CPU SAMPLES TAKEN DURING A BURST ARE DISTORTED AND CARRY NO MARKING AT ALL")
	assert.Contains(t, joined, "ProjectExecutions")
	assert.Contains(t, joined, "off-CPU profiling exists to measure")

	// 3. CUDA graphs, where the tier is unavailable rather than degraded.
	assert.Contains(t, joined, "TIER A IS UNAVAILABLE WHERE CUDA GRAPHS ARE IN USE")
	assert.Contains(t, joined, "downgrading silently to Tier B")

	// And that it says it stands, so a reader does not take it for a startup
	// banner that has since stopped applying.
	assert.Contains(t, joined, "stands for the whole run")
}

// TestPCSamplingStandingWarningRenderedOutput prints the warning verbatim so
// the text an operator will actually read can be reviewed with
// `go test -run TestPCSamplingStandingWarningRenderedOutput -v ./gpu/`.
func TestPCSamplingStandingWarningRenderedOutput(t *testing.T) {
	t.Log("\n" + strings.Join(PCSamplingStandingWarning(PCSamplingSerialized), "\n"))
}

// ------------------------------------------------------- the two ends agree

// The agent writes PERFAGENT_GPU_PC_SAMPLING and the producer reads it, from
// two parsers in two languages. A spelling that only one of them knows is a
// setting that silently does nothing — which for "serialized" means the
// operator gets an unperturbed profile they will read as a perturbed one, and
// for "off" would mean the opposite.
func TestTheShimAndTheAgentAgreeOnTheTierSpellings(t *testing.T) {
	header, err := os.ReadFile(filepath.Join("..", "shim", "core", "pctier.h"))
	require.NoError(t, err, "shim/core/pctier.h is the producer half of this setting")
	src := string(header)

	for name, tier := range map[string]string{
		PCSamplingNameOff:        "kOff",
		PCSamplingNameContinuous: "kContinuous",
		PCSamplingNameSerialized: "kSerialized",
	} {
		assert.Contains(t, src, `{"`+name+`", PCSamplingTier::`+tier+`}`,
			"the producer must accept the name the agent writes")
	}
	for numeral, want := range map[string]PCSamplingTier{
		"0": PCSamplingOff, "1": PCSamplingContinuous, "2": PCSamplingSerialized,
	} {
		assert.Contains(t, src, `{"`+numeral+`", PCSamplingTier::k`+
			strings.ToUpper(want.String()[:1])+want.String()[1:]+`}`,
			"the producer must accept the numeral an existing container spec carries")
		parsed, err := ParsePCSamplingTier(numeral)
		require.NoError(t, err)
		assert.Equal(t, want, parsed,
			"the agent must read back the numeral the producer accepts")
	}
}
