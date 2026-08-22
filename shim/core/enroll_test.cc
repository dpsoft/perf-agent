// The producer half of the #49 startup rendezvous, without a consumer: a
// hand-rolled abstract-socket server stands in for gpuprobe's listener.
#include "enroll.h"

#include <cassert>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <thread>

#include <atomic>
#include <csignal>
#include <ctime>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

using perfagent::enroll_connect;
using perfagent::enroll_name_from_maps;
using perfagent::enroll_result_name;
using perfagent::enroll_timeout_ms;
using perfagent::enroll_with_consumer;
using perfagent::EnrollResult;
using perfagent::kEnrollTimedOut;

namespace {

std::atomic<int> g_signals{0};
void count_signal(int) { g_signals.fetch_add(1); }

long mono_ms() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (long)t.tv_sec * 1000L + t.tv_nsec / 1000000L;
}

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

// Accepts one connection and answers with `reply`, or, when `reply` is 0,
// holds the connection open without answering at all.
std::thread serve_once(int lfd, char reply, unsigned hold_ms) {
    return std::thread([lfd, reply, hold_ms] {
        const int cfd = accept(lfd, nullptr, nullptr);
        if (cfd < 0) return;
        if (reply) {
            const ssize_t n = write(cfd, &reply, 1);
            (void)n;
        } else {
            usleep(hold_ms * 1000u);
        }
        close(cfd);
    });
}

std::string temp_maps(const char *body) {
    char path[] = "/tmp/perfagent_enroll_mapsXXXXXX";
    const int fd = mkstemp(path);
    assert(fd >= 0);
    const ssize_t n = write(fd, body, strlen(body));
    assert(n == (ssize_t)strlen(body));
    close(fd);
    return std::string(path);
}

}  // namespace

int main() {
    // ---- the name is derived from the mapping that contains the address --
    {
        const std::string p = temp_maps(
            "55e0d0a00000-55e0d0a01000 r--p 00000000 fd:01 100 /bin/thing\n"
            "7fc9a0a00000-7fc9a0a21000 r-xp 00001000 08:02 4242 /lib/libshim.so\n"
            "7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0 [stack]\n");
        char name[128];
        assert(enroll_name_from_maps(p.c_str(), 0x7fc9a0a10000UL, name, sizeof(name)));
        assert(std::string(name) == "perfagent-gpu-enroll.v1.8.2.4242");

        // A different image in the same file gives a different name, which is
        // what keeps two consumers watching two shim copies apart.
        assert(enroll_name_from_maps(p.c_str(), 0x55e0d0a00100UL, name, sizeof(name)));
        assert(std::string(name) == "perfagent-gpu-enroll.v1.253.1.100");

        // An anonymous mapping has no inode a uprobe could have attached to.
        assert(!enroll_name_from_maps(p.c_str(), 0x7ffd00000100UL, name, sizeof(name)));
        // An address in no mapping at all.
        assert(!enroll_name_from_maps(p.c_str(), 0x10UL, name, sizeof(name)));
        // A missing maps file is a clean false, not a crash.
        assert(!enroll_name_from_maps("/nonexistent/maps", 0x10UL, name, sizeof(name)));
        unlink(p.c_str());
    }

    // ---- 'K' means the tables are in ------------------------------------
    {
        const char *n = "perfagent-gpu-enroll.test.confirm";
        const int lfd = bind_abstract(n);
        std::thread t = serve_once(lfd, 'K', 0);
        assert(enroll_connect(n, 2000) == perfagent::kEnrollConfirmed);
        t.join();
        close(lfd);
    }

    // ---- 'X' means the consumer would not, and the producer still runs ---
    {
        const char *n = "perfagent-gpu-enroll.test.refuse";
        const int lfd = bind_abstract(n);
        std::thread t = serve_once(lfd, 'X', 0);
        assert(enroll_connect(n, 2000) == perfagent::kEnrollRefused);
        t.join();
        close(lfd);
    }

    // ---- nobody listening: the ordinary unprofiled case ------------------
    assert(enroll_connect("perfagent-gpu-enroll.test.nobody-at-all", 2000) ==
           perfagent::kEnrollNoListener);

    // ---- a consumer that accepts and never answers must not hang us ------
    {
        const char *n = "perfagent-gpu-enroll.test.silent";
        const int lfd = bind_abstract(n);
        std::thread t = serve_once(lfd, 0, 400);
        assert(enroll_connect(n, 60) == perfagent::kEnrollTimedOut);
        t.join();
        close(lfd);
    }

    // ---- a signal storm must not extend the budget -----------------------
    //
    // The regression test for the one outcome this design cannot tolerate: an
    // unbounded stall inside the profiled application's cuInit. A per-syscall
    // SO_RCVTIMEO is re-armed from zero on every EINTR, so a signal arriving
    // faster than the budget makes the wait never expire -- measured at 25s
    // and climbing against a 500ms budget. A profiled process producing
    // periodic signals is the normal case, not a corner: a Go runtime's
    // SIGURG, a JVM, setitimer, another profiler.
    //
    // SA_RESTART is deliberately NOT set, because that is what makes the read
    // return EINTR rather than being restarted by the kernel.
    {
        const char *n = "perfagent-gpu-enroll.test.signals";
        const int lfd = bind_abstract(n);
        std::thread t = serve_once(lfd, 0, 3000);

        struct sigaction sa;
        memset(&sa, 0, sizeof(sa));
        sa.sa_handler = count_signal;
        sigemptyset(&sa.sa_mask);
        sa.sa_flags = 0;
        struct sigaction old_sa;
        assert(sigaction(SIGALRM, &sa, &old_sa) == 0);
        struct itimerval it;
        memset(&it, 0, sizeof(it));
        it.it_interval.tv_usec = 50000;  // every 50ms, well inside the budget
        it.it_value.tv_usec = 50000;
        assert(setitimer(ITIMER_REAL, &it, nullptr) == 0);

        const long t0 = mono_ms();
        const EnrollResult r = enroll_connect(n, 300);
        const long elapsed = mono_ms() - t0;

        memset(&it, 0, sizeof(it));
        setitimer(ITIMER_REAL, &it, nullptr);
        sigaction(SIGALRM, &old_sa, nullptr);

        assert(g_signals.load() >= 2 && "the timer never fired; the test proves nothing");
        assert(r == kEnrollTimedOut);
        // The budget is 300ms; anything past a second means EINTR is
        // re-arming the wait rather than eating into one deadline.
        assert(elapsed < 1000 && "a signal storm extended the rendezvous past its budget");
        t.join();
        close(lfd);
    }

    // ---- the two phases share one budget, they do not each get it --------
    //
    // A listener that accepts and never answers spends the whole budget in
    // the read. A connect that also spent one would double the worst case,
    // which is what SO_SNDTIMEO plus SO_RCVTIMEO used to do.
    {
        const char *n = "perfagent-gpu-enroll.test.onebudget";
        const int lfd = bind_abstract(n);
        std::thread t = serve_once(lfd, 0, 2000);
        const long t0 = mono_ms();
        assert(enroll_connect(n, 200) == kEnrollTimedOut);
        const long elapsed = mono_ms() - t0;
        assert(elapsed < 400 && "the rendezvous spent more than its stated budget");
        t.join();
        close(lfd);
    }

    // ---- an empty or oversized name is refused, not truncated ------------
    assert(enroll_connect("", 100) == perfagent::kEnrollNoAddress);
    {
        const std::string huge(200, 'x');
        assert(enroll_connect(huge.c_str(), 100) == perfagent::kEnrollNoAddress);
    }

    // ---- the budget ------------------------------------------------------
    unsetenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS");
    assert(enroll_timeout_ms(2000) == 2000);
    setenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS", "150", 1);
    assert(enroll_timeout_ms(2000) == 150);
    // An explicit zero is "off", not "use the default" -- which is why this
    // is not shim/core's usual non-positive-means-default parse.
    setenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS", "0", 1);
    assert(enroll_timeout_ms(2000) == 0);
    assert(enroll_with_consumer(0) == perfagent::kEnrollDisabled);
    setenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS", "not-a-number", 1);
    assert(enroll_timeout_ms(2000) == 2000);
    unsetenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS");

    // ---- the self address resolves in a real process ---------------------
    // No consumer is bound for this test binary's own inode, so the only
    // correct answer is "nobody listening" -- and reaching that answer proves
    // the /proc/self/maps lookup found this image.
    assert(enroll_with_consumer(200) == perfagent::kEnrollNoListener);

    assert(std::string(enroll_result_name(perfagent::kEnrollConfirmed)) == "confirmed");
    printf("enroll_test OK\n");
    return 0;
}
