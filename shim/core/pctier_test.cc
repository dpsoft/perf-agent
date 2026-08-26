// core/pctier.h: the producer half of tier selection.
//
// The parser is the one place where an operator's text becomes a decision that
// perturbs — or does not perturb — somebody else's production workload, so it
// is tested for what it REFUSES at least as hard as for what it accepts. Every
// refusal must land on kOff: a setting that cannot be read is never resolved
// to a guess, because "the cheaper tier" and "the first token" are both
// decisions the operator did not make and cannot see in the output.
//
// The agent's half is gpu/tier.go, which writes the variable this parses. The
// spelling table here and the one there are asserted to agree by
// TestTheShimAndTheAgentAgreeOnTheTierSpellings in gpu/tier_test.go, which
// reads this header.
#include "pctier.h"

#include <cassert>
#include <cstdio>
#include <cstring>

using perfagent::PCSamplingTier;
using perfagent::PCTierParse;

static int g_bad;

static void want(const char *value, PCTierParse expect_rc, PCSamplingTier expect_tier) {
    PCSamplingTier got = PCSamplingTier::kSerialized;   // poisoned, not kOff
    char bad[96];
    const PCTierParse rc = perfagent::pc_tier_parse(value, &got, bad, sizeof(bad));
    if (rc != expect_rc || got != expect_tier) {
        fprintf(stderr, "pctier_test: %-28s -> rc=%d tier=%s, want rc=%d tier=%s\n",
                value ? value : "<null>", (int)rc, perfagent::pc_tier_name(got),
                (int)expect_rc, perfagent::pc_tier_name(expect_tier));
        g_bad = 1;
    }
}

int main() {
    // The three values, in both spellings the ABI has ever used. The numerals
    // are not legacy debt to be dropped: PERFAGENT_GPU_PC_SAMPLING has been
    // 0/1/2 since Task 6 and container specs are already set that way, so
    // ignoring them would turn a configured Tier A run into a silent off one.
    want("off", PCTierParse::kOK, PCSamplingTier::kOff);
    want("0", PCTierParse::kOK, PCSamplingTier::kOff);
    want("continuous", PCTierParse::kOK, PCSamplingTier::kContinuous);
    want("1", PCTierParse::kOK, PCSamplingTier::kContinuous);
    want("serialized", PCTierParse::kOK, PCSamplingTier::kSerialized);
    want("2", PCTierParse::kOK, PCSamplingTier::kSerialized);

    // Case and surrounding whitespace are an operator's typing, not a
    // different setting.
    want("SERIALIZED", PCTierParse::kOK, PCSamplingTier::kSerialized);
    want("  Continuous  ", PCTierParse::kOK, PCSamplingTier::kContinuous);

    // Unset and empty are the default, and the default is off.
    want(nullptr, PCTierParse::kOK, PCSamplingTier::kOff);
    want("", PCTierParse::kOK, PCSamplingTier::kOff);
    want("   ", PCTierParse::kOK, PCSamplingTier::kOff);

    // Naming one tier twice is redundant, not contradictory.
    want("continuous,continuous", PCTierParse::kOK, PCSamplingTier::kContinuous);
    want("2 2", PCTierParse::kOK, PCSamplingTier::kSerialized);

    // BOTH TIERS. The rule this file exists for. Note the second ordering:
    // a parser that took the first token would answer "continuous" for one of
    // these and "serialized" for the other, which is the shape of a silent
    // pick that looks correct in half the runs.
    want("continuous,serialized", PCTierParse::kNotExclusive, PCSamplingTier::kOff);
    want("serialized,continuous", PCTierParse::kNotExclusive, PCSamplingTier::kOff);
    want("1+2", PCTierParse::kNotExclusive, PCSamplingTier::kOff);
    want("serialized off", PCTierParse::kNotExclusive, PCSamplingTier::kOff);

    // Unknown, including the near-misses an operator actually types.
    want("nonsense", PCTierParse::kUnknown, PCSamplingTier::kOff);
    want("3", PCTierParse::kUnknown, PCSamplingTier::kOff);
    want("true", PCTierParse::kUnknown, PCSamplingTier::kOff);
    want("on", PCTierParse::kUnknown, PCSamplingTier::kOff);
    want("serialised", PCTierParse::kUnknown, PCSamplingTier::kOff);
    // A good token beside a bad one is still a refusal, not the good token.
    want("continuous,nonsense", PCTierParse::kUnknown, PCSamplingTier::kOff);
    want("nonsense,continuous", PCTierParse::kUnknown, PCSamplingTier::kOff);

    // The offending text reaches the log, because a refusal that does not say
    // WHAT it refused makes the operator guess at their own typo.
    {
        PCSamplingTier got;
        char bad[96];
        assert(perfagent::pc_tier_parse("nonsense", &got, bad, sizeof(bad)) ==
               PCTierParse::kUnknown);
        assert(strcmp(bad, "nonsense") == 0);
        assert(perfagent::pc_tier_parse("continuous,serialized", &got, bad, sizeof(bad)) ==
               PCTierParse::kNotExclusive);
        assert(strcmp(bad, "continuous,serialized") == 0);
    }

    // A token longer than the log buffer must truncate, not overrun it. The
    // producer is inside somebody else's process; a stack smash here is their
    // crash, not ours.
    {
        char huge[512];
        memset(huge, 'x', sizeof(huge) - 1);
        huge[sizeof(huge) - 1] = '\0';
        PCSamplingTier got = PCSamplingTier::kSerialized;
        char bad[16];
        assert(perfagent::pc_tier_parse(huge, &got, bad, sizeof(bad)) == PCTierParse::kUnknown);
        assert(got == PCSamplingTier::kOff);
        assert(strlen(bad) == sizeof(bad) - 1);
    }

    // A null `bad` buffer is a supported call: the adapter's report path has
    // nowhere to put it.
    {
        PCSamplingTier got = PCSamplingTier::kSerialized;
        assert(perfagent::pc_tier_parse("nonsense", &got, nullptr, 0) == PCTierParse::kUnknown);
        assert(got == PCSamplingTier::kOff);
    }

    // The names round-trip, and an out-of-range tier does NOT render as "off".
    // A value that fell out of a bad cast must not read as the safe default —
    // "off" is exactly the answer nobody would investigate.
    assert(strcmp(perfagent::pc_tier_name(PCSamplingTier::kOff), "off") == 0);
    assert(strcmp(perfagent::pc_tier_name(PCSamplingTier::kContinuous), "continuous") == 0);
    assert(strcmp(perfagent::pc_tier_name(PCSamplingTier::kSerialized), "serialized") == 0);
    assert(strcmp(perfagent::pc_tier_name((PCSamplingTier)7), "invalid") == 0);

    if (!g_bad) printf("pctier_test: ok\n");
    return g_bad;
}
