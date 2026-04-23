package ehmaps

import (
	"os"
	"testing"

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
