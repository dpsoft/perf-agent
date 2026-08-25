// The producer half of the cubin transport: how a module's BYTES reach the
// consumer, on a channel that is not the enrolment rendezvous.
//
// # Why the bytes travel at all
//
// gpu_pc_sample_batch_v1 keys on cubin_crc and carries a pc_offset and a
// function_index. It names nothing. The cubin IS the missing table: its
// symbol table names the functions and its .debug_line turns a pc_offset into
// a source line. Without the bytes, a PC sample is four integers.
//
// gpu_module_load_v1 already carries a bytes_ptr and that pointer is NOT a
// transport. It points into THIS process's address space, and the consumer
// reading it would need /proc/<pid>/mem or process_vm_readv - both of which
// need CAP_SYS_PTRACE, which the agent's capability set deliberately does not
// contain. The record is unchanged and still accurate; it simply is not how
// the bytes get across.
//
// # Why this is not the enrolment socket
//
// enroll.h states the protocol as "The producer sends nothing", and the
// consumer's handler implements exactly that - it never reads. An offer must
// send a header, so a shared address would force the consumer to read in
// order to tell an offer from an enrolment, and on a genuine enrolment that
// read would block until this end's own budget expired. Every rendezvous
// would become a timeout, which is issue #49's ~38% stack loss returning by a
// different route. Three more, all on the same socket: the consumer's accept
// loop is serial, so offers queue AHEAD of a producer already in the backlog;
// admission is charged per connection out of one bucket, so a module-heavy
// process would have its own ENROLMENT refused; and it would put a connect()
// and a multi-megabyte write on the application's cuModuleLoad path.
//
// So: a second abstract address, derived from the same dev:inode by the same
// parser (cubin_name_from_maps calls enroll_name_from_maps and re-prefixes
// its result, so the two names cannot disagree about which shim they mean),
// and a consumer-side listener, goroutine and bucket of its own.
//
// # The payload is a sealed memfd
//
// The bytes go into a memfd, the memfd is sealed, and the DESCRIPTOR is what
// crosses the socket, by SCM_RIGHTS. The consumer mmaps it. Nothing streams,
// so this end never blocks on the consumer's read rate - which is what lets
// the offer run on the drain thread instead of on the application's thread.
//
// All four seals, and they are not decoration:
//
//   F_SEAL_SHRINK  without it we could ftruncate under the consumer's mmap
//                  and SIGBUS the profiler.
//   F_SEAL_WRITE   without it the ELF could mutate under its parser.
//   F_SEAL_GROW    without it the size it validated is not the size it maps.
//   F_SEAL_SEAL    without it any of the other three can be removed again.
//
// The consumer verifies them with fcntl(F_GET_SEALS) before it maps anything
// and refuses an offer that is missing one, with no fallback. Applying them
// here is therefore not politeness; an unsealed offer is a refused offer.
//
// # This never stalls the application
//
// Nothing in this header may be called from a CUPTI callback. The MODULE_LOADED
// callback's job is the memcpy - CUPTI's buffer is only valid there - and the
// offer belongs to the drain thread, which nothing is waiting on. See Task 5:
// the copy is deadline-bound by CUPTI's buffer lifetime and the send is
// deadline-bound by nothing at all, and collapsing them is how the
// application ends up paying for the profiler.
#ifndef PERFAGENT_CUBIN_H
#define PERFAGENT_CUBIN_H

#include <cstddef>
#include <cstdint>

namespace perfagent {

// How one offer ended. Only kCubinOfferAccepted means the consumer holds the
// bytes; every other value means that module's PC samples will read
// gpu_src_status "no-module", which is a worse profile and never a wrong one.
enum CubinOfferResult {
    kCubinOfferDisabled = 0,  // timeout of 0: the caller turned it off
    kCubinOfferNoAddress,     // /proc/self/maps had no file-backed line for us
    kCubinOfferNoListener,    // nobody bound the address
    kCubinOfferAccepted,      // 'K'
    kCubinOfferRefused,       // 'X': counted on the consumer's side, by reason
    kCubinOfferTimedOut,      // connected, but no reply inside the budget
    kCubinOfferError,         // memfd, seal, socket, connect or send failed
};

// A stable short name for logs. Never null.
const char *cubin_offer_result_name(CubinOfferResult r);

// The offer header, on the wire ahead of the descriptor. Fixed size, little-
// endian, naturally aligned - the same rules every USDT record follows - and
// mirrored by gpuprobe.cubinHeader, whose decoder pins the same offsets from
// the other side.
//
// `crc` is cuptiGetCubinCrc() over exactly the `size` bytes in the memfd. The
// consumer does not recompute it: CUPTI's polynomial is not published and the
// agent has no CUDA toolkit. It is the key gpu_pc_sample_batch_v1 joins on,
// so what matters is that the same number reaches both ends.
struct CubinOfferHeader {
    uint32_t magic;
    uint16_t version;
    uint16_t flags;  // reserved; must be 0, and a non-zero one is refused
    uint64_t size;
    uint64_t crc;
};

// 'C' 'U' 'B' '1' read little-endian, i.e. the four bytes in that order.
constexpr uint32_t kCubinOfferMagic =
    (uint32_t)'C' | ((uint32_t)'U' << 8) | ((uint32_t)'B' << 16) |
    ((uint32_t)'1' << 24);
constexpr uint16_t kCubinOfferVersion = 1;

static_assert(sizeof(CubinOfferHeader) == 24, "the offer header is 24 bytes");
static_assert(offsetof(CubinOfferHeader, magic) == 0, "magic at 0");
static_assert(offsetof(CubinOfferHeader, version) == 4, "version at 4");
static_assert(offsetof(CubinOfferHeader, flags) == 6, "flags at 6");
static_assert(offsetof(CubinOfferHeader, size) == 8, "size at 8");
static_assert(offsetof(CubinOfferHeader, crc) == 16, "crc at 16");

// The reply, one byte, spelled the same way the enrolment reply is.
constexpr char kCubinReplyOK = 'K';
constexpr char kCubinReplyRefused = 'X';

// Builds the offer name for the mapping that contains `addr`, reading
// `maps_path` in the /proc/<pid>/maps format. Implemented on top of
// enroll_name_from_maps so there is ONE parser and ONE dev:inode derivation
// for both channels: they must always agree about which shim image they mean.
bool cubin_name_from_maps(const char *maps_path, unsigned long addr, char *out,
                          size_t outsz);

// The offer name this image derives for itself, without connecting.
bool cubin_self_name(char *out, size_t outsz);

// Copies `len` bytes into a fresh memfd and applies every required seal.
// Returns the descriptor, or -1. The caller closes it.
//
// One place applies the seals, so "which seals" is a fact about this function
// rather than a convention spread over its callers.
int cubin_seal_bytes(const void *bytes, size_t len);

// Offers an already-sealed descriptor. Does NOT close `fd`.
//
// `timeout_ms` is ONE budget for the whole offer - connect, send and reply
// together - against a CLOCK_MONOTONIC deadline taken on entry, for the same
// two measured reasons enroll.cc spells out: a per-syscall timeout re-armed
// on EINTR never expires, and one timeout per phase is two budgets.
CubinOfferResult cubin_offer_fd(const char *name, int fd, uint64_t size,
                                uint64_t crc, unsigned timeout_ms);

// Seals `bytes` and offers them to `name`.
CubinOfferResult cubin_offer(const char *name, const void *bytes, size_t len,
                             uint64_t crc, unsigned timeout_ms);

// The whole offer for the image this function is linked into: derive the name
// from our own mapping, seal, connect, send, wait.
//
// Call it from the DRAIN THREAD. Never from a CUPTI callback.
CubinOfferResult cubin_offer_to_consumer(const void *bytes, size_t len,
                                         uint64_t crc, unsigned timeout_ms);

// The offer budget, from PERFAGENT_GPU_CUBIN_TIMEOUT_MS, or `dflt` when unset
// or unparseable. An explicit 0 disables offers outright.
unsigned cubin_timeout_ms(unsigned dflt);

// The two producer-side counters from the plan's table.
//
//   cubins_offered      modules this shim tried to hand over
//   cubins_send_failed  refused, timed out, or short-written
//
// Both are monotonic and both are read by the adapter's stats dump. A module
// that is offered and not accepted is a module whose PC samples will read
// "no-module", so the difference between these two is exactly the size of
// what this process could not explain.
uint64_t cubins_offered();
uint64_t cubins_send_failed();

}  // namespace perfagent

#endif
