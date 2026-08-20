#include "batch.h"
#include <cassert>
#include <cstdio>
#include <vector>

using perfagent::Batch;

struct Rec { uint64_t v; };

static std::vector<std::pair<unsigned long, unsigned long>> g_emits; // count, seq
static bool g_enabled = true;

static void fake_emit(const void *, unsigned long count, unsigned long seq) {
    g_emits.push_back({count, seq});
}
static bool fake_enabled() { return g_enabled; }

int main() {
    // A batch emits when it fills, and the sequence number advances per emit.
    {
        g_emits.clear(); g_enabled = true;
        Batch<Rec, 4> b(fake_emit, fake_enabled);
        for (int i = 0; i < 9; i++) assert(b.add(Rec{(uint64_t)i}));
        assert(g_emits.size() == 2);
        assert(g_emits[0].first == 4 && g_emits[0].second == 0);
        assert(g_emits[1].first == 4 && g_emits[1].second == 1);
        b.flush();
        assert(g_emits.size() == 3);
        assert(g_emits[2].first == 1 && g_emits[2].second == 2);
        assert(b.dropped() == 0);
    }
    // Flushing an empty batch emits nothing and does not burn a sequence number.
    {
        g_emits.clear(); g_enabled = true;
        Batch<Rec, 4> b(fake_emit, fake_enabled);
        b.flush(); b.flush();
        assert(g_emits.empty());
        assert(b.seq() == 0);
    }
    // With no consumer attached, adds are counted as drops, never buffered.
    {
        g_emits.clear(); g_enabled = false;
        Batch<Rec, 4> b(fake_emit, fake_enabled);
        for (int i = 0; i < 10; i++) assert(!b.add(Rec{(uint64_t)i}));
        b.flush();
        assert(g_emits.empty());
        assert(b.dropped() == 10);
    }
    // Records buffered while consumer attached, then consumer detaches before flush.
    {
        g_emits.clear(); g_enabled = true;
        Batch<Rec, 4> b(fake_emit, fake_enabled);
        for (int i = 0; i < 3; i++) assert(b.add(Rec{(uint64_t)i}));
        g_enabled = false;
        b.flush();
        assert(g_emits.empty());
        assert(b.dropped() == 3);
        assert(b.seq() == 0);
    }
    printf("batch_test OK\n");
    return 0;
}
