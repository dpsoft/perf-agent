package ehmaps

import "testing"

func TestTableIDForBuildIDKnownValue(t *testing.T) {
	// FNV-1a 64-bit of 20 bytes of 0xAA. Known-value anchor; if the
	// calculation drifts, the test catches it.
	buildID := make([]byte, 20)
	for i := range buildID {
		buildID[i] = 0xAA
	}
	const want uint64 = 0x88ebb801b154ad85
	if got := TableIDForBuildID(buildID); got != want {
		t.Fatalf("TableIDForBuildID(0xAA*20) = %#x, want %#x", got, want)
	}
}

func TestTableIDForBuildIDDiffersByInput(t *testing.T) {
	a := TableIDForBuildID([]byte{1, 2, 3})
	b := TableIDForBuildID([]byte{1, 2, 4})
	if a == b {
		t.Fatalf("distinct inputs produced same table_id %#x", a)
	}
}

func TestTableIDForBuildIDEmpty(t *testing.T) {
	// FNV-1a offset basis for an empty input.
	const want uint64 = 0xcbf29ce484222325
	if got := TableIDForBuildID(nil); got != want {
		t.Fatalf("empty buildID = %#x, want %#x", got, want)
	}
}
