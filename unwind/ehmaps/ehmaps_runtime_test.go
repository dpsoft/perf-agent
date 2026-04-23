package ehmaps

import (
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// TestPopulateCFIRoundtrip creates a minimal HASH_OF_MAPS + length HASH in
// userspace, populates them via PopulateCFI, and reads back the length to
// confirm installation. Skips without CAP_BPF.
//
// Note: we can't directly read the inner map via the outer map's userspace
// handle (cilium/ebpf returns the inner map's ID rather than a readable
// handle when looking up). The integration test in Task 9 validates the
// round-trip through the BPF walker end-to-end.
func TestPopulateCFIRoundtrip(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}

	outer, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.HashOfMaps,
		KeySize:    8,
		ValueSize:  4,
		MaxEntries: 4,
		InnerMap: &ebpf.MapSpec{
			Type:       ebpf.Array,
			KeySize:    4,
			ValueSize:  CFIEntryByteSize,
			MaxEntries: 1,
			Flags:      BPF_F_INNER_MAP,
		},
	})
	if err != nil {
		t.Fatalf("outer: %v", err)
	}
	defer outer.Close()

	lengths, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.Hash,
		KeySize:    8,
		ValueSize:  4,
		MaxEntries: 4,
	})
	if err != nil {
		t.Fatalf("lengths: %v", err)
	}
	defer lengths.Close()

	entries := []ehcompile.CFIEntry{
		{PCStart: 0x100, PCEndDelta: 0x40, CFAType: ehcompile.CFATypeSP, FPType: ehcompile.FPTypeOffsetCFA, CFAOffset: 16, FPOffset: -16, RAOffset: -8, RAType: ehcompile.RATypeOffsetCFA},
		{PCStart: 0x140, PCEndDelta: 0x20, CFAType: ehcompile.CFATypeFP, FPType: ehcompile.FPTypeSameValue, CFAOffset: 16, RAOffset: -8, RAType: ehcompile.RATypeOffsetCFA},
	}
	const tableID uint64 = 0xabc
	if err := PopulateCFI(PopulateCFIArgs{
		TableID:   tableID,
		Entries:   entries,
		OuterMap:  outer,
		LengthMap: lengths,
	}); err != nil {
		t.Fatalf("PopulateCFI: %v", err)
	}

	var gotLen uint32
	if err := lengths.Lookup(tableID, &gotLen); err != nil {
		t.Fatalf("length lookup: %v", err)
	}
	if gotLen != uint32(len(entries)) {
		t.Fatalf("length = %d, want %d", gotLen, len(entries))
	}
}

func requireBPFCaps(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	caps := cap.GetProc()
	have, err := caps.GetFlag(cap.Permitted, cap.BPF)
	if err != nil || !have {
		t.Skip("CAP_BPF not available")
	}
}
