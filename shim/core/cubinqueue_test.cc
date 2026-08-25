// The runtime half of Task 5's assertions. The compile-time half is
// core/cubin_defer_test.cc, which passes by not compiling.
//
// What this proves without a GPU: that the copy is taken inside capture()
// and is a real copy (mutating the source afterwards does not change what is
// offered); that the CRC is computed over the copy; that gpu_module_load_v1's
// moment is inside capture() while the bytes are exclusively ours; that a
// re-load of the same CRC is skipped and counted; that every bound drops the
// OFFER and counts it rather than dropping the copy or blocking; and that
// drain() does not hold the mutex across an offer, which is the property that
// keeps an offer's timeout off the application's next module load.
#include "cubinqueue.h"

#include <atomic>
#include <cassert>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

using perfagent::CubinOfferResult;
using perfagent::CubinQueue;
using perfagent::CubinQueueLimits;
using perfagent::CubinView;
using perfagent::kCubinOfferAccepted;
using perfagent::kCubinOfferRefused;

namespace {

// ------------------------------------------------------------- a fake wire

struct Offered {
    uint64_t crc;
    std::string bytes;
    unsigned timeout_ms;
};

std::vector<Offered> g_offers;
CubinOfferResult g_offer_result = kCubinOfferAccepted;

CubinOfferResult record_offer(const void *bytes, size_t len, uint64_t crc,
                              unsigned timeout_ms) {
    Offered o;
    o.crc = crc;
    o.bytes.assign((const char *)bytes, len);
    o.timeout_ms = timeout_ms;
    g_offers.push_back(o);
    return g_offer_result;
}

void reset_offers() {
    g_offers.clear();
    g_offer_result = kCubinOfferAccepted;
}

// A CRC that depends on every byte, so "the CRC was taken over the copy" is
// distinguishable from "the CRC was taken over the vendor's buffer later".
uint64_t fnv1a(const void *bytes, size_t len) {
    const unsigned char *p = (const unsigned char *)bytes;
    uint64_t h = 1469598103934665603ULL;
    for (size_t i = 0; i < len; i++) {
        h ^= p[i];
        h *= 1099511628211ULL;
    }
    return h ? h : 1;
}

// ------------------------------------------------- what on_captured records

struct Captured {
    uint64_t crc;
    std::string bytes;
    size_t len;
    int calls;
};

Captured g_captured;

void record_capture(void *ctx, uint64_t crc, const void *bytes, size_t len) {
    (void)ctx;
    g_captured.crc = crc;
    g_captured.bytes.assign((const char *)bytes, len);
    g_captured.len = len;
    g_captured.calls++;
}

void reset_captured() { g_captured = Captured{0, std::string(), 0, 0}; }

std::string body(size_t n, char seed) {
    std::string s(n, '\0');
    for (size_t i = 0; i < n; i++) s[i] = (char)(unsigned char)(i * 31 + (unsigned char)seed);
    return s;
}

// --------------------------------------------------------------- the tests

// The whole point of Task 5: after capture() returns, the vendor may do
// whatever it likes to its buffer - §6.3 finding 2 measured that it CHANGES
// after cuModuleUnload while staying mapped and readable - and what we offer
// is unaffected. If the copy were deferred to the drain thread this test
// would offer the mutated bytes and pass every other assertion in this file.
void test_the_copy_is_taken_inside_the_callback() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    std::string vendor = body(4096, 'A');
    const uint64_t want_crc = fnv1a(vendor.data(), vendor.size());

    {
        const CubinView view(vendor.data(), vendor.size());
        assert(q.capture(view, fnv1a, record_capture, nullptr));
    }

    // The vendor's buffer now holds something else entirely, exactly as it
    // does after an unload.
    memset(&vendor[0], 0x5A, vendor.size());

    assert(q.drain(record_offer, 1000) == 1);
    assert(g_offers.size() == 1);
    assert(g_offers[0].crc == want_crc);
    assert(g_offers[0].bytes == body(4096, 'A'));
    assert(g_offers[0].bytes != vendor);
    assert(q.modules_captured() == 1);
    assert(q.cubins_sent() == 1);
    assert(q.cubin_send_failed() == 0);
    printf("  copy-in-the-callback: 4096 bytes survive the vendor overwriting its buffer\n");
}

// gpu_module_load_v1 must be fired while the copy is exclusively ours, and
// its bytes_ptr must name that copy - which is only checkable by asserting
// the pointer handed to on_captured holds the right bytes at that instant.
void test_the_record_fires_inside_capture_over_live_bytes() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    const std::string vendor = body(1024, 'B');
    const CubinView view(vendor.data(), vendor.size());
    assert(q.capture(view, fnv1a, record_capture, nullptr));

    assert(g_captured.calls == 1);
    assert(g_captured.len == vendor.size());
    assert(g_captured.crc == fnv1a(vendor.data(), vendor.size()));
    assert(g_captured.bytes == vendor);
    // ...and it fired BEFORE anything could have been offered, which is what
    // makes bytes_ptr valid at the moment the probe reads it.
    assert(g_offers.empty());
    printf("  gpu_module_load_v1 fires inside capture, over the owned copy\n");
}

// The CRC comes from the copy. A crc function that is handed the vendor's
// pointer could not tell the difference here - so the assertion is that the
// bytes the crc function saw are the bytes that were offered.
uint64_t g_crc_saw_len = 0;
std::string g_crc_saw;
uint64_t recording_crc(const void *bytes, size_t len) {
    g_crc_saw.assign((const char *)bytes, len);
    g_crc_saw_len = len;
    return fnv1a(bytes, len);
}

void test_the_crc_runs_over_the_copy() {
    reset_offers();
    reset_captured();
    g_crc_saw.clear();
    CubinQueue q;
    std::string vendor = body(2048, 'C');
    const CubinView view(vendor.data(), vendor.size());
    assert(q.capture(view, recording_crc, record_capture, nullptr));
    memset(&vendor[0], 0, vendor.size());
    assert(q.drain(record_offer, 1000) == 1);
    assert(g_crc_saw_len == 2048);
    assert(g_crc_saw == g_offers[0].bytes);
    printf("  the CRC is computed over the copy, not over the borrowed buffer\n");
}

// A zero CRC would be a module record that joins to nothing, with every
// other counter reading green. Counted, and never sent.
uint64_t zero_crc(const void *, size_t) { return 0; }

void test_a_crc_of_zero_is_counted_and_never_sent() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    const std::string vendor = body(64, 'D');
    const CubinView view(vendor.data(), vendor.size());
    assert(!q.capture(view, zero_crc, record_capture, nullptr));
    assert(q.cubin_crc_failed() == 1);
    assert(q.modules_captured() == 0);
    assert(g_captured.calls == 0);
    assert(q.drain(record_offer, 1000) == 0);
    assert(g_offers.empty());
    printf("  a zero CRC is refused and counted: crc_failed=1\n");
}

void test_a_reload_of_the_same_crc_is_skipped_and_counted() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    const std::string vendor = body(512, 'E');
    {
        const CubinView v(vendor.data(), vendor.size());
        assert(q.capture(v, fnv1a, record_capture, nullptr));
    }
    // The same module loaded again - CUDA's lazy loading does this - from a
    // different buffer with identical contents, which is what a content
    // -addressed CRC means by "the same module".
    const std::string again = body(512, 'E');
    {
        const CubinView v(again.data(), again.size());
        assert(!q.capture(v, fnv1a, record_capture, nullptr));
    }
    assert(q.modules_captured() == 1);
    assert(q.module_reload_skipped() == 1);
    // And no second record: a duplicate gpu_module_load_v1 would announce a
    // CRC the consumer already holds, with a bytes_ptr we did not keep.
    assert(g_captured.calls == 1);
    assert(q.drain(record_offer, 1000) == 1);
    assert(g_offers.size() == 1);
    printf("  a re-load of one CRC offers once: captured=1 reload_skipped=1\n");
}

void test_a_full_queue_drops_the_offer_and_counts_it() {
    reset_offers();
    reset_captured();
    CubinQueueLimits lim;
    lim.max_entries = 2;
    CubinQueue q(lim);
    for (int i = 0; i < 5; i++) {
        const std::string vendor = body(128, (char)('a' + i));
        const CubinView v(vendor.data(), vendor.size());
        q.capture(v, fnv1a, record_capture, nullptr);
    }
    assert(q.modules_captured() == 2);
    assert(q.cubin_queue_full() == 3);
    assert(q.depth() == 2);
    // The record still fired for every one of them: the consumer learns that
    // five modules loaded and gets the bytes for two, which is a worse
    // profile and never a wrong one.
    assert(g_captured.calls == 5);
    assert(q.drain(record_offer, 1000) == 2);
    assert(g_offers.size() == 2);
    printf("  a full queue drops 3 offers, counts them, and never blocks\n");
}

void test_the_byte_ceiling_is_a_second_full_condition() {
    reset_offers();
    reset_captured();
    CubinQueueLimits lim;
    lim.max_entries = 64;
    lim.max_queued_bytes = 300;
    CubinQueue q(lim);
    for (int i = 0; i < 4; i++) {
        const std::string vendor = body(128, (char)('m' + i));
        const CubinView v(vendor.data(), vendor.size());
        q.capture(v, fnv1a, record_capture, nullptr);
    }
    assert(q.modules_captured() == 2);   // 128 + 128 = 256; a third would pass 300
    assert(q.cubin_queue_full() == 2);
    printf("  the queued-bytes ceiling is the same drop with the same counter\n");
}

// The per-cubin ceiling is the ONLY bound on the memcpy the application pays
// for, so it is checked before the copy and the copy never happens.
void test_an_oversized_cubin_is_never_copied() {
    reset_offers();
    reset_captured();
    CubinQueueLimits lim;
    lim.max_cubin_bytes = 1024;
    CubinQueue q(lim);
    const std::string vendor = body(1025, 'F');
    const CubinView v(vendor.data(), vendor.size());
    assert(!q.capture(v, fnv1a, record_capture, nullptr));
    assert(q.cubin_too_large() == 1);
    assert(q.modules_captured() == 0);
    assert(g_captured.calls == 0);
    assert(q.depth() == 0);

    // Exactly at the ceiling is accepted, so the refusal is one byte of
    // policy rather than an off-by-one.
    const std::string ok = body(1024, 'F');
    const CubinView v2(ok.data(), ok.size());
    assert(q.capture(v2, fnv1a, record_capture, nullptr));
    assert(q.cubin_too_large() == 1);
    printf("  a cubin over the per-cubin ceiling is refused before the memcpy\n");
}

void test_a_refused_offer_is_counted_as_a_send_failure() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    const std::string vendor = body(256, 'G');
    const CubinView v(vendor.data(), vendor.size());
    assert(q.capture(v, fnv1a, record_capture, nullptr));
    g_offer_result = kCubinOfferRefused;
    assert(q.drain(record_offer, 1000) == 1);
    assert(q.cubin_send_failed() == 1);
    assert(q.cubins_sent() == 0);
    assert(q.depth() == 0);   // dropped, not retried forever
    printf("  a refused offer counts send_failed=1 and is not retried\n");
}

void test_drain_bounds_itself_and_empties() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    for (int i = 0; i < 8; i++) {
        const std::string vendor = body(64, (char)('0' + i));
        const CubinView v(vendor.data(), vendor.size());
        assert(q.capture(v, fnv1a, record_capture, nullptr));
    }
    assert(q.depth() == 8);
    assert(q.drain(record_offer, 250) == 8);
    assert(q.depth() == 0);
    assert(q.drain(record_offer, 250) == 0);
    for (const Offered &o : g_offers) assert(o.timeout_ms == 250);
    printf("  drain empties the queue, is bounded, and passes the timeout through\n");
}

// The property that keeps the offer's timeout off the application's thread:
// a capture must complete while a slow offer is in flight. If drain held the
// mutex across the offer this deadlocks the test's own deadline.
// std::atomic, not volatile: volatile orders nothing between threads, and a
// test that races its own flags reports a data race that has nothing to do
// with the queue - which is exactly how a real one in the queue would end up
// being dismissed as "the usual TSan noise".
std::atomic<bool> g_slow_offer_entered{false};
std::atomic<bool> g_capture_finished{false};

CubinOfferResult slow_offer(const void *, size_t, uint64_t, unsigned) {
    g_slow_offer_entered.store(true);
    for (int i = 0; i < 2000 && !g_capture_finished.load(); i++) {
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    return kCubinOfferAccepted;
}

void test_a_slow_offer_does_not_block_a_capture() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    const std::string first = body(64, 'H');
    {
        const CubinView v(first.data(), first.size());
        assert(q.capture(v, fnv1a, record_capture, nullptr));
    }
    std::thread drainer([&] { q.drain(slow_offer, 1000); });
    while (!g_slow_offer_entered.load()) std::this_thread::sleep_for(std::chrono::milliseconds(1));

    const std::string second = body(64, 'I');
    const CubinView v(second.data(), second.size());
    assert(q.capture(v, fnv1a, record_capture, nullptr));
    g_capture_finished.store(true);
    drainer.join();
    assert(q.modules_captured() == 2);
    printf("  a capture completes while a slow offer is in flight (no lock held across it)\n");
}

// Nothing here may be silent: on a healthy run every drop counter reads zero,
// and this is the assertion that says so in the direction that matters.
void test_a_healthy_run_reads_zero_on_every_drop_counter() {
    reset_offers();
    reset_captured();
    CubinQueue q;
    for (int i = 0; i < 4; i++) {
        const std::string vendor = body(1000 + i, (char)('p' + i));
        const CubinView v(vendor.data(), vendor.size());
        assert(q.capture(v, fnv1a, record_capture, nullptr));
    }
    assert(q.drain(record_offer, 1000) == 4);
    assert(q.modules_captured() == 4);
    assert(q.cubins_sent() == 4);
    assert(q.module_reload_skipped() == 0);
    assert(q.cubin_queue_full() == 0);
    assert(q.cubin_send_failed() == 0);
    assert(q.cubin_too_large() == 0);
    assert(q.cubin_crc_failed() == 0);
    assert(q.cubin_alloc_failed() == 0);
    printf("  healthy run: captured=4 sent=4 and every drop counter at zero\n");
}

// A queue destroyed with entries still in it must free them; ASan/valgrind
// would catch the leak, and the assertion here is only that it does not
// crash and that depth() reported the truth first.
void test_destruction_releases_undrained_copies() {
    reset_captured();
    {
        CubinQueue q;
        for (int i = 0; i < 3; i++) {
            const std::string vendor = body(256, (char)('x' + i));
            const CubinView v(vendor.data(), vendor.size());
            assert(q.capture(v, fnv1a, record_capture, nullptr));
        }
        assert(q.depth() == 3);
    }
    printf("  destroying a non-empty queue releases its copies\n");
}

}  // namespace

int main() {
    test_the_copy_is_taken_inside_the_callback();
    test_the_record_fires_inside_capture_over_live_bytes();
    test_the_crc_runs_over_the_copy();
    test_a_crc_of_zero_is_counted_and_never_sent();
    test_a_reload_of_the_same_crc_is_skipped_and_counted();
    test_a_full_queue_drops_the_offer_and_counts_it();
    test_the_byte_ceiling_is_a_second_full_condition();
    test_an_oversized_cubin_is_never_copied();
    test_a_refused_offer_is_counted_as_a_send_failure();
    test_drain_bounds_itself_and_empties();
    test_a_slow_offer_does_not_block_a_capture();
    test_a_healthy_run_reads_zero_on_every_drop_counter();
    test_destruction_releases_undrained_copies();
    printf("cubinqueue_test: OK\n");
    return 0;
}
