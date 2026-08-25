// The startup rendezvous: the producer waits, once, for the consumer to
// install its unwind tables before it emits a single sampled launch.
//
// # Why this exists in the producer and not in the consumer
//
// A sampled launch's CPU stack is walked IN THE KERNEL, inside walk_step
// (bpf/unwind_common.h), at the instant the uprobe traps. The walker unwinds
// by CFI only for frames whose PC it can find in `pid_mappings`; a miss is
// silent and falls through to the frame-pointer chain, which in a CUDA
// process dies in the vendor libraries. So the tables have to be in the BPF
// maps BEFORE the probe fires -- not "sooner", before.
//
// Nothing the consumer can observe in userspace happens before the probe
// fires. Registration driven by the first batch, by an mmap notification, or
// by an exec notification all share the same defect: the event that triggers
// them is delivered after the sample that would have needed the tables, and
// everything arriving during the compile is walked without them too. On an
// RTX 3090, libcuda alone is 135805 CFI entries and ~73ms to compile against
// a ~540ms workload, which is why issue #49 measured ~38% of sampled stacks
// lost rather than a rounding error.
//
// The producer, on the other hand, has a point in its life that provably
// precedes every launch: the vendor adapter's init. The CUDA driver dlopens
// the adapter through CUDA_INJECTION64_PATH during cuInit, so by the time
// InitializeInjection runs, libcuda and the application are mapped, and no
// kernel can have been launched yet. Blocking there until the consumer says
// "your tables are in" closes the window instead of narrowing it.
//
// # The channel
//
// An abstract AF_UNIX stream socket whose name both ends compute
// independently from the shim's own device and inode:
//
//	\0perfagent-gpu-enroll.v1.<dev_major>.<dev_minor>.<inode>
//
// The inode is the right key because it is exactly what a uprobe attaches
// to: a producer whose probes this consumer armed necessarily maps the same
// inode, and two consumers watching two different shim copies (a CI machine
// running two gate copies at once, say) can never collide. The producer
// reads its own dev:inode out of /proc/self/maps, so no path resolution, no
// environment variable, and no cooperation from whoever launched the process
// is involved.
//
// The producer sends nothing. The consumer takes the peer's PID from
// SO_PEERCRED -- kernel-supplied, unspoofable -- installs that PID's tables
// synchronously, and only then writes one status byte:
//
//	'K'  tables are installed; go
//	'X'  registration was refused or installed nothing; go anyway
//
// # Do NOT gate this on the probe semaphore
//
// The first version of this fix ran only under PERFAGENT_USDT_ENABLED, on the
// reasoning that the semaphore says a consumer is attached. It does not. It
// says the KERNEL HAS TOLD THIS PROCESS SO, which is a different fact with a
// different arrival time, and the two diverge exactly where it matters.
//
// Measured on an RTX 3090: with the gate in place, UnwindEnrollRequests was 0
// across three runs and the loss was unchanged (~175 of 500 sampled stacks
// walked with no tables). The same build passed the GPU-free gate with
// no-tables=0, because that producer is EXEC'd - the kernel arms the
// semaphore while building the new mm, long before main runs. A CUDA adapter
// is DLOPEN'd by libcuda and InitializeInjection is called essentially the
// instant the mapping appears, which is the earliest moment the question can
// be asked and the least likely moment for it to have been answered. The
// probes fired perfectly later in those same runs - 4000 of them - so the
// semaphore did arm; just not yet.
//
// The connect is the gate, and it is strictly better:
//
//   - It is authoritative. The consumer binds this address BEFORE it creates
//     the uprobe link (gpuprobe/enroll.go), so it is listening whenever any
//     profiling of this shim is happening - there is no window in which a
//     consumer exists and the address does not.
//   - It is already free. An abstract-socket connect to an unbound address
//     fails immediately with ECONNREFUSED; the semaphore gate was never what
//     made the unprofiled case cheap.
//
// # Every failure falls through to today's behaviour
//
// Nothing bound the address, the connect failed, the reply never came: the
// producer proceeds immediately and the consumer registers lazily on the
// first batch exactly as before. A degraded profile is never turned into no
// profile.
#ifndef PERFAGENT_ENROLL_H
#define PERFAGENT_ENROLL_H

#include <cstddef>

namespace perfagent {

// How the rendezvous ended. Only kEnrollConfirmed means the consumer's
// walker has this process's CFI tables; every other value means the run
// continues on the pre-#49 lazy path.
enum EnrollResult {
    kEnrollDisabled = 0, // timeout of 0: the caller turned it off
    kEnrollNoAddress,    // /proc/self/maps had no file-backed line for us
    kEnrollNoListener,   // nobody bound the address
    kEnrollConfirmed,    // 'K': the tables are in
    kEnrollRefused,      // 'X': the consumer would not, or could not
    kEnrollTimedOut,     // connected, but no reply inside the budget
    kEnrollError,        // socket, connect or read failed
};

// A stable short name for logs. Never null.
const char *enroll_result_name(EnrollResult r);

// Builds the rendezvous name (without the leading NUL of the abstract
// namespace) for the mapping that contains `addr`, reading `maps_path` in
// the /proc/<pid>/maps format. Separated from the socket work so the name
// derivation is testable against a fixture.
bool enroll_name_from_maps(const char *maps_path, unsigned long addr,
                           char *out, size_t outsz);

// Connects to `name` in the abstract namespace and waits for the consumer's
// status byte.
//
// `timeout_ms` is ONE budget for the whole rendezvous - the connect and the
// reply together - measured against a CLOCK_MONOTONIC deadline taken on
// entry. It is not a per-syscall timeout: a signal that interrupts either
// call cannot extend the total, and the two phases cannot each spend the full
// budget. This is a hard bound on how long the profiled application is held
// inside its own initialisation, so it is stated as one.
EnrollResult enroll_connect(const char *name, unsigned timeout_ms);

// Writes the rendezvous name this image derives for itself into `out`, without
// connecting to anything. Exists so a producer can LOG the name it computed:
// the two ends derive it independently and never exchange it, so when they
// disagree every counter on both sides reads zero and nothing says why. One
// CUDA run comparing this string against Stats.UnwindEnrollAddress settles
// what a round trip of inference could not.
bool enroll_self_name(char *out, size_t outsz);

// The whole rendezvous for the image this function is linked into: derive
// the name from our own mapping, connect, wait. `timeout_ms` of 0 disables
// it outright.
//
// Blocks the calling thread for at most `timeout_ms` in total, signals
// included. Call it UNCONDITIONALLY at producer init - see "Do NOT gate this
// on the probe semaphore" above. In an unprofiled process the whole call is
// one /proc/self/maps read and a connect that is refused before it blocks.
EnrollResult enroll_with_consumer(unsigned timeout_ms);

// The rendezvous budget, from PERFAGENT_GPU_ENROLL_TIMEOUT_MS, or `dflt`
// when unset or unparseable. An explicit 0 disables the rendezvous, which is
// why this is not shim/core's usual "non-positive means default" parse.
unsigned enroll_timeout_ms(unsigned dflt);

}  // namespace perfagent

#endif
