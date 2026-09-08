package gpuprobe

import (
	"os"
	"testing"
)

// The check has to answer both ways for the SAME process, or it cannot
// distinguish a shim the driver loaded from one it silently refused -- which
// is the entire reason it exists. CUDA_INJECTION64_PATH fails open: a driver
// that cannot load the library carries on as though the variable were unset,
// so "the shim is broken" and "this workload launched no kernels" produce
// identical output, and only the mapping tells them apart.
func TestShimIsMappedInAnswersBothWaysForOneProcess(t *testing.T) {
	// libc is mapped into this test binary by construction.
	mapped := "/lib64/libc.so.6"
	if _, err := os.Stat(mapped); err != nil {
		mapped = "/lib/x86_64-linux-gnu/libc.so.6"
		if _, err := os.Stat(mapped); err != nil {
			t.Skip("no libc at a known path on this machine")
		}
	}
	ok, err := ShimIsMappedIn(os.Getpid(), mapped)
	if err != nil {
		t.Fatalf("ShimIsMappedIn(libc): %v", err)
	}
	if !ok {
		t.Errorf("libc is not reported as mapped into this process; the check cannot " +
			"confirm an injection that DID happen, so it would report every run as broken")
	}

	// A real ELF that this process certainly has not loaded. Using a file
	// rather than a fabricated path keeps the negative honest: the identity
	// is resolved from a genuine dev/ino, exactly as the positive case is.
	notMapped := "/bin/true"
	if _, err := os.Stat(notMapped); err != nil {
		t.Skip("no /bin/true")
	}
	ok, err = ShimIsMappedIn(os.Getpid(), notMapped)
	if err != nil {
		t.Fatalf("ShimIsMappedIn(/bin/true): %v", err)
	}
	if ok {
		t.Errorf("/bin/true is reported as mapped into this process; the check would " +
			"confirm an injection that never happened, which is worse than not checking")
	}
}

// A path that is not a file must be an error rather than a confident "no":
// answering "not injected" because the shim path was mistyped would send the
// reader looking at the driver instead of at their own command line.
func TestShimIsMappedInRefusesAPathItCannotIdentify(t *testing.T) {
	if _, err := ShimIsMappedIn(os.Getpid(), "/nonexistent/shim.so"); err == nil {
		t.Error("a missing shim path must be an error, not a negative answer")
	}
}
