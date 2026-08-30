package pyunwind

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// The script the live tests run. It starts a worker thread and then blocks
// the main thread in join(), which is the shape the whole TSD mechanism
// exists for: the worker's PyThreadState is reachable only through its own
// pthread TSD slot.
//
// It prints READY once the worker is running so the test attaches to an
// interpreter that has actually executed Python on both threads, rather
// than racing interpreter startup.
const liveWorkerScript = `
import ctypes, threading, time

ctypes.pythonapi.PyThreadState_Get.restype = ctypes.c_void_p

def report():
    # GROUND TRUTH. PyThreadState_Get() is CPython's own authoritative
    # answer to "which PyThreadState is this thread running", and
    # get_native_id() is the OS tid the TSD lookup will be performed
    # against. Printing the pair is what lets the test assert that the
    # pthread-TSD walk found the RIGHT state for the RIGHT thread, rather
    # than merely something that looked like a frame.
    print("TSTATE %d %#x" % (threading.get_native_id(),
                             ctypes.pythonapi.PyThreadState_Get()), flush=True)

def leaf(x):
    t = 0
    for i in range(200):
        t += (x * i) % 7
    return t

def spin(deadline):
    report()
    while time.time() < deadline:
        leaf(3)

t = threading.Thread(target=spin, args=(time.time() + 60,), daemon=True)
t.start()
time.sleep(0.3)
report()
print("READY", flush=True)
time.sleep(60)
`

// livePython returns the interpreter the live tests should attach to.
// PYUNWIND_TEST_PYTHON overrides it, which is how a second interpreter
// (a different CPython version, or the same version from a different
// toolchain) gets covered on a machine that has only one python3 on PATH.
func livePython(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("PYUNWIND_TEST_PYTHON"); p != "" {
		return p
	}
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("no python3 on PATH: %v", err)
	}
	return p
}

// startLiveInterpreter launches the worker script, waits for READY, and
// returns the ground-truth PyThreadState the interpreter itself reported
// for each of its threads, keyed by OS tid.
func startLiveInterpreter(t *testing.T) (*exec.Cmd, map[int]uint64) {
	t.Helper()
	cmd := exec.Command(livePython(t), "-c", liveWorkerScript)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start interpreter: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	type result struct {
		tstates map[int]uint64
		err     string
	}
	done := make(chan result, 1)
	go func() {
		tstates := map[int]uint64{}
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "READY" {
				done <- result{tstates: tstates}
				return
			}
			f := strings.Fields(line)
			if len(f) == 3 && f[0] == "TSTATE" {
				tid, err1 := strconv.Atoi(f[1])
				ts, err2 := strconv.ParseUint(f[2], 0, 64)
				if err1 == nil && err2 == nil {
					tstates[tid] = ts
				}
			}
		}
		done <- result{err: "interpreter exited before printing READY"}
	}()
	select {
	case r := <-done:
		if r.err != "" {
			t.Fatal(r.err)
		}
		if len(r.tstates) != 2 {
			t.Fatalf("expected one PyThreadState report from each of the two threads, got %d: %v", len(r.tstates), r.tstates)
		}
		return cmd, r.tstates
	case <-time.After(20 * time.Second):
		t.Fatal("interpreter never printed READY")
	}
	return nil, nil
}

// TestTSDLookupFindsEachThreadsOwnThreadState is the first test in this
// package that runs against a REAL interpreter rather than a fake reader.
// Everything before it asserted that the code agrees with itself: that a Go
// struct mirrors a C one, that a synthetic frame validates, that a fixture
// body parses. None of them could tell whether the TSS key, the glibc TSD
// offsets and the frame layout are right about a running CPython.
//
// It asserts an EXACT VALUE against ground truth, not a plausibility: the
// PyThreadState the pthread-TSD walk finds for a thread must be the pointer
// that thread's own PyThreadState_Get() returned. Offsets.Validate is
// explicitly a plausibility screen (see its doc), and a neighbouring TSD
// slot can satisfy it -- measured, on this machine, at key 0 -- so
// "validates" is not evidence that the right pointer was followed. Pointer
// equality is.
//
// It also asserts the per-thread part, which is the whole reason the TSD
// mechanism exists: the worker thread's state must come back for the worker
// thread and the main thread's for the main thread. A lookup that always
// found the main thread's state would pass every other test in this file.
//
// Needs no BPF and no capabilities: process_vm_readv and ptrace against
// one's own child are allowed under every ptrace_scope setting.
func TestTSDLookupFindsEachThreadsOwnThreadState(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("pyunwind has measured glibc TSD offsets for amd64 only; %s is a named refusal (ErrUnsupportedArch)", runtime.GOARCH)
	}
	cmd, want := startLiveInterpreter(t)
	pid := uint32(cmd.Process.Pid)

	libPath, version, err := FindInterpreter(pid)
	if err != nil {
		t.Fatalf("FindInterpreter(%d): %v", pid, err)
	}
	if !version.Supported() {
		t.Skipf("%s is CPython %s; this build walks 3.12-3.14 only", libPath, version)
	}
	code, err := GILStateCode(libPath)
	if err != nil {
		t.Fatalf("GILStateCode(%s): %v", libPath, err)
	}
	keyOff, err := ParseAutoTSSKeyOffset(code)
	if err != nil {
		t.Fatalf("ParseAutoTSSKeyOffset(%s, %d-byte body): %v", libPath, len(code), err)
	}
	t.Logf("interpreter: %s (CPython %s); PyGILState_GetThisThreadState: %d bytes, autoTSSkey at %#x",
		libPath, version, len(code), keyOff)

	tsd, err := glibcTSDOffsets(runtime.GOARCH)
	if err != nil {
		t.Fatalf("glibcTSDOffsets: %v", err)
	}
	key, err := liveTSSKey(pid, libPath, code)
	if err != nil {
		t.Fatalf("read the live autoTSSkey value: %v", err)
	}
	t.Logf("live autoTSSkey = %d", key)

	// The two threads must have DIFFERENT states, or "found the right one"
	// would be unfalsifiable.
	seen := map[uint64]bool{}
	for tid, ts := range want {
		if ts == 0 {
			t.Fatalf("tid %d reported a NULL PyThreadState", tid)
		}
		if seen[ts] {
			t.Fatalf("two threads reported the same PyThreadState %#x: %v", ts, want)
		}
		seen[ts] = true
	}

	for tid, wantTState := range want {
		r := NewProcReader(int(pid), tid)
		tlsBase, err := r.TLSBase()
		if err != nil {
			t.Fatalf("tid %d: TLSBase: %v", tid, err)
		}
		got, err := hostTSSGet(r, tlsBase, tsd, key)
		if err != nil {
			t.Fatalf("tid %d: TSD lookup: %v", tid, err)
		}
		if got != wantTState {
			t.Errorf("tid %d: TSD lookup found PyThreadState %#x, the interpreter itself reports %#x",
				tid, got, wantTState)
			continue
		}
		t.Logf("tid %d: TSD lookup = %#x, matches PyThreadState_Get()", tid, got)
	}
}

// TestNoOtherTSDKeyHoldsAThreadState is the mutation of the assertion
// above: the same interpreter, the same thread, every OTHER key in the
// first TSD block. None of them may yield a PyThreadState the interpreter
// reported.
//
// This is what says the answer came from CPython's key rather than from the
// TSD block being full of pointers that all happen to work. Note what it
// does NOT assert: that other keys fail to VALIDATE. On this machine key 0
// holds something Offsets.Validate accepts, which is exactly why the test
// above compares pointers instead.
func TestNoOtherTSDKeyHoldsAThreadState(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("amd64 only; %s is a named refusal", runtime.GOARCH)
	}
	cmd, want := startLiveInterpreter(t)
	pid := uint32(cmd.Process.Pid)

	libPath, version, err := FindInterpreter(pid)
	if err != nil {
		t.Fatalf("FindInterpreter: %v", err)
	}
	if !version.Supported() {
		t.Skipf("CPython %s is outside the supported range", version)
	}
	code, err := GILStateCode(libPath)
	if err != nil {
		t.Fatalf("GILStateCode: %v", err)
	}
	tsd, err := glibcTSDOffsets(runtime.GOARCH)
	if err != nil {
		t.Fatalf("glibcTSDOffsets: %v", err)
	}
	realKey, err := liveTSSKey(pid, libPath, code)
	if err != nil {
		t.Fatalf("read the live autoTSSkey value: %v", err)
	}

	reported := map[uint64]int{}
	for tid, ts := range want {
		reported[ts] = tid
	}

	r := NewProcReader(int(pid), int(pid))
	tlsBase, err := r.TLSBase()
	if err != nil {
		t.Fatalf("TLSBase: %v", err)
	}
	// The real key must work here, or the loop below proves nothing.
	if got, err := hostTSSGet(r, tlsBase, tsd, realKey); err != nil || reported[got] != int(pid) {
		t.Fatalf("key %d on the main thread: got %#x (err %v), want the main thread's reported state", realKey, got, err)
	}
	for key := uint32(0); key < pyTSSKeysPerBlock; key++ {
		if key == realKey {
			continue
		}
		got, err := hostTSSGet(r, tlsBase, tsd, key)
		if err != nil {
			continue
		}
		if tid, ok := reported[got]; ok {
			t.Errorf("TSD key %d also holds tid %d's PyThreadState %#x; the key this package parses is not distinguishable from a neighbour",
				key, tid, got)
		}
	}
}

// TestPrepareInfoAgainstLiveInterpreter carries the live check the whole
// way to the record that gets written into py_procs: through the frame
// chain and Offsets.Validate, on every thread.
func TestPrepareInfoAgainstLiveInterpreter(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("amd64 only; %s is a named refusal", runtime.GOARCH)
	}
	cmd, want := startLiveInterpreter(t)
	pid := uint32(cmd.Process.Pid)

	libPath, version, err := FindInterpreter(pid)
	if err != nil {
		t.Fatalf("FindInterpreter: %v", err)
	}
	if !version.Supported() {
		t.Skipf("CPython %s is outside the supported range", version)
	}
	code, err := GILStateCode(libPath)
	if err != nil {
		t.Fatalf("GILStateCode: %v", err)
	}
	table, err := TableFor(version)
	if err != nil {
		t.Fatalf("TableFor(%s): %v", version, err)
	}

	for tid := range want {
		info, res := prepareInfo(pid, libPath, code, NewProcReader(int(pid), tid))
		if res.Refused != "" {
			t.Fatalf("tid %d: refused: %s", tid, res.Refused)
		}
		if info.Enabled != 1 {
			t.Errorf("tid %d: Enabled = %d, want 1", tid, info.Enabled)
		}
		if info.ThreadstateFrame != table.ThreadStateFrame || info.FramePrevious != table.FramePrevious ||
			info.FrameOwner != table.FrameOwner || info.FrameExecutable != table.FrameExecutable {
			t.Errorf("tid %d: installed offsets do not match the %s table: %+v", tid, version, info)
		}
		if info.PthreadSpecific1stblock == 0 {
			t.Errorf("tid %d: glibc TSD offsets were never filled in", tid)
		}
		if version.Minor >= 13 && info.NoneAddr == 0 {
			t.Errorf("tid %d: CPython %s needs _Py_NoneStruct resolved, got 0", tid, version)
		}
	}
}

// liveTSSKey reads the autoTSSkey VALUE out of a running interpreter, the
// same way prepareInfo does (see the comment there on why the key is the
// HIGH half of the 8-byte read).
func liveTSSKey(pid uint32, libPath string, code []byte) (uint32, error) {
	keyOff, err := ParseAutoTSSKeyOffset(code)
	if err != nil {
		return 0, err
	}
	resolver, err := newSymbolResolver(procmap.NewResolver(), pid, libPath)
	if err != nil {
		return 0, err
	}
	addr, err := resolver.addr("_PyRuntime")
	if err != nil {
		return 0, err
	}
	raw, err := NewProcReader(int(pid), int(pid)).ReadU64(addr + uint64(keyOff))
	if err != nil {
		return 0, err
	}
	return uint32(raw >> 32), nil
}

// TestLiveInterpreterEvalRangeCoversTheEvalLoop pins the range that is the
// interpreter arm's on-switch against the interpreter installed on this
// machine.
//
// It is allowed to SKIP on a build whose dispatch loop has no symbol
// (stripped and PGO-partitioned, e.g. Fedora 44's libpython3.14) because
// that is a documented, named refusal -- but it must skip with that reason
// attached, not by quietly finding nothing.
func TestLiveInterpreterEvalRangeCoversTheEvalLoop(t *testing.T) {
	cmd, _ := startLiveInterpreter(t)
	pid := uint32(cmd.Process.Pid)
	libPath, version, err := FindInterpreter(pid)
	if err != nil {
		t.Fatalf("FindInterpreter: %v", err)
	}
	r, err := EvalRangeForFile(libPath)
	if err != nil {
		if errors.Is(err, ErrEvalLoopNotLocatable) {
			t.Skipf("%s (CPython %s): %v", libPath, version, err)
		}
		t.Fatalf("EvalRangeForFile(%s): %v", libPath, err)
	}
	if r.Hi <= r.Lo {
		t.Fatalf("empty range [%#x,%#x)", r.Lo, r.Hi)
	}
	if size := r.Hi - r.Lo; size < minEvalLoopBytes {
		t.Fatalf("range is %d bytes, below the floor the picker enforces", size)
	}
	t.Logf("%s: eval range [%#x,%#x), %d bytes", libPath, r.Lo, r.Hi, r.Hi-r.Lo)
}
