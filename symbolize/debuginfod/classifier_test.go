package debuginfod

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestClassifierSkipPaths(t *testing.T) {
	c := newClassifier(nil /* cache unused for skip-path tests */, nil /* fetcher unused */)
	skipPaths := []string{"", "[vdso]", "[stack]", "[vsyscall]", "[heap]"}
	for _, p := range skipPaths {
		t.Run(p, func(t *testing.T) {
			m := procmap.Mapping{Path: p}
			got := c.classify(t.Context(), m)
			if got.route != routeSkip {
				t.Errorf("classify(%q) route = %v, want routeSkip", p, got.route)
			}
		})
	}
}

func TestClassifierTier2ProcessMode(t *testing.T) {
	tmp := t.TempDir()

	dwarfPresent := filepath.Join(tmp, "dwarf-bin")
	if err := writeELFWithSection(dwarfPresent, ".debug_info", []byte("placeholder dwarf payload")); err != nil {
		t.Fatal(err)
	}

	debugLinkBin := filepath.Join(tmp, "linked-bin")
	linkeeName := "linked-bin.debug"
	linkeePath := filepath.Join(tmp, linkeeName)
	if err := os.WriteFile(linkeePath, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	// Payload layout: NUL-terminated filename, padded to 4 bytes, then 4-byte CRC32.
	payload := append([]byte(linkeeName), 0)
	for len(payload)%4 != 0 {
		payload = append(payload, 0)
	}
	payload = append(payload, 0xde, 0xad, 0xbe, 0xef) // dummy CRC32
	if err := writeELFWithSection(debugLinkBin, ".gnu_debuglink", payload); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(nil, nil)
	cases := []struct {
		name string
		m    procmap.Mapping
		want routeKind
	}{
		{"dwarf in binary", procmap.Mapping{Path: dwarfPresent}, routeProcessMode},
		{"resolvable debug-link", procmap.Mapping{Path: debugLinkBin}, routeProcessMode},
		{"unreadable file-like path", procmap.Mapping{Path: "/nope/missing"}, routeProcessMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.classify(t.Context(), tc.m)
			if got.route != tc.want {
				t.Errorf("route = %v, want %v", got.route, tc.want)
			}
		})
	}
}
