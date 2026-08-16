package main

import (
	"testing"

	"github.com/google/pprof/profile"
)

// TestCountKernelResolution_NamedAndHexMixed asserts the helper
// returns (total, named) where named excludes Locations whose
// Function.Name is a "0x<hex>" raw-address fallback.
func TestCountKernelResolution_NamedAndHexMixed(t *testing.T) {
	kernelMap := &profile.Mapping{ID: 7, File: "[kernel]"}
	userMap := &profile.Mapping{ID: 8, File: "/usr/bin/foo"}
	fnNamed := &profile.Function{ID: 1, Name: "do_sys_openat2"}
	fnHex := &profile.Function{ID: 2, Name: "0xffffffff80001234"}
	fnEmpty := &profile.Function{ID: 3, Name: ""}
	fnUser := &profile.Function{ID: 4, Name: "main.run"}

	p := &profile.Profile{
		Mapping:  []*profile.Mapping{kernelMap, userMap},
		Function: []*profile.Function{fnNamed, fnHex, fnEmpty, fnUser},
		Location: []*profile.Location{
			{ID: 100, Mapping: kernelMap, Line: []profile.Line{{Function: fnNamed}}},
			{ID: 101, Mapping: kernelMap, Line: []profile.Line{{Function: fnHex}}},
			{ID: 102, Mapping: kernelMap, Line: []profile.Line{{Function: fnEmpty}}},
			{ID: 103, Mapping: kernelMap, Line: nil}, // no Line at all
			{ID: 104, Mapping: userMap, Line: []profile.Line{{Function: fnUser}}},
		},
	}

	total, named := countKernelResolution(p)
	if total != 4 {
		t.Errorf("total = %d, want 4 (locations in kernel mapping; user-mapping one excluded)", total)
	}
	if named != 1 {
		t.Errorf("named = %d, want 1 (only do_sys_openat2 qualifies; hex / empty / no-line excluded)", named)
	}
}

// TestCountKernelResolution_NoKernelMapping asserts the helper
// returns (0, 0) when the profile has no kernelSentinel mapping —
// e.g., a --kernel-stacks=off capture.
func TestCountKernelResolution_NoKernelMapping(t *testing.T) {
	userMap := &profile.Mapping{ID: 8, File: "/usr/bin/foo"}
	fnUser := &profile.Function{ID: 1, Name: "main.run"}
	p := &profile.Profile{
		Mapping:  []*profile.Mapping{userMap},
		Function: []*profile.Function{fnUser},
		Location: []*profile.Location{
			{ID: 100, Mapping: userMap, Line: []profile.Line{{Function: fnUser}}},
		},
	}
	total, named := countKernelResolution(p)
	if total != 0 || named != 0 {
		t.Errorf("got (%d, %d), want (0, 0)", total, named)
	}
}

// TestCountKernelResolution_AllResolved asserts perfect resolution
// rate yields total == named.
func TestCountKernelResolution_AllResolved(t *testing.T) {
	kernelMap := &profile.Mapping{ID: 7, File: "[kernel]"}
	fn1 := &profile.Function{ID: 1, Name: "tcp_sendmsg"}
	fn2 := &profile.Function{ID: 2, Name: "kvm_vcpu_ioctl"}
	p := &profile.Profile{
		Mapping:  []*profile.Mapping{kernelMap},
		Function: []*profile.Function{fn1, fn2},
		Location: []*profile.Location{
			{ID: 100, Mapping: kernelMap, Line: []profile.Line{{Function: fn1}}},
			{ID: 101, Mapping: kernelMap, Line: []profile.Line{{Function: fn2}}},
		},
	}
	total, named := countKernelResolution(p)
	if total != 2 || named != 2 {
		t.Errorf("got (%d, %d), want (2, 2)", total, named)
	}
}
