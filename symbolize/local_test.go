package symbolize

import (
	"bufio"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// requireMapFilesAccess skips when this process cannot follow
// /proc/<pid>/map_files/, which is the capability perf-agent documents as
// required and NewLocalSymbolizer now refuses to start without.
//
// Gating on checkMapFilesAccess - the same probe the constructor uses - can
// only ever cause a false SKIP, never a false pass: if the probe wrongly
// reports access, the constructor proceeds and the assertions below run for
// real against a process that cannot resolve anything, and fail. The skip
// this replaces was different in kind. It hid a bug that was present *with*
// the capability set the gate mandated, because the product claimed to work
// there and did not.
func requireMapFilesAccess(t *testing.T) {
	t.Helper()
	if err := checkMapFilesAccess(); err != nil {
		t.Skipf("needs CAP_CHECKPOINT_RESTORE: %v", err)
	}
}

// The contract NewLocalSymbolizer now carries, pinned in both directions with
// no skip in either: without the capability it must refuse loudly and name
// the fix, and with it, symbolizing a live process must produce a real name
// rather than a hex address.
//
// The target is a shared library mapped into this very process. Unlike the Go
// test binary (`go test` strips the symbol table from the binary it runs;
// `go test -c` does not), libc always carries a dynamic symbol table, so a
// bare address here means symbolization failed and nothing else. That is the
// exact shape the map_files EPERM produced: 63 GPU stacks in the phase gate
// symbolized into nothing but addresses, silently, because a no_map_files
// retry stood underneath and swallowed the error.
func TestLocalSymbolizerRefusesWithoutMapFilesAndResolvesWithIt(t *testing.T) {
	if testing.Short() {
		t.Skip("uses /proc/self/maps")
	}
	probeErr := checkMapFilesAccess()

	s, err := NewLocalSymbolizer()
	if probeErr != nil {
		if s != nil {
			_ = s.Close()
		}
		if !errors.Is(err, ErrMapFilesUnavailable) {
			t.Fatalf("without map_files access the constructor must refuse with ErrMapFilesUnavailable, got %v", err)
		}
		// The error is the only thing an operator sees, so it has to carry
		// the remedy, not just the diagnosis.
		for _, want := range []string{"CAP_CHECKPOINT_RESTORE", "cap_checkpoint_restore+ep", "hex address"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must be actionable: %q missing from %q", want, err.Error())
			}
		}
		return
	}
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	defer func() { _ = s.Close() }()

	addr, want := mappedLibcFuncAddr(t)
	frames, err := s.SymbolizeProcess(uint32(os.Getpid()), []uint64{addr})
	if err != nil {
		t.Fatalf("SymbolizeProcess: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	// Not an equality check against `want`: aliases (memcpy/__memcpy_avx_unaligned
	// and friends) mean blazesym may legitimately name it something else. The
	// property under test is that it resolved a NAME rather than an address.
	if frames[0].Reason != FailureNone || strings.HasPrefix(frames[0].Name, "0x") {
		t.Fatalf("0x%x (%s in libc) did not resolve: Name=%q Reason=%s",
			addr, want, frames[0].Name, frames[0].Reason)
	}
	if st := s.Stats(); st.RawAddrBatches != 0 {
		t.Fatalf("a resolved batch must not be counted as a raw-address batch: %+v", st)
	}
}

// A batch for a pid that does not exist is a genuine per-process failure, not
// a capability one: it must still yield hex frames rather than an error (the
// stack shape is worth keeping), and it must be counted.
func TestSymbolizeProcessCountsGenuineResolutionFailures(t *testing.T) {
	requireMapFilesAccess(t)
	s, err := NewLocalSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	defer func() { _ = s.Close() }()

	// PID 0 is never a live process, so blazesym cannot read its /proc entry.
	frames, err := s.SymbolizeProcess(0, []uint64{0xdeadbeef})
	if err != nil {
		t.Fatalf("a dead pid must not fail the batch: %v", err)
	}
	if len(frames) != 1 || frames[0].Reason != FailureMissingSymbols || frames[0].Name != "0xdeadbeef" {
		t.Fatalf("want one hex-named unresolved frame, got %+v", frames)
	}
	if st := s.Stats(); st.RawAddrBatches != 1 {
		t.Fatalf("a per-process failure must be counted, not silent: %+v", st)
	}
}

// The second real-resolution target: this test binary itself. Kept alongside
// the libc one because it exercises .symtab rather than .dynsym.
func TestLocalSymbolizerSymbolizeSelf(t *testing.T) {
	if testing.Short() {
		t.Skip("uses /proc/self/maps")
	}
	requireMapFilesAccess(t)
	if !hasSymtab(t, "/proc/self/exe") {
		t.Skip("this test binary was linked without a symbol table (go test strips the binary it runs; go test -c does not)")
	}
	s, err := NewLocalSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	defer func() { _ = s.Close() }()

	// main is a symbol in our own binary — its address is the runtime PC of any
	// stack frame inside it. We don't need to find it precisely; we just need an
	// address that's in our own process. Use the address of os.Getpid (a real
	// runtime function) — its address is in our binary's mapping.
	addr := uint64(getOsGetpidAddr())
	frames, err := s.SymbolizeProcess(uint32(os.Getpid()), []uint64{addr})
	if err != nil {
		t.Fatalf("SymbolizeProcess: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("got 0 frames, want ≥1")
	}
	if frames[0].Name == "" {
		t.Fatalf("frame Name empty (Reason=%s)", frames[0].Reason)
	}
	// A name is not enough: the fallback paths in this file fill Name with
	// the hex address, so "0x4017c2" is a FAILURE wearing a name's clothes.
	// That is the shape the map_files EPERM produced, and asserting only on
	// non-emptiness is what let it through.
	if frames[0].Reason != FailureNone || strings.HasPrefix(frames[0].Name, "0x") {
		t.Fatalf("frame did not resolve to a real symbol: Name=%q Reason=%s", frames[0].Name, frames[0].Reason)
	}
}

func TestLocalSymbolizerCloseIdempotent(t *testing.T) {
	requireMapFilesAccess(t)
	s, err := NewLocalSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must not panic; either err or nil is acceptable.
	if err := s.Close(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close: unexpected err %v", err)
	}
}

//go:noinline
func getOsGetpidAddr() uintptr {
	// Reflect on os.Getpid's PC. It's a real function we can guarantee is mapped.
	return reflect.ValueOf(os.Getpid).Pointer()
}

// hasSymtab reports whether the ELF at path carries a .symtab section.
func hasSymtab(t *testing.T, path string) bool {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("elf.Open(%s): %v", path, err)
	}
	defer func() { _ = f.Close() }()
	return f.Section(".symtab") != nil
}

// mappedLibcFuncAddr finds libc's executable mapping in this process and
// returns the runtime address of one exported function in it, with that
// function's name. It resolves the load bias from the mapping's own file
// offset rather than assuming a zero-based one, so it is correct for a
// prelinked or non-standard libc too.
func mappedLibcFuncAddr(t *testing.T) (uint64, string) {
	t.Helper()
	start, off, path := libcExecMapping(t)
	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("elf.Open(%s): %v", path, err)
	}
	defer func() { _ = f.Close() }()

	// The PT_LOAD this mapping came from: file offset `off` in it lands at
	// runtime address `start`, which pins the bias for every address in it.
	var bias uint64
	var found bool
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || off < p.Off || off >= p.Off+p.Filesz {
			continue
		}
		bias = start - (p.Vaddr + off - p.Off)
		found = true
		break
	}
	if !found {
		t.Skipf("no PT_LOAD in %s covers file offset 0x%x", path, off)
	}

	syms, err := f.DynamicSymbols()
	if err != nil {
		t.Skipf("no dynamic symbols in %s: %v", path, err)
	}
	for _, sym := range syms {
		if elf.ST_TYPE(sym.Info) != elf.STT_FUNC || sym.Value == 0 || sym.Name == "" || sym.Size == 0 {
			continue
		}
		// Must land inside the mapping we measured the bias from, or the
		// address is not actually resident at bias+Value.
		if addr := bias + sym.Value; addr >= start && addr < start+0x1000000 {
			return addr, sym.Name
		}
	}
	t.Skipf("no usable STT_FUNC dynamic symbol in %s", path)
	return 0, ""
}

// libcExecMapping returns the start address, file offset and path of this
// process's executable libc mapping.
func libcExecMapping(t *testing.T) (uint64, uint64, string) {
	t.Helper()
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		t.Fatalf("open /proc/self/maps: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || !strings.Contains(fields[1], "x") {
			continue
		}
		path := fields[5]
		if !strings.Contains(path, "/libc.so") && !strings.Contains(path, "/libc-") {
			continue
		}
		var start, end, off uint64
		if _, err := fmt.Sscanf(fields[0], "%x-%x", &start, &end); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(fields[2], "%x", &off); err != nil {
			continue
		}
		return start, off, path
	}
	t.Skip("no executable libc mapping in this process (static binary?)")
	return 0, 0, ""
}
