package elfsym

import "testing"

func TestParseLibpythonSONAME(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantMajor   int
		wantMinor   int
		wantIsPy    bool
	}{
		{"py312_typical", "/usr/lib/x86_64-linux-gnu/libpython3.12.so.1.0", 3, 12, true},
		{"py312_no_minor_suffix", "/usr/lib/libpython3.12.so", 3, 12, true},
		{"py313_future", "/usr/lib/libpython3.13.so.1.0", 3, 13, true},
		{"py399_far_future", "/usr/lib/libpython3.99.so", 3, 99, true},
		{"py311_too_old", "/usr/lib/libpython3.11.so.1.0", 3, 11, true},
		{"py27_legacy", "/usr/lib/libpython2.7.so", 2, 7, true},
		{"non_python_lib", "/usr/lib/libfoo.so.1", 0, 0, false},
		{"non_python_lib_with_python_substr", "/opt/mypython-helper.so", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, ok := ParseLibpythonSONAME(tc.path)
			if ok != tc.wantIsPy {
				t.Fatalf("ParseLibpythonSONAME(%q): ok=%v, want %v", tc.path, ok, tc.wantIsPy)
			}
			if major != tc.wantMajor || minor != tc.wantMinor {
				t.Fatalf("ParseLibpythonSONAME(%q): major=%d minor=%d, want %d %d",
					tc.path, major, minor, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

func TestIsPython312Plus(t *testing.T) {
	tests := []struct {
		major int
		minor int
		want  bool
	}{
		{3, 12, true},
		{3, 13, true},
		{3, 99, true},
		{4, 0, true},
		{3, 11, false},
		{3, 10, false},
		{2, 7, false},
		{0, 0, false},
	}
	for _, tc := range tests {
		got := IsPython312Plus(tc.major, tc.minor)
		if got != tc.want {
			t.Fatalf("IsPython312Plus(%d,%d) = %v, want %v",
				tc.major, tc.minor, got, tc.want)
		}
	}
}
