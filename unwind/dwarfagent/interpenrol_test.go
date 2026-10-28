package dwarfagent

import (
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// fakeEnroller stands in for *interp.Set: it recognises the PIDs in claim,
// records every call, and can hold Enroll open until released.
type fakeEnroller struct {
	mu       sync.Mutex
	claim    map[uint32]bool
	enrolls  []uint32
	detaches []uint32
	// When non-nil, Enroll signals entered and then blocks until release.
	entered chan uint32
	release chan struct{}
	called  chan uint32
}

func newFakeEnroller(claim ...uint32) *fakeEnroller {
	f := &fakeEnroller{claim: map[uint32]bool{}, called: make(chan uint32, 64)}
	for _, p := range claim {
		f.claim[p] = true
	}
	return f
}

func (f *fakeEnroller) Enroll(pid uint32, _ func(string, ...any)) bool {
	if f.entered != nil {
		f.entered <- pid
		<-f.release
	}
	f.mu.Lock()
	f.enrolls = append(f.enrolls, pid)
	ok := f.claim[pid]
	f.mu.Unlock()
	f.called <- pid
	return ok
}

func (f *fakeEnroller) Detach(pid uint32, _ func(string, ...any)) {
	f.mu.Lock()
	f.detaches = append(f.detaches, pid)
	f.mu.Unlock()
}

func (f *fakeEnroller) counts() (enrolls, detaches []uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.enrolls...), append([]uint32(nil), f.detaches...)
}

func waitEnroll(t *testing.T, f *fakeEnroller, want uint32) {
	t.Helper()
	select {
	case got := <-f.called:
		if got != want {
			t.Fatalf("Enroll(%d), want Enroll(%d)", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Enroll(%d) never happened", want)
	}
}

// settle waits for the worker to have taken pid's verdict, so the next event
// is ordered after it rather than racing it.
func settle(t *testing.T, l *lazyInterp, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		st := l.pids[pid]
		l.mu.Unlock()
		if st == pidClaimed || st == pidForeign {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pid %d never settled", pid)
}

func noLog(string, ...any) {}

func execMmap(pid uint32) ehmaps.MmapEventRecord {
	return ehmaps.MmapEventRecord{Kind: ehmaps.MmapEvent, PID: pid, TID: pid, Prot: 0x5, Filename: "/usr/bin/python3.14"}
}

func exit(pid uint32) ehmaps.MmapEventRecord {
	return ehmaps.MmapEventRecord{Kind: ehmaps.ExitEvent, PID: pid, TID: pid}
}

func TestLazyInterpEnrolsOncePerProcess(t *testing.T) {
	f := newFakeEnroller(100)
	l := newLazyInterp(f, noLog)
	for range 50 {
		l.sampled(100)
	}
	waitEnroll(t, f, 100)
	settle(t, l, 100)
	for range 50 {
		l.sampled(100)
	}
	l.sampled(0) // the idle task, never a process
	l.close()

	enrolls, _ := f.counts()
	if len(enrolls) != 1 {
		t.Fatalf("enrolls = %v, want exactly one look at pid 100", enrolls)
	}
	if !l.recognisedAny() {
		t.Fatal("recognisedAny false after a module recognised pid 100")
	}
}

// An exec into an interpreter shows up as executable mmaps of the new image.
// A process already judged foreign must be looked at again after one; a
// claimed process must not be.
func TestLazyInterpExecReArmsForeignOnly(t *testing.T) {
	f := newFakeEnroller(200)
	l := newLazyInterp(f, noLog)
	defer l.close()

	l.sampled(100) // foreign: a shell about to exec
	waitEnroll(t, f, 100)
	settle(t, l, 100)
	l.sampled(200) // claimed
	waitEnroll(t, f, 200)
	settle(t, l, 200)

	l.observe(execMmap(100))
	l.observe(execMmap(200))
	// A mapping that is not executable re-arms nothing.
	l.observe(ehmaps.MmapEventRecord{Kind: ehmaps.MmapEvent, PID: 100, TID: 100, Prot: 0x1, Filename: "/data"})

	l.sampled(100)
	waitEnroll(t, f, 100)
	l.sampled(200)
	select {
	case p := <-f.called:
		t.Fatalf("Enroll(%d) again; a claimed process is not re-armed by an mmap", p)
	case <-time.After(100 * time.Millisecond):
	}
}

// PID reuse: a claimed process's module state is detached at exit, and the
// PID is looked at afresh by its next occupant's first sample.
func TestLazyInterpExitDetachesClaimedAndForgets(t *testing.T) {
	f := newFakeEnroller(100)
	l := newLazyInterp(f, noLog)
	defer l.close()

	l.sampled(100)
	waitEnroll(t, f, 100)
	settle(t, l, 100)
	l.sampled(300) // foreign
	waitEnroll(t, f, 300)
	settle(t, l, 300)

	// A thread exit is not a process exit.
	l.observe(ehmaps.MmapEventRecord{Kind: ehmaps.ExitEvent, PID: 100, TID: 101})
	l.observe(exit(100))
	l.observe(exit(300))
	l.observe(exit(999)) // never seen

	if _, d := f.counts(); len(d) != 1 || d[0] != 100 {
		t.Fatalf("detaches = %v, want [100]: only a claimed process has module state", d)
	}
	l.sampled(100)
	waitEnroll(t, f, 100)
}

// The exit that lands while Enroll is running must not detach under it -- the
// record Enroll writes would survive -- but must not be lost either.
func TestLazyInterpExitDuringEnrolDetachesAfter(t *testing.T) {
	f := newFakeEnroller(100)
	f.entered = make(chan uint32)
	f.release = make(chan struct{})
	l := newLazyInterp(f, noLog)

	l.sampled(100)
	<-f.entered
	l.observe(exit(100))
	if _, d := f.counts(); len(d) != 0 {
		t.Fatalf("detached %v while Enroll was still running", d)
	}
	close(f.release)
	waitEnroll(t, f, 100)
	l.close()

	if _, d := f.counts(); len(d) != 1 || d[0] != 100 {
		t.Fatalf("detaches = %v, want [100] once Enroll returned", d)
	}
	l.mu.Lock()
	_, kept := l.pids[100]
	l.mu.Unlock()
	if kept {
		t.Fatal("pid 100 still tracked after it exited; a recycled pid would never be looked at")
	}
}

// A full queue defers a first look rather than losing it: the PID is not
// marked seen, so its next sample offers it again.
func TestLazyInterpFullQueueDefers(t *testing.T) {
	l := &lazyInterp{pids: map[uint32]pidInterpState{}, queue: make(chan uint32, 1)}
	l.sampled(100)
	l.sampled(200)
	if got := l.deferred.Load(); got != 1 {
		t.Fatalf("deferred = %d, want 1", got)
	}
	if _, seen := l.pids[200]; seen {
		t.Fatal("pid 200 marked seen although it never reached the queue")
	}
	<-l.queue
	l.sampled(200)
	if got := <-l.queue; got != 200 {
		t.Fatalf("queued %d, want 200 on its next sample", got)
	}
}

func TestLazyInterpNilIsNoop(t *testing.T) {
	var l *lazyInterp
	l.sampled(1)
	l.observe(exit(1))
	l.close()
	if l.recognisedAny() {
		t.Fatal("nil enroller recognised something")
	}
}
