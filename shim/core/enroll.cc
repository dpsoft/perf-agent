#include "enroll.h"

#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>

#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

namespace perfagent {

const char *enroll_result_name(EnrollResult r) {
    switch (r) {
        case kEnrollDisabled:   return "disabled";
        case kEnrollNoAddress:  return "no-address";
        case kEnrollNoListener: return "no-listener";
        case kEnrollConfirmed:  return "confirmed";
        case kEnrollRefused:    return "refused";
        case kEnrollTimedOut:   return "timed-out";
        case kEnrollError:      return "error";
    }
    return "unknown";
}

bool enroll_name_from_maps(const char *maps_path, unsigned long addr,
                           char *out, size_t outsz) {
    if (!out || outsz == 0) return false;
    out[0] = '\0';
    FILE *f = fopen(maps_path, "re");
    if (!f) return false;
    char line[4096];
    bool ok = false;
    while (fgets(line, sizeof(line), f)) {
        unsigned long lo = 0, hi = 0, off = 0, ino = 0;
        unsigned maj = 0, min = 0;
        char perms[8] = {0};
        // 7fc9a0a00000-7fc9a0a21000 r-xp 00000000 fd:01 1234567  /path/to.so
        // The device is hex in /proc, and the consumer spells it as the
        // decimal major/minor of the same st_dev, so it is converted here
        // rather than passed through.
        if (sscanf(line, "%lx-%lx %7s %lx %x:%x %lu",
                   &lo, &hi, perms, &off, &maj, &min, &ino) != 7) {
            continue;
        }
        if (addr < lo || addr >= hi) continue;
        // An anonymous mapping has nothing the consumer's uprobe could have
        // attached to, so there is no name to agree on.
        if (ino == 0) break;
        const int n = snprintf(out, outsz, "perfagent-gpu-enroll.v1.%u.%u.%lu",
                               maj, min, ino);
        ok = n > 0 && (size_t)n < outsz;
        if (!ok) out[0] = '\0';
        break;
    }
    fclose(f);
    return ok;
}

namespace {

// The budget is ONE absolute deadline for the whole rendezvous - connect and
// reply together - not a per-syscall timeout.
//
// Two bugs live in the obvious alternative, and both were measured:
//
//   1. A per-call SO_RCVTIMEO is re-armed from zero on every retry. EINTR is
//      not exotic in a profiled process - a Go runtime's SIGURG, a JVM, a
//      setitimer, a second profiler - and a signal arriving faster than the
//      budget makes the wait NEVER expire. With a 500ms budget and SIGALRM
//      every 100ms, a producer sat blocked past 25s with no upper bound. In a
//      design whose whole premise is blocking the profiled application inside
//      cuInit, an unbounded stall is the one outcome that must be impossible.
//   2. SO_SNDTIMEO on the connect plus SO_RCVTIMEO on the read is TWO budgets,
//      so the real worst case was 2x what the documentation claimed.
//
// So: read the clock once, and before every blocking call set the socket
// timeout to what is left of it. Zero or less means give up now - never "wait
// forever", which is what a zero struct timeval would mean to the kernel.
struct deadline {
    long ms;  // CLOCK_MONOTONIC milliseconds

    static long now_ms() {
        struct timespec t;
        clock_gettime(CLOCK_MONOTONIC, &t);
        return (long)t.tv_sec * 1000L + t.tv_nsec / 1000000L;
    }
    static deadline in(unsigned budget_ms) {
        return deadline{now_ms() + (long)budget_ms};
    }
    // Remaining budget, floored at zero.
    long left() const {
        const long d = ms - now_ms();
        return d > 0 ? d : 0;
    }
};

// Arms both socket timeouts at the remaining budget. Returns false when the
// budget is spent, which the caller must treat as a timeout rather than as a
// blocking call.
bool arm(int fd, const deadline &dl) {
    const long left = dl.left();
    if (left <= 0) return false;
    struct timeval tv;
    tv.tv_sec = (time_t)(left / 1000);
    tv.tv_usec = (suseconds_t)((left % 1000) * 1000);
    if (setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv)) != 0) return false;
    return setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv)) == 0;
}

}  // namespace

EnrollResult enroll_connect(const char *name, unsigned timeout_ms) {
    if (!name || !*name) return kEnrollNoAddress;
    const size_t nlen = strlen(name);
    struct sockaddr_un sa;
    memset(&sa, 0, sizeof(sa));
    sa.sun_family = AF_UNIX;
    // Abstract namespace: a leading NUL, then the name, and the address
    // length says where it ends. No filesystem entry, so nothing to clean up
    // if either end dies.
    if (nlen + 1 > sizeof(sa.sun_path)) return kEnrollNoAddress;
    memcpy(sa.sun_path + 1, name, nlen);
    const socklen_t salen =
        (socklen_t)(offsetof(struct sockaddr_un, sun_path) + 1 + nlen);

    const int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (fd < 0) return kEnrollError;

    const deadline dl = deadline::in(timeout_ms);
    EnrollResult res = kEnrollError;
    for (;;) {
        // Re-armed from the REMAINING budget before every attempt, so a
        // stream of signals cannot extend the total wait. See `deadline`.
        if (!arm(fd, dl)) {
            res = kEnrollTimedOut;
            break;
        }
        if (connect(fd, (struct sockaddr *)&sa, salen) == 0) {
            res = kEnrollConfirmed;  // provisional; the reply decides
            break;
        }
        // A connect interrupted by a signal may already have completed, and
        // the retry then reports EISCONN. That is success, not failure -
        // treating it as an error would throw away a rendezvous that had
        // actually connected, and put the producer back on the lazy path for
        // no reason.
        if (errno == EISCONN) {
            res = kEnrollConfirmed;
            break;
        }
        if (errno == EINTR) continue;
        if (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINPROGRESS) {
            // The listener's backlog is full and SO_SNDTIMEO expired. The
            // consumer is alive but saturated; treat it as a timeout rather
            // than reconnecting into the same queue.
            res = kEnrollTimedOut;
            break;
        }
        // ECONNREFUSED is what an unbound abstract address gives back, and
        // it is the ordinary case: no profiler is listening for this shim.
        res = (errno == ECONNREFUSED || errno == ENOENT) ? kEnrollNoListener
                                                         : kEnrollError;
        break;
    }
    if (res != kEnrollConfirmed) {
        close(fd);
        return res;
    }

    // One status byte. The consumer writes it only after the walker's tables
    // are installed, so the read returning is the synchronisation point this
    // whole file exists for.
    char b = 0;
    for (;;) {
        if (!arm(fd, dl)) {
            res = kEnrollTimedOut;
            break;
        }
        const ssize_t n = read(fd, &b, 1);
        if (n == 1) {
            res = (b == 'K') ? kEnrollConfirmed : kEnrollRefused;
            break;
        }
        if (n == 0) {
            // The consumer closed without answering: it went away mid-
            // registration. Not a timeout, and not our problem to retry.
            res = kEnrollError;
            break;
        }
        if (errno == EINTR) continue;
        res = (errno == EAGAIN || errno == EWOULDBLOCK) ? kEnrollTimedOut
                                                        : kEnrollError;
        break;
    }
    close(fd);
    return res;
}

unsigned enroll_timeout_ms(unsigned dflt) {
    const char *v = getenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS");
    if (!v || !*v) return dflt;
    char *end = nullptr;
    errno = 0;
    const long n = strtol(v, &end, 10);
    if (errno != 0 || end == v || *end != '\0' || n < 0 || n > 600000) return dflt;
    return (unsigned)n;
}

bool enroll_self_name(char *out, size_t outsz) {
    // The address of THIS function: enroll.cc is linked into the shim, so the
    // mapping containing it is the mapping the consumer's uprobes are on.
    const void *self = (const void *)&enroll_self_name;
    return enroll_name_from_maps("/proc/self/maps", (unsigned long)self, out, outsz);
}

EnrollResult enroll_with_consumer(unsigned timeout_ms) {
    if (timeout_ms == 0) return kEnrollDisabled;
    char name[sizeof(((struct sockaddr_un *)nullptr)->sun_path)];
    // Our own address, not a caller-supplied one: this translation unit is
    // linked into the shim itself, so the mapping that contains it is the
    // mapping the consumer's uprobes are attached to. Taking a caller's
    // function address instead would risk a PLT entry in another object.
    const void *self = (const void *)&enroll_with_consumer;
    if (!enroll_name_from_maps("/proc/self/maps", (unsigned long)self, name,
                               sizeof(name))) {
        return kEnrollNoAddress;
    }
    return enroll_connect(name, timeout_ms);
}

}  // namespace perfagent
