package pyunwind

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// TestClassifyReportsAMeasuredMicroVersion drives classify against a real
// interpreter file and requires the micro version to come back measured.
//
// This is the H2 fix at the point where it matters: Result.Version is what
// enrolment logs, so an operator reading "CPython 3.12.0" for a machine
// running 3.12.3 has been told the identity of a build that was never
// there.
func TestClassifyReportsAMeasuredMicroVersion(t *testing.T) {
	path := findSystemLibpython(t)
	res := classify(path)
	if res.Refused != "" {
		t.Skipf("%s: %s", path, res.Refused)
	}
	if res.Version.Micro == MicroUnknown {
		t.Fatalf("%s classified as %v; the micro version is readable from Py_Version and must be reported",
			path, res.Version)
	}
	fromELF, err := VersionFromELF(path)
	if err != nil {
		t.Fatalf("VersionFromELF(%s): %v", path, err)
	}
	if res.Version != fromELF {
		t.Fatalf("classify reported %v, the file says %v", res.Version, fromELF)
	}
}

// TestClassifyRefusesAVersionMismatch: the offset table is chosen by minor
// version, so a file whose name and whose Py_Version disagree must be
// refused rather than walked with the table its NAME selects.
//
// The mismatch is constructed by copying a real interpreter to a path named
// for a different minor version -- which is exactly the shape the hazard
// takes in the wild (a build system symlinking or copying an interpreter
// under a version-suffixed name).
func TestClassifyRefusesAVersionMismatch(t *testing.T) {
	real := findSystemLibpython(t)
	v, err := VersionFromELF(real)
	if err != nil {
		t.Skipf("%s has no readable Py_Version: %v", real, err)
	}
	wrongMinor := v.Minor + 1
	if wrongMinor > 14 {
		wrongMinor = 12
	}
	if wrongMinor == v.Minor {
		t.Fatalf("test constructed no actual mismatch for %v", v)
	}
	lie := filepath.Join(t.TempDir(), fmt.Sprintf("libpython%d.%d.so.1.0", v.Major, wrongMinor))
	in, err := os.ReadFile(real)
	if err != nil {
		t.Skipf("cannot read %s: %v", real, err)
	}
	if err := os.WriteFile(lie, in, 0o644); err != nil {
		t.Fatalf("write %s: %v", lie, err)
	}

	res := classify(lie)
	if res.Refused == "" {
		t.Fatalf("%s (really %v) was accepted as %v", lie, v, res.Version)
	}
	if !errors.Is(res.Reason, ErrVersionMismatch) {
		t.Fatalf("Reason = %v, want errors.Is(..., ErrVersionMismatch)", res.Reason)
	}

	// Control: the same bytes under their HONEST name must classify
	// cleanly, so the refusal above is about the disagreement and not about
	// the copy.
	honest := filepath.Join(t.TempDir(), fmt.Sprintf("libpython%d.%d.so.1.0", v.Major, v.Minor))
	if err := os.WriteFile(honest, in, 0o644); err != nil {
		t.Fatalf("write %s: %v", honest, err)
	}
	if res := classify(honest); res.Refused != "" {
		t.Fatalf("the same bytes under an honest name were refused: %s", res.Refused)
	}
}

// TestClassifyIdentifiesAnUnversionedInterpreter is M2: an interpreter
// whose path carries no version at all must be identified from its own
// Py_Version, not reported as "not an interpreter".
//
// /usr/local/bin/python, a pyenv shim and a conda environment all look like
// this, and before the second pass they came back found=false, which
// enrolment logged as nothing whatsoever.
func TestClassifyIdentifiesAnUnversionedInterpreter(t *testing.T) {
	real := findSystemLibpython(t)
	v, err := VersionFromELF(real)
	if err != nil {
		t.Skipf("%s has no readable Py_Version: %v", real, err)
	}
	in, err := os.ReadFile(real)
	if err != nil {
		t.Skipf("cannot read %s: %v", real, err)
	}
	// No version anywhere in the name, exactly like a pyenv shim.
	anon := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(anon, in, 0o755); err != nil {
		t.Fatalf("write %s: %v", anon, err)
	}
	if _, ok := DetectFromSoname(anon); ok {
		t.Fatalf("test fixture %s still carries a version in its path", anon)
	}

	res := classify(anon)
	if res.Refused != "" {
		t.Fatalf("%s (really CPython %v) was refused: %s", anon, v, res.Refused)
	}
	if res.Version != v {
		t.Fatalf("classified as %v, the file says %v", res.Version, v)
	}
}
