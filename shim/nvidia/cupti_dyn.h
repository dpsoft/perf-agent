// Resolve CUPTI at runtime instead of linking against it.
//
// WHY
// ---
// A DT_NEEDED on libcupti.so.13 pins this adapter to one CUDA major version
// and makes the library unloadable anywhere that version is absent -- and
// CUPTI ships with the CUDA TOOLKIT, not the driver, so "absent" is the normal
// case. nvidia-container-toolkit never injects it (zero cupti hits in
// nvc_info.c), so in a container the operator must supply it themselves.
//
// It also made the shim unbuildable in CI, which is the reason this exists
// now: the runners have no CUDA toolkit, so nothing could compile the shim,
// so nothing could gate it or ship it, so GPU mode had no release artifact at
// all (issue #121). Resolving at runtime needs only the HEADERS at build time.
//
// Safe to do because all twenty entry points are FUNCTIONS -- measured, zero
// data symbols -- so there is nothing that needs the linker's participation.
//
// One artifact then works against CUDA 12 and 13, and against the pip-installed
// CUPTI that PyTorch already pulls in, which is often the only one present.
#ifndef PERFAGENT_CUPTI_DYN_H
#define PERFAGENT_CUPTI_DYN_H

#include <dlfcn.h>
#include <cstdio>
#include <cstdlib>

#include <cupti.h>

namespace perfagent {
namespace cupti {
namespace dyn {

// Every CUPTI function this adapter calls, as a pointer.
//
// Listed once here rather than resolved at each call site: a missing symbol
// must be a startup failure with a name in it, not a null dereference inside
// somebody else's process on their first kernel launch.
struct Table {
    CUptiResult (*Subscribe)(CUpti_SubscriberHandle *, CUpti_CallbackFunc, void *);
    CUptiResult (*EnableDomain)(uint32_t, CUpti_SubscriberHandle, CUpti_CallbackDomain);
    CUptiResult (*Finalize)(void);
    CUptiResult (*GetTimestamp)(uint64_t *);
    CUptiResult (*GetResultString)(CUptiResult, const char **);
    CUptiResult (*GetCubinCrc)(CUpti_GetCubinCrcParams *);
    CUptiResult (*ActivityEnable)(CUpti_ActivityKind);
    CUptiResult (*ActivityFlushAll)(uint32_t);
    CUptiResult (*ActivityGetNextRecord)(uint8_t *, size_t, CUpti_Activity **);
    CUptiResult (*ActivityGetNumDroppedRecords)(CUcontext, uint32_t, size_t *);
    CUptiResult (*ActivityRegisterCallbacks)(CUpti_BuffersCallbackRequestFunc,
                                             CUpti_BuffersCallbackCompleteFunc);
    CUptiResult (*PCSamplingEnable)(CUpti_PCSamplingEnableParams *);
    CUptiResult (*PCSamplingDisable)(CUpti_PCSamplingDisableParams *);
    CUptiResult (*PCSamplingStart)(CUpti_PCSamplingStartParams *);
    CUptiResult (*PCSamplingStop)(CUpti_PCSamplingStopParams *);
    CUptiResult (*PCSamplingGetData)(CUpti_PCSamplingGetDataParams *);
    CUptiResult (*PCSamplingGetNumStallReasons)(CUpti_PCSamplingGetNumStallReasonsParams *);
    CUptiResult (*PCSamplingGetStallReasons)(CUpti_PCSamplingGetStallReasonsParams *);
    CUptiResult (*PCSamplingSetConfigurationAttribute)(CUpti_PCSamplingConfigurationInfoParams *);
    CUptiResult (*PCSamplingGetConfigurationAttribute)(CUpti_PCSamplingConfigurationInfoParams *);
};

// The sonames to try, most specific first.
//
// RTLD_NOLOAD is deliberately NOT used: the injected process is not required to
// have CUPTI mapped already, and in the common case it does not -- our shim is
// what brings it in. The plain name last catches an operator who put a
// versionless symlink on the loader path, which is what the init-container
// pattern produces.
inline const char *const *soNames(size_t *n) {
    static const char *const names[] = {
        "libcupti.so.13", "libcupti.so.12", "libcupti.so.11", "libcupti.so",
    };
    *n = sizeof(names) / sizeof(names[0]);
    return names;
}

inline void *&handle() {
    static void *h = nullptr;
    return h;
}

inline Table &table() {
    static Table *t = new Table();
    return *t;
}

// Resolved reports whether load() has succeeded. Callers on the injection path
// check this and decline rather than crashing the host.
inline bool &resolved() {
    static bool ok = false;
    return ok;
}

// load resolves the table, once. Returns false with a reason on stderr.
//
// Failure is reported and survivable: this runs inside a process that did not
// ask to be profiled, and taking it down because the operator did not install
// the CUDA toolkit would be an unacceptable way to say so.
inline bool load() {
    if (resolved()) return true;
    if (handle() == nullptr) {
        size_t n = 0;
        const char *const *names = soNames(&n);
        for (size_t i = 0; i < n && handle() == nullptr; i++) {
            handle() = dlopen(names[i], RTLD_LAZY | RTLD_LOCAL);
        }
        if (handle() == nullptr) {
            // Read dlerror ONCE: it clears on read, so a
            // `dlerror() ? dlerror() : "..."` reports (null) for every real
            // failure -- which is what the first version of this printed.
            const char *why = dlerror();
            fprintf(stderr,
                    "perfagent-cupti: cannot load libcupti (tried libcupti.so.13, .12, .11, "
                    "libcupti.so): %s\n"
                    "perfagent-cupti: CUPTI ships with the CUDA toolkit, not the driver, and is "
                    "not injected by nvidia-container-toolkit; put it on the loader path\n",
                    why ? why : "not found");
            return false;
        }
    }
    Table &t = table();
    bool ok = true;
    // Named individually so a missing one says WHICH: a CUPTI too old for a
    // function this adapter needs is a real deployment, and "cannot load
    // libcupti" would send the operator looking in the wrong place.
#define PERFAGENT_CUPTI_BIND(field, sym)                                     \
    do {                                                                     \
        *(void **)(&t.field) = dlsym(handle(), #sym);                        \
        if (t.field == nullptr) {                                            \
            fprintf(stderr, "perfagent-cupti: %s missing from this CUPTI\n", #sym); \
            ok = false;                                                      \
        }                                                                    \
    } while (0)

    PERFAGENT_CUPTI_BIND(Subscribe, cuptiSubscribe);
    PERFAGENT_CUPTI_BIND(EnableDomain, cuptiEnableDomain);
    PERFAGENT_CUPTI_BIND(Finalize, cuptiFinalize);
    PERFAGENT_CUPTI_BIND(GetTimestamp, cuptiGetTimestamp);
    PERFAGENT_CUPTI_BIND(GetResultString, cuptiGetResultString);
    PERFAGENT_CUPTI_BIND(GetCubinCrc, cuptiGetCubinCrc);
    PERFAGENT_CUPTI_BIND(ActivityEnable, cuptiActivityEnable);
    PERFAGENT_CUPTI_BIND(ActivityFlushAll, cuptiActivityFlushAll);
    PERFAGENT_CUPTI_BIND(ActivityGetNextRecord, cuptiActivityGetNextRecord);
    PERFAGENT_CUPTI_BIND(ActivityGetNumDroppedRecords, cuptiActivityGetNumDroppedRecords);
    PERFAGENT_CUPTI_BIND(ActivityRegisterCallbacks, cuptiActivityRegisterCallbacks);
    PERFAGENT_CUPTI_BIND(PCSamplingEnable, cuptiPCSamplingEnable);
    PERFAGENT_CUPTI_BIND(PCSamplingDisable, cuptiPCSamplingDisable);
    PERFAGENT_CUPTI_BIND(PCSamplingStart, cuptiPCSamplingStart);
    PERFAGENT_CUPTI_BIND(PCSamplingStop, cuptiPCSamplingStop);
    PERFAGENT_CUPTI_BIND(PCSamplingGetData, cuptiPCSamplingGetData);
    PERFAGENT_CUPTI_BIND(PCSamplingGetNumStallReasons, cuptiPCSamplingGetNumStallReasons);
    PERFAGENT_CUPTI_BIND(PCSamplingGetStallReasons, cuptiPCSamplingGetStallReasons);
    PERFAGENT_CUPTI_BIND(PCSamplingSetConfigurationAttribute,
                         cuptiPCSamplingSetConfigurationAttribute);
    PERFAGENT_CUPTI_BIND(PCSamplingGetConfigurationAttribute,
                         cuptiPCSamplingGetConfigurationAttribute);
#undef PERFAGENT_CUPTI_BIND

    resolved() = ok;
    return ok;
}

}  // namespace dyn
}  // namespace cupti
}  // namespace perfagent

// NO macros redefining cuptiX here.
//
// cupti_guard.h poisons those names deliberately -- a direct cuptiSubscribe
// outside the wrapper becomes a compile error, which is what stops a future
// call site reaching CUPTI without the guard that issue #99 exists for.
// Shadowing them to point at this table would silently disable that check, and
// the check is load-bearing: two of our threads inside CUPTI at once
// deadlocked the profiled application permanently.
//
// The wrappers in cupti_guard.h call table() directly instead.

#endif
