package symbolize

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// hasCheckpointRestore reports whether the running process (or the test
// binary's file caps) holds CAP_CHECKPOINT_RESTORE in the effective set.
// blazesym requires this to follow /proc/<pid>/map_files/ symlinks.
func hasCheckpointRestore() bool {
	// Check file caps on the binary first (setcap'd test binary).
	if caps, err := cap.GetFile(os.Args[0]); err == nil {
		if have, err := caps.GetFlag(cap.Effective, cap.CHECKPOINT_RESTORE); err == nil && have {
			return true
		}
	}
	// Fall back to process caps.
	proccaps := cap.GetProc()
	have, err := proccaps.GetFlag(cap.Effective, cap.CHECKPOINT_RESTORE)
	return err == nil && have
}

func TestLocalSymbolizerSymbolizeSelf(t *testing.T) {
	if testing.Short() {
		t.Skip("uses /proc/self/maps")
	}
	if os.Getuid() != 0 && !hasCheckpointRestore() {
		t.Skip("requires CAP_CHECKPOINT_RESTORE (or root) for /proc/<pid>/map_files")
	}
	s, err := NewLocalSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	defer s.Close()

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
