package gpu

import (
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Tier selection: one setting, three values, and why "both" is an error.
//
// GPU PC sampling has two collection tiers and they cannot run at the same
// time. The reason matters more than the rule, because a reader who knows only
// the rule will eventually try to route around it:
//
// CUPTI's COLLECTION_MODE is a single per-CUcontext attribute. A process could
// in principle set KERNEL_SERIALIZED on one context and CONTINUOUS on another
// -- nothing in CUPTI forbids it. What forbids it is that WHICH CONTEXT A
// GIVEN KERNEL LANDS ON IS THE APPLICATION'S CHOICE, NOT THE PROFILER'S. A
// "both" mode would therefore produce one profile in which some kernels carry
// exact launch attribution and perturbed durations while others carry inferred
// attribution and honest durations, split along an axis the operator can
// neither see nor control. That is worse than either tier alone: a profile
// whose trustworthiness varies invisibly is not a profile, it is a hazard.
//
// So the selection is PROCESS-WIDE AND EXCLUSIVE. Naming two tiers is a
// startup error, logged loudly, and never resolved by a silent pick -- not
// "last one wins", not "the safer one wins". Both of those are decisions the
// operator did not make and cannot see in the output.
//
// Switching tiers mid-run is out of scope. It is gated on the Task 10 hardware
// question of whether COLLECTION_MODE can be changed between
// cuptiPCSamplingStop and cuptiPCSamplingStart without a full Disable/Enable,
// which cupti_pcsampling.h does not answer and no machine here can.
// ---------------------------------------------------------------------------

// PCSamplingEnvVar is the one variable that carries the selected tier to the
// producer. The CUPTI adapter (shim/nvidia/cupti_adapter.cc) and the GPU-free
// stub (shim/stub/stub.cc) both read it through shim/core/pctier.h, so the two
// producers cannot drift apart on what "off" means.
//
// It is set EXPLICITLY on the producer's environment for every tier including
// off, never left to be inherited: an operator who exported
// PERFAGENT_GPU_PC_SAMPLING=serialized in their shell last week must not have
// it leak into a run this agent believes is off.
const PCSamplingEnvVar = "PERFAGENT_GPU_PC_SAMPLING"

// PCSamplingTier is the selected PC-sampling collection tier.
//
// The zero value is PCSamplingOff, and that is load-bearing in the same way
// SerializationUnknown's zero value is: a config nobody filled in, a struct a
// test built by hand, or a field lost in a copy all land on "off". Turning PC
// sampling ON has to be written deliberately somewhere.
type PCSamplingTier uint8

const (
	// PCSamplingOff is the default. Off means OFF: the producer makes no
	// PC-sampling call at all, allocates no PC buffers, enables no extra
	// CUPTI domain and fires no PC-sampling probe. It is not "enabled but
	// idle" and not "enabled at a low rate".
	PCSamplingOff PCSamplingTier = iota

	// PCSamplingContinuous is Tier B: CUPTI_PC_SAMPLING_COLLECTION_MODE_
	// CONTINUOUS. Kernels are not serialized, so this is the only tier that
	// is a candidate for always-on profiling. The price is that
	// correlationId is zero on every PC record, so a sample joins to its
	// kernel through the module and never to the launch that issued it.
	PCSamplingContinuous

	// PCSamplingSerialized is Tier A: CUPTI_PC_SAMPLING_COLLECTION_MODE_
	// KERNEL_SERIALIZED, duty-cycled in bursts. CUPTI populates
	// correlationId on every record, so a sample joins to a launch -- and
	// therefore to a CPU stack -- exactly. The price is that every kernel
	// that runs while a burst is open RUNS SERIALIZED, which perturbs the
	// very durations the profile reports, and perturbs the CPU and off-CPU
	// profiles taken alongside it with no marking there at all.
	//
	// It is refused unless the operator acknowledges that explicitly. See
	// PCSamplingRequest.Select and PCSamplingStandingWarning.
	PCSamplingSerialized
)

// The three spellings, and the only three. They are exported because they are
// what an operator types and what a --help string must quote; a driver that
// spelled a fourth one would be inventing a tier.
const (
	PCSamplingNameOff        = "off"
	PCSamplingNameContinuous = "continuous"
	PCSamplingNameSerialized = "serialized"
)

// PCSamplingTierNames lists the three legal values in the order a help string
// should show them: the default first, then increasing cost to the workload.
var PCSamplingTierNames = []string{
	PCSamplingNameOff, PCSamplingNameContinuous, PCSamplingNameSerialized,
}

// The errors. They are sentinels rather than strings so a driver can tell a
// misconfiguration (exit with usage) from a refusal it must explain
// (ErrPCSamplingNotAcknowledged has an instruction attached), and so tests
// assert on the CONDITION rather than on wording that will be edited.
var (
	// ErrPCSamplingUnknownTier: the value is not one of the three.
	ErrPCSamplingUnknownTier = errors.New("unknown GPU PC-sampling tier")

	// ErrPCSamplingTiersExclusive: more than one tier was named, either in
	// one value or across the flag and the environment. This is the
	// process-wide exclusivity rule; see the file header for why it is a
	// startup error and not a pick.
	ErrPCSamplingTiersExclusive = errors.New("GPU PC-sampling tiers are mutually exclusive")

	// ErrPCSamplingNotAcknowledged: "serialized" was selected without the
	// operator acknowledging that it perturbs the workload. Tier A is a
	// destructive flag in the ordinary sense -- it changes the thing being
	// measured -- so it takes the same shape as one.
	ErrPCSamplingNotAcknowledged = errors.New("GPU PC-sampling tier \"serialized\" perturbs the workload and was not acknowledged")
)

// String renders the tier as the value an operator types. An out-of-range
// value renders as an explicit "invalid(N)" rather than as "off": a tier that
// fell out of a bad conversion must not read as the safe default, because
// "off" is exactly the answer nobody would investigate.
func (t PCSamplingTier) String() string {
	switch t {
	case PCSamplingOff:
		return PCSamplingNameOff
	case PCSamplingContinuous:
		return PCSamplingNameContinuous
	case PCSamplingSerialized:
		return PCSamplingNameSerialized
	default:
		return fmt.Sprintf("invalid(%d)", uint8(t))
	}
}

// Valid reports whether t is one of the three defined tiers.
func (t PCSamplingTier) Valid() bool {
	return t <= PCSamplingSerialized
}

// EnvValue is what goes into PCSamplingEnvVar on the producer's environment.
//
// The NAME, not a number. Both producers accept the numeric spellings too (an
// operator's exported PERFAGENT_GPU_PC_SAMPLING=2 still works), but what this
// agent writes is legible in `ps eauxwww` and in a container spec, where "2"
// is a value somebody has to go look up and may guess wrong about.
func (t PCSamplingTier) EnvValue() string { return t.String() }

// MarshalText makes a serialized Snapshot say "serialized" rather than "2".
// The Snapshot is an operator-facing artifact; a numeric tier in it would be
// one more thing to decode correctly under time pressure.
func (t PCSamplingTier) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// UnmarshalText round-trips MarshalText, and rejects what ParsePCSamplingTier
// rejects. A JSON document naming two tiers fails to decode rather than
// decoding to one of them.
func (t *PCSamplingTier) UnmarshalText(b []byte) error {
	parsed, err := ParsePCSamplingTier(string(b))
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// pcSamplingTierByName maps one token to a tier. The numeric spellings exist
// because PCSamplingEnvVar is a plain environment variable that operators and
// container specs have already been setting as 0/1/2, and silently ignoring
// those would turn a configured Tier A run into a quiet Tier-off one.
func pcSamplingTierByName(tok string) (PCSamplingTier, bool) {
	switch tok {
	case PCSamplingNameOff, "0":
		return PCSamplingOff, true
	case PCSamplingNameContinuous, "1":
		return PCSamplingContinuous, true
	case PCSamplingNameSerialized, "2":
		return PCSamplingSerialized, true
	default:
		return PCSamplingOff, false
	}
}

// ParsePCSamplingTier turns one setting's text into a tier.
//
// The empty string is PCSamplingOff: an unset environment variable and an
// unspecified flag both mean "the default", and the default is off.
//
// A value naming MORE THAN ONE tier is the exclusivity error, not a pick. It
// is parsed rather than rejected as a syntax error on purpose: "both" has to
// be EXPRESSIBLE for the refusal to be reachable, and a parser that quietly
// took the first token of "continuous,serialized" would be the silent pick
// this rule exists to prevent.
func ParsePCSamplingTier(value string) (PCSamplingTier, error) {
	toks := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '+' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(toks) == 0 {
		return PCSamplingOff, nil
	}

	var (
		named []string
		set   []PCSamplingTier
	)
	for _, tok := range toks {
		tier, ok := pcSamplingTierByName(strings.ToLower(tok))
		if !ok {
			return PCSamplingOff, fmt.Errorf("%w: %q is not a tier; the three values are %s",
				ErrPCSamplingUnknownTier, tok, strings.Join(PCSamplingTierNames, ", "))
		}
		dup := false
		for _, have := range set {
			if have == tier {
				dup = true
				break
			}
		}
		if !dup {
			set = append(set, tier)
			named = append(named, tier.String())
		}
	}
	if len(set) > 1 {
		return PCSamplingOff, fmt.Errorf("%w: %q names %s. "+
			"COLLECTION_MODE is a single per-CUcontext CUPTI attribute, and which context a "+
			"kernel lands on is the application's choice rather than the profiler's — so a "+
			"\"both\" mode would produce one profile whose attribution quality varied along an "+
			"axis the operator can neither see nor control. Name exactly one of %s",
			ErrPCSamplingTiersExclusive, value, strings.Join(named, " and "),
			strings.Join(PCSamplingTierNames, ", "))
	}
	return set[0], nil
}

// PCSamplingRequest is what the operator expressed, before it is a tier: the
// two places a tier can come from, plus the acknowledgement that Tier A
// requires.
//
// Two sources, because there genuinely are two. A driver takes a flag, and the
// producer's environment may already carry PCSamplingEnvVar from a shell
// export or a container spec. Resolving a disagreement between them by
// precedence would be a silent pick of exactly the kind the exclusivity rule
// forbids, so a disagreement is an error and agreement is not.
type PCSamplingRequest struct {
	// Flag is the driver's --gpu-pc-sampling value. Empty means the flag was
	// not given, which is different from "off" being given: unspecified
	// defers to Env, whereas an explicit "off" that contradicts Env is a
	// disagreement and is refused.
	Flag string

	// Env is the inherited value of PCSamplingEnvVar, as read from the
	// agent's own environment (os.Getenv). Empty means unset.
	Env string

	// AcknowledgePerturbation is the operator's explicit acknowledgement
	// that Tier A perturbs the workload it measures. It gates nothing else:
	// off and continuous do not need it and are not affected by it.
	AcknowledgePerturbation bool
}

// Select resolves the request to a tier, or refuses.
//
// It refuses in exactly three ways, and every one of them is a startup error
// rather than a downgrade:
//
//   - an unknown value in either source;
//   - both sources naming different tiers, or one source naming two;
//   - "serialized" without AcknowledgePerturbation.
//
// On any error the returned tier is PCSamplingOff, so a caller that logs and
// exits and a caller that logs and continues both end up not sampling, rather
// than one of them ending up in a tier nobody chose.
func (r PCSamplingRequest) Select() (PCSamplingTier, error) {
	fromFlag, err := ParsePCSamplingTier(r.Flag)
	if err != nil {
		return PCSamplingOff, fmt.Errorf("--gpu-pc-sampling: %w", err)
	}
	fromEnv, err := ParsePCSamplingTier(r.Env)
	if err != nil {
		return PCSamplingOff, fmt.Errorf("%s: %w", PCSamplingEnvVar, err)
	}

	tier := fromEnv
	switch {
	case r.Flag != "" && r.Env != "" && fromFlag != fromEnv:
		return PCSamplingOff, fmt.Errorf("%w: --gpu-pc-sampling=%s and %s=%s name different "+
			"tiers. The selection is process-wide and there is no precedence between them: "+
			"picking one silently would leave the profile's attribution quality decided by "+
			"which source the operator forgot about. Unset one of them",
			ErrPCSamplingTiersExclusive, fromFlag, PCSamplingEnvVar, fromEnv)
	case r.Flag != "":
		tier = fromFlag
	}

	if tier == PCSamplingSerialized && !r.AcknowledgePerturbation {
		return PCSamplingOff, fmt.Errorf("%w. Tier A serializes GPU kernels in bursts: it "+
			"inflates the kernel durations this profile reports, it distorts any CPU and "+
			"off-CPU profile taken alongside it with no marking in those profiles at all, and "+
			"it is unavailable where CUDA graphs are in use. Re-run with "+
			"--gpu-pc-sampling-acknowledge-perturbation if that is what you want",
			ErrPCSamplingNotAcknowledged)
	}
	return tier, nil
}

// PCSamplingWarningPrefix marks the standing Tier A warning. It is distinct
// from joinAnomalyPrefix because the two say different things: an anomaly is
// something that went wrong, while this is something the operator asked for
// and must keep in mind while reading the result.
const PCSamplingWarningPrefix = "gpu pc sampling WARNING"

// PCSamplingStandingWarning is the whole-run disclosure for Tier A, and it is
// nil for every other tier.
//
// It STANDS. JoinHealthWith emits it on every render, not once at startup,
// because a warning printed before a sixty-second profile is a warning that
// has scrolled off by the time anyone reads the profile it applies to.
//
// It names ALL THREE perturbations, and that is the requirement rather than a
// stylistic preference. An operator told only about the first will read the
// other two backwards: they will see gpu_serialized="true" on the GPU samples,
// conclude that the marked ones are the perturbed ones, and then trust an
// off-CPU profile whose synchronization waits are inflated by exactly this
// mechanism and carry no marking at all. A limitation an operator is told
// about is a limitation; one they discover from a misleading profile is a
// defect.
func PCSamplingStandingWarning(tier PCSamplingTier) []string {
	if tier != PCSamplingSerialized {
		return nil
	}
	p := PCSamplingWarningPrefix + ": "
	return []string{
		p + "Tier A (\"serialized\", CUPTI KERNEL_SERIALIZED) PC sampling was selected for this " +
			"run — the profiler deliberately perturbs the workload it is measuring. This warning " +
			"stands for the whole run, not just at startup. It names three distinct " +
			"perturbations, because an operator told only about the first will misread the " +
			"other two.",
		p + "(1) GPU KERNEL DURATIONS INSIDE A BURST ARE INFLATED by serialization — kernels " +
			"that would have overlapped ran one at a time. Those executions are marked " +
			"gpu_serialized=\"true\"; executions that cannot be shown to have run outside every " +
			"burst are marked \"unknown\" and must never be read as \"false\".",
		p + "(2) CPU AND OFF-CPU SAMPLES TAKEN DURING A BURST ARE DISTORTED AND CARRY NO MARKING " +
			"AT ALL. gpu_serialized reaches only the GPU projection (ProjectExecutions); the " +
			"on-CPU and off-CPU profilers are a separate path with no window awareness, and " +
			"serialization inflates precisely the synchronization wait that off-CPU profiling " +
			"exists to measure. cudaDeviceSynchronize-shaped off-CPU time will look worse than " +
			"it is, with nothing in that profile saying why.",
		p + "(3) TIER A IS UNAVAILABLE WHERE CUDA GRAPHS ARE IN USE. A graph launch fires one " +
			"runtime callback for N kernels, so Tier A's exact-launch attribution would be false " +
			"while still looking exact; the producer refuses to open bursts in such a process " +
			"rather than downgrading silently to Tier B, and this profile then carries no Tier A " +
			"PC samples at all.",
	}
}
