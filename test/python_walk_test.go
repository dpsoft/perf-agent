package test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/pyunwind"
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
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("no python3 on PATH: %v", err)
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

// requireWalkableInterpreter skips -- with the reason attached -- when the
// interpreter this machine runs is one the walker declines by design, and
// returns otherwise. Every other outcome downstream is a failure.
//
// The three skips are the three NAMED refusals: an architecture with no
// measured glibc TSD offsets, a CPython outside 3.12-3.14, and a binary
// whose eval loop or PyGILState_GetThisThreadState cannot be read (a
// stripped PGO-partitioned build, or a toolchain shape the parser has never
// seen). They are checked against the live process rather than guessed at
// from the environment.
func requireWalkableInterpreter(t *testing.T, pid uint32) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skipf("the CPython walker is amd64-only: %s has no measured glibc TSD offsets (pyunwind.ErrUnsupportedArch)", runtime.GOARCH)
	}
	libPath, version, err := pyunwind.FindInterpreter(pid)
	require.NoError(t, err, "the workload is a python process; its interpreter must be findable")
	if !version.Supported() {
		t.Skipf("%s is CPython %s; this build walks 3.12 through 3.14 only", libPath, version)
	}
	if _, err := pyunwind.EvalRangeForFile(libPath); err != nil {
		if errors.Is(err, pyunwind.ErrEvalLoopNotLocatable) {
			t.Skipf("%s: %v", libPath, err)
		}
		t.Fatalf("EvalRangeForFile(%s): %v", libPath, err)
	}
	code, err := pyunwind.GILStateCode(libPath)
	require.NoError(t, err)
	if _, err := pyunwind.ParseAutoTSSKeyOffset(code); err != nil {
		if errors.Is(err, pyunwind.ErrTSSPatternUnrecognised) {
			t.Skipf("%s: %v (a toolchain shape this build has not measured)", libPath, err)
		}
		t.Fatalf("ParseAutoTSSKeyOffset(%s): %v", libPath, err)
	}
	t.Logf("interpreter under test: %s (CPython %s)", libPath, version)
}

// TestPythonFramesInterleavedWithNative is the end-to-end gate: run the
// agent against a live multi-threaded interpreter and require that the
// profile carries this workload's Python frames, in the right order,
// interleaved with the native frames they ran under.
func TestPythonFramesInterleavedWithNative(t *testing.T) {
	agentPath := getAgentPath(t)
	requireBPFRunnable(t, agentPath)

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
			addr, ok := parsePythonFrameName(name)
			if !ok {
				continue
			}
			pythonFrames++
			if fn, ok := want[addr]; ok {
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

	// ---- 3. The interleaving: one sample carrying both segments, in
	// order, with native frames between them and none of the Python frames
	// repeated.
	var best *profile.Sample
	var bestNames []string
	for _, s := range prof.Sample {
		names := sampleFrameNames(s)
		if _, ok := pythonOrderIn(names, codes); ok {
			best, bestNames = s, names
			break
		}
	}
	require.NotNilf(t, best,
		"no sample carried the full interleaved sequence %v -> <native> -> %v.\nOne sample rendered:\n%s\nagent log:\n%s",
		pythonInnerSegment, pythonOuterSegment, renderSample(prof.Sample[0]), agentLog.String())

	for _, defect := range pythonSampleDefects(bestNames, codes) {
		t.Errorf("%s\n%s", defect, renderSample(best))
	}
	t.Logf("interleaved sample:\n%s", renderSample(best))
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
// gives an unsymbolized Python frame (dwarfagent.pythonFrameName).
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
