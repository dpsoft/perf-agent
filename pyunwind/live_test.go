package pyunwind

import (
	"errors"
	"os"
	"testing"
)

// TestMuslSonameMatching pins the musl detection against the paths that
// actually appear in /proc/<pid>/maps, and against the glibc paths that
// must NOT match.
//
// It matters because the alternative to detecting musl is not "no Python
// frames" -- it is a walk through glibc's struct pthread offsets applied to
// musl's TSD, which returns a plausible pointer and produces invented
// frames. Only Offsets.Validate stands behind it, and that is a
// plausibility screen by design.
func TestMuslSonameMatching(t *testing.T) {
	musl := []string{
		"/lib/ld-musl-x86_64.so.1",
		"/lib/ld-musl-aarch64.so.1",
		"/usr/lib/libc.musl-x86_64.so.1",
		"/lib/libc.musl-aarch64.so.1",
	}
	for _, p := range musl {
		if !muslSonameRe.MatchString(p) {
			t.Errorf("%s was not recognised as musl", p)
		}
	}
	glibc := []string{
		"/usr/lib64/libc.so.6",
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/lib64/ld-linux-x86-64.so.2",
		"/usr/lib64/libpython3.12.so.1.0",
		"/usr/bin/python3.12",
		// A path that merely contains the word: a musl-named directory is
		// not a musl libc, and matching it would refuse a glibc target.
		"/opt/musl-toolchain/lib/libc.so.6",
	}
	for _, p := range glibc {
		if muslSonameRe.MatchString(p) {
			t.Errorf("%s was wrongly recognised as musl", p)
		}
	}
}

// RequireGlibc must return nil for this very process, which is glibc on
// every machine this test runs on. Without this the test above could pass
// against a matcher that matched nothing at all.
func TestRequireGlibcAcceptsThisProcess(t *testing.T) {
	if err := RequireGlibc(uint32(selfPID())); err != nil {
		if errors.Is(err, ErrUnsupportedLibc) {
			t.Fatalf("this test process was classified as musl: %v", err)
		}
		t.Skipf("cannot read own mappings: %v", err)
	}
}

func selfPID() int { return os.Getpid() }
