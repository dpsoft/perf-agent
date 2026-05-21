package symbolize

import (
	"strings"
	"testing"
)

// TestKallsymsSymbolizerResolveAtOrBelow asserts the binary search
// semantic: an IP that lands in the middle of a function resolves to
// the function's name with the right offset.
func TestKallsymsSymbolizerResolveAtOrBelow(t *testing.T) {
	k := &kallsymsSymbolizer{
		addrs:   []uint64{0xffffffff81000000, 0xffffffff81000100, 0xffffffff81000200},
		names:   []string{"do_sys_openat2", "vfs_open", "tcp_sendmsg"},
		modules: []string{"", "", ""},
	}
	frames := k.Resolve([]uint64{
		0xffffffff81000000, // exact match → do_sys_openat2
		0xffffffff8100007f, // 0x7f past do_sys_openat2 start → still in do_sys_openat2
		0xffffffff81000180, // 0x80 past vfs_open start → vfs_open
	})
	want := []struct {
		name   string
		offset uint64
	}{
		{"do_sys_openat2", 0x0},
		{"do_sys_openat2", 0x7f},
		{"vfs_open", 0x80},
	}
	for i, w := range want {
		if frames[i].Name != w.name {
			t.Errorf("frame[%d].Name = %q, want %q", i, frames[i].Name, w.name)
		}
		if frames[i].Offset != w.offset {
			t.Errorf("frame[%d].Offset = %#x, want %#x", i, frames[i].Offset, w.offset)
		}
		if frames[i].Module != "[kernel.kallsyms]" {
			t.Errorf("frame[%d].Module = %q, want [kernel.kallsyms]", i, frames[i].Module)
		}
	}
}

// TestKallsymsSymbolizerModuleSymbols asserts that module symbols
// retain their module marker (e.g., "[kvm]") in Frame.Module.
func TestKallsymsSymbolizerModuleSymbols(t *testing.T) {
	k := &kallsymsSymbolizer{
		addrs:   []uint64{0xffffffffc0001000},
		names:   []string{"kvm_vcpu_ioctl"},
		modules: []string{"[kvm]"},
	}
	frames := k.Resolve([]uint64{0xffffffffc0001042})
	if frames[0].Name != "kvm_vcpu_ioctl" {
		t.Errorf("Name = %q, want kvm_vcpu_ioctl", frames[0].Name)
	}
	if frames[0].Module != "[kvm]" {
		t.Errorf("Module = %q, want [kvm]", frames[0].Module)
	}
	if frames[0].Offset != 0x42 {
		t.Errorf("Offset = %#x, want 0x42", frames[0].Offset)
	}
}

// TestKallsymsSymbolizerRejectsWildOffset asserts that an IP that
// lands "obviously too far" past the closest symbol (> 64 KiB) is
// reported as unknown instead of being mis-attributed to a distant
// function. Matches the awk-hack guard rail.
func TestKallsymsSymbolizerRejectsWildOffset(t *testing.T) {
	k := &kallsymsSymbolizer{
		addrs:   []uint64{0xffffffff81000000},
		names:   []string{"do_sys_openat2"},
		modules: []string{""},
	}
	// 0x20000 (128 KiB) past the only known symbol → reject.
	frames := k.Resolve([]uint64{0xffffffff81020000})
	if frames[0].Name == "do_sys_openat2" {
		t.Errorf("wild offset attributed to do_sys_openat2; want raw-hex name")
	}
	if frames[0].Reason != FailureUnknownAddress {
		t.Errorf("Reason = %v, want FailureUnknownAddress", frames[0].Reason)
	}
}

// TestKallsymsSymbolizerBelowFirstSymbol asserts that an IP below the
// lowest symbol address is reported as unknown rather than wrapping
// into the high end of the table.
func TestKallsymsSymbolizerBelowFirstSymbol(t *testing.T) {
	k := &kallsymsSymbolizer{
		addrs:   []uint64{0xffffffff81000000},
		names:   []string{"do_sys_openat2"},
		modules: []string{""},
	}
	frames := k.Resolve([]uint64{0xffffffff80ffffff})
	if frames[0].Reason != FailureUnknownAddress {
		t.Errorf("Reason = %v, want FailureUnknownAddress (IP below first symbol)", frames[0].Reason)
	}
}

// TestNewKallsymsSymbolizerLive asserts that on a host with
// kptr_restrict=0 the parser produces a non-empty index and resolves
// real kallsyms addresses to named frames. Skips when the host has
// kallsyms restricted.
//
// We don't assert the exact symbol name returned: /proc/kallsyms
// frequently has aliased symbols at the same address (e.g., kernel
// entry points expose both their canonical name and a __pi_* alias),
// and the sort order across aliases is implementation-defined. The
// load-bearing property is that resolution produces *some* valid
// symbol — not "[unknown]" or a raw hex — for an address that came
// from kallsyms itself.
func TestNewKallsymsSymbolizerLive(t *testing.T) {
	if !kallsymsReadable() {
		t.Skip("requires kptr_restrict=0")
	}
	k, err := newKallsymsSymbolizer()
	if err != nil {
		t.Fatalf("newKallsymsSymbolizer: %v", err)
	}
	if len(k.addrs) == 0 {
		t.Fatalf("empty kallsyms index")
	}

	addr, _ := pickKnownKernelSymbol(t)
	// Probe addr + 1: any aliased symbol at addr is still a valid
	// resolution since +1 lands inside whichever function the kernel
	// chose to start there.
	frames := k.Resolve([]uint64{addr + 1})
	if frames[0].Reason == FailureUnknownAddress {
		t.Fatalf("addr+1 resolved as unknown; want named symbol (got %+v)", frames[0])
	}
	if strings.HasPrefix(frames[0].Name, "0x") {
		t.Fatalf("got raw-hex name %q; want named symbol", frames[0].Name)
	}
	if frames[0].Offset != 1 {
		t.Errorf("offset = %d, want 1 (probe was addr+1)", frames[0].Offset)
	}
}
