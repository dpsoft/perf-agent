//go:build ignore
//
// python_walk.bpf.c — the CPython frame walker, as a BPF object of its own.
//
// It is not linked into perf_dwarf, offcpu_dwarf or gpu_usdt and shares no
// header with them except bpf/unwind_record.h, which carries the record both
// sides append to and the three maps that carry a walk across a tail call.
// Userspace loads this object with those three maps REPLACED by the driver's
// (cilium/ebpf MapReplacements, see unwind/interp), then installs the program
// matching the driver's program type into the driver's interp_progs table at
// slot INTERP_ID_PYTHON.
//
// WHY THREE PROGRAMS THAT DO THE SAME THING. Every entry of a BPF prog array
// must share the program type of the program that tail-calls into it, and the
// three drivers are three types: perf_event, tp_btf/sched_switch and
// uprobe.multi. For BPF_PROG_TYPE_TRACING the kernel is stricter still --
// bpf_prog_map_compatible refuses a prog array whose entries have different
// attach_func_proto -- which is why the tp_btf program names sched_switch
// specifically rather than any raw tracepoint. None of them is ever attached;
// they exist only to be tail-called.
//
// A fourth driver of an existing type needs no change here. A driver of a NEW
// type needs one more three-line program, and that is the whole cost of
// adding one.

#if defined(__TARGET_ARCH_arm64)
#include "vmlinux_arm64.h"
#else
#include "vmlinux.h"
#endif
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#include "unwind_record.h"
#include "interp/python/python_walk.h"

SEC("perf_event")
int interp_python_perf_event(void *ctx) {
    py_walk(ctx);
    return 0;
}

SEC("tp_btf/sched_switch")
int interp_python_sched_switch(void *ctx) {
    py_walk(ctx);
    return 0;
}

SEC("uprobe.multi")
int interp_python_uprobe(void *ctx) {
    py_walk(ctx);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
