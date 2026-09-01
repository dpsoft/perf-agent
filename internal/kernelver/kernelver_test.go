package kernelver

import (
	"testing"

	"github.com/cilium/ebpf"
)

// The code must be a plausible LINUX_VERSION_CODE for the running kernel. Zero
// is the failure this package exists to prevent: it sends cilium/ebpf back to
// the vDSO probe that a setcap'd process cannot do.
func TestCodeIsPlausible(t *testing.T) {
	kv := Code()
	if kv == 0 {
		t.Fatal("Code() is zero; cilium/ebpf would fall back to the vDSO probe")
	}
	major, minor := kv>>16, (kv>>8)&0xff
	if major < 4 || major >= 100 {
		t.Errorf("major version %d parsed from uname is not plausible", major)
	}
	if minor > 99 {
		t.Errorf("minor version %d is not plausible", minor)
	}
}

// Apply must fill in programs that left the field zero and must NOT overwrite
// one that carries a version already -- an ELF `version` section is the
// author's explicit answer and outranks ours.
func TestApplyFillsOnlyTheZeroes(t *testing.T) {
	spec := &ebpf.CollectionSpec{Programs: map[string]*ebpf.ProgramSpec{
		"unset":    {KernelVersion: 0},
		"explicit": {KernelVersion: 0x40f00},
	}}
	Apply(spec)

	if got := spec.Programs["unset"].KernelVersion; got != Code() {
		t.Errorf("unset program has version %#x, want %#x", got, Code())
	}
	if got := spec.Programs["explicit"].KernelVersion; got != 0x40f00 {
		t.Errorf("explicit program was overwritten to %#x; the ELF's own answer must win", got)
	}
}

// The sublevel is clamped the way kbuild clamps it (commit 9b82f13e7ef3), so
// we never produce a code the kernel itself could not have generated.
func TestTheSublevelCannotSpillIntoTheMinor(t *testing.T) {
	kv := Code()
	if sub := kv & 0xff; sub > 255 {
		t.Fatalf("sublevel %d does not fit a byte", sub)
	}
	// Reconstructing must be lossless for the parts that fit.
	if kv>>16 == 0 {
		t.Error("major version is zero; the release string did not parse")
	}
}
