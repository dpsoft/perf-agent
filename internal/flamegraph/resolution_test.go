package flamegraph

import "testing"

// The four kinds we currently render as if they were two.
//
// Measured on one PyTorch MNIST capture: 45 module+offset, 10 bare addresses,
// 30 obfuscated. The first two share a colour today even though one can be
// looked up later and the other cannot; the third is coloured exactly like a
// fully resolved frame even though no human-readable name exists for it.
func TestResolutionTellsApartWhatDomainCannot(t *testing.T) {
	cases := map[string]Resolution{
		// Named by a symbol table.
		"at::native::add_kernel(at::TensorIteratorBase&)": ResolutionResolved,
		"main":                         ResolutionResolved,
		"Conv2d.forward (conv.py:564)": ResolutionResolved,
		"cuLaunchKernel":               ResolutionResolved,

		// Module known, symbol not: stable across ASLR, matchable later.
		"libcudnn_engines_precompiled.so.9+0x824fbd": ResolutionModuleOffset,
		"libcuda.so.1+0x1b71c6":                      ResolutionModuleOffset,

		// Nothing known at all.
		"0x7f2c945b2c2b": ResolutionBareAddress,
		"":               ResolutionBareAddress,

		// Symbolized by NVIDIA's server; the name is a stable opaque id.
		"libcupti_afe8ffb67fac57d8829b1194a93b8ec676e80f7d": ResolutionObfuscated,
		"libcuda_8e2eae48ba8eb68460582f76460557784d48a71a":  ResolutionObfuscated,

		// Interpreter frame placed correctly, code object unread.
		"python:0x26f84de0": ResolutionInterpreter,
	}
	for name, want := range cases {
		if got := ResolutionOf(name); got != want {
			t.Errorf("ResolutionOf(%q) = %v, want %v", name, got, want)
		}
	}
}

// A real name that merely looks like a hash must not be called obfuscated.
// The pattern is <library>_<exactly 40 hex>, and anything else is a symbol
// someone chose.
func TestOrdinaryNamesAreNotMistakenForObfuscated(t *testing.T) {
	for _, n := range []string{
		"libfoo_bar",
		"libcuda_8e2eae48ba8eb68460582f76460557784d48a71",   // 39
		"libcuda_8e2eae48ba8eb68460582f76460557784d48a71aa", // 41
		"libcuda_8e2eae48ba8eb68460582f76460557784d48a71z",  // not hex
		"cuda_8e2eae48ba8eb68460582f76460557784d48a71a",     // no lib prefix
		"at::native::add_kernel",
	} {
		if got := ResolutionOf(n); got == ResolutionObfuscated {
			t.Errorf("ResolutionOf(%q) = obfuscated, want a real name", n)
		}
	}
}

// Every resolution must describe itself, because the panel and the legend both
// render these and a blank one would be an empty row rather than a missing
// feature.
func TestEveryResolutionDescribesItself(t *testing.T) {
	for _, r := range []Resolution{
		ResolutionResolved, ResolutionModuleOffset, ResolutionBareAddress,
		ResolutionObfuscated, ResolutionInterpreter,
	} {
		i := r.Info()
		if i.Key == "" || i.Label == "" || i.Desc == "" {
			t.Errorf("%v has an incomplete Info: %+v", r, i)
		}
	}
	// Keys must be distinct: they become CSS classes and data attributes.
	seen := map[string]bool{}
	for _, r := range []Resolution{
		ResolutionResolved, ResolutionModuleOffset, ResolutionBareAddress,
		ResolutionObfuscated, ResolutionInterpreter,
	} {
		k := r.Info().Key
		if seen[k] {
			t.Errorf("duplicate resolution key %q", k)
		}
		seen[k] = true
	}
}
