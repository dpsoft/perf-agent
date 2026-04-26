package ehcompile

import (
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkCompile_Glibc(b *testing.B) {
	path := "/lib64/libc.so.6"
	if _, err := os.Stat(path); err != nil {
		b.Skip("/lib64/libc.so.6 not found")
	}
	b.ResetTimer()
	for b.Loop() {
		_, _, _, err := Compile(path)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile_HelloX86(b *testing.B) {
	if _, err := os.Stat("testdata/hello"); err != nil {
		b.Skip("testdata/hello fixture missing")
	}
	b.ResetTimer()
	for b.Loop() {
		_, _, _, err := Compile("testdata/hello")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile_HelloArm64(b *testing.B) {
	if _, err := os.Stat("testdata/hello_arm64.o"); err != nil {
		b.Skip("testdata/hello_arm64.o fixture missing")
	}
	b.ResetTimer()
	for b.Loop() {
		_, _, _, err := Compile("testdata/hello_arm64.o")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile_LargeRustRelease(b *testing.B) {
	// Locate the Rust release binary built by `make test-workloads`.
	// Cargo.toml's [package].name is "rust-workload".
	candidates := []string{
		"../../test/workloads/rust/target/release/rust-workload",
	}
	var path string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		b.Skipf("test/workloads/rust/target/release/rust-workload not found; run `make test-workloads`")
	}

	var entries []CFIEntry
	var ehFrameBytes int
	b.ResetTimer()
	for b.Loop() {
		var err error
		entries, _, ehFrameBytes, err = Compile(path)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(ehFrameBytes), "eh_frame_bytes/op")
	b.ReportMetric(float64(len(entries)), "entries/op")
}

func BenchmarkCompile_LibPython(b *testing.B) {
	// Find any libpython3.X.so on the system. Glob across common locations.
	var candidates []string
	for _, pat := range []string{
		"/lib/x86_64-linux-gnu/libpython3.*.so*",
		"/lib64/libpython3.*.so*",
		"/usr/lib64/libpython3.*.so*",
	} {
		matches, _ := filepath.Glob(pat)
		candidates = append(candidates, matches...)
	}

	var path string
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err != nil || fi.IsDir() {
			continue
		}
		// Resolve symlinks.
		real, err := filepath.EvalSymlinks(c)
		if err != nil {
			continue
		}
		path = real
		break
	}
	if path == "" {
		b.Skip("no libpython3.X.so found in standard locations")
	}

	var entries []CFIEntry
	var ehFrameBytes int
	b.ResetTimer()
	for b.Loop() {
		var err error
		entries, _, ehFrameBytes, err = Compile(path)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(ehFrameBytes), "eh_frame_bytes/op")
	b.ReportMetric(float64(len(entries)), "entries/op")
}
