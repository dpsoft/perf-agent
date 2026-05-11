package symbolize

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// kcoreReadable reports whether the current process can open /proc/kcore.
// blazesym's kernel source tries /proc/kcore as a vmlinux candidate; it
// is mode 0400 root:root, so a non-root unit-test process gets EACCES and
// the whole symbolize call fails. Production paths run with the elevated
// caps perf-agent already requires (CAP_SYS_RAWIO comes via root).
func kcoreReadable() bool {
	f, err := os.Open("/proc/kcore")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// kallsymsReadable reports whether /proc/kallsyms returns real (non-zero)
// addresses to the current process. With kptr_restrict=2 the kernel zeros
// every address; with kptr_restrict=0 we get the real ones (root or
// CAP_SYSLOG required when kptr_restrict=1).
func kallsymsReadable() bool {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 1 {
		return false
	}
	n, err := strconv.ParseUint(fields[0], 16, 64)
	if err != nil {
		return false
	}
	return n != 0
}

// pickKnownKernelSymbol returns one (addr, name) pair from /proc/kallsyms
// suitable for round-trip testing. Picks the first 'T' (text) symbol with
// a non-zero address.
func pickKnownKernelSymbol(t *testing.T) (uint64, string) {
	t.Helper()
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		t.Skipf("/proc/kallsyms: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		typ := fields[1]
		if typ != "T" && typ != "t" {
			continue
		}
		addr, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil || addr == 0 {
			continue
		}
		return addr, fields[2]
	}
	t.Skip("no usable kernel text symbol in /proc/kallsyms")
	return 0, ""
}

func TestNewLocalKernelSymbolizer(t *testing.T) {
	if !kallsymsReadable() {
		t.Skip("requires kptr_restrict=0 (or root + CAP_SYSLOG)")
	}
	s, err := NewLocalKernelSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalKernelSymbolizer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLocalKernelSymbolizerRoundTrip(t *testing.T) {
	if !kallsymsReadable() {
		t.Skip("requires kptr_restrict=0 (or root + CAP_SYSLOG)")
	}
	if !kcoreReadable() {
		t.Skip("blazesym kernel source needs /proc/kcore readable; requires root")
	}
	s, err := NewLocalKernelSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalKernelSymbolizer: %v", err)
	}
	defer func() { _ = s.Close() }()

	addr, name := pickKnownKernelSymbol(t)
	frames, err := s.SymbolizeKernel([]uint64{addr})
	if err != nil {
		t.Fatalf("SymbolizeKernel: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Name != name {
		t.Errorf("name = %q, want %q (addr %#x)", frames[0].Name, name, addr)
	}
}

func TestLocalKernelSymbolizerCloseIdempotent(t *testing.T) {
	if !kallsymsReadable() {
		t.Skip("requires kptr_restrict=0")
	}
	s, err := NewLocalKernelSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalKernelSymbolizer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must not panic; either err or nil is acceptable.
	if err := s.Close(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close: unexpected err %v", err)
	}
}

// Compile-time check that LocalKernelSymbolizer satisfies KernelSymbolizer.
var _ KernelSymbolizer = (*LocalKernelSymbolizer)(nil)
