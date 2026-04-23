package ehmaps_test

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// TestReadBuildIDRustWorkload checks that ReadBuildID returns a non-empty
// byte slice for the Rust workload binary committed to the repo. Exact
// bytes change per-build; just assert presence.
func TestReadBuildIDRustWorkload(t *testing.T) {
	const path = "../../test/workloads/rust/target/release/rust-workload"
	id, err := ehmaps.ReadBuildID(path)
	if err != nil {
		t.Skipf("rust workload not built (%v); skipping", err)
	}
	if len(id) == 0 {
		t.Fatalf("ReadBuildID returned empty slice for %s", path)
	}
	// GNU build-id is conventionally 20 bytes for sha1 / 16 for md5.
	if len(id) < 8 {
		t.Fatalf("build-id suspiciously short (%d bytes): %x", len(id), id)
	}
}

func TestReadBuildIDMissingFile(t *testing.T) {
	if _, err := ehmaps.ReadBuildID("/nonexistent/binary"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
