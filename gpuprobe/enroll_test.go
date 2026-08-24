package gpuprobe

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"golang.org/x/sys/unix"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// The rendezvous is tested against a real socket and this test binary's own
// process, not against a mock of either. That is deliberate: the property
// under test - "the reply does not go out until the tables are installed" -
// lives in the ordering of two real operations, and the identity check it
// depends on (does the peer map the shim inode?) is exactly the kind of thing
// a mock would agree with while the real /proc parser disagreed.
//
// The trick that makes it possible without CAP_BPF or a GPU: point the
// listener at /proc/self/exe. The test binary genuinely maps that inode, so
// the peer check passes for real, and the registrar behind the registry is
// the same fake the rest of gpuprobe's unwind tests use.

// selfExe is the shim path a test listener attaches to: this process's own
// executable, which this process therefore provably maps.
func selfExe(t *testing.T) string {
	t.Helper()
	p, err := os.Readlink("/proc/self/exe")
	require.NoError(t, err)
	return p
}

// testEnrollListener binds a listener over a fake registrar and tears both
// down with the test, so a leaked goroutine or a leaked abstract address
// fails the next run rather than this one.
func testEnrollListener(t *testing.T, shimPath string, pid int, reg pidRegistrar) (*enrollListener, *pidRegistry) {
	t.Helper()
	r := newPIDRegistry(reg, 0)
	t.Cleanup(r.close)
	l, err := newEnrollListener(Config{ShimPath: shimPath, PID: pid}, r)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.close() })
	return l, r
}

// dialEnroll connects to a listener the way the shim does and returns the
// status byte, or 0 if the connection was closed without one.
func dialEnroll(t *testing.T, shimPath string) byte {
	t.Helper()
	addr, err := enrollAddress(shimPath)
	require.NoError(t, err)
	conn, err := net.DialTimeout("unix", addr, 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	var b [1]byte
	n, err := conn.Read(b[:])
	if err != nil || n != 1 {
		return 0
	}
	return b[0]
}

// The address is a pure function of the shim's identity on disk, which is
// what lets the two ends agree without exchanging anything - and what keeps
// two consumers watching two copies of the same shim apart. The gate makes a
// private copy of the stub per run for exactly that reason.
func TestTheRendezvousAddressIsTheShimsDeviceAndInode(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "shim-a")
	b := filepath.Join(dir, "shim-b")
	require.NoError(t, os.WriteFile(a, []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("b"), 0o600))

	addrA, err := enrollAddress(a)
	require.NoError(t, err)
	addrB, err := enrollAddress(b)
	require.NoError(t, err)

	assert.NotEqual(t, addrA, addrB,
		"two copies of a shim must not share a rendezvous: a consumer would then release a producer whose tables it never installed")

	again, err := enrollAddress(a)
	require.NoError(t, err)
	assert.Equal(t, addrA, again, "the address must be stable for one file")

	// The exact spelling is an ABI with shim/core/enroll.cc, which builds it
	// from the decimal major/minor and inode it reads out of
	// /proc/self/maps. Pin it here so a change on this side cannot silently
	// stop matching the producer.
	var st unix.Stat_t
	require.NoError(t, unix.Stat(a, &st))
	dev := uint64(st.Dev) //nolint:unconvert // matches enrollAddress
	assert.Equal(t,
		"@perfagent-gpu-enroll.v1."+itoa(uint64(unix.Major(dev)))+"."+itoa(uint64(unix.Minor(dev)))+"."+itoa(st.Ino),
		addrA)

	_, err = enrollAddress(filepath.Join(dir, "not-there"))
	assert.Error(t, err, "an unstattable shim has no address, and Attach must hear about it")
}

// The property the whole change rests on: the producer is not released until
// its tables are installed. A registrar that has not returned cannot have
// been followed by a reply.
func TestTheProducerIsNotReleasedUntilItsTablesAreInstalled(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	fake.gate = make(chan struct{})
	l, _ := testEnrollListener(t, shim, 0, fake)

	replied := make(chan byte, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		replied <- dialEnroll(t, shim)
	}()

	// The registrar is wedged, so no reply may have gone out.
	select {
	case b := <-replied:
		t.Fatalf("released with %q while registration was still running", b)
	case <-time.After(150 * time.Millisecond):
	}

	close(fake.gate)
	select {
	case b := <-replied:
		assert.Equal(t, byte(enrollReplyOK), b)
	case <-time.After(5 * time.Second):
		t.Fatal("never released after registration finished")
	}
	wg.Wait()

	assert.Equal(t, []uint32{uint32(os.Getpid())}, fake.registeredPIDs(),
		"the PID registered must be the one SO_PEERCRED reported, not one the peer sent")
	st := l.snapshot()
	assert.Equal(t, uint64(1), st.requests)
	assert.Equal(t, uint64(1), st.confirmed)
	assert.Zero(t, st.refused)
	assert.Zero(t, st.failed)
}

// A second rendezvous for a PID that already holds tables must not recompile
// them. ehmaps.PIDTracker.Attach appends mappings and takes a table reference
// every call, so a repeat would duplicate both - and the rendezvous address
// is reachable by anything on the machine, so "a repeat" is not hypothetical.
func TestASecondRendezvousDoesNotRecompile(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	l, r := testEnrollListener(t, shim, 0, fake)

	assert.Equal(t, byte(enrollReplyOK), dialEnroll(t, shim))
	assert.Equal(t, byte(enrollReplyOK), dialEnroll(t, shim))

	assert.Len(t, fake.registeredPIDs(), 1,
		"the second rendezvous recompiled: mappings and table references would both be duplicated")
	assert.Equal(t, uint64(2), l.snapshot().confirmed,
		"both producers were still told the tables are there, because they are")
	assert.True(t, r.ready(uint32(os.Getpid())))
}

// A registration that installs nothing releases the producer anyway - a
// degraded profile is never turned into no profile - and says so, both in the
// reply byte and in the counters.
func TestAFailedRegistrationStillReleasesTheProducer(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	fake.binaries = 0 // registers, installs nothing
	l, r := testEnrollListener(t, shim, 0, fake)

	assert.Equal(t, byte(enrollReplyRefused), dialEnroll(t, shim))
	st := l.snapshot()
	assert.Equal(t, uint64(1), st.requests)
	assert.Equal(t, uint64(1), st.failed)
	assert.Zero(t, st.confirmed)
	assert.NotEmpty(t, st.lastErr)

	// And the PID is marked enrolled even though it has no tables, which is
	// what makes StacksNoTablesAfterEnroll count this case instead of
	// letting it hide in the ordinary startup population.
	pid := uint32(os.Getpid())
	assert.True(t, r.enrolled(pid))
	assert.False(t, r.ready(pid))
}

// The identity check: a peer that does not map the shim this consumer
// attached to is refused without any registration at all. Without it, any
// local process could make a root profiler compile CFI for a PID of its
// choosing.
func TestAPeerThatDoesNotMapTheShimIsRefused(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "some-other-shim")
	require.NoError(t, os.WriteFile(shim, []byte("not mapped by anyone"), 0o600))

	fake := newFakeRegistrar()
	l, _ := testEnrollListener(t, shim, 0, fake)

	assert.Equal(t, byte(enrollReplyRefused), dialEnroll(t, shim))
	assert.Empty(t, fake.registeredPIDs(), "a refused peer must cost no compile at all")
	st := l.snapshot()
	assert.Equal(t, uint64(1), st.refused)
	assert.Zero(t, st.requests)
	assert.Contains(t, st.lastErr, "does not map the shim")
}

// A per-PID attach filters its uprobes to one process; the rendezvous address
// cannot be filtered the same way, so the listener has to do it.
func TestAPerPIDConsumerRefusesEveryOtherPID(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	// Some PID that is not ours.
	l, _ := testEnrollListener(t, shim, os.Getpid()+1, fake)

	assert.Equal(t, byte(enrollReplyRefused), dialEnroll(t, shim))
	assert.Empty(t, fake.registeredPIDs())
	assert.Contains(t, l.snapshot().lastErr, "is not the attached pid")
}

// Closing the consumer releases a producer that is still waiting, rather than
// leaving it parked on a socket nobody serves.
func TestCloseReleasesAWaitingProducer(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	fake.gate = make(chan struct{})
	l, _ := testEnrollListener(t, shim, 0, fake)

	got := make(chan byte, 1)
	go func() { got <- dialEnroll(t, shim) }()

	// Let the handler reach the wedged registrar, then let it through and
	// close underneath the reply.
	time.Sleep(100 * time.Millisecond)
	close(fake.gate)
	require.NoError(t, l.close())

	select {
	case <-got:
		// Either a status byte or a 0 for "closed without one"; both mean
		// the producer is running again, which is the requirement.
	case <-time.After(5 * time.Second):
		t.Fatal("a producer stayed parked after the consumer closed")
	}
}

// Two consumers cannot both own one shim's rendezvous. The second is told so
// rather than silently shadowing the first, because a consumer that thinks it
// has a rendezvous and does not is the failure mode #49 is about.
func TestASecondListenerForTheSameShimIsRefused(t *testing.T) {
	shim := selfExe(t)
	testEnrollListener(t, shim, 0, newFakeRegistrar())

	r := newPIDRegistry(newFakeRegistrar(), 0)
	t.Cleanup(r.close)
	_, err := newEnrollListener(Config{ShimPath: shim}, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bind")
}

// The /proc/<pid>/maps parser, against a fixture: the device is hex in /proc
// and decimal in every other API, and getting that wrong would refuse every
// genuine producer while looking like a working identity check.
func TestProcMapsInodeMatchIsDeviceAware(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "77"), 0o755))
	maps := "" +
		"55e0d0a00000-55e0d0a01000 r--p 00000000 fd:01 100 /bin/thing\n" +
		"7fc9a0a00000-7fc9a0a21000 r-xp 00001000 08:02 4242 /lib/libshim.so\n" +
		"7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0 [stack]\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "77", "maps"), []byte(maps), 0o600))

	dev := func(maj, min uint32) uint64 { return unix.Mkdev(maj, min) }

	ok, err := procMapsHaveInode(root, 77, dev(8, 2), 4242)
	require.NoError(t, err)
	assert.True(t, ok, "08:02 in /proc is major 8 minor 2, not 8 and 2 read as decimal from elsewhere")

	ok, err = procMapsHaveInode(root, 77, dev(253, 1), 100)
	require.NoError(t, err)
	assert.True(t, ok, "fd:01 is major 253 minor 1")

	// Right inode, wrong device: two filesystems can each have inode 4242.
	ok, err = procMapsHaveInode(root, 77, dev(9, 2), 4242)
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = procMapsHaveInode(root, 77, dev(8, 2), 4243)
	require.NoError(t, err)
	assert.False(t, ok)

	// The anonymous line must never match inode 0 for a shim that is not
	// there.
	ok, err = procMapsHaveInode(root, 77, dev(0, 0), 0)
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = procMapsHaveInode(root, 78, dev(8, 2), 4242)
	assert.Error(t, err, "a process that has exited must be an error, not a silent false")
}

// itoa without importing strconv into the assertion above, kept local so the
// pinned address string reads as one expression.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// The one seam neither side's own tests can cover: the wire.
//
// The rendezvous has no negotiation in it. The producer computes the socket
// name from /proc/self/maps in C++ (shim/core/enroll.cc) and the consumer
// computes it from stat(2) in Go (enrollAddress), and if the two spellings
// ever diverge every producer silently takes the pre-#49 lazy path - with no
// error anywhere, because "nobody is listening" is also what an unprofiled
// process sees. The same goes for the reply byte.
//
// So this builds a real producer out of shim/core, points a real listener at
// it, and runs it. It needs no BPF, no GPU and no privilege: the producer
// calls the rendezvous directly instead of waiting for a probe semaphore to
// arm it.
func TestTheCppProducerAndTheGoListenerAgreeOnTheWire(t *testing.T) {
	cxx, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("no c++ in PATH; this test builds the shim's producer half")
	}
	core, err := filepath.Abs(filepath.Join("..", "shim", "core"))
	require.NoError(t, err)
	if _, err := os.Stat(filepath.Join(core, "enroll.cc")); err != nil {
		t.Skipf("shim/core not present: %v", err)
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "producer.cc")
	require.NoError(t, os.WriteFile(src, []byte(`
#include "enroll.h"
#include <cstdio>
int main() {
    // Unconditional: in a real producer this is gated on the sampled-launch
    // probe's semaphore, which needs a live uprobe to arm.
    printf("%s\n", perfagent::enroll_result_name(
        perfagent::enroll_with_consumer(perfagent::enroll_timeout_ms(5000))));
    return 0;
}
`), 0o600))
	// Built where the test runs, not in /tmp only, so the binary's device and
	// inode are ordinary ones; the address embeds both.
	bin := filepath.Join(dir, "producer")
	build := exec.Command(cxx, "-std=c++17", "-O2", "-fvisibility=hidden",
		"-I", core, "-o", bin, src, filepath.Join(core, "enroll.cc"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build the producer half: %v\n%s", err, out)
	}

	fake := newFakeRegistrar()
	l, r := testEnrollListener(t, bin, 0, fake)

	run := exec.Command(bin)
	out, err := run.Output()
	require.NoError(t, err, "producer failed: %s", out)

	assert.Equal(t, "confirmed\n", string(out),
		"the producer did not get a confirmation. Either the two ends spell the socket name differently - C++ builds it from /proc/self/maps, Go from stat(2) - or the reply byte changed on one side only")

	// And the consumer registered the process that was waiting, not some
	// other one: the PID came from SO_PEERCRED, and the producer sent no
	// payload at all.
	pids := fake.registeredPIDs()
	require.Len(t, pids, 1)
	assert.Equal(t, uint64(1), l.snapshot().confirmed)
	assert.True(t, r.ready(pids[0]))
	assert.True(t, r.enrolled(pids[0]))
	assert.NotEqual(t, uint32(os.Getpid()), pids[0],
		"the registered PID is the test's own: the peer lookup is not reading the peer")
}

// The rendezvous serves one producer at a time, so the cost of admitting one
// is the cost of blocking every other. Any local user who can map the shim
// could otherwise fork-loop enrolments, occupy the single goroutine, and
// expire genuine producers' budgets - which puts them back on the pre-#49
// lazy path, i.e. it is an attack on the fix itself.
func TestOneUIDCannotMonopoliseTheRendezvous(t *testing.T) {
	now := time.Now()
	a := newEnrollAdmission(func() time.Time { return now })

	admitted := 0
	for range enrollUIDBurst * 4 {
		if a.admit(1000) {
			admitted++
		}
	}
	assert.Equal(t, enrollUIDBurst, admitted,
		"one uid spent more than its burst without any time passing")

	// A different uid is unaffected: the throttled one must not have drained
	// the shared bucket on requests it was never granted.
	assert.True(t, a.admit(1001),
		"a second uid was refused because the first exhausted itself")

	// And the bucket refills with time rather than resetting on a boundary.
	now = now.Add(time.Second)
	refilled := 0
	for range enrollUIDBurst * 2 {
		if a.admit(1000) {
			refilled++
		}
	}
	assert.Equal(t, enrollUIDBurst, refilled)
}

// The listener-wide bucket bounds a spray across many uids, which one per-uid
// bucket alone cannot.
func TestManyUIDsTogetherAreStillBounded(t *testing.T) {
	now := time.Now()
	a := newEnrollAdmission(func() time.Time { return now })
	admitted := 0
	for uid := uint32(0); uid < 200; uid++ {
		for range 4 {
			if a.admit(uid) {
				admitted++
			}
		}
	}
	assert.LessOrEqual(t, admitted, enrollTotalBurst,
		"a spray across uids escaped the listener-wide bound")
	assert.Positive(t, admitted)
}

// The defence must not become the exhaustion: the per-uid table is bounded
// too, and past the bound everything falls back to the shared bucket.
func TestThePerUIDTableIsBounded(t *testing.T) {
	now := time.Now()
	a := newEnrollAdmission(func() time.Time { return now })
	for uid := uint32(0); uid < 10_000; uid++ {
		a.admit(uid)
	}
	assert.LessOrEqual(t, len(a.perUID), enrollUIDTrackMax,
		"the rate limiter's own bookkeeping grew without bound")
}

// A legitimate burst - several CUDA processes starting together - must not be
// throttled, or the rate limit would cause the loss it exists to prevent.
func TestALegitimateBurstOfProducersIsNotThrottled(t *testing.T) {
	now := time.Now()
	a := newEnrollAdmission(func() time.Time { return now })
	for range 16 {
		require.True(t, a.admit(1000),
			"16 processes starting at once is an ordinary workload, not an attack")
	}
}

// End to end over a real socket: a throttled peer is refused instantly, told
// so, and costs no /proc read or compile at all.
func TestAThrottledPeerIsReleasedAndCountedNotServed(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	r := newPIDRegistry(fake, 0)
	t.Cleanup(r.close)
	// Built but not yet serving: the buckets are drained before any peer can
	// reach the listener, so nothing touches `admit` except serve().
	l, err := buildEnrollListener(Config{ShimPath: shim}, r)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.close() })
	for l.admit.admit(uint32(os.Getuid())) { //nolint:revive // drain until empty
	}
	l.start()

	assert.Equal(t, byte(enrollReplyRefused), dialEnroll(t, shim),
		"a throttled producer must still be released, not left parked")
	assert.Empty(t, fake.registeredPIDs(), "a throttled peer must cost no compile")
	st := l.snapshot()
	assert.Equal(t, uint64(1), st.throttled)
	assert.Zero(t, st.requests)
	assert.Zero(t, st.refused, "throttling is its own outcome, not an identity refusal")
	assert.Contains(t, st.lastErr, "throttled")
}

// The per-uid table must not be permanently occupiable by one-shot uids: 64
// of them would otherwise pin it forever and push every later producer -
// including the real one - onto the shared bucket alone.
func TestIdlePerUIDEntriesAreReclaimed(t *testing.T) {
	now := time.Now()
	a := newEnrollAdmission(func() time.Time { return now })
	for uid := uint32(0); uid < enrollUIDTrackMax; uid++ {
		require.True(t, a.admit(uid))
	}
	require.Len(t, a.perUID, enrollUIDTrackMax)

	// A window later every one of those buckets has refilled, so none of them
	// carries information a fresh bucket would not.
	now = now.Add(2 * time.Second)
	require.True(t, a.admit(9999))
	assert.LessOrEqual(t, len(a.perUID), enrollUIDTrackMax)
	b, ok := a.perUID[9999]
	require.True(t, ok, "a new uid was not tracked because the table was full of idle entries")
	assert.Less(t, b.tokens, float64(enrollUIDBurst),
		"the new uid must be charged against its own bucket, not waved through")

	// A uid that is ACTIVELY spending is not reclaimed out from under itself.
	now = now.Add(time.Millisecond)
	for range enrollUIDBurst {
		a.admit(7)
	}
	spent := a.perUID[7]
	require.NotNil(t, spent)
	for uid := uint32(1000); uid < 1000+enrollUIDTrackMax; uid++ {
		a.admit(uid)
	}
	if still, ok := a.perUID[7]; ok {
		assert.Less(t, still.tokens, float64(enrollUIDBurst),
			"an actively throttled uid had its bucket reset by the sweep")
	}
}

// An unprivileged consumer cannot read another user's /proc/<pid>/maps, so it
// could never have served that producer; pinning it to its own uid refuses
// nothing it could have done and closes the rendezvous to every other local
// user. A privileged one must NOT be pinned, or it would refuse the target in
// the commonest production shape - root profiler, application as its own user
// - and silently reinstate the loss #49 is about.
func TestOnlyAnUnprivilegedConsumerIsPinnedToItsOwnUID(t *testing.T) {
	got := enrollRequiredUID()
	if os.Geteuid() == 0 || hasPtraceCap() {
		assert.Nil(t, got,
			"a privileged consumer must serve any uid: it legitimately profiles other users' processes")
		return
	}
	require.NotNil(t, got,
		"an unprivileged consumer must serve only its own uid; it cannot read anyone else's maps anyway")
	assert.Equal(t, uint32(os.Geteuid()), *got)
}

// A peer whose uid the listener will not serve is refused before any /proc
// read, and released.
func TestAPeerOfAnotherUIDIsRefusedByAnUnprivilegedConsumer(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	r := newPIDRegistry(fake, 0)
	t.Cleanup(r.close)
	l, err := buildEnrollListener(Config{ShimPath: shim}, r)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.close() })
	// Force the unprivileged shape regardless of how this test is run.
	other := uint32(os.Geteuid()) + 1
	l.requireUID = &other
	l.start()

	assert.Equal(t, byte(enrollReplyRefused), dialEnroll(t, shim))
	assert.Empty(t, fake.registeredPIDs(), "a uid-refused peer must cost no compile")
	st := l.snapshot()
	assert.Equal(t, uint64(1), st.refused)
	assert.Zero(t, st.requests)
	assert.Contains(t, st.lastErr, "serves only uid")
}

// hasPtraceCap mirrors enrollRequiredUID's own probe so the test above states
// the same condition the code does rather than a paraphrase of it.
func hasPtraceCap() bool {
	caps := cap.GetProc()
	if caps == nil {
		return false
	}
	for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
		if have, err := caps.GetFlag(flag, cap.SYS_PTRACE); err == nil && have {
			return true
		}
	}
	return false
}

// The uid is an accounting key, not an authorisation check. A root profiler
// legitimately profiles other users' processes; refusing anything but its own
// euid would turn the rendezvous off in the commonest production shape.
func TestAPeerIsAuthorisedByTheShimItMapsNotByItsUID(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	testEnrollListener(t, shim, 0, fake)

	// This test process shares the listener's uid, so the uid path cannot
	// prove much on its own; what it can prove is that the identity that got
	// the peer in was the shim mapping, by taking the shim away.
	assert.Equal(t, byte(enrollReplyOK), dialEnroll(t, shim))
	require.Len(t, fake.registeredPIDs(), 1)

	other := filepath.Join(t.TempDir(), "other-shim")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o600))
	l2, _ := testEnrollListener(t, other, 0, newFakeRegistrar())
	assert.Equal(t, byte(enrollReplyRefused), dialEnroll(t, other),
		"same uid, same everything except the mapped inode: that is what must decide")
	assert.Equal(t, uint64(1), l2.snapshot().refused)
}
