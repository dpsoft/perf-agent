// Package kernelver supplies the kernel version code that BPF_PROG_TYPE_KPROBE
// programs must carry at load time.
//
// IT EXISTS BECAUSE THE ALTERNATIVE FAILS ONLY WHERE IT MATTERS. Kernels before
// 5.0 checked the version field on kprobe-type program loads, so cilium/ebpf
// still fills it in when a spec leaves it zero -- by discovering the running
// kernel's version through the vDSO, which it reaches via the process's own
// auxv and memory. A process running with FILE CAPABILITIES is non-dumpable,
// so both reads are refused, and the load fails with a message that names
// neither capabilities nor uprobes:
//
//	detecting kernel version: read auxv from runtime: no such file or directory
//	detecting kernel version: opening mem: open /proc/self/mem: permission denied
//
// Which of the two you get depends on how the binary was built; the cause is
// the same. Running as root hides it completely, which is why CI never sees it
// and why it only ever appears on the one path that loads a uprobe program in
// a setcap'd deployment. It is NOT a verifier problem and not specific to this
// project: any setcap'd program loading a uprobe hits it.
//
// Supplying the version from uname skips the discovery entirely. On any kernel
// this project supports the value is ignored by the kernel anyway (5.0 removed
// the check, commit 6c4fc209fcf9), so what matters is only that it is present
// and plausible.
//
// This lives in one package because it was independently rediscovered three
// times -- in the GPU consumer, in cmd/bpfload, and in the interpreter module
// loader -- each time after a confusing failure. A workaround that is easier to
// re-derive than to find is one that will be re-derived again.
package kernelver

import (
	"fmt"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

// Code returns the running kernel's LINUX_VERSION_CODE, or 0 if uname cannot
// be read or parsed -- in which case the caller should leave the spec alone and
// let cilium/ebpf try its own discovery, which is no worse than guessing.
func Code() uint32 {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return 0
	}
	release := unix.ByteSliceToString(u.Release[:])
	var a, b, c uint32
	// A release like "6.19.10-300.fc44.x86_64" parses as 6.19.10; a two-part
	// release like "6.19" leaves c at zero, which is what LINUX_VERSION_CODE
	// would say too.
	if _, err := fmt.Sscanf(release, "%d.%d.%d", &a, &b, &c); err != nil {
		if _, err := fmt.Sscanf(release, "%d.%d", &a, &b); err != nil {
			return 0
		}
	}
	// Kernels 4.4 and 4.9 clamp SUBLEVEL to 255 so it cannot spill into
	// PATCHLEVEL (kbuild commit 9b82f13e7ef3); mirror that rather than
	// producing a code the kernel itself would never have generated.
	if c > 255 {
		c = 255
	}
	return a<<16 | b<<8 | c
}

// Apply fills in the kernel version on every program in a spec that left it
// zero. Programs that carry one already -- from an ELF `version` section -- are
// left alone.
//
// Applied to every program rather than only the kprobe-typed ones on purpose:
// the field is ignored for the others, and a filter here would have to be kept
// in step with cilium/ebpf's own notion of which types trigger the discovery.
func Apply(spec *ebpf.CollectionSpec) {
	kv := Code()
	if kv == 0 {
		return
	}
	for _, p := range spec.Programs {
		if p.KernelVersion == 0 {
			p.KernelVersion = kv
		}
	}
}
