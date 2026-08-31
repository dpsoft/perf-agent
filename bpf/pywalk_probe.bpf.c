// pywalk_probe — NOT part of the build. A verifier measurement, kept in the
// tree because the number it produces decided an architecture.
//
// It models the minimal tail-call variant of R-D: a program whose ONLY job is
// the CPython chain walk, verified in its own state space, writing into the
// same sample_record. If that program verifies cheaply on its own, moving the
// arm out of walk_step by one bpf_tail_call escapes the cost; if it does not,
// R-D is dead too and no amount of restructuring inside walk_step will help.
//
// Compile by hand (go generate never sees it):
//
//   clang-18 -target bpf -D__TARGET_ARCH_x86 -O2 -g -Wall -Ibpf \
//            -c bpf/pywalk_probe.bpf.c -o /tmp/pywalk_probe.o
//   bpfload -v /tmp/pywalk_probe.o
//
// PY_PROBE_CHAIN bounds the walk the way the real program would bound it: the
// whole chain in one invocation, no chunking (B8 showed the outer bound buys
// nothing, so there is no reason to chunk).
#include "vmlinux.h"

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#include "unwind_common.h"

#ifndef PY_PROBE_CHAIN
#define PY_PROBE_CHAIN 32
#endif

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct sample_record);
} pywalk_probe_scratch SEC(".maps");

SEC("perf_event")
int pywalk_probe(struct bpf_perf_event_data *ctx) {
    __u32 zero = 0;
    struct sample_record *rec = bpf_map_lookup_elem(&pywalk_probe_scratch, &zero);
    if (!rec) return 0;

    __u32 tgid = bpf_get_current_pid_tgid() >> 32;
    struct py_proc_info *pi = bpf_map_lookup_elem(&py_procs, &tgid);
    if (!pi || !pi->enabled) return 0;

    struct walk_ctx w = {
        .pid = tgid,
        .rec = rec,
        .n_pcs = 0,
    };

    py_begin_chain(&w, pi);
    for (int i = 0; i < PY_PROBE_CHAIN; i++) {
        if (w.py_state != PY_CHAIN_WALKING) break;
        if (py_step_one(&w)) break;
    }
    if (w.py_state == PY_CHAIN_WALKING) py_count(PY_CNT_CHAIN_TRUNCATED);
    else if (w.py_state == PY_CHAIN_ACTIVE) py_count(PY_CNT_CHAIN_ABANDONED);

    rec->hdr.n_pcs = (__u8)(w.n_pcs > MAX_FRAMES ? MAX_FRAMES : w.n_pcs);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
