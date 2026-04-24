package dwarfagent

import "testing"

func TestHashPCsStable(t *testing.T) {
	a := hashPCs([]uint64{0x1000, 0x2000, 0x3000})
	b := hashPCs([]uint64{0x1000, 0x2000, 0x3000})
	if a != b {
		t.Fatalf("same input → different hash: %#x vs %#x", a, b)
	}
}

func TestHashPCsDiffersByContent(t *testing.T) {
	a := hashPCs([]uint64{0x1000, 0x2000, 0x3000})
	b := hashPCs([]uint64{0x1000, 0x2000, 0x3001})
	if a == b {
		t.Fatalf("different input → same hash: %#x", a)
	}
}

func TestHashPCsEmpty(t *testing.T) {
	const want uint64 = 0xcbf29ce484222325 // FNV-1a offset basis
	if got := hashPCs(nil); got != want {
		t.Fatalf("empty chain hash = %#x, want %#x", got, want)
	}
}
