// pyunwind/version_test.go
package pyunwind

import (
	"os"
	"testing"
)

// A soname carries no micro version, so DetectFromSoname must report
// MicroUnknown rather than a zero. A zero is a real version: Ubuntu noble's
// /usr/bin/python3.12 is 3.12.3, and it was reported as "3.12.0" -- a build
// that was never on the machine -- in the one field an operator uses to
// identify which interpreter a run walked.
func TestDetectFromSoname(t *testing.T) {
	cases := []struct {
		path string
		want Version
		ok   bool
	}{
		{"/usr/local/lib/libpython3.12.so.1.0", Version{3, 12, MicroUnknown}, true},
		{"/usr/lib64/libpython3.14.so.1.0", Version{3, 14, MicroUnknown}, true},
		{"/usr/bin/python3.13", Version{3, 13, MicroUnknown}, true},
		{"/usr/lib/libfoo.so", Version{}, false},
		{"/usr/local/lib/libpython3.11.so.1.0", Version{3, 11, MicroUnknown}, true}, // detected...
	}
	for _, tc := range cases {
		got, ok := DetectFromSoname(tc.path)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("%s: got (%v,%v), want (%v,%v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

// The rendering is the whole point of MicroUnknown, so it is pinned
// separately from the detection.
func TestVersionStringHidesAnUnmeasuredMicro(t *testing.T) {
	if got := (Version{3, 12, MicroUnknown}).String(); got != "3.12.x" {
		t.Fatalf("unmeasured micro renders %q, want 3.12.x", got)
	}
	if got := (Version{3, 12, 3}).String(); got != "3.12.3" {
		t.Fatalf("measured micro renders %q, want 3.12.3", got)
	}
	if got := (Version{3, 12, 0}).String(); got != "3.12.0" {
		t.Fatalf("a measured .0 renders %q, want 3.12.0 -- zero is a real micro version", got)
	}
}

// VersionFromELF against whatever interpreter this machine has: the version
// it reads must agree with the soname's major/minor, and it must carry a
// micro the soname could not have supplied.
//
// This is the test that would catch reading the wrong four bytes -- a
// mis-mapped file offset, or the high half of the unsigned long -- because
// the two sources are independent and both are real.
func TestVersionFromELFAgreesWithTheSoname(t *testing.T) {
	path := findSystemLibpython(t)
	fromELF, err := VersionFromELF(path)
	if err != nil {
		t.Fatalf("VersionFromELF(%s): %v", path, err)
	}
	fromName, ok := DetectFromSoname(path)
	if !ok {
		t.Skipf("%s carries no version in its path", path)
	}
	if fromELF.Major != fromName.Major || fromELF.Minor != fromName.Minor {
		t.Fatalf("%s: Py_Version says %v, the path says %v", path, fromELF, fromName)
	}
	if fromELF.Micro == MicroUnknown {
		t.Fatalf("%s: Py_Version produced no micro version", path)
	}
	t.Logf("%s: %v (path alone said %v)", path, fromELF, fromName)
}

// A file that is not CPython must be refused rather than reported as some
// version: the screen is on the VALUE, not just on the symbol's presence.
func TestVersionFromELFRefusesANonInterpreter(t *testing.T) {
	for _, path := range []string{"/bin/sh", "/usr/lib64/libc.so.6", "/lib/x86_64-linux-gnu/libc.so.6"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if v, err := VersionFromELF(path); err == nil {
			t.Fatalf("%s reported CPython %v", path, v)
		}
		return
	}
	t.Skip("no non-interpreter ELF found to test against")
}

// ...but 3.11 must not be SUPPORTED. Detection and support are separate:
// we want to say "3.11, which we refuse" rather than "unknown".
func TestSupportedRange(t *testing.T) {
	for _, v := range []Version{{3, 12, 14}, {3, 13, 15}, {3, 14, 3}} {
		if !v.Supported() {
			t.Fatalf("%v must be supported", v)
		}
	}
	for _, v := range []Version{{3, 11, 16}, {3, 10, 0}, {2, 7, 18}, {4, 0, 0}} {
		if v.Supported() {
			t.Fatalf("%v must NOT be supported", v)
		}
	}
}

func TestDetectFromPyVersionHex(t *testing.T) {
	// PY_VERSION_HEX layout: MAJOR<<24 | MINOR<<16 | MICRO<<8 | level<<4 | serial
	if got := DetectFromPyVersionHex(0x030c0e00); got != (Version{3, 12, 14}) {
		t.Fatalf("got %v, want 3.12.14", got)
	}
}
