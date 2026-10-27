// Which libnvperf_host CUPTI's PC sampling will call into, decided before it
// does (issue #215).
//
// cuptiPCSamplingEnable loads libnvperf_host.so BY NAME, and the CUPTI in the
// PyTorch wheels has no RUNPATH, so that name goes through the ordinary loader
// search: LD_LIBRARY_PATH, then the ldconfig cache. On a host that also has a
// CUDA toolkit installed, both point at the TOOLKIT's copy, and a CUPTI 13.0
// calling into a 13.3 libnvperf_host segfaulted in NVPW_CUDA_LoadDriver on 20
// of 20 runs - killing the profiled process, not just the profile.
//
// The fix leans on one loader rule: a by-name dlopen returns an object already
// mapped under that soname rather than searching again. So the adapter loads
// the libnvperf_host that sits next to the libcupti it resolved, before the
// first PC-sampling call, and CUPTI's own dlopen then gets that one.
//
// This header is the decision only, with no dl calls in it, so the table below
// is tested without a GPU. The adapter supplies the facts.
#ifndef PERFAGENT_NVPERFPIN_H
#define PERFAGENT_NVPERFPIN_H

#include <sys/stat.h>

#include <cstddef>
#include <cstdint>
#include <cstring>

namespace perfagent {

// A file's identity, or its absence. Compared by device and inode rather than
// by path: /usr/local/cuda/lib64 and /usr/local/cuda-13.3/lib64 are one
// directory under two names on every toolkit install, and a path comparison
// would refuse PC sampling there for no reason.
struct NvperfFile {
    bool present = false;
    uint64_t dev = 0;
    uint64_t ino = 0;

    static NvperfFile of(const char *path) {
        NvperfFile f;
        struct stat st;
        if (path && stat(path, &st) == 0) {
            f.present = true;
            f.dev = (uint64_t)st.st_dev;
            f.ino = (uint64_t)st.st_ino;
        }
        return f;
    }

    bool same(const NvperfFile &o) const {
        return present && o.present && dev == o.dev && ino == o.ino;
    }
};

enum class NvperfPin {
    // Nothing loaded yet, and libcupti has a sibling: load the sibling.
    kPinSibling,
    // Already loaded, and it IS the sibling. Nothing to do.
    kAlreadyMatched,
    // Already loaded, and it is NOT the sibling. CUPTI will call into it and
    // nothing can unload it, so PC sampling is refused for the process.
    kMismatched,
    // libcupti has no sibling, so there is nothing to compare against. CUPTI
    // gets whatever its own dlopen finds, which is the behaviour before #215.
    kNoSibling,
};

// sibling is the libnvperf_host next to libcupti; loaded is the one already
// mapped in the process, if any.
inline NvperfPin nvperf_pin_decide(const NvperfFile &sibling, const NvperfFile &loaded) {
    if (!sibling.present) return NvperfPin::kNoSibling;
    if (!loaded.present) return NvperfPin::kPinSibling;
    return sibling.same(loaded) ? NvperfPin::kAlreadyMatched : NvperfPin::kMismatched;
}

inline const char *nvperf_pin_name(NvperfPin p) {
    switch (p) {
    case NvperfPin::kPinSibling: return "pinned";
    case NvperfPin::kAlreadyMatched: return "matched";
    case NvperfPin::kMismatched: return "mismatched";
    case NvperfPin::kNoSibling: return "no-sibling";
    }
    return "?";
}

// Writes the path of `name` in the directory holding `lib_path` into out.
// False when lib_path has no directory part or the result does not fit:
// a truncated path would stat some other file, and comparing against that is
// worse than not comparing.
inline bool nvperf_sibling_path(const char *lib_path, const char *name, char *out, size_t n) {
    if (!lib_path || !name || !out || n == 0) return false;
    const char *slash = strrchr(lib_path, '/');
    if (!slash) return false;
    const size_t dir = (size_t)(slash - lib_path) + 1;   // keeps the '/'
    const size_t len = strlen(name);
    if (dir + len + 1 > n) return false;
    memcpy(out, lib_path, dir);
    memcpy(out + dir, name, len + 1);
    return true;
}

}  // namespace perfagent

#endif
