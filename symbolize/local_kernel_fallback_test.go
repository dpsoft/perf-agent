package symbolize

import (
	"errors"
	"fmt"
	"testing"
)

// stubKernelSymbolizer builds a LocalKernelSymbolizer with no CGO state
// so the Go-level fallback ladder in SymbolizeKernel can be exercised
// without a real blazesym handle. The caller's `call` stands in for the
// CGO blazesym invocation: useFallback reflects which kernel source
// variant the production code would have asked for.
func stubKernelSymbolizer(call func(ips []uint64, useFallback bool) ([]Frame, error)) *LocalKernelSymbolizer {
	s := &LocalKernelSymbolizer{}
	s.callBlazesym = call
	return s
}

// TestSymbolizeKernel_RetriesOnPermissionDenied covers Bug 1: when
// blazesym's default kernel source returns BLAZE_ERR_PERMISSION_DENIED
// (the lockdown=integrity case where on-disk vmlinux candidates are
// unreadable), SymbolizeKernel must retry once asking for the
// kallsyms-only path (vmlinux=""), and surface those frames.
func TestSymbolizeKernel_RetriesOnPermissionDenied(t *testing.T) {
	var defaultCalls, fallbackCalls int
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
		if !useFallback {
			defaultCalls++
			return nil, errBlazePermissionDenied
		}
		fallbackCalls++
		out := make([]Frame, len(ips))
		for i, ip := range ips {
			out[i] = Frame{Address: ip, Name: "stub_sym", Module: "[kernel.kallsyms]"}
		}
		return out, nil
	})

	frames, err := s.SymbolizeKernel([]uint64{0xffffffff80001000})
	if err != nil {
		t.Fatalf("SymbolizeKernel: %v", err)
	}
	if len(frames) != 1 || frames[0].Name != "stub_sym" {
		t.Fatalf("got %+v, want resolved frame from fallback path", frames)
	}
	if defaultCalls != 1 {
		t.Errorf("default-path calls = %d, want 1", defaultCalls)
	}
	if fallbackCalls != 1 {
		t.Errorf("fallback-path calls = %d, want 1", fallbackCalls)
	}
}

// TestSymbolizeKernel_StickyFallback verifies that once SymbolizeKernel
// has observed permission-denied on the default path and switched to
// the fallback, it skips the default path for the symbolizer's
// remaining lifetime — that path is going to fail with the same error
// on the same host, so re-probing it wastes a CGO call per batch.
func TestSymbolizeKernel_StickyFallback(t *testing.T) {
	var defaultCalls, fallbackCalls int
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
		if !useFallback {
			defaultCalls++
			return nil, errBlazePermissionDenied
		}
		fallbackCalls++
		return []Frame{{Address: ips[0], Name: "ok"}}, nil
	})

	for i := range 3 {
		if _, err := s.SymbolizeKernel([]uint64{uint64(0xffffffff80001000) + uint64(i)}); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
	}
	if defaultCalls != 1 {
		t.Errorf("default-path calls = %d, want 1 (sticky after first EPERM)", defaultCalls)
	}
	if fallbackCalls != 3 {
		t.Errorf("fallback-path calls = %d, want 3", fallbackCalls)
	}
}

// TestSymbolizeKernel_RawAddressesOnTotalFailure covers Bug 2: when
// both the default and fallback blazesym paths fail, SymbolizeKernel
// must synthesize Frames with the raw kernel address rendered as
// "0x<hex>" in Name and Reason=FailureMissingSymbols, so the kernel
// portion of the stack survives into the pprof. Previously the whole
// batch was discarded, dropping kernel context entirely.
func TestSymbolizeKernel_RawAddressesOnTotalFailure(t *testing.T) {
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
		return nil, errors.New("blazesym total failure")
	})

	ips := []uint64{0xffffffff80001234, 0xffffffff80005678}
	frames, err := s.SymbolizeKernel(ips)
	if err != nil {
		t.Fatalf("SymbolizeKernel: expected nil err with raw fallback, got %v", err)
	}
	if len(frames) != len(ips) {
		t.Fatalf("got %d frames, want %d", len(frames), len(ips))
	}
	for i, f := range frames {
		wantName := fmt.Sprintf("0x%x", ips[i])
		if f.Name != wantName {
			t.Errorf("frame[%d].Name = %q, want %q", i, f.Name, wantName)
		}
		if f.Module != "[kernel.kallsyms]" {
			t.Errorf("frame[%d].Module = %q, want [kernel.kallsyms]", i, f.Module)
		}
		if f.Reason != FailureMissingSymbols {
			t.Errorf("frame[%d].Reason = %v, want FailureMissingSymbols", i, f.Reason)
		}
		if f.Address != ips[i] {
			t.Errorf("frame[%d].Address = %#x, want %#x", i, f.Address, ips[i])
		}
	}
}

// TestSymbolizeKernel_DefaultPathSucceedsNoRetry confirms the happy
// path: when the default blazesym call succeeds, the fallback is not
// consulted and the symbolizer stays out of sticky-fallback mode.
func TestSymbolizeKernel_DefaultPathSucceedsNoRetry(t *testing.T) {
	var defaultCalls, fallbackCalls int
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
		if useFallback {
			fallbackCalls++
			t.Fatalf("fallback path called unexpectedly")
		}
		defaultCalls++
		return []Frame{{Address: ips[0], Name: "ok"}}, nil
	})
	if _, err := s.SymbolizeKernel([]uint64{0xffffffff80001000}); err != nil {
		t.Fatalf("SymbolizeKernel: %v", err)
	}
	if _, err := s.SymbolizeKernel([]uint64{0xffffffff80002000}); err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if defaultCalls != 2 || fallbackCalls != 0 {
		t.Errorf("default=%d fallback=%d, want 2 / 0", defaultCalls, fallbackCalls)
	}
}
