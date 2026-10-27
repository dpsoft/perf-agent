// core/nvperfpin.h: which libnvperf_host PC sampling may call into (#215).
//
// The table is small, but one of its four answers turns PC sampling off and
// another loads a library into somebody else's process, so each is pinned
// here. The file identities are real stat() results on files this test
// creates, including a hard link, which is the case a path comparison gets
// wrong.
#include "nvperfpin.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <unistd.h>

using perfagent::NvperfFile;
using perfagent::NvperfPin;

static int g_bad;

static void want(const char *what, NvperfPin got, NvperfPin expect) {
    if (got != expect) {
        fprintf(stderr, "nvperfpin_test: %s -> %s, want %s\n", what,
                perfagent::nvperf_pin_name(got), perfagent::nvperf_pin_name(expect));
        g_bad = 1;
    }
}

static void want_path(const char *lib, const char *name, size_t n, const char *expect) {
    char out[256];
    memset(out, 'x', sizeof(out));
    const bool ok = perfagent::nvperf_sibling_path(lib, name, out, n);
    if (expect == nullptr ? ok : (!ok || strcmp(out, expect) != 0)) {
        fprintf(stderr, "nvperfpin_test: sibling(%s, %s, %zu) -> %s \"%s\", want %s\n",
                lib ? lib : "<null>", name, n, ok ? "ok" : "fail", ok ? out : "",
                expect ? expect : "fail");
        g_bad = 1;
    }
}

static void touch(const std::string &p) {
    FILE *f = fopen(p.c_str(), "w");
    if (!f) { perror(p.c_str()); exit(1); }
    fputs(p.c_str(), f);
    fclose(f);
}

int main() {
    char tmpl[] = "/tmp/nvperfpin_test.XXXXXX";
    const char *root = mkdtemp(tmpl);
    if (!root) { perror("mkdtemp"); return 1; }
    const std::string wheel = std::string(root) + "/wheel.so";
    const std::string toolkit = std::string(root) + "/toolkit.so";
    const std::string wheel_link = std::string(root) + "/wheel-link.so";
    const std::string missing = std::string(root) + "/missing.so";
    touch(wheel);
    touch(toolkit);
    if (link(wheel.c_str(), wheel_link.c_str()) != 0) { perror("link"); return 1; }

    const NvperfFile w = NvperfFile::of(wheel.c_str());
    const NvperfFile t = NvperfFile::of(toolkit.c_str());
    const NvperfFile wl = NvperfFile::of(wheel_link.c_str());
    const NvperfFile none = NvperfFile::of(missing.c_str());
    const NvperfFile null = NvperfFile::of(nullptr);
    if (!w.present || !t.present || !wl.present || none.present || null.present) {
        fprintf(stderr, "nvperfpin_test: stat fixture is wrong\n");
        return 1;
    }

    // The #215 crash: the wheel's CUPTI, with the toolkit's copy already in.
    want("sibling=wheel loaded=toolkit", perfagent::nvperf_pin_decide(w, t),
         NvperfPin::kMismatched);
    // The fix's normal path: nothing loaded yet.
    want("sibling=wheel loaded=none", perfagent::nvperf_pin_decide(w, none),
         NvperfPin::kPinSibling);
    want("sibling=wheel loaded=wheel", perfagent::nvperf_pin_decide(w, w),
         NvperfPin::kAlreadyMatched);
    // One file under two names - /usr/local/cuda vs /usr/local/cuda-13.3 -
    // must not read as a mismatch and switch PC sampling off.
    want("sibling=wheel loaded=hard link to wheel", perfagent::nvperf_pin_decide(w, wl),
         NvperfPin::kAlreadyMatched);
    // No sibling means nothing to compare, whatever is loaded.
    want("sibling=none loaded=none", perfagent::nvperf_pin_decide(none, none),
         NvperfPin::kNoSibling);
    want("sibling=none loaded=toolkit", perfagent::nvperf_pin_decide(none, t),
         NvperfPin::kNoSibling);

    // Two absent files are not "the same file".
    if (none.same(none)) {
        fprintf(stderr, "nvperfpin_test: two absent files compared equal\n");
        g_bad = 1;
    }

    want_path("/opt/venv/nvidia/cu13/lib/libcupti.so.13", "libnvperf_host.so", 256,
              "/opt/venv/nvidia/cu13/lib/libnvperf_host.so");
    want_path("/libcupti.so", "libnvperf_host.so", 256, "/libnvperf_host.so");
    want_path("libcupti.so.13", "libnvperf_host.so", 256, nullptr);   // no directory
    want_path(nullptr, "libnvperf_host.so", 256, nullptr);
    // Exactly fits ("/a/" + 17 chars + NUL = 21), then one byte short.
    want_path("/a/libcupti.so", "libnvperf_host.so", 21, "/a/libnvperf_host.so");
    want_path("/a/libcupti.so", "libnvperf_host.so", 20, nullptr);

    unlink(wheel_link.c_str());
    unlink(wheel.c_str());
    unlink(toolkit.c_str());
    rmdir(root);
    if (g_bad) return 1;
    printf("nvperfpin_test: ok\n");
    return 0;
}
