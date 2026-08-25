#include "cubin.h"

#include "enroll.h"

#include <atomic>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>

#include <fcntl.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

// The sealing interface lives in <linux/fcntl.h>, which cannot be included
// beside <fcntl.h> on every toolchain without redefinition warnings. The
// numbers are kernel ABI and have not moved since 3.17.
#ifndef F_ADD_SEALS
#define F_ADD_SEALS 1033
#endif
#ifndef F_GET_SEALS
#define F_GET_SEALS 1034
#endif
#ifndef F_SEAL_SEAL
#define F_SEAL_SEAL 0x0001
#endif
#ifndef F_SEAL_SHRINK
#define F_SEAL_SHRINK 0x0002
#endif
#ifndef F_SEAL_GROW
#define F_SEAL_GROW 0x0004
#endif
#ifndef F_SEAL_WRITE
#define F_SEAL_WRITE 0x0008
#endif

namespace perfagent {

namespace {

std::atomic<uint64_t> g_cubins_offered{0};
std::atomic<uint64_t> g_cubins_send_failed{0};

// All four, always. See cubin.h for what each one prevents; the short version
// is that three of them stop the consumer being SIGBUSed or handed mutating
// bytes, and the fourth stops the other three being removed again.
constexpr int kRequiredSeals =
    F_SEAL_SEAL | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_WRITE;

// The same one-deadline-for-the-whole-exchange discipline enroll.cc uses, and
// for the same two measured reasons: a per-syscall SO_RCVTIMEO re-armed on
// every EINTR never expires, and a send timeout plus a receive timeout is two
// budgets rather than one.
struct deadline {
    long ms;

    static long now_ms() {
        struct timespec t;
        clock_gettime(CLOCK_MONOTONIC, &t);
        return (long)t.tv_sec * 1000L + t.tv_nsec / 1000000L;
    }
    static deadline in(unsigned budget_ms) {
        return deadline{now_ms() + (long)budget_ms};
    }
    long left() const {
        const long d = ms - now_ms();
        return d > 0 ? d : 0;
    }
};

bool arm(int fd, const deadline &dl) {
    const long left = dl.left();
    if (left <= 0) return false;
    struct timeval tv;
    tv.tv_sec = (time_t)(left / 1000);
    tv.tv_usec = (suseconds_t)((left % 1000) * 1000);
    if (setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv)) != 0) return false;
    return setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv)) == 0;
}

// Fills sa/salen for an abstract name. False when the name cannot fit.
bool abstract_addr(const char *name, struct sockaddr_un *sa, socklen_t *salen) {
    const size_t nlen = strlen(name);
    memset(sa, 0, sizeof(*sa));
    sa->sun_family = AF_UNIX;
    if (nlen + 1 > sizeof(sa->sun_path)) return false;
    memcpy(sa->sun_path + 1, name, nlen);
    *salen = (socklen_t)(offsetof(struct sockaddr_un, sun_path) + 1 + nlen);
    return true;
}

}  // namespace

const char *cubin_offer_result_name(CubinOfferResult r) {
    switch (r) {
        case kCubinOfferDisabled:   return "disabled";
        case kCubinOfferNoAddress:  return "no-address";
        case kCubinOfferNoListener: return "no-listener";
        case kCubinOfferAccepted:   return "accepted";
        case kCubinOfferRefused:    return "refused";
        case kCubinOfferTimedOut:   return "timed-out";
        case kCubinOfferError:      return "error";
    }
    return "unknown";
}

// One parser, one dev:inode derivation, two prefixes.
//
// The alternative - a second copy of the maps parser with a different format
// string - is how the two channels would come to disagree about which shim
// image they mean, and the enrolment address has already been through exactly
// that failure once: stat(2) and /proc/<pid>/maps report different devices for
// every file on a btrfs subvolume, so an address derived from the wrong one
// bound a name no producer ever dialled and every counter on both sides read
// zero. Deriving through the function that got that right is not a
// convenience.
bool cubin_name_from_maps(const char *maps_path, unsigned long addr, char *out,
                          size_t outsz) {
    if (!out || outsz == 0) return false;
    out[0] = '\0';
    char enroll[sizeof(((struct sockaddr_un *)nullptr)->sun_path)];
    if (!enroll_name_from_maps(maps_path, addr, enroll, sizeof(enroll))) {
        return false;
    }
    static const char kEnrollPrefix[] = "perfagent-gpu-enroll.v1.";
    const size_t plen = sizeof(kEnrollPrefix) - 1;
    if (strncmp(enroll, kEnrollPrefix, plen) != 0) return false;
    const int n = snprintf(out, outsz, "perfagent-gpu-cubin.v1.%s", enroll + plen);
    const bool ok = n > 0 && (size_t)n < outsz;
    if (!ok) out[0] = '\0';
    return ok;
}

bool cubin_self_name(char *out, size_t outsz) {
    const void *self = (const void *)&cubin_self_name;
    return cubin_name_from_maps("/proc/self/maps", (unsigned long)self, out, outsz);
}

int cubin_seal_bytes(const void *bytes, size_t len) {
    if (!bytes || len == 0) return -1;
    // MFD_ALLOW_SEALING is the whole point: without it F_ADD_SEALS returns
    // EPERM and the offer would be refused by the consumer for being
    // unsealed, which is the correct outcome but an avoidable one.
    const int fd = memfd_create("perfagent-cubin", MFD_CLOEXEC | MFD_ALLOW_SEALING);
    if (fd < 0) return -1;

    const char *p = (const char *)bytes;
    size_t off = 0;
    while (off < len) {
        const ssize_t n = write(fd, p + off, len - off);
        if (n > 0) {
            off += (size_t)n;
            continue;
        }
        if (n < 0 && errno == EINTR) continue;
        close(fd);
        return -1;
    }

    // Sealed only after the last byte is in. F_SEAL_WRITE would also fail
    // with EBUSY if a writable MAPPING were outstanding, which is why the
    // bytes go in through write() rather than through an mmap.
    if (fcntl(fd, F_ADD_SEALS, kRequiredSeals) != 0) {
        close(fd);
        return -1;
    }
    // Read them back rather than assuming the kernel took them. A descriptor
    // that reaches the consumer without every seal is refused there and the
    // module is silently unresolvable; finding that out here makes it a
    // counted send failure with a reason instead.
    const int got = fcntl(fd, F_GET_SEALS, 0);
    if (got < 0 || (got & kRequiredSeals) != kRequiredSeals) {
        close(fd);
        return -1;
    }
    return fd;
}

CubinOfferResult cubin_offer_fd(const char *name, int fd, uint64_t size,
                                uint64_t crc, unsigned timeout_ms) {
    if (timeout_ms == 0) return kCubinOfferDisabled;
    if (!name || !*name) return kCubinOfferNoAddress;
    if (fd < 0 || size == 0) return kCubinOfferError;

    struct sockaddr_un sa;
    socklen_t salen = 0;
    if (!abstract_addr(name, &sa, &salen)) return kCubinOfferNoAddress;

    g_cubins_offered.fetch_add(1, std::memory_order_relaxed);

    const int sock = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (sock < 0) {
        g_cubins_send_failed.fetch_add(1, std::memory_order_relaxed);
        return kCubinOfferError;
    }

    const deadline dl = deadline::in(timeout_ms);
    CubinOfferResult res = kCubinOfferError;
    for (;;) {
        if (!arm(sock, dl)) {
            res = kCubinOfferTimedOut;
            break;
        }
        if (connect(sock, (struct sockaddr *)&sa, salen) == 0) {
            res = kCubinOfferAccepted;  // provisional; the reply decides
            break;
        }
        if (errno == EISCONN) {
            res = kCubinOfferAccepted;
            break;
        }
        if (errno == EINTR) continue;
        if (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINPROGRESS) {
            res = kCubinOfferTimedOut;
            break;
        }
        // An unbound abstract address gives ECONNREFUSED, and that is the
        // ordinary case: no profiler is listening for this shim. Not a
        // failure to send, so it is not counted as one - there was nothing to
        // send to.
        res = (errno == ECONNREFUSED || errno == ENOENT) ? kCubinOfferNoListener
                                                         : kCubinOfferError;
        break;
    }
    if (res != kCubinOfferAccepted) {
        close(sock);
        if (res != kCubinOfferNoListener) {
            g_cubins_send_failed.fetch_add(1, std::memory_order_relaxed);
        }
        return res;
    }

    CubinOfferHeader h;
    memset(&h, 0, sizeof(h));
    h.magic = kCubinOfferMagic;
    h.version = kCubinOfferVersion;
    h.flags = 0;
    h.size = size;
    h.crc = crc;

    struct iovec iov;
    iov.iov_base = &h;
    iov.iov_len = sizeof(h);

    // The descriptor rides on the first byte of the header, so one sendmsg
    // carries both and the consumer's first recvmsg has everything it needs
    // to decide. Nothing streams: the payload is the memfd, not the socket.
    union {
        char buf[CMSG_SPACE(sizeof(int))];
        struct cmsghdr align;
    } cmsg;
    memset(&cmsg, 0, sizeof(cmsg));

    struct msghdr msg;
    memset(&msg, 0, sizeof(msg));
    msg.msg_iov = &iov;
    msg.msg_iovlen = 1;
    msg.msg_control = cmsg.buf;
    msg.msg_controllen = sizeof(cmsg.buf);

    struct cmsghdr *c = CMSG_FIRSTHDR(&msg);
    c->cmsg_level = SOL_SOCKET;
    c->cmsg_type = SCM_RIGHTS;
    c->cmsg_len = CMSG_LEN(sizeof(int));
    memcpy(CMSG_DATA(c), &fd, sizeof(int));

    for (;;) {
        if (!arm(sock, dl)) {
            res = kCubinOfferTimedOut;
            break;
        }
        const ssize_t n = sendmsg(sock, &msg, MSG_NOSIGNAL);
        if (n == (ssize_t)sizeof(h)) {
            res = kCubinOfferAccepted;
            break;
        }
        if (n < 0 && errno == EINTR) continue;
        // A partial header is not retried. Retrying would resend the
        // descriptor, and a header this end cannot vouch for is exactly the
        // kind of thing that turns into a wrong line table on the other side.
        res = (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
                  ? kCubinOfferTimedOut
                  : kCubinOfferError;
        break;
    }
    if (res != kCubinOfferAccepted) {
        close(sock);
        g_cubins_send_failed.fetch_add(1, std::memory_order_relaxed);
        return res;
    }

    char b = 0;
    for (;;) {
        if (!arm(sock, dl)) {
            res = kCubinOfferTimedOut;
            break;
        }
        const ssize_t n = read(sock, &b, 1);
        if (n == 1) {
            res = (b == kCubinReplyOK) ? kCubinOfferAccepted : kCubinOfferRefused;
            break;
        }
        if (n == 0) {
            res = kCubinOfferError;
            break;
        }
        if (errno == EINTR) continue;
        res = (errno == EAGAIN || errno == EWOULDBLOCK) ? kCubinOfferTimedOut
                                                        : kCubinOfferError;
        break;
    }
    close(sock);
    if (res != kCubinOfferAccepted) {
        g_cubins_send_failed.fetch_add(1, std::memory_order_relaxed);
    }
    return res;
}

CubinOfferResult cubin_offer(const char *name, const void *bytes, size_t len,
                             uint64_t crc, unsigned timeout_ms) {
    if (timeout_ms == 0) return kCubinOfferDisabled;
    const int fd = cubin_seal_bytes(bytes, len);
    if (fd < 0) {
        g_cubins_offered.fetch_add(1, std::memory_order_relaxed);
        g_cubins_send_failed.fetch_add(1, std::memory_order_relaxed);
        return kCubinOfferError;
    }
    const CubinOfferResult r = cubin_offer_fd(name, fd, (uint64_t)len, crc, timeout_ms);
    close(fd);
    return r;
}

CubinOfferResult cubin_offer_to_consumer(const void *bytes, size_t len,
                                         uint64_t crc, unsigned timeout_ms) {
    if (timeout_ms == 0) return kCubinOfferDisabled;
    char name[sizeof(((struct sockaddr_un *)nullptr)->sun_path)];
    const void *self = (const void *)&cubin_offer_to_consumer;
    if (!cubin_name_from_maps("/proc/self/maps", (unsigned long)self, name,
                              sizeof(name))) {
        return kCubinOfferNoAddress;
    }
    return cubin_offer(name, bytes, len, crc, timeout_ms);
}

unsigned cubin_timeout_ms(unsigned dflt) {
    const char *v = getenv("PERFAGENT_GPU_CUBIN_TIMEOUT_MS");
    if (!v || !*v) return dflt;
    char *end = nullptr;
    errno = 0;
    const long n = strtol(v, &end, 10);
    if (errno != 0 || end == v || *end != '\0' || n < 0 || n > 600000) return dflt;
    return (unsigned)n;
}

uint64_t cubins_offered() {
    return g_cubins_offered.load(std::memory_order_relaxed);
}

uint64_t cubins_send_failed() {
    return g_cubins_send_failed.load(std::memory_order_relaxed);
}

}  // namespace perfagent
