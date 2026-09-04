package test

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/pyunwind"
	"github.com/dpsoft/perf-agent/unwind/interp"
)

// This file is the first place on this branch where a Python frame produced
// by a REAL interpreter is observed. Everything before it -- the offset
// tables, the frame records, the TSD walk, the chain walk in walk_step --
// was asserted structurally: that source text contains a string, that a Go
// struct mirrors a C one, that a decoder handles a synthetic record. The
// BPF verifier accepted the program; that is all anyone knew.
//
// So the shape of the test matters as much as its subject:
//
//   - It asserts CONTENT, not a count. The workload prints id(f.__code__)
//     for each of its functions, which in CPython is the code object's
//     address -- the exact word the walker pushes. Python frames are
//     unsymbolized in this slice, and without those printed addresses the
//     strongest available claim would be "something Python-shaped
//     appeared", which a walker returning garbage pointers would also
//     satisfy.
//
//   - It asserts ORDER, not adjacency. Two unrelated stacks concatenated
//     would pass a test that counted kinds.
//
//   - It asserts NO DUPLICATES. _PyInterpreterFrame.previous links run
//     through the C boundary, so a walker that restarts from
//     tstate->current_frame at the second eval-loop frame re-pushes the
//     inner segment beneath the outer one's native position: a plausible
//     stack in a plausible order, with three frames that were never there.
//     That is the bug Task 7 found and fixed, and this is its runtime test.
//
//   - Its subject is a NON-MAIN thread. The workload's main thread blocks
//     in join() and the asserted functions run only on the worker, so a
//     walker that could only ever reach the main thread's PyThreadState --
//     which is what the whole pthread-TSD mechanism exists to avoid --
//     fails here rather than passing quietly.

// pythonFuncs are the workload's functions, leaf-ward first, as they must
// appear in a sample: the inner segment (leaf, inner, key_fn) runs under
// list.sort()'s C frames, which run under the outer segment (middle, outer,
// worker).
var pythonInnerSegment = []string{"leaf", "inner", "key_fn"}
var pythonOuterSegment = []string{"middle", "outer", "worker"}

// codeLineRe matches the workload's "CODE <name> 0x<addr>" lines.
var codeLineRe = regexp.MustCompile(`^CODE (\w+) (0x[0-9a-f]+)$`)

// framesPushedRe pulls the BPF-side per-CPU counter out of the agent's
// shutdown line. It is an INDEPENDENT measurement of the same fact the
// profile carries: the counter is bumped inside py_push_frames, the profile
// frames come from the ringbuf record and the userspace decoder. Agreement
// between them is worth having; a profile with Python frames and a zero
// counter would mean the decoder was inventing them.
var framesPushedRe = regexp.MustCompile(`python walk: frames_pushed=(\d+)`)

// startPythonFixture launches the two-segment workload and returns its
// process plus the code-object address of each named function.
func startPythonFixture(t *testing.T, seconds int) (*exec.Cmd, map[string]uint64) {
	t.Helper()
	// PYUNWIND_TEST_PYTHON picks the interpreter, exactly as pyunwind's
	// live tests do. A machine usually has several -- the distro's, a
	// tool-cache one, a container's -- and they are DIFFERENT BUILDS with
	// different prologues and different eval-loop layouts, so which one ran
	// is part of the result. Without the override, reproducing a run means
	// undocumented PATH surgery.
	py := os.Getenv("PYUNWIND_TEST_PYTHON")
	if py == "" {
		var err error
		py, err = exec.LookPath("python3")
		if err != nil {
			declinePythonGate(t, "no python3 on PATH and %s is unset: %v", "PYUNWIND_TEST_PYTHON", err)
		}
	}
	cmd := exec.Command(py, "workloads/python/interleaved_threads.py", strconv.Itoa(seconds))
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	type res struct {
		codes map[string]uint64
		err   string
	}
	done := make(chan res, 1)
	go func() {
		codes := map[string]uint64{}
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "READY" {
				done <- res{codes: codes}
				return
			}
			if m := codeLineRe.FindStringSubmatch(line); m != nil {
				v, err := strconv.ParseUint(m[2], 0, 64)
				if err == nil {
					codes[m[1]] = v
				}
			}
		}
		done <- res{err: "workload exited before printing READY"}
	}()
	select {
	case r := <-done:
		if r.err != "" {
			t.Fatal(r.err)
		}
		want := append(append([]string{}, pythonInnerSegment...), pythonOuterSegment...)
		for _, name := range want {
			if _, ok := r.codes[name]; !ok {
				t.Fatalf("workload did not report a code object for %q: %v", name, r.codes)
			}
		}
		return cmd, r.codes
	case <-time.After(30 * time.Second):
		t.Fatal("workload never printed READY")
	}
	return nil, nil
}

// ---- The gate must not be able to stop gating.
//
// On the first CI run this test SKIPPED, and the job went green, because
// Ubuntu's own /usr/bin/python3.12 has a PyGILState_GetThisThreadState
// shape the parser had never seen and an unrecognised shape was classified
// as an environment refusal. The branch's only end-to-end evidence
// disappeared and reported success. Two things guard against that now.
//
// FIRST, the taxonomy. A refusal is a property of the ENVIRONMENT only when
// we never claimed to handle it: another architecture, a CPython outside
// 3.12-3.14, a musl libc, or a stripped PGO build whose eval loop has no
// symbol. An interpreter that is amd64 + glibc + CPython 3.12-3.14 +
// locatable eval loop is one we CLAIM TO SUPPORT, and every refusal on it
// -- an unrecognised prologue above all -- is OUR defect and fails.
//
// SECOND, PERF_AGENT_REQUIRE_PYTHON_WALK. CI sets it on the amd64
// integration runner, where we know what the interpreter is, and it turns
// every remaining skip into a failure. That moves the decision "may this
// machine skip" out of the code under test -- which decides it by calling
// the very functions the gate exists to check -- and into CI configuration.
// A regression in EvalRangeForFile or GILStateCode that made every build
// look unsupported would otherwise silently disarm the gate everywhere.
//
// THIRD, the audit in TestMain below: if this test neither ran its
// assertions nor recorded a decline reason, the run fails. That covers the
// test being renamed, removed, or skipped by a path nobody thought to
// classify.

// pythonGateOutcome is "ran", "declined: <reason>", or absent. Package
// level and written once, read by TestMain after the run.
var pythonGateOutcome atomic.Value

const requireWalkEnv = "PERF_AGENT_REQUIRE_PYTHON_WALK"

// declinePythonGate records why the gate is not running and skips -- or
// fails, when this machine was declared one that must walk Python.
func declinePythonGate(t *testing.T, format string, args ...any) {
	t.Helper()
	reason := fmt.Sprintf(format, args...)
	if os.Getenv(requireWalkEnv) != "" {
		t.Fatalf("%s is set, so this machine must walk Python frames, but: %s", requireWalkEnv, reason)
	}
	pythonGateOutcome.Store("declined: " + reason)
	t.Skip(reason)
}

// TestMain fails a run in which the gate silently stopped gating.
//
// It deliberately does nothing when -run is narrowing the selection: a
// developer running one unrelated test has not disarmed anything.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && pythonGateOutcome.Load() == nil && !testRunFilterActive() {
		fmt.Fprintf(os.Stderr,
			"\nFAIL: %s neither ran nor recorded why it declined.\n"+
				"The Python end-to-end gate is the only test that observes a Python frame produced by a real\n"+
				"interpreter; a run in which it silently does not execute must not be green.\n",
			"TestPythonFramesInterleavedWithNative")
		code = 1
	}
	os.Exit(code)
}

func testRunFilterActive() bool {
	f := flag.Lookup("test.run")
	return f != nil && f.Value.String() != ""
}

// requireWalkableInterpreter classifies this machine's interpreter and
// either returns (the gate must run and must pass) or declines with the
// reason attached. See the taxonomy note above.
func requireWalkableInterpreter(t *testing.T, pid uint32) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		declinePythonGate(t, "the CPython walker is amd64-only: %s has no measured glibc TSD offsets (pyunwind.ErrUnsupportedArch)", runtime.GOARCH)
	}
	libPath, version, err := pyunwind.FindInterpreter(pid)
	require.NoError(t, err, "the workload is a python process; its interpreter must be findable")
	if !version.Supported() {
		declinePythonGate(t, "%s is CPython %s; this build walks 3.12 through 3.14 only", libPath, version)
	}
	if err := pyunwind.RequireGlibc(pid); err != nil {
		declinePythonGate(t, "%v", err)
	}

	// From here on the interpreter is one we claim to support, and every
	// refusal is a defect of ours -- with one exception, called out by its
	// own sentinel: a build whose dispatch loop has no symbol at all.
	frags, err := pyunwind.EvalRangesForFile(libPath)
	if err != nil {
		if errors.Is(err, pyunwind.ErrEvalLoopNotLocatable) {
			declinePythonGate(t, "%s: %v", libPath, err)
		}
		t.Fatalf("EvalRangesForFile(%s): %v", libPath, err)
	}
	// A build whose eval loop is split into more fragments than the walker
	// can scan is walkable, but only partly: samples in the fragments that
	// were dropped carry no Python frames. Logged rather than skipped,
	// because it is the shape that made the handoff look completely dead on
	// uv's cpython-3.12.14 and it should be visible in the CI output the day
	// CI's interpreter grows a third partition.
	if len(frags) > interp.MaxSpans {
		t.Logf("NOTE: %s has %d eval-loop fragments; the walker claims the largest %d, "+
			"so some eval-loop samples will carry no Python frames", libPath, len(frags), interp.MaxSpans)
	}
	code, err := pyunwind.GILStateCode(libPath)
	require.NoErrorf(t, err, "reading PyGILState_GetThisThreadState out of %s", libPath)
	if _, err := pyunwind.ParseAutoTSSKeyRef(code); err != nil {
		// NOT a skip. This is the failure the first CI run turned into a
		// green build: /usr/bin/python3.12 on Ubuntu noble is squarely an
		// interpreter we claim to support, and our inability to decode its
		// prologue is our defect, not a property of the machine. Measure
		// the body, add the shape to pyunwind/tssparse.go.
		t.Fatalf("%s (CPython %s): %v\n"+
			"This is a supported interpreter whose prologue this build cannot decode. Disassemble the %d-byte "+
			"PyGILState_GetThisThreadState body and add its shape; do not make this a skip.",
			libPath, version, err, len(code))
	}
	t.Logf("interpreter under test: %s (CPython %s)", libPath, version)
}

// TestPythonFramesInterleavedWithNative is the end-to-end gate: run the
// agent against a live multi-threaded interpreter and require that the
// profile carries this workload's Python frames, in the right order,
// interleaved with the native frames they ran under.
func TestPythonFramesInterleavedWithNative(t *testing.T) {
	agentPath := getAgentPath(t)
	if !bpfRunnable(agentPath) {
		declinePythonGate(t, "requires root, CAP_BPF in the test process, or a setcap'd perf-agent")
	}

	workload, codes := startPythonFixture(t, 40)
	pid := uint32(workload.Process.Pid)
	requireWalkableInterpreter(t, pid)

	out := filepath.Join(t.TempDir(), "python-walk.pb.gz")
	agent := exec.Command(agentPath,
		"--profile", "--unwind", "dwarf",
		"--pid", strconv.FormatUint(uint64(pid), 10),
		"--duration", "10s",
		"--sample-rate", "199",
		"--profile-output", out)
	var agentLog bytes.Buffer
	agent.Stdout = &agentLog
	agent.Stderr = &agentLog
	require.NoError(t, agent.Run(), "agent run failed:\n%s", agentLog.String())
	t.Logf("agent log:\n%s", agentLog.String())

	// The enrolment decision itself, before anything about frames. It
	// separates the two failures that otherwise look identical from the
	// profile alone: "the agent never tried to attach this interpreter" and
	// "it attached and the walk found nothing". enrollPython logs every
	// outcome, including refusals, precisely so this can be checked.
	require.Containsf(t, agentLog.String(), "python frames: pid",
		"the agent reported no CPython enrolment decision at all for pid %d:\n%s", pid, agentLog.String())
	require.NotContainsf(t, agentLog.String(), "REFUSED",
		"the agent refused this interpreter after requireWalkableInterpreter found it walkable:\n%s", agentLog.String())

	prof := parseProfile(t, out)
	require.NotEmpty(t, prof.Sample, "no samples at all: the capture, not the walker, is what failed")

	// ---- 1. Python frames exist at all, and they are THIS workload's.
	//
	// A positive lower bound, not "> 0 somewhere": a run that found one
	// Python frame in ten seconds at 199 Hz has not demonstrated a working
	// walker. And the frames are matched by code-object address, so a
	// walker emitting plausible garbage pointers fails here.
	want := map[uint64]string{}
	for name, addr := range codes {
		want[addr] = name
	}
	seenFuncs := map[string]int{}
	pythonFrames := 0
	for _, s := range prof.Sample {
		for _, name := range sampleFrameNames(s) {
			fn, ok := pythonFuncName(name, want)
			if !ok {
				continue
			}
			pythonFrames++
			if fn != "" {
				seenFuncs[fn]++
			}
		}
	}
	require.Greaterf(t, pythonFrames, 20,
		"only %d Python frames in %d samples; py_eval_ranges, py_procs or the walk itself is not doing its job.\nagent log:\n%s",
		pythonFrames, len(prof.Sample), agentLog.String())
	for _, name := range append(append([]string{}, pythonInnerSegment...), pythonOuterSegment...) {
		require.Containsf(t, seenFuncs, name,
			"no frame carried %s's code object (%#x); frames seen: %v\nagent log:\n%s",
			name, codes[name], seenFuncs, agentLog.String())
	}

	// ---- 2. The BPF counter agrees with the decoded profile.
	m := framesPushedRe.FindStringSubmatch(agentLog.String())
	require.NotNil(t, m, "the agent did not report py_walk_counters:\n%s", agentLog.String())
	pushed, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	require.Positivef(t, pushed, "py_walk_counters reports 0 frames pushed while the profile shows %d; one of the two is lying", pythonFrames)

	// ---- 3. The interleaving, across EVERY sample that carries both
	// segments -- not the first one found.
	//
	// The selector's predicate (the two segments in order) is one a
	// defective-but-plausible stack satisfies: the Task 7 duplication bug
	// produces stacks that pass it. Checking only the first match lets an
	// intermittent defect be cherry-picked away by whichever sample the map
	// iteration happened to yield first. The union of defects over all
	// matching samples cannot be.
	var matching []*profile.Sample
	defects := map[string]*profile.Sample{}
	for _, s := range prof.Sample {
		names := sampleFrameNames(s)
		if _, ok := pythonOrderIn(names, codes); !ok {
			continue
		}
		matching = append(matching, s)
		for _, d := range pythonSampleDefects(names, codes) {
			if _, seen := defects[d]; !seen {
				defects[d] = s
			}
		}
	}
	require.NotEmptyf(t, matching,
		"no sample carried the full interleaved sequence %v -> <native> -> %v.\nOne sample rendered:\n%s\nagent log:\n%s",
		pythonInnerSegment, pythonOuterSegment, renderSample(prof.Sample[0]), agentLog.String())

	for d, sample := range defects {
		t.Errorf("%s\n%s", d, renderSample(sample))
	}
	t.Logf("%d of %d samples carried both Python segments; one of them:\n%s",
		len(matching), len(prof.Sample), renderSample(matching[0]))
	pythonGateOutcome.Store("ran")
}

// pythonSampleDefects reports everything wrong with one walked stack,
// leaf-first, given the workload's code-object addresses. An empty result
// means the stack is the one the design promises.
//
// It is a pure function over frame names so it can be driven by
// TestPythonSampleDefectsCatchesTheFailureShapes with stacks that never
// came from a profiler -- including the ones that a walker with the Task 7
// bug would produce. An assertion that has only ever seen passing input is
// not known to be able to fail.
func pythonSampleDefects(names []string, codes map[string]uint64) []string {
	byAddr := map[uint64]string{}
	for name, addr := range codes {
		byAddr[addr] = name
	}
	var defects []string

	positions, ok := pythonOrderIn(names, codes)
	if !ok {
		return []string{fmt.Sprintf("the stack does not carry %v then %v in that leaf-to-root order",
			pythonInnerSegment, pythonOuterSegment)}
	}

	// A native frame BETWEEN the two segments is the whole claim: the inner
	// segment ran under list.sort()'s C frames, which ran under the outer
	// segment. Without it the two segments are merely adjacent, and
	// adjacency is what a walker that consumed the native stack in one go
	// would also produce.
	innerEnd := positions[pythonInnerSegment[len(pythonInnerSegment)-1]]
	outerStart := positions[pythonOuterSegment[0]]
	natives := 0
	for i := innerEnd + 1; i < outerStart; i++ {
		if _, isPython := parsePythonFrameName(names[i]); !isPython {
			natives++
		}
	}
	if natives == 0 {
		defects = append(defects, "the two Python segments are adjacent with no native frame between them")
	}

	// Every code object at most once. Duplication is the exact failure the
	// entry-frame stop and the resume cursor prevent, and it renders as a
	// perfectly plausible stack.
	counts := map[uint64]int{}
	for _, name := range names {
		if addr, ok := parsePythonFrameName(name); ok {
			counts[addr]++
		}
	}
	for addr, n := range counts {
		if n > 1 {
			defects = append(defects, fmt.Sprintf(
				"code object %#x (%s) appears %d times: the inner segment was re-pushed beneath the outer one",
				addr, byAddr[addr], n))
		}
	}

	// A Python frame must sit on top of a native frame, not at the very
	// leaf: the eval-loop frame it is running in is native, and ruling
	// T7-R7 puts the Python frames root-ward of it.
	if positions[pythonInnerSegment[0]] == 0 {
		defects = append(defects, "the leafmost frame is a Python frame: the eval-loop native frame it should sit above is missing")
	}

	// The native frames must survive the Python walk. A walk that ran the
	// chain to NULL would consume them.
	nativeTotal := 0
	for _, name := range names {
		if _, isPython := parsePythonFrameName(name); !isPython {
			nativeTotal++
		}
	}
	if nativeTotal == 0 {
		defects = append(defects, "no native frames left in the sample: the Python walk consumed the stack")
	}
	return defects
}

// pythonOrderIn reports whether names carries every workload function in
// the required leaf-to-root order, and where each landed.
//
// Order, not membership: the returned positions must be strictly
// increasing across leaf, inner, key_fn, middle, outer, worker. A profile
// whose Python frames are all piled at one end of the stack, or whose
// segments came back swapped, has the right frames in a call path that
// never happened.
func pythonOrderIn(names []string, codes map[string]uint64) (map[string]int, bool) {
	positions := map[string]int{}
	want := append(append([]string{}, pythonInnerSegment...), pythonOuterSegment...)
	at := -1
	for _, fn := range want {
		found := -1
		for i := at + 1; i < len(names); i++ {
			addr, ok := parsePythonFrameName(names[i])
			if ok && addr == codes[fn] {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, false
		}
		positions[fn] = found
		at = found
	}
	return positions, true
}

// parsePythonFrameName decodes the "python:0x…" rendering the profiler
// gives an UNRESOLVED Python frame (pyunwind.FrameName).
func parsePythonFrameName(name string) (uint64, bool) {
	const prefix = "python:"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(name, prefix), 0, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// resolvedPythonFrame matches the RESOLVED rendering, "qualname (file.py:12)",
// and returns the qualname.
//
// Both forms have to be recognised, and which one appears is a property of the
// interpreter rather than of the walk: pyunwind resolves a frame by reading
// co_qualname out of the live process, and it only has measured code-object
// offsets for some CPython versions. On an interpreter it has not been
// measured against, the walk works exactly as well and the frames stay
// addresses. A test that understood only one form would report a walker
// failure ("only 0 Python frames") for a naming difference.
var resolvedPythonFrame = regexp.MustCompile(`^(\S+) \([^()]*\.py:\d+\)$`)

func parseResolvedPythonFrame(name string) (string, bool) {
	m := resolvedPythonFrame.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// pythonFuncName reduces either rendering to the function this test knows by
// name. byAddr maps a code-object address to the function defined at it.
//
// A resolved frame gives the QUALIFIED name -- "Widget.method_here" -- so the
// bare function name is the part after the last dot.
func pythonFuncName(name string, byAddr map[uint64]string) (string, bool) {
	if addr, ok := parsePythonFrameName(name); ok {
		// A Python frame either way. byAddr names the ones this workload
		// defined; an address it does not know is still a Python frame and
		// still counts toward the lower bound, it just names no function.
		return byAddr[addr], true
	}
	if q, ok := parseResolvedPythonFrame(name); ok {
		if i := strings.LastIndex(q, "."); i >= 0 {
			q = q[i+1:]
		}
		return q, true
	}
	return "", false
}

// sampleFrameNames renders a sample's frames leaf-first, one entry per
// location, using the function name where there is one and the mapping plus
// address where there is not.
func sampleFrameNames(s *profile.Sample) []string {
	out := make([]string, 0, len(s.Location))
	for _, loc := range s.Location {
		out = append(out, locationName(loc))
	}
	return out
}

func locationName(loc *profile.Location) string {
	for _, ln := range loc.Line {
		if ln.Function != nil && ln.Function.Name != "" {
			return ln.Function.Name
		}
	}
	file := "?"
	if loc.Mapping != nil && loc.Mapping.File != "" {
		file = filepath.Base(loc.Mapping.File)
	}
	return fmt.Sprintf("[%s]+%#x", file, loc.Address)
}

// renderSample prints a sample leaf-first, marking Python frames, so a
// failure message shows the stack that was actually walked rather than
// asking the reader to reconstruct it.
func renderSample(s *profile.Sample) string {
	var b strings.Builder
	for i, loc := range s.Location {
		name := locationName(loc)
		kind := "native"
		if _, ok := parsePythonFrameName(name); ok {
			kind = "PYTHON"
		}
		fmt.Fprintf(&b, "  #%02d %-6s %s\n", i, kind, name)
	}
	return b.String()
}
