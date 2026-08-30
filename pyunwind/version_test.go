// pyunwind/version_test.go
package pyunwind

import "testing"

func TestDetectFromSoname(t *testing.T) {
	cases := []struct {
		path string
		want Version
		ok   bool
	}{
		{"/usr/local/lib/libpython3.12.so.1.0", Version{3, 12, 0}, true},
		{"/usr/lib64/libpython3.14.so.1.0", Version{3, 14, 0}, true},
		{"/usr/bin/python3.13", Version{3, 13, 0}, true},
		{"/usr/lib/libfoo.so", Version{}, false},
		{"/usr/local/lib/libpython3.11.so.1.0", Version{3, 11, 0}, true}, // detected...
	}
	for _, tc := range cases {
		got, ok := DetectFromSoname(tc.path)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("%s: got (%v,%v), want (%v,%v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
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
