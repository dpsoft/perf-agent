package gpuprobe

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRegistrar stands in for ehmapsRegistrar. The real one compiles CFI and
// writes BPF maps, which needs CAP_BPF; this one records the calls so the
// question these tests ask - *when* is a PID registered, and what bounds the
// set - can be answered without any privilege at all.
type fakeRegistrar struct {
	mu           sync.Mutex
	registered   []uint32
	unregistered []uint32
	binaries     int   // what Register reports on success
	err          error // when non-nil, every Register fails with it
	unregErr     error
	gate         chan struct{} // when non-nil, Register blocks on it first
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{binaries: 3}
}

func (f *fakeRegistrar) Register(pid uint32) (int, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.registered = append(f.registered, pid)
	n, err := f.binaries, f.err
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (f *fakeRegistrar) Unregister(pid uint32) error {
	f.mu.Lock()
	f.unregistered = append(f.unregistered, pid)
	err := f.unregErr
	f.mu.Unlock()
	return err
}

func (f *fakeRegistrar) registeredPIDs() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.registered...)
}

func (f *fakeRegistrar) unregisteredPIDs() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.unregistered...)
}

// testRegistry builds a registry over a fake registrar and stops it when the
// test ends, so a leaked worker goroutine fails under -race rather than
// quietly outliving the test.
func testRegistry(t *testing.T, reg pidRegistrar, capacity int) *pidRegistry {
	t.Helper()
	r := newPIDRegistry(reg, capacity)
	t.Cleanup(r.close)
	return r
}

// eventuallyReady waits for the worker to finish a registration. Registration
// is deliberately off the drain goroutine (a CFI compile is 4ms for libc and
// 78ms for libcuda, measured), so every assertion about it is asynchronous.
func eventuallyReady(t *testing.T, r *pidRegistry, pid uint32) {
	t.Helper()
	require.Eventually(t, func() bool { return r.ready(pid) }, 2*time.Second, time.Millisecond,
		"pid %d never became ready", pid)
}

// --- Step 2: registration happens at the right lifecycle points.

// The eager path: Config.PID != 0 registers that PID synchronously, so the
// tables exist before the uprobe link that makes the probe fire. Attach itself
// needs CAP_BPF to get that far, so what is pinned here is the call it makes.
func TestRegisterNowInstallsTablesBeforeAnythingElseRuns(t *testing.T) {
	f := newFakeRegistrar()
	r := testRegistry(t, f, 8)

	n, err := r.registerNow(4242)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.True(t, r.ready(4242), "the eager path must not return before the tables are in")
	assert.Equal(t, []uint32{4242}, f.registeredPIDs())

	st, tracked := r.snapshot()
	assert.Equal(t, uint64(1), st.registered)
	assert.Equal(t, uint64(3), st.binariesAttached)
	assert.Equal(t, 1, tracked)
}

// The lazy path: a system-wide consumer learns a PID from the records that
// arrive and registers it then. Any batch counts as first sight, not just a
// sampled launch - see applyBatch for why.
func TestASystemWideConsumerRegistersAPIDOnItsFirstBatch(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})
	f := newFakeRegistrar()
	c.unwind = testRegistry(t, f, 8)

	// A plain launch batch - no sampled record, no stack.
	b, err := decodeBatch(launchBatchWith(777, 1, 2))
	require.NoError(t, err)
	c.applyBatch(b)

	eventuallyReady(t, c.unwind, 777)
	assert.Equal(t, []uint32{777}, f.registeredPIDs(),
		"first sight of a process is any batch from it, not its first sampled launch")

	// Every later batch from the same PID is a lookup, never a second
	// registration: re-compiling CFI per batch would be pathological.
	for range 5 {
		c.applyBatch(b)
	}
	assert.Equal(t, []uint32{777}, f.registeredPIDs())
	st, _ := c.unwind.snapshot()
	assert.Equal(t, uint64(1), st.registered)
}

func TestARegistrationThatInstallsNothingIsCountedNotRetriedForever(t *testing.T) {
	f := newFakeRegistrar()
	f.err = errors.New("read /proc/999/maps: no such process")
	r := testRegistry(t, f, 8)

	require.False(t, r.note(999))
	require.Eventually(t, func() bool {
		st, _ := r.snapshot()
		return st.failed == 1
	}, 2*time.Second, time.Millisecond)

	assert.False(t, r.ready(999))
	st, _ := r.snapshot()
	assert.Contains(t, st.lastErr, "no such process",
		"a bare failure count cannot tell an exited process from an unreadable /proc")

	// Later sightings do not re-run a failed registration.
	for range 10 {
		r.note(999)
	}
	assert.Equal(t, []uint32{999}, f.registeredPIDs())
}

// --- Step 2: the bound, and what happens at it.

func TestTheRegistrationSetIsBoundedAndEvictionsAreCounted(t *testing.T) {
	f := newFakeRegistrar()
	r := testRegistry(t, f, 3)

	for pid := uint32(1); pid <= 3; pid++ {
		r.note(pid)
		eventuallyReady(t, r, pid)
	}
	st, tracked := r.snapshot()
	require.Equal(t, 3, tracked)
	require.Zero(t, st.evicted)

	// Re-touch 1 and 3 so 2 is the least recently seen. Eviction is LRU on
	// last sighting, not FIFO on registration: the oldest registered PID on a
	// busy machine is usually the long-running GPU process that matters most.
	r.note(1)
	r.note(3)
	r.note(4)

	require.Eventually(t, func() bool {
		st, tracked := r.snapshot()
		return st.evicted == 1 && tracked == 3
	}, 2*time.Second, time.Millisecond)

	assert.False(t, r.ready(2), "the evicted PID no longer has tables")
	assert.True(t, r.ready(1))
	assert.True(t, r.ready(3))
	require.Eventually(t, func() bool {
		return len(f.unregisteredPIDs()) == 1
	}, 2*time.Second, time.Millisecond)
	assert.Equal(t, []uint32{2}, f.unregisteredPIDs(),
		"an evicted PID's tables must be released, or the bound bounds nothing")
}

func TestAnEvictedPIDCanBeRegisteredAgainOnItsNextBatch(t *testing.T) {
	f := newFakeRegistrar()
	r := testRegistry(t, f, 1)

	r.note(10)
	eventuallyReady(t, r, 10)
	r.note(20)
	eventuallyReady(t, r, 20)
	require.False(t, r.ready(10))

	r.note(10)
	eventuallyReady(t, r, 10)
	assert.Equal(t, []uint32{10, 20, 10}, f.registeredPIDs())
	st, tracked := r.snapshot()
	assert.Equal(t, 1, tracked, "the bound holds across re-registration")
	assert.Equal(t, uint64(2), st.evicted)
}

func TestAFullWorkQueueIsCountedAndThePIDRetriesLater(t *testing.T) {
	f := newFakeRegistrar()
	f.gate = make(chan struct{}) // hold the worker inside the first Register
	r := newPIDRegistry(f, unwindWorkDepth+64)
	defer func() {
		close(f.gate)
		r.close()
	}()

	// One request is taken by the worker (and blocks); the queue then holds
	// unwindWorkDepth more before it refuses.
	for pid := uint32(1); pid <= unwindWorkDepth+16; pid++ {
		r.note(pid)
	}
	st, tracked := r.snapshot()
	assert.Positive(t, st.requestsDropped, "a full queue must be counted, not absorbed")
	assert.Equal(t, unwindWorkDepth+16-int(st.requestsDropped), tracked,
		"a PID whose request was refused is forgotten, so its next batch retries")
}

// --- Step 3: a walk with no tables is visible.

// stackFlagsFor builds a one-record sampled batch whose capture carries the
// given walker flags, drives it through the consumer, and returns the Stats.
func walkWithFlags(t *testing.T, c *Consumer, sm *fakeStackStore, pid uint32, id uint32, flags uint32) {
	t.Helper()
	sm.entries[id] = []uint64{0x401000, 0x401100}
	sm.flags[id] = flags
	b, err := decodeBatch(sampledBatchWith(pid, 99, int32(id), 8))
	require.NoError(t, err)
	c.applyBatch(b)
}

func TestAWalkForAnUnregisteredPIDIsCountedNoTables(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	f := newFakeRegistrar()
	f.gate = make(chan struct{}) // registration never completes during the test
	c.unwind = newPIDRegistry(f, 8)
	defer func() {
		close(f.gate)
		c.unwind.close()
	}()

	walkWithFlags(t, c, sm, 555, 1, 0) // no DWARF_USED: a frame-pointer walk

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksWalkedFPOnly)
	assert.Equal(t, uint64(1), st.StacksWalkedNoTables,
		"a walk for a PID with no CFI tables must say so, not read as an ordinary FP walk")
	assert.Zero(t, st.StacksWalkedDWARF)
}

func TestAWalkForARegisteredPIDIsNotCountedNoTables(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	f := newFakeRegistrar()
	c.unwind = testRegistry(t, f, 8)
	_, err := c.unwind.registerNow(555)
	require.NoError(t, err)

	walkWithFlags(t, c, sm, 555, 1, 0)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksWalkedFPOnly)
	assert.Zero(t, st.StacksWalkedNoTables,
		"tables were installed; this FP-only walk is a property of the call path, not of registration")
}

func TestNoTablesIsNeverClaimedForADWARFWalk(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	c.unwind = testRegistry(t, newFakeRegistrar(), 8)

	walkWithFlags(t, c, sm, 555, 1, walkerFlagDWARFUsed)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksWalkedDWARF)
	assert.Zero(t, st.StacksWalkedFPOnly)
	assert.Zero(t, st.StacksWalkedNoTables,
		"a DWARF walk proves tables existed; the two counters are disjoint by construction")
}

func TestACFIMissIsCountedApartFromHavingNoTables(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	f := newFakeRegistrar()
	c.unwind = testRegistry(t, f, 8)
	_, err := c.unwind.registerNow(555)
	require.NoError(t, err)

	walkWithFlags(t, c, sm, 555, 1, walkerFlagCFIMiss)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksWalkedCFIMiss,
		"a table that does not cover this PC is a different problem from having no table")
	assert.Zero(t, st.StacksWalkedNoTables)
	assert.Equal(t, uint64(1), st.StackWalkAbandoned,
		"the walk stopped at the miss, so it is still an abandoned walk")
}

// --- Lifecycle.

func TestCloseIsIdempotentAndSafeOnANilRegistry(t *testing.T) {
	var nilReg *pidRegistry
	assert.NotPanics(t, nilReg.close)
	assert.False(t, nilReg.ready(1))
	assert.False(t, nilReg.note(1))
	st, tracked := nilReg.snapshot()
	assert.Equal(t, unwindStats{}, st)
	assert.Zero(t, tracked)

	r := newPIDRegistry(newFakeRegistrar(), 4)
	r.close()
	r.close()
	assert.False(t, r.note(7), "a closed registry accepts no new work")
}

func TestAClosedRegistryDoesNotStartNewRegistrations(t *testing.T) {
	f := newFakeRegistrar()
	r := newPIDRegistry(f, 4)
	r.close()
	r.note(1)
	r.note(2)
	assert.Empty(t, f.registeredPIDs())
}

// A consumer built without BPF objects has no registry at all, and must report
// that honestly rather than crediting walks with tables that do not exist.
func TestAConsumerWithNoRegistryReportsNoTables(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	require.Nil(t, c.unwind)

	walkWithFlags(t, c, sm, 555, 1, 0)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksWalkedNoTables)
	assert.Zero(t, st.UnwindPIDsTracked)
}
