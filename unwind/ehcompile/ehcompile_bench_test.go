package ehcompile

import (
	"os"
	"testing"
)

func BenchmarkCompile_Glibc(b *testing.B) {
	path := "/lib64/libc.so.6"
	if _, err := os.Stat(path); err != nil {
		b.Skip("/lib64/libc.so.6 not found")
	}
	b.ResetTimer()
	for b.Loop() {
		_, _, err := Compile(path)
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
		_, _, err := Compile("testdata/hello")
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
		_, _, err := Compile("testdata/hello_arm64.o")
		if err != nil {
			b.Fatal(err)
		}
	}
}
