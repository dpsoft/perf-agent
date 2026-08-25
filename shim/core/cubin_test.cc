// The producer half of the cubin transport, without a consumer: a hand-rolled
// abstract-socket receiver stands in for gpuprobe's listener.
//
// What this proves that the Go tests cannot: that the bytes THIS end puts on
// the wire are the 24 bytes the format says, in the order it says, and that
// the descriptor it hands over really carries all four seals. The Go tests
// prove the other direction against a Go producer; only a test that reads the
// C++ producer's own sendmsg can catch a struct-layout or endianness drift
// between the two halves.
#include "cubin.h"

#include "enroll.h"

#include <cassert>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

#include <fcntl.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

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

using perfagent::CubinOfferHeader;
using perfagent::CubinOfferResult;
using perfagent::cubin_name_from_maps;
using perfagent::cubin_offer;
using perfagent::cubin_offer_result_name;
using perfagent::cubin_seal_bytes;
using perfagent::cubin_timeout_ms;
using perfagent::cubins_offered;
using perfagent::cubins_send_failed;
using perfagent::kCubinOfferAccepted;
using perfagent::kCubinOfferDisabled;
using perfagent::kCubinOfferMagic;
using perfagent::kCubinOfferNoListener;
using perfagent::kCubinOfferRefused;
using perfagent::kCubinOfferVersion;
using perfagent::kCubinReplyOK;
using perfagent::kCubinReplyRefused;

namespace {

int bind_abstract(const char *name) {
    const int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    assert(fd >= 0);
    struct sockaddr_un sa;
    memset(&sa, 0, sizeof(sa));
    sa.sun_family = AF_UNIX;
    const size_t nlen = strlen(name);
    memcpy(sa.sun_path + 1, name, nlen);
    const socklen_t salen =
        (socklen_t)(offsetof(struct sockaddr_un, sun_path) + 1 + nlen);
    assert(bind(fd, (struct sockaddr *)&sa, salen) == 0);
    assert(listen(fd, 8) == 0);
    return fd;
}

// What one accepted offer looked like from the receiving side.
struct received {
    bool got = false;
    unsigned char raw[64] = {0};
    ssize_t raw_len = 0;
    int fd = -1;
    int seals = 0;
    std::string body;
};

// Accepts one offer, records exactly what arrived, and answers with `reply`.
std::thread receive_once(int lfd, received *out, char reply) {
    return std::thread([lfd, out, reply] {
        const int cfd = accept(lfd, nullptr, nullptr);
        if (cfd < 0) return;
        struct iovec iov;
        iov.iov_base = out->raw;
        iov.iov_len = sizeof(out->raw);
        union {
            char buf[CMSG_SPACE(sizeof(int) * 4)];
            struct cmsghdr align;
        } cmsg;
        memset(&cmsg, 0, sizeof(cmsg));
        struct msghdr msg;
        memset(&msg, 0, sizeof(msg));
        msg.msg_iov = &iov;
        msg.msg_iovlen = 1;
        msg.msg_control = cmsg.buf;
        msg.msg_controllen = sizeof(cmsg.buf);

        out->raw_len = recvmsg(cfd, &msg, 0);
        for (struct cmsghdr *c = CMSG_FIRSTHDR(&msg); c; c = CMSG_NXTHDR(&msg, c)) {
            if (c->cmsg_level != SOL_SOCKET || c->cmsg_type != SCM_RIGHTS) continue;
            memcpy(&out->fd, CMSG_DATA(c), sizeof(int));
        }
        if (out->fd >= 0) {
            out->seals = fcntl(out->fd, F_GET_SEALS, 0);
            char buf[4096];
            ssize_t n = 0;
            off_t off = 0;
            while ((n = pread(out->fd, buf, sizeof(buf), off)) > 0) {
                out->body.append(buf, (size_t)n);
                off += n;
            }
        }
        out->got = out->raw_len > 0;
        const ssize_t w = write(cfd, &reply, 1);
        (void)w;
        close(cfd);
    });
}

uint32_t le32(const unsigned char *p) {
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) |
           ((uint32_t)p[3] << 24);
}
uint16_t le16(const unsigned char *p) {
    return (uint16_t)((uint16_t)p[0] | ((uint16_t)p[1] << 8));
}
uint64_t le64(const unsigned char *p) {
    uint64_t v = 0;
    for (int i = 7; i >= 0; i--) v = (v << 8) | p[i];
    return v;
}

// The seals are the whole reason an offer is safe to map, so they are checked
// here on the descriptor that actually crossed the socket rather than on the
// one the producer still holds.
void test_seals_are_applied_and_survive_the_socket() {
    const std::string body(5 * 1024, 'z');
    const char *name = "perfagent-cubin-test.seals";
    const int lfd = bind_abstract(name);
    received got;
    std::thread t = receive_once(lfd, &got, kCubinReplyOK);

    const CubinOfferResult r = cubin_offer(name, body.data(), body.size(), 0xC0FFEEULL, 2000);
    t.join();
    close(lfd);
    if (got.fd >= 0) close(got.fd);

    assert(r == kCubinOfferAccepted);
    assert(got.got);
    assert(got.fd >= 0 && "the descriptor never crossed: SCM_RIGHTS did not go out");
    assert(got.seals >= 0 && "F_GET_SEALS failed: this is not a sealable memfd");
    assert((got.seals & F_SEAL_SEAL) && "F_SEAL_SEAL missing: the others can be removed again");
    assert((got.seals & F_SEAL_SHRINK) &&
           "F_SEAL_SHRINK missing: a peer could ftruncate under the consumer's mmap and SIGBUS it");
    assert((got.seals & F_SEAL_GROW) && "F_SEAL_GROW missing: the validated size is not the mapped size");
    assert((got.seals & F_SEAL_WRITE) && "F_SEAL_WRITE missing: the ELF can mutate under the parser");
    assert(got.body == body && "the memfd's contents are not the bytes offered");
}

// The 24 bytes on the wire, read as bytes rather than as a struct: a struct
// cast would agree with itself no matter what the layout was.
void test_the_header_is_the_documented_24_bytes() {
    const std::string body = "cubin";
    const char *name = "perfagent-cubin-test.header";
    const int lfd = bind_abstract(name);
    received got;
    std::thread t = receive_once(lfd, &got, kCubinReplyOK);

    const CubinOfferResult r =
        cubin_offer(name, body.data(), body.size(), 0x0123456789ABCDEFULL, 2000);
    t.join();
    close(lfd);
    if (got.fd >= 0) close(got.fd);

    assert(r == kCubinOfferAccepted);
    assert(got.raw_len == (ssize_t)sizeof(CubinOfferHeader));
    assert(le32(got.raw + 0) == kCubinOfferMagic);
    assert(got.raw[0] == 'C' && got.raw[1] == 'U' && got.raw[2] == 'B' && got.raw[3] == '1');
    assert(le16(got.raw + 4) == kCubinOfferVersion);
    assert(le16(got.raw + 6) == 0 && "flags are reserved and must go out as zero");
    assert(le64(got.raw + 8) == body.size());
    assert(le64(got.raw + 16) == 0x0123456789ABCDEFULL);
}

// A refusal is not a crash and not a retry: the module simply goes
// unresolvable, and the counter says so.
void test_a_refusal_is_counted_as_a_send_failure() {
    const uint64_t before_offered = cubins_offered();
    const uint64_t before_failed = cubins_send_failed();

    const std::string body = "refused";
    const char *name = "perfagent-cubin-test.refused";
    const int lfd = bind_abstract(name);
    received got;
    std::thread t = receive_once(lfd, &got, kCubinReplyRefused);

    const CubinOfferResult r = cubin_offer(name, body.data(), body.size(), 7, 2000);
    t.join();
    close(lfd);
    if (got.fd >= 0) close(got.fd);

    assert(r == kCubinOfferRefused);
    assert(cubins_offered() == before_offered + 1);
    assert(cubins_send_failed() == before_failed + 1);
}

// Nobody listening is the ordinary case in an unprofiled process, and it must
// cost nothing and count nothing as a failure: there was nothing to send to.
void test_no_listener_is_not_a_send_failure() {
    const uint64_t before_failed = cubins_send_failed();
    const std::string body = "x";
    const CubinOfferResult r =
        cubin_offer("perfagent-cubin-test.nobody-is-here", body.data(), body.size(), 1, 500);
    assert(r == kCubinOfferNoListener);
    assert(cubins_send_failed() == before_failed &&
           "an unprofiled process must not accumulate send failures");
}

// A timeout of 0 turns the channel off outright, and must do so before a
// memfd is created or an offer is counted.
void test_zero_timeout_disables_offers_entirely() {
    const uint64_t before_offered = cubins_offered();
    const std::string body = "x";
    assert(cubin_offer("perfagent-cubin-test.disabled", body.data(), body.size(), 1, 0) ==
           kCubinOfferDisabled);
    assert(cubins_offered() == before_offered);
}

// The two channels must never disagree about WHICH shim image they mean, so
// the cubin name is derived through the enrolment name's own parser. This
// pins that: same dev:inode tail, different prefix.
void test_the_two_channel_names_share_one_derivation() {
    char path[] = "/tmp/perfagent_cubin_mapsXXXXXX";
    const int fd = mkstemp(path);
    assert(fd >= 0);
    const char *body =
        "55e0d0a00000-55e0d0a01000 r--p 00000000 fd:01 100 /bin/thing\n"
        "7fc9a0a00000-7fc9a0a21000 r-xp 00001000 08:02 4242 /lib/libshim.so\n";
    const ssize_t n = write(fd, body, strlen(body));
    assert(n == (ssize_t)strlen(body));
    close(fd);

    char enroll_name[128] = {0};
    char cubin_name[128] = {0};
    assert(perfagent::enroll_name_from_maps(path, 0x7fc9a0a00100UL, enroll_name,
                                            sizeof(enroll_name)));
    assert(cubin_name_from_maps(path, 0x7fc9a0a00100UL, cubin_name, sizeof(cubin_name)));
    unlink(path);

    assert(strcmp(enroll_name, "perfagent-gpu-enroll.v1.8.2.4242") == 0);
    assert(strcmp(cubin_name, "perfagent-gpu-cubin.v1.8.2.4242") == 0);

    // And the tails are literally the same characters, not merely equal
    // today: that is what "one derivation" means.
    const char *etail = strrchr(enroll_name, 'v');
    const char *ctail = strrchr(cubin_name, 'v');
    assert(etail && ctail && strcmp(etail, ctail) == 0);

    // An anonymous mapping has no inode a uprobe could attach to, so it has
    // no name on either channel.
    char none[128] = {0};
    assert(!cubin_name_from_maps(path, 0x1UL, none, sizeof(none)));
    assert(none[0] == '\0');
}

void test_seal_bytes_refuses_nothing_to_seal() {
    assert(cubin_seal_bytes(nullptr, 0) < 0);
    const char b = 'x';
    assert(cubin_seal_bytes(&b, 0) < 0);
    const int fd = cubin_seal_bytes(&b, 1);
    assert(fd >= 0);
    const int seals = fcntl(fd, F_GET_SEALS, 0);
    assert((seals & (F_SEAL_SEAL | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_WRITE)) ==
           (F_SEAL_SEAL | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_WRITE));
    close(fd);
}

void test_timeout_env_parsing() {
    unsetenv("PERFAGENT_GPU_CUBIN_TIMEOUT_MS");
    assert(cubin_timeout_ms(1500) == 1500);
    setenv("PERFAGENT_GPU_CUBIN_TIMEOUT_MS", "250", 1);
    assert(cubin_timeout_ms(1500) == 250);
    // An explicit 0 is a choice, not an absent value.
    setenv("PERFAGENT_GPU_CUBIN_TIMEOUT_MS", "0", 1);
    assert(cubin_timeout_ms(1500) == 0);
    setenv("PERFAGENT_GPU_CUBIN_TIMEOUT_MS", "not-a-number", 1);
    assert(cubin_timeout_ms(1500) == 1500);
    unsetenv("PERFAGENT_GPU_CUBIN_TIMEOUT_MS");
}

void test_result_names_are_never_null() {
    for (int i = 0; i <= (int)perfagent::kCubinOfferError; i++) {
        const char *n = cubin_offer_result_name((CubinOfferResult)i);
        assert(n && *n);
    }
}

}  // namespace

int main() {
    test_the_two_channel_names_share_one_derivation();
    test_seal_bytes_refuses_nothing_to_seal();
    test_seals_are_applied_and_survive_the_socket();
    test_the_header_is_the_documented_24_bytes();
    test_a_refusal_is_counted_as_a_send_failure();
    test_no_listener_is_not_a_send_failure();
    test_zero_timeout_disables_offers_entirely();
    test_timeout_env_parsing();
    test_result_names_are_never_null();
    printf("cubin_test: OK\n");
    return 0;
}
