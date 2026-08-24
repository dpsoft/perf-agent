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
// # Every failure falls through to today's behaviour
//
// No consumer attached (the semaphore reads zero), nothing bound the
// address, the connect failed, the reply never came: the producer proceeds
// immediately and the consumer registers lazily on the first batch exactly
// as before. A degraded profile is never turned into no profile, and an
// unprofiled process pays nothing at all -- the caller checks its probe
// semaphore before it ever gets here.
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

// The whole rendezvous for the image this function is linked into: derive
// the name from our own mapping, connect, wait. `timeout_ms` of 0 disables
// it outright.
//
// Blocks the calling thread for at most `timeout_ms` in total, signals
// included. Call it only when a consumer is attached -- i.e. under the
// sampled-launch probe's semaphore -- so a process nobody is profiling never
// touches a socket.
EnrollResult enroll_with_consumer(unsigned timeout_ms);

// The rendezvous budget, from PERFAGENT_GPU_ENROLL_TIMEOUT_MS, or `dflt`
// when unset or unparseable. An explicit 0 disables the rendezvous, which is
// why this is not shim/core's usual "non-positive means default" parse.
unsigned enroll_timeout_ms(unsigned dflt);

}  // namespace perfagent

#endif
