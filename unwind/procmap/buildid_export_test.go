package procmap_test

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestExportedReadBuildID(t *testing.T) {
	id, err := procmap.ReadBuildID("/bin/ls")
	if err != nil {
		t.Skipf("/bin/ls not available or unreadable: %v", err)
	}
	if id == "" {
		t.Skip("/bin/ls has no build-id (some distros)")
	}
	if len(id)%2 != 0 || len(id) < 8 {
		t.Fatalf("ReadBuildID returned suspicious value: %q", id)
	}
}

func TestExportedResolverBuildID(t *testing.T) {
	r := procmap.NewResolver()
	defer r.Close()
	got := r.BuildID("/bin/ls")
	want, err := procmap.ReadBuildID("/bin/ls")
	if err != nil {
		t.Skipf("/bin/ls not available: %v", err)
	}
	if got != want {
		t.Fatalf("Resolver.BuildID = %q; ReadBuildID = %q (mismatch)", got, want)
	}
}
