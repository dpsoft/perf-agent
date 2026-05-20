package perfdata

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAddSample_OnNewPID_FiresOncePerPID covers the contract that
// the OnNewPID callback fires exactly once per unique pid that
// AddSample observes — multiple samples for the same pid must NOT
// re-trigger the callback. The seenPIDs map is the dedup.
func TestAddSample_OnNewPID_FiresOncePerPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")
	w, err := Open(path, EventSpec{Type: 1, Config: 0, SamplePeriod: 99, Frequency: true}, MetaInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = w.Close(); _ = os.Remove(path) }()

	seen := map[uint32]int{}
	w.OnNewPID = func(pid uint32) { seen[pid]++ }

	w.AddSample(SampleRecord{Pid: 100, Tid: 100, IP: 0x1})
	w.AddSample(SampleRecord{Pid: 100, Tid: 100, IP: 0x2})
	w.AddSample(SampleRecord{Pid: 200, Tid: 200, IP: 0x3})
	w.AddSample(SampleRecord{Pid: 100, Tid: 100, IP: 0x4})
	w.AddSample(SampleRecord{Pid: 200, Tid: 200, IP: 0x5})

	if seen[100] != 1 {
		t.Errorf("pid=100 fired %d times, want 1", seen[100])
	}
	if seen[200] != 1 {
		t.Errorf("pid=200 fired %d times, want 1", seen[200])
	}
	if len(seen) != 2 {
		t.Errorf("saw %d distinct pids, want 2: %v", len(seen), seen)
	}
}

// TestAddSample_OnNewPID_SkipsSentinels covers the sentinel filter:
// pid=0 (no-pid / kernel-only sample) and pid=0xffffffff (kernel
// MMAP2 marker fed back through somehow) must NOT trigger the
// callback. Without this filter, a synthetic kernel sample at
// startup would burn one MMAP2 walk on a non-existent /proc/-1/maps.
func TestAddSample_OnNewPID_SkipsSentinels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")
	w, err := Open(path, EventSpec{Type: 1, Config: 0, SamplePeriod: 99, Frequency: true}, MetaInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = w.Close(); _ = os.Remove(path) }()

	fired := 0
	w.OnNewPID = func(pid uint32) { fired++ }
	w.AddSample(SampleRecord{Pid: 0, Tid: 0})
	w.AddSample(SampleRecord{Pid: 0xffffffff, Tid: 0xffffffff})
	if fired != 0 {
		t.Errorf("OnNewPID fired %d times for sentinels, want 0", fired)
	}
}

// TestAddSample_NilCallback_NoOp covers the zero-config case: no
// OnNewPID set, AddSample still encodes the sample. Sanity-check
// for the eager-walk migration path: existing --pid-mode call sites
// don't set OnNewPID and must keep working.
func TestAddSample_NilCallback_NoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")
	w, err := Open(path, EventSpec{Type: 1, Config: 0, SamplePeriod: 99, Frequency: true}, MetaInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w.AddSample(SampleRecord{Pid: 42, Tid: 42, IP: 0x1234})
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
