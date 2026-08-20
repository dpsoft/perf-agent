package symbolize

import (
	"bufio"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	blazesym "github.com/libbpf/blazesym/go"
)

// Symbolizing a live process must produce real names WITHOUT any capability.
// It did not: blazesym's process source resolves each mapping through
// /proc/<pid>/map_files/, whose magic symlinks the kernel refuses to open
// unless the caller holds CAP_CHECKPOINT_RESTORE (or CAP_SYS_ADMIN), so every
// batch failed with "permission denied" and SymbolizeProcess quietly handed
// back hex-named frames. This test used to SKIP on exactly that condition,
// which is why nothing caught it — the GPU phase-4a gate, run from a binary
// setcap'd with only cap_bpf,cap_perfmon, symbolized 63 stacks into nothing
// but addresses. The capability skip is gone on purpose: the no_map_files
// retry in SymbolizeProcess is what makes this pass unprivileged.
//
// The remaining skip is an environment fact, not a permission one: `go test`
// links the test binary it RUNS with the symbol table stripped (`go test -c`
// does not), so under a plain `go test ./symbolize/` there is genuinely
// nothing here to resolve. TestLocalSymbolizerResolvesRealNamesUnprivileged
// below covers the same ground with a target that always has symbols, and it
// never skips.
func TestLocalSymbolizerSymbolizeSelf(t *testing.T) {
	if testing.Short() {
		t.Skip("uses /proc/self/maps")
	}
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

// The regression guard for the map_files EPERM, with a target that cannot
// go missing: a shared library mapped into this very process. Unlike the Go
// test binary above, libc always carries a dynamic symbol table, so a bare
// address here means symbolization failed and nothing else.
//
// No capability gate, by design. This test is the assertion that perf-agent
// resolves user symbols when running with cap_bpf,cap_perfmon and nothing
// more, which is the capability set its own phase gates mandate.
func TestLocalSymbolizerResolvesRealNamesUnprivileged(t *testing.T) {
	if testing.Short() {
		t.Skip("uses /proc/self/maps")
	}
	addr, want := mappedLibcFuncAddr(t)

	s, err := NewLocalSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	defer func() { _ = s.Close() }()

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
		t.Fatalf("0x%x (%s in libc) did not resolve: Name=%q Reason=%s\n"+
			"this is the map_files/CAP_CHECKPOINT_RESTORE failure if it says missing_symbols",
			addr, want, frames[0].Name, frames[0].Reason)
	}
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

// The map_files fallback used to latch on ANY first-attempt failure, and
// then stay latched, for every process, for the life of the symbolizer -
// with no counter and no log. That is a permanent, invisible loss of
// inode-accurate resolution bought with one transient failure: a mapping
// deleted between the walk and the read, a JIT region, a pid that vanished
// mid-batch. And it is not merely a loss of precision: a symbolic path can
// be re-pointed between the mmap and the read (overlayfs), which resolves to
// the WRONG symbols rather than to none.
//
// Only a permission failure may latch, because only a permission failure
// cannot improve on its own.
func TestOnlyAPermissionFailureLatchesTheMapFilesFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("uses /proc/self/maps")
	}
	addr, _ := mappedLibcFuncAddr(t)
	self := uint32(os.Getpid())

	// Both cases inject only the FIRST attempt's failure; the no_map_files
	// retry underneath is the real one, against this live process, so the
	// rescue being asserted is a real rescue.
	newForcedFailure := func(t *testing.T, injected error) *LocalSymbolizer {
		t.Helper()
		s, err := NewLocalSymbolizer()
		if err != nil {
			t.Fatalf("NewLocalSymbolizer: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		// Undo whatever the startup capability probe decided: this test is
		// about the runtime decision, and it must read the same on a root
		// host and on a cap_bpf-only one.
		s.noMapFiles.Store(false)
		s.stats.disabledReason.Store("")
		s.mapFilesAttempt = func([]uint64, uint32, []blazesym.ProcessSourceOption) ([]blazesym.Sym, error) {
			return nil, injected
		}
		return s
	}

	t.Run("transient failure does not latch", func(t *testing.T) {
		s := newForcedFailure(t, errors.New("failed to read /proc/1234/maps: entity not found"))
		frames, err := s.SymbolizeProcess(self, []uint64{addr})
		if err != nil {
			t.Fatalf("SymbolizeProcess: %v", err)
		}
		if len(frames) != 1 || frames[0].Reason != FailureNone {
			t.Fatalf("the retry must still rescue the batch: %+v", frames)
		}
		st := s.Stats()
		if st.MapFilesDisabled {
			t.Fatalf("a transient failure must not cost inode accuracy for the rest of the run (reason %q)",
				st.MapFilesDisabledReason)
		}
		if st.MapFilesTransientFailure != 1 || st.MapFilesPermissionDenied != 0 {
			t.Fatalf("misclassified: %+v", st)
		}
		if st.FallbackRescued != 1 {
			t.Fatalf("the rescue must be counted, not silent: %+v", st)
		}
	})

	t.Run("permission failure latches", func(t *testing.T) {
		// Exactly what blazesym produces: BLAZE_ERR_PERMISSION_DENIED is
		// rendered by blaze_err_str as this bare string and wrapped with
		// errors.New, so no errno survives to match on.
		s := newForcedFailure(t, errors.New("permission denied"))
		if _, err := s.SymbolizeProcess(self, []uint64{addr}); err != nil {
			t.Fatalf("SymbolizeProcess: %v", err)
		}
		st := s.Stats()
		if !st.MapFilesDisabled {
			t.Fatal("a permission failure cannot improve on its own; it must latch")
		}
		if st.MapFilesDisabledReason == "" {
			t.Fatal("the transition must never be silent: it needs a reason")
		}
		if st.MapFilesPermissionDenied != 1 || st.MapFilesTransientFailure != 0 {
			t.Fatalf("misclassified: %+v", st)
		}
		// Latched means latched: the second batch skips the first attempt
		// outright, so neither classification counter moves again.
		if _, err := s.SymbolizeProcess(self, []uint64{addr}); err != nil {
			t.Fatalf("second SymbolizeProcess: %v", err)
		}
		if st2 := s.Stats(); st2.MapFilesPermissionDenied != 1 || st2.MapFilesTransientFailure != 0 {
			t.Fatalf("a skipped first attempt is not a failure: %+v", st2)
		}
	})

	t.Run("errno-typed permission errors are recognized too", func(t *testing.T) {
		s := newForcedFailure(t, fmt.Errorf("open: %w", syscall.EACCES))
		if _, err := s.SymbolizeProcess(self, []uint64{addr}); err != nil {
			t.Fatalf("SymbolizeProcess: %v", err)
		}
		if !s.Stats().MapFilesDisabled {
			t.Fatal("EACCES is a permission failure")
		}
	})
}

// The classifier is the whole difference between the fixed behavior and the
// bug, so it is pinned directly.
func TestIsPermissionDenied(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("permission denied"), true},
		{errors.New("Permission denied (os error 13)"), true},
		{fmt.Errorf("wrapped: %w", syscall.EPERM), true},
		{fmt.Errorf("wrapped: %w", syscall.EACCES), true},
		{os.ErrPermission, true},
		{errors.New("entity not found"), false},
		{errors.New("invalid data"), false},
		{fmt.Errorf("wrapped: %w", syscall.ENOENT), false},
		{errSkippedMapFiles, false},
	}
	for _, c := range cases {
		if got := isPermissionDenied(c.err); got != c.want {
			t.Errorf("isPermissionDenied(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
