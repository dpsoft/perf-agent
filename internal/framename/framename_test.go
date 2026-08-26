package framename

import "testing"

func TestFormat(t *testing.T) {
	cases := []struct {
		module string
		off    uint64
		want   string
	}{
		{"/usr/lib/x86_64-linux-gnu/libcuda.so.1", 0x1b71c6, "libcuda.so.1+0x1b71c6"},
		{"libcupti.so.12", 0, "libcupti.so.12+0x0"},
		{"/opt/app/bin/worker", 0xdeadbeef, "worker+0xdeadbeef"},

		// Never invent a module: the bracketed sentinels perf-agent uses for
		// non-file mappings are not files, and an empty module is not a name.
		{"", 0x10, ""},
		{"[kernel]", 0x10, ""},
		{"[jit]", 0x10, ""},
	}
	for _, c := range cases {
		if got := Format(c.module, c.off); got != c.want {
			t.Errorf("Format(%q, %#x) = %q, want %q", c.module, c.off, got, c.want)
		}
	}
}

func TestIsAddressOnly(t *testing.T) {
	yes := []string{
		"0x7f2c945b2c2b",
		"0x0",
		"0xDEADBEEF",
		"libcuda.so.1+0x1b71c6",
		"libcupti.so.12+0x0",
		"worker+0xdeadbeef",
	}
	for _, n := range yes {
		if !IsAddressOnly(n) {
			t.Errorf("IsAddressOnly(%q) = false, want true", n)
		}
	}

	// The second group is the one that matters: a real symbol must never be
	// mistaken for an unresolved frame, or the profile's symbolization-gap
	// warning would under-report itself.
	no := []string{
		"main.main",
		"cuLaunchKernel",
		"0x",
		"0xNotHex",
		"+0x10",
		"std::operator+(int)+0x10", // demangled C++: parens and colons
		"foo bar+0x10",             // space
		"/usr/lib/libc.so.6+0x10",  // a path, not a base: Format never emits this
		"[kernel]+0x10",            // sentinel
		"a<b>+0x10",                // template
		"libcuda.so.1+0xzz",
		"libcuda.so.1+",
	}
	for _, n := range no {
		if IsAddressOnly(n) {
			t.Errorf("IsAddressOnly(%q) = true, want false", n)
		}
	}
}

// TestIsAddressOnlyAcceptsEverythingFormatEmits is the property that keeps the
// producer and the three consumers from drifting apart.
func TestIsAddressOnlyAcceptsEverythingFormatEmits(t *testing.T) {
	mods := []string{
		"/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/usr/lib/libcupti.so.12.4.127",
		"/opt/a-b_c.d/exe",
		"ld-linux-x86-64.so.2",
	}
	for _, m := range mods {
		for _, off := range []uint64{0, 1, 0x1b71c6, ^uint64(0)} {
			n := Format(m, off)
			if n == "" {
				t.Fatalf("Format(%q, %#x) returned empty", m, off)
			}
			if !IsAddressOnly(n) {
				t.Errorf("Format produced %q which IsAddressOnly rejects", n)
			}
			if mod, ok := Module(n); !ok || mod == "" {
				t.Errorf("Module(%q) = %q, %v; want a non-empty module", n, mod, ok)
			}
		}
	}
}

// TestKnownAmbiguity states the one thing this test cannot rule out, so that
// nobody later reads TestIsAddressOnly as a proof it is impossible.
//
// A symbol whose demangled name is EXACTLY an identifier followed by "+0x" and
// hex digits - no parentheses, colons, spaces or template brackets anywhere -
// is indistinguishable from the module-relative form once the profile is
// written, because pprof has no unsymbolized bit to carry the difference. No
// such symbol has been observed; the consequence if one appeared would be a
// frame drawn in the unsymbolized domain and counted in the symbolization-gap
// warning. That direction is the safe one: it over-reports the gap rather than
// hiding it.
func TestKnownAmbiguity(t *testing.T) {
	if !IsAddressOnly("operator+0x10") {
		t.Skip("behaviour changed; update the doc above with the new limit")
	}
}

func TestModuleRefusesBareAddress(t *testing.T) {
	// A bare address has no module, and the caller must not be handed one.
	if mod, ok := Module("0x7f2c945b2c2b"); ok {
		t.Errorf("Module(bare address) = %q, true; want ok=false", mod)
	}
	if mod, ok := Module("main.main"); ok {
		t.Errorf("Module(symbol) = %q, true; want ok=false", mod)
	}
}
