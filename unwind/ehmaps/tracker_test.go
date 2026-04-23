package ehmaps

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// TestTrackerAttachSelf attaches the tracker to the test process itself
// and verifies that at least one pid_mappings entry was written. On
// detach, the pid_mapping_lengths entry should disappear.
//
// requireBPFCaps is defined in ehmaps_runtime_test.go; do not redefine.
func TestTrackerAttachSelf(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}
	cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen := newTestMaps(t)
	defer closeAll(cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen)

	store := NewTableStore(cfi, cfiLen, cls, clsLen)
	tracker := NewPIDTracker(store, pidMaps, pidMapLen)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := tracker.Attach(uint32(os.Getpid()), self); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	var gotLen uint32
	if err := pidMapLen.Lookup(uint32(os.Getpid()), &gotLen); err != nil {
		t.Fatalf("pid_mapping_lengths lookup: %v", err)
	}
	if gotLen == 0 {
		t.Fatal("expected at least one pid_mappings entry, got zero")
	}

	if err := tracker.Detach(uint32(os.Getpid())); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := pidMapLen.Lookup(uint32(os.Getpid()), &gotLen); err == nil {
		t.Fatalf("pid_mapping_lengths still present after Detach: %d", gotLen)
	}
}

// newTestMaps creates S3-shape BPF maps that mirror bpf2go's output.
func newTestMaps(t *testing.T) (cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen *ebpf.Map) {
	t.Helper()
	const innerFlag = 0x1000 // BPF_F_INNER_MAP
	mk := func(spec *ebpf.MapSpec) *ebpf.Map {
		m, err := ebpf.NewMap(spec)
		if err != nil {
			t.Fatalf("NewMap %s: %v", spec.Type, err)
		}
		return m
	}
	cfi = mk(&ebpf.MapSpec{
		Type: ebpf.HashOfMaps, KeySize: 8, ValueSize: 4, MaxEntries: 4,
		InnerMap: &ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: CFIEntryByteSize, MaxEntries: 1, Flags: innerFlag},
	})
	cfiLen = mk(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 8, ValueSize: 4, MaxEntries: 4})
	cls = mk(&ebpf.MapSpec{
		Type: ebpf.HashOfMaps, KeySize: 8, ValueSize: 4, MaxEntries: 4,
		InnerMap: &ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: ClassificationByteSize, MaxEntries: 1, Flags: innerFlag},
	})
	clsLen = mk(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 8, ValueSize: 4, MaxEntries: 4})
	pidMaps = mk(&ebpf.MapSpec{
		Type: ebpf.HashOfMaps, KeySize: 4, ValueSize: 4, MaxEntries: 4,
		InnerMap: &ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: PIDMappingByteSize, MaxEntries: MaxPIDMappings, Flags: innerFlag},
	})
	pidMapLen = mk(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 4, ValueSize: 4, MaxEntries: 4})
	return
}

func closeAll(ms ...*ebpf.Map) {
	for _, m := range ms {
		if m != nil {
			_ = m.Close()
		}
	}
}

// TestTrackerAutoAttachOnMmap exercises the full S4 flow: launch a
// child process that, after a small delay, execs a different program
// (the inner `exec` fires MMAP2 events AFTER our watcher is attached —
// avoiding the child-startup race where libc's initial load happens
// before we can start the watcher). Run() should see those events and
// Attach the child automatically, populating pid_mappings.
func TestTrackerAutoAttachOnMmap(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}
	cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen := newTestMaps(t)
	defer closeAll(cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen)

	store := NewTableStore(cfi, cfiLen, cls, clsLen)
	tracker := NewPIDTracker(store, pidMaps, pidMapLen)

	// The inner `exec` runs inside the already-tracked PID after a
	// 300ms sleep. That re-exec fires fresh MMAP2 events for cat's
	// dynamic libraries — events the watcher, set up below, can see.
	child := exec.Command("/bin/sh", "-c", "sleep 0.3 && exec /bin/cat /dev/null")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()

	w, err := NewMmapWatcher(uint32(child.Process.Pid))
	if err != nil {
		t.Fatalf("NewMmapWatcher: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go tracker.Run(ctx, w)

	deadline := time.After(3 * time.Second)
	for {
		var gotLen uint32
		err := pidMapLen.Lookup(uint32(child.Process.Pid), &gotLen)
		if err == nil && gotLen > 0 {
			return // success
		}
		select {
		case <-deadline:
			t.Fatal("pid_mapping_lengths never got a non-zero entry for child PID")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestAttachAllMappings attaches to the test process itself (which is
// multi-binary — the Go test harness + blazesym.so + libc + ld.so +
// libpthread + etc.) and verifies that more than one cfi_lengths entry
// gets installed (i.e. AttachAllMappings found several unique binaries
// in /proc/self/maps and Attach'd each).
func TestAttachAllMappings(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}
	cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen := newTestMaps(t)
	defer closeAll(cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen)

	store := NewTableStore(cfi, cfiLen, cls, clsLen)
	tracker := NewPIDTracker(store, pidMaps, pidMapLen)

	n, err := AttachAllMappings(tracker, uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("AttachAllMappings: %v", err)
	}
	if n < 2 {
		t.Fatalf("AttachAllMappings installed %d tables, want >= 2 (main + at least one .so)", n)
	}

	installed := 0
	it := cfiLen.Iterate()
	var tid uint64
	var cnt uint32
	for it.Next(&tid, &cnt) {
		installed++
	}
	if installed != n {
		t.Fatalf("cfi_lengths has %d entries, AttachAllMappings claimed %d", installed, n)
	}
}
