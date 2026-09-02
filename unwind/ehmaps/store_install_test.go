package ehmaps

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The refcount says who HOLDS a table; `installed` says the table is in the
// maps. Conflating them is a real defect with a misleading symptom: a second
// caller was told "already installed" the instant the first caller bumped the
// refcount, i.e. before a single row had been written, and its PID's stack
// walks then consulted a half-populated table. Those walks have mappings, so
// they never surface as "no tables" - they look like CFI misses instead.
//
// These are internal tests because the barrier is internal: AcquireBinary
// itself needs a real ELF and real BPF maps, and the property under test is
// the ordering, not the compile.

func TestASecondInstallerWaitsForTheFirstInsteadOfAssumingItFinished(t *testing.T) {
	s := NewTableStore(nil, nil)
	const tid uint64 = 0x1234

	if !s.beginInstall(tid) {
		t.Fatal("the first caller must be the one that installs")
	}

	second := make(chan bool, 1)
	go func() { second <- s.beginInstall(tid) }()

	select {
	case got := <-second:
		t.Fatalf("a second caller returned %v while the install was still in flight; "+
			"it would go on to use a table with no rows in it", got)
	case <-time.After(100 * time.Millisecond):
	}

	s.endInstall(tid, true)
	select {
	case got := <-second:
		if got {
			t.Fatal("the second caller recompiled a table that was already installed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second caller was never woken")
	}
}

func TestAFailedInstallLetsTheNextCallerTryAgain(t *testing.T) {
	s := NewTableStore(nil, nil)
	const tid uint64 = 0x1234

	if !s.beginInstall(tid) {
		t.Fatal("first caller")
	}
	s.endInstall(tid, false) // compile or populate failed

	if !s.beginInstall(tid) {
		t.Fatal("a table whose install FAILED must not be recorded as present: " +
			"the next caller would inherit rows that were never written")
	}
	s.endInstall(tid, true)
}

func TestAnEvictedTableIsNoLongerConsideredInstalled(t *testing.T) {
	s := NewTableStore(nil, nil)
	const tid uint64 = 0x1234

	if !s.beginInstall(tid) {
		t.Fatal("first caller")
	}
	s.endInstall(tid, true)
	if s.beginInstall(tid) {
		t.Fatal("an installed table must not be recompiled")
	}

	s.forgetInstall(tid) // what ReleaseBinary does once the refcount hits zero
	if !s.beginInstall(tid) {
		t.Fatal("after eviction the rows are gone from the maps; the next acquire must compile again")
	}
	s.endInstall(tid, true)
}

// Distinct tables never block each other: the barrier is per-table, so one
// slow libcuda compile must not serialize every other binary in the process.
func TestInstallsOfDifferentTablesDoNotBlockEachOther(t *testing.T) {
	s := NewTableStore(nil, nil)
	if !s.beginInstall(1) {
		t.Fatal("table 1")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !s.beginInstall(2) {
			t.Error("table 2 was refused while table 1 was installing")
		}
		s.endInstall(2, true)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an install of one table blocked an install of another")
	}
	s.endInstall(1, true)
}

// Under contention exactly one caller compiles, and nobody proceeds early.
func TestExactlyOneCallerInstallsUnderContention(t *testing.T) {
	s := NewTableStore(nil, nil)
	const tid uint64 = 0x1234
	const n = 16

	var mu sync.Mutex
	installs := 0
	finishedBeforeInstall := 0
	installDone := false

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.beginInstall(tid) {
				mu.Lock()
				installs++
				mu.Unlock()
				time.Sleep(20 * time.Millisecond) // stand in for the compile
				mu.Lock()
				installDone = true
				mu.Unlock()
				s.endInstall(tid, true)
				return
			}
			mu.Lock()
			if !installDone {
				finishedBeforeInstall++
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if installs != 1 {
		t.Fatalf("%d callers compiled the same table; the barrier is not exclusive", installs)
	}
	if finishedBeforeInstall != 0 {
		t.Fatalf("%d callers were told the table was ready before it was written", finishedBeforeInstall)
	}
}

// The bug the barrier tests above could not see, because they drive the
// primitives and it lives in the caller.
//
// AcquireBinary uses named return values, and every failure path after the
// claim returns `0, false, err` - which ASSIGNS the named tableID before the
// deferred endInstall runs. A defer closing over that name therefore released
// the claim on table 0 and left the real tableID marked in flight forever,
// with no timeout and no cancellation on the next caller.
//
// The trigger is not exotic. ehcompile.Compile returns ErrNoEHFrame for any
// ELF without .eh_frame - a case AttachAllMappings expects, logs and skips -
// so the first process mapping such a library poisoned its tableID and the
// second wedged. In perf_dwarf and offcpu_dwarf the wedged caller is the
// PIDTracker.Run goroutine, which also services exit events and Detach, so
// mmap tracking and PID teardown stop with it. Reachable on any system-wide
// DWARF run, GPU or not.
func TestASecondAcquireAfterAFailedInstallDoesNotWedge(t *testing.T) {
	// Both failure arms, because the clobber is on every return after the
	// claim: the first fails in ehcompile, the second gets past a successful
	// compile and fails in the populate (the store's maps are nil).
	for _, tc := range []struct{ name, path string }{
		{name: "compile fails (no .eh_frame)", path: elfWithoutEHFrame(t)},
		{name: "populate fails after a good compile", path: "../ehcompile/testdata/hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewTableStore(nil, nil)

			if _, _, err := s.AcquireBinary(tc.path, "", 1); err == nil {
				t.Fatalf("expected the first acquire to fail; the test proves nothing otherwise")
			}

			// The claim must be gone. Asserting on the internals as well as on
			// the behaviour, because "the second call returned" could also be
			// true for the wrong reason.
			s.instMu.Lock()
			inflight := len(s.inflight)
			s.instMu.Unlock()
			if inflight != 0 {
				t.Fatalf("%d table(s) still claimed after a failed install; the next acquire for that build-id blocks forever", inflight)
			}

			done := make(chan error, 1)
			go func() {
				_, _, err := s.AcquireBinary(tc.path, "", 2)
				done <- err
			}()
			select {
			case <-done:
				// Errors again, which is correct - what matters is that it returned.
			case <-time.After(5 * time.Second):
				t.Fatal("a second acquire for the same binary never returned: the failed install left its table claimed")
			}
		})
	}
}

// elfWithoutEHFrame builds a real ELF that has a build-id (so ReadBuildID
// succeeds and the claim IS taken) but no .eh_frame (so ehcompile fails).
// That is the shape of a stripped vendor library, and it is the realistic
// trigger for the clobber above.
func elfWithoutEHFrame(t *testing.T) string {
	t.Helper()
	objcopy, err := exec.LookPath("objcopy")
	if err != nil {
		t.Skip("no objcopy; cannot build an ELF without .eh_frame")
	}
	src := "../ehcompile/testdata/hello"
	if _, err := os.Stat(src); err != nil {
		t.Skipf("testdata: %v", err)
	}
	out := filepath.Join(t.TempDir(), "no_eh_frame")
	cmd := exec.Command(objcopy, "--remove-section=.eh_frame",
		"--remove-section=.eh_frame_hdr", src, out)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("objcopy: %v\n%s", err, o)
	}
	f, err := elf.Open(out)
	if err != nil {
		t.Skipf("open stripped elf: %v", err)
	}
	defer func() { _ = f.Close() }()
	if f.Section(".eh_frame") != nil {
		t.Skip("objcopy did not remove .eh_frame")
	}
	if _, err := ReadBuildID(out); err != nil {
		t.Skipf("the fixture has no build-id, so no claim would be taken: %v", err)
	}
	return out
}
