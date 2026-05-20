package symbolize

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkParseKallsymsFresh measures end-to-end /proc/kallsyms
// parse cost. This is the cold-cache path that every perf-agent
// invocation pays on lockdown hosts before iter 6's disk cache
// shipped; the benchmark exists to catch regressions in the
// allocation-free parser (iter 5) and the 256 KiB read buffer
// (iter 3). Skips on hosts where kallsyms is unreadable.
//
// Reference numbers on a Ryzen 9 7940HS / Fedora 44 kernel
// 7.0.8-200, ~225k T/t/W/w/i symbols after filtering:
//   ~200 ms/op, 1 alloc-per-symbol (the Name string copy).
func BenchmarkParseKallsymsFresh(b *testing.B) {
	if !kallsymsReadable() {
		b.Skip("requires kptr_restrict=0")
	}
	b.ReportAllocs()
	for b.Loop() {
		s, err := parseKallsymsFresh()
		if err != nil {
			b.Fatalf("parseKallsymsFresh: %v", err)
		}
		if len(s.addrs) == 0 {
			b.Fatalf("empty result")
		}
	}
}

// BenchmarkLoadCachedKallsyms measures the warm-cache path —
// the disk format read + decode. Expected to be ~50x faster
// than BenchmarkParseKallsymsFresh; if the gap closes, the
// cache format or read path has regressed.
func BenchmarkLoadCachedKallsyms(b *testing.B) {
	if !kallsymsReadable() {
		b.Skip("requires kptr_restrict=0")
	}
	// Prime the cache once outside the timed loop.
	tmpDir := b.TempDir()
	cachePath := filepath.Join(tmpDir, "kallsyms.cache")
	prevPath := cachePathFn
	prevBoot := readBootIDFn
	cachePathFn = func() string { return cachePath }
	readBootIDFn = func() ([16]byte, error) { return [16]byte{1, 2, 3, 4}, nil }
	defer func() {
		cachePathFn = prevPath
		readBootIDFn = prevBoot
	}()
	fresh, err := parseKallsymsFresh()
	if err != nil {
		b.Fatalf("parseKallsymsFresh: %v", err)
	}
	if err := writeKallsymsCache(fresh); err != nil {
		b.Fatalf("writeKallsymsCache: %v", err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		b.Fatalf("cache not written: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		s, err := loadCachedKallsyms()
		if err != nil {
			b.Fatalf("loadCachedKallsyms: %v", err)
		}
		if len(s.addrs) == 0 {
			b.Fatalf("empty cache read")
		}
	}
}

// BenchmarkResolveKernelIPs measures the per-IP resolve cost
// (binary search + frame construction) at production batch
// size. The path is hot — every BPF kernel sample goes through
// it on lockdown hosts.
func BenchmarkResolveKernelIPs(b *testing.B) {
	if !kallsymsReadable() {
		b.Skip("requires kptr_restrict=0")
	}
	k, err := parseKallsymsFresh()
	if err != nil {
		b.Fatalf("parseKallsymsFresh: %v", err)
	}
	// Probe addresses spread across the kallsyms range so binary
	// search doesn't degenerate to one bucket.
	probes := make([]uint64, 0, 32)
	step := len(k.addrs) / 32
	if step == 0 {
		step = 1
	}
	for i := 0; i < len(k.addrs) && len(probes) < 32; i += step {
		probes = append(probes, k.addrs[i]+1) // +1 to land inside the function
	}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		frames := k.Resolve(probes)
		if len(frames) != len(probes) {
			b.Fatalf("frame count mismatch")
		}
	}
}
