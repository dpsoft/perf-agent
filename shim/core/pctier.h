// PC-sampling tier selection, producer side.
//
// One setting, three values, read from ONE environment variable:
//
//   PERFAGENT_GPU_PC_SAMPLING = off | continuous | serialized     (0 | 1 | 2)
//
// off (the default, and what an unset variable means) is OFF: nothing in the
// producer allocates a PC buffer, subscribes an extra CUPTI domain, calls a
// cupti PC-sampling entry point or fires a PC-sampling probe. Not "enabled but
// idle", not "enabled at a low rate". shim/stub/pc_tier_test.cc asserts that
// negatively, by patching the four PC-sampling probe sites with int3 and
// running the real producer with the tier off: not one of them may fire.
//
// Why the two tiers are exclusive, and why "both" is refused rather than
// resolved
// --------------------------------------------------------------------------
// CUPTI's COLLECTION_MODE is a single per-CUcontext attribute, so a process
// could in principle run KERNEL_SERIALIZED on one context and CONTINUOUS on
// another. What rules that out is not CUPTI: it is that WHICH CONTEXT A GIVEN
// KERNEL LANDS ON IS THE APPLICATION'S CHOICE, NOT THE PROFILER'S. A "both"
// mode would emit one profile in which some kernels carry exact launch
// attribution and inflated durations while others carry inferred attribution
// and honest ones, split along an axis the operator can neither see nor
// control. That is worse than either tier alone.
//
// So a value naming two tiers is refused, loudly, and the producer falls
// CLOSED to off. It never picks one. "Last one wins" and "the cheaper one
// wins" are both decisions the operator did not make and cannot see in the
// output, which is the failure mode this whole file exists to prevent.
//
// The agent's own copy of these rules is gpu/tier.go, which is what writes the
// variable this file reads. The two must agree on the spellings; both accept
// the names and the numerals, and the agent writes the NAME.
#ifndef PERFAGENT_PCTIER_H
#define PERFAGENT_PCTIER_H

#include <stddef.h>
#include <string.h>

namespace perfagent {

enum class PCSamplingTier : unsigned {
    kOff = 0,
    kContinuous = 1,   // Tier B, CUPTI_PC_SAMPLING_COLLECTION_MODE_CONTINUOUS
    kSerialized = 2,   // Tier A, ..._KERNEL_SERIALIZED, duty-cycled
};

enum class PCTierParse {
    kOK,
    kUnknown,       // a token that is not one of the three values
    kNotExclusive,  // more than one value named in the same setting
};

inline const char *pc_tier_name(PCSamplingTier t) {
    switch (t) {
    case PCSamplingTier::kOff: return "off";
    case PCSamplingTier::kContinuous: return "continuous";
    case PCSamplingTier::kSerialized: return "serialized";
    }
    return "invalid";
}

// One token to a tier. The numeric spellings are accepted because this
// variable has been 0/1/2 since Task 6 and operators and container specs are
// already setting it that way; silently ignoring those would turn a configured
// Tier A run into a quiet off one.
inline bool pc_tier_token(const char *tok, size_t len, PCSamplingTier *out) {
    struct { const char *name; PCSamplingTier tier; } kNames[] = {
        {"off", PCSamplingTier::kOff},
        {"0", PCSamplingTier::kOff},
        {"continuous", PCSamplingTier::kContinuous},
        {"1", PCSamplingTier::kContinuous},
        {"serialized", PCSamplingTier::kSerialized},
        {"2", PCSamplingTier::kSerialized},
    };
    for (const auto &n : kNames) {
        const size_t nl = strlen(n.name);
        if (nl != len) continue;
        size_t i = 0;
        for (; i < len; i++) {
            char c = tok[i];
            if (c >= 'A' && c <= 'Z') c = (char)(c - 'A' + 'a');
            if (c != n.name[i]) break;
        }
        if (i == len) { *out = n.tier; return true; }
    }
    return false;
}

// Parses the whole setting. On ANY error *out is kOff -- the producer falls
// closed, never into a tier nobody chose -- and `bad` receives the offending
// token (kUnknown) or the whole value (kNotExclusive) for the log line.
//
// A value naming two tiers is PARSED rather than rejected as syntax, because
// "both" has to be expressible for the refusal to be reachable at all. A
// parser that quietly took the first token of "continuous,serialized" would be
// exactly the silent pick this rule exists to prevent.
inline PCTierParse pc_tier_parse(const char *value, PCSamplingTier *out,
                                 char *bad, size_t badlen) {
    *out = PCSamplingTier::kOff;
    if (bad && badlen) bad[0] = '\0';
    if (!value) return PCTierParse::kOK;

    bool seen = false;
    PCSamplingTier first = PCSamplingTier::kOff;
    const char *p = value;
    while (*p) {
        while (*p && (*p == ',' || *p == '+' || *p == ';' || *p == ' ' ||
                      *p == '\t' || *p == '\n')) p++;
        if (!*p) break;
        const char *start = p;
        while (*p && !(*p == ',' || *p == '+' || *p == ';' || *p == ' ' ||
                       *p == '\t' || *p == '\n')) p++;
        const size_t len = (size_t)(p - start);

        PCSamplingTier tier;
        if (!pc_tier_token(start, len, &tier)) {
            if (bad && badlen) {
                const size_t n = len < badlen - 1 ? len : badlen - 1;
                memcpy(bad, start, n);
                bad[n] = '\0';
            }
            *out = PCSamplingTier::kOff;
            return PCTierParse::kUnknown;
        }
        if (!seen) {
            first = tier;
            seen = true;
        } else if (tier != first) {
            if (bad && badlen) {
                const size_t vl = strlen(value);
                const size_t n = vl < badlen - 1 ? vl : badlen - 1;
                memcpy(bad, value, n);
                bad[n] = '\0';
            }
            *out = PCSamplingTier::kOff;
            return PCTierParse::kNotExclusive;
        }
    }
    *out = seen ? first : PCSamplingTier::kOff;
    return PCTierParse::kOK;
}

}  // namespace perfagent

#endif  // PERFAGENT_PCTIER_H
