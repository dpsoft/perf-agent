// Package ptraceop provides low-level ptrace primitives for remote function
// invocation: attach, save registers, write a payload, run a sequence of
// remote function calls (each returning via SIGSEGV at address 0), restore
// registers, detach. Language-agnostic — Python uses it today; future runtimes
// (Node.js if extending V8 internals, JVM via JNI shims, etc.) can reuse it.
package ptraceop

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// SymbolAddrs holds the absolute remote addresses of the three CPython
// functions we need to call. ptraceop deliberately does not depend on
// inject/python — naming is Python-flavored but the struct is just three
// uint64s; future runtimes can pass equivalent triples.
type SymbolAddrs struct {
	PyGILEnsure  uint64
	PyGILRelease uint64
	PyRunString  uint64
}

// Injector performs ptrace-based remote function calls.
type Injector struct {
	log *slog.Logger
}

// New creates an Injector. log may be nil (uses slog.Default()).
func New(log *slog.Logger) *Injector {
	if log == nil {
		log = slog.Default()
	}
	return &Injector{log: log}
}

// RemoteActivate runs the three-call sequence
// (PyGILState_Ensure → PyRun_SimpleString(payload) → PyGILState_Release)
// inside one ptrace session. Returns nil on success, a wrapped error otherwise.
//
// The payload must be NUL-terminated; the caller (typically inject/python)
// supplies python.ActivatePayload() or python.DeactivatePayload().
func (i *Injector) RemoteActivate(pid uint32, addrs SymbolAddrs, payload []byte) error {
	return i.runSequence(pid, addrs, payload)
}

// RemoteDeactivate is identical to RemoteActivate; the only difference is the
// payload string. Kept as a separate method for callsite clarity.
func (i *Injector) RemoteDeactivate(pid uint32, addrs SymbolAddrs, payload []byte) error {
	return i.runSequence(pid, addrs, payload)
}

// runSequence implements the full attach → 3 calls → detach sequence from
// design §6.2.
func (i *Injector) runSequence(pid uint32, addrs SymbolAddrs, payload []byte) error {
	if pid == 0 {
		return errors.New("ptraceop: pid is zero")
	}
	if len(payload) == 0 {
		return errors.New("ptraceop: empty payload")
	}
	if payload[len(payload)-1] != 0 {
		return errors.New("ptraceop: payload not NUL-terminated")
	}
	if addrs.PyGILEnsure == 0 || addrs.PyGILRelease == 0 || addrs.PyRunString == 0 {
		return errors.New("ptraceop: SymbolAddrs has zero entry")
	}

	// Step 1-2: attach + waitpid.
	if err := unix.PtraceAttach(int(pid)); err != nil {
		return fmt.Errorf("ptrace attach pid=%d: %w", pid, err)
	}
	defer func() {
		// Best-effort detach. If we error before reaching the explicit detach
		// below, this ensures the target is resumed.
		_ = unix.PtraceDetach(int(pid))
	}()

	var status unix.WaitStatus
	if _, err := unix.Wait4(int(pid), &status, 0, nil); err != nil {
		return fmt.Errorf("waitpid for attach stop pid=%d: %w", pid, err)
	}
	if !status.Stopped() {
		return fmt.Errorf("expected stopped after attach pid=%d, got status %v", pid, status)
	}

	// Step 3: save original registers.
	var orig unix.PtraceRegs
	if err := unix.PtraceGetRegs(int(pid), &orig); err != nil {
		return fmt.Errorf("ptrace getregs pid=%d: %w", pid, err)
	}

	// Step 4: find stack mapping and verify headroom.
	stackLow, ok := stackLowAddr(pid)
	if !ok {
		return fmt.Errorf("cannot determine stack mapping for pid=%d", pid)
	}
	currentSP := stackPointer(orig)
	if currentSP < stackLow+1024 {
		// Headroom check failed; spec §6.4 specifies remote-mmap fallback.
		// v1 ships without the fallback (rare in practice); we fail with a
		// clear sentinel-style message so the caller can decide.
		return fmt.Errorf("ptraceop: insufficient stack headroom (sp=0x%x, low=0x%x); remote-mmap fallback not implemented in v1",
			currentSP, stackLow)
	}

	// Step 5-6: choose payload_addr, write payload to stack.
	payloadAddr := (currentSP - 256) &^ 0xF
	if _, err := unix.PtracePokeData(int(pid), uintptr(payloadAddr), payload); err != nil {
		return fmt.Errorf("ptrace pokedata payload pid=%d: %w", pid, err)
	}

	// Step 7-9: three remote calls (Ensure → Run → Release).
	gstate, err := i.remoteCall(pid, orig, addrs.PyGILEnsure, payloadAddr, 0)
	if err != nil {
		return fmt.Errorf("PyGILState_Ensure: %w", err)
	}
	runResult, runErr := i.remoteCall(pid, orig, addrs.PyRunString, payloadAddr, payloadAddr)
	// Always release the GIL we acquired, even if PyRun_SimpleString failed.
	if _, relErr := i.remoteCall(pid, orig, addrs.PyGILRelease, payloadAddr, gstate); relErr != nil {
		i.log.Warn("PyGILState_Release failed after attempted activation",
			"pid", pid, "err", relErr)
	}
	if runErr != nil {
		return fmt.Errorf("PyRun_SimpleString: %w", runErr)
	}
	if runResult != 0 {
		return fmt.Errorf("PyRun_SimpleString returned non-zero: %d (likely activation refused at runtime)", runResult)
	}

	// Step 10-11: restore registers, detach (defer above handles detach).
	if err := unix.PtraceSetRegs(int(pid), &orig); err != nil {
		return fmt.Errorf("ptrace setregs restore pid=%d: %w", pid, err)
	}
	if err := unix.PtraceDetach(int(pid)); err != nil {
		return fmt.Errorf("ptrace detach pid=%d: %w", pid, err)
	}
	return nil
}

// remoteCall performs one remote function invocation:
//  1. Build call frame from orig with fnAddr/arg, return-addr=0 (SIGSEGV
//     sentinel), payload_addr-derived stack pointer.
//  2. PtraceSetRegs.
//  3. PtraceCont with signal=0.
//  4. Wait4 for SIGSEGV (or other terminal stop).
//  5. Read regs, return the call's return value.
//
// payloadAddr is used to derive a fresh stack pointer for each call so that
// successive calls don't trample each other's frames.
func (i *Injector) remoteCall(pid uint32, orig unix.PtraceRegs, fnAddr, payloadAddr, arg1 uint64) (uint64, error) {
	frame, err := setupCallFrame(orig, fnAddr, arg1, payloadAddr)
	if err != nil {
		return 0, fmt.Errorf("setup call frame: %w", err)
	}
	// Some arches (amd64) require us to write the sentinel return address (0)
	// to *(SP) before the call. setupCallFrame returns the SP that needs the
	// sentinel; we write a 0 word there.
	zero := make([]byte, 8)
	if _, err := unix.PtracePokeData(int(pid), uintptr(stackPointer(frame)), zero); err != nil {
		return 0, fmt.Errorf("write return-addr sentinel: %w", err)
	}
	if err := unix.PtraceSetRegs(int(pid), &frame); err != nil {
		return 0, fmt.Errorf("setregs: %w", err)
	}
	if err := unix.PtraceCont(int(pid), 0); err != nil {
		return 0, fmt.Errorf("ptrace cont: %w", err)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(int(pid), &status, 0, nil); err != nil {
		return 0, fmt.Errorf("wait4 after remote call: %w", err)
	}
	if !status.Stopped() {
		return 0, fmt.Errorf("expected stop after remote call; got status %v", status)
	}
	if sig := status.StopSignal(); sig != unix.SIGSEGV {
		return 0, fmt.Errorf("expected SIGSEGV sentinel; got signal %v", sig)
	}
	var post unix.PtraceRegs
	if err := unix.PtraceGetRegs(int(pid), &post); err != nil {
		return 0, fmt.Errorf("getregs post-call: %w", err)
	}
	return extractReturn(post), nil
}

// stackLowAddr reads /proc/<pid>/maps and returns the low address of the
// [stack] mapping.
func stackLowAddr(pid uint32) (uint64, bool) {
	mapsPath := filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "maps")
	f, err := os.Open(mapsPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, _ := f.Read(buf)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if !strings.Contains(line, "[stack]") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		low, err := strconv.ParseUint(fields[0][:dash], 16, 64)
		if err != nil {
			continue
		}
		return low, true
	}
	return 0, false
}
