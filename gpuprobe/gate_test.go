package gpuprobe_test

import (
	"bytes"
	"context"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/internal/cubin"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// hasCaps checks Permitted as well as Effective, because a setcap'd binary
// has not promoted Permitted yet, and never gates on Getuid alone.
func hasCaps(want ...cap.Value) bool {
	if os.Geteuid() == 0 {
		return true
	}
	set := cap.GetProc()
	if set == nil {
		return false
	}
	for _, w := range want {
		ok := false
		for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
			if have, err := set.GetFlag(flag, w); err == nil && have {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// hasBPFAndPerfmon is the attach-side capability question: can this binary
// load the BPF object and attach the uprobes at all.
func hasBPFAndPerfmon() bool { return hasCaps(cap.BPF, cap.PERFMON) }

// hasGateCaps is the whole-pipeline question, and it is strictly larger.
// CAP_CHECKPOINT_RESTORE is what lets blazesym follow /proc/<pid>/map_files/;
// without it symbolize.NewLocalSymbolizer refuses outright, and before it
// refused, this gate ran green while all 63 sampled stacks resolved to bare
// hex addresses. Gating on the capability the code actually needs is what
// turns that into a visible skip instead of a passing run with useless
// output.
func hasGateCaps() bool { return hasCaps(cap.BPF, cap.PERFMON, cap.CHECKPOINT_RESTORE) }

// The phase gate: a GPU-free producer drives the full pipeline to pprof
// samples on a machine with no GPU.
//
// # Why this drives perfagent-gpu-fpless and not perfagent-gpu-stub
//
// Through Phase 4a this gate drove `shim/perfagent-gpu-stub`, whose chain is
// main -> perfagent_stub_run -> probe. Phase 4a's assertion was "some frame
// is named perfagent_stub_run", which the leaf satisfies on its own, so the
// gate passed before the DWARF walker existed and would pass again if the
// DWARF walker were deleted: it could not tell this phase from the previous
// one. Adding `StacksWalkedDWARF > 0` to a gate driven by that producer would
// have been worse than useless - a green assertion that never ran the code it
// names.
//
// It is worse than "an FP walk follows that chain on its own", which is what
// this comment used to say. It does not. GCC 16 at -O2 gives that `main` no
// frame pointer at all: it spills the caller's %rbp to the stack and holds
// the launch count there instead (`mov %rbp,0x20(%rsp)` ... `mov $0x3e8,%ebp`
// - re-derivable with objdump on shim/perfagent-gpu-stub), so
// perfagent_stub_run saves an integer as its caller's frame pointer and the
// chain dies one step out. Phase 4a captured with bpf_get_stackid, and the
// kernel's perf_callchain_user stores each frame's return address BEFORE it
// follows that frame's saved FP, so those stacks were two frames deep -
// perfagent_stub_run and main - and reached nothing above main. This walker
// is stricter: walk_step drops a frame whose saved FP is not monotonic
// without keeping the return address it already read, so the SAME producer
// under THIS walker yields one frame. Either way, driving the phase off that
// producer would prove nothing about crossing an FP-less frame.
//
// `shim/perfagent-gpu-fpless` emits byte-for-byte the same records (it links
// the same stub/stub.cc and the same shim/core/), but reaches them through
// two frames compiled -fomit-frame-pointer:
//
//	main                      frame pointer      <- the walk must reach here
//	perfagent_fpless_caller   NO frame pointer      DWARF only
//	perfagent_fpless_bridge   NO frame pointer      DWARF only
//	perfagent_stub_run        frame pointer      <- the probe fires here
//
// so every Phase 4a assertion below still means exactly what it meant, and
// the new ones have something real to bite on. See shim/stub/fpless_bridge.cc
// for why a saved-RBP walk provably cannot produce `perfagent_fpless_caller`,
// and `make -C shim check-fpless` (run below) for the build-time proof that
// the toolchain actually omitted those frame pointers.
func TestStubDrivesThePipelineToPprofWithoutAGPU(t *testing.T) {
	if !hasGateCaps() {
		t.Skip("needs CAP_BPF, CAP_PERFMON and CAP_CHECKPOINT_RESTORE " +
			"(the last one so blazesym can follow /proc/<pid>/map_files/; without it " +
			"symbolize.NewLocalSymbolizer refuses and every frame would be a bare address); " +
			"sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep <test binary>")
	}
	built := filepath.Join("..", "shim", "perfagent-gpu-fpless")
	requireBuilt(t, built)
	// Not a build step: a check. If GCC ignored -fomit-frame-pointer - and
	// Fedora ships -fno-omit-frame-pointer in its RPM build flags, so that is
	// a real failure mode - the two bridge frames would be followable by the
	// frame-pointer walker this phase replaces, and every new assertion below
	// would go green while proving nothing. Fail here, where the cause is
	// legible, rather than there.
	requireFPLess(t, built)
	// Attach to a private copy, not to the shared build output. Uprobes key
	// on the binary's *inode*, and this consumer must attach system-wide
	// (Config.PID is zero) because the stub process does not exist yet — the
	// stub only emits once the semaphore says someone is listening, so it has
	// to be launched after Attach. A system-wide attach on the shared inode
	// therefore also collects records from *any other* process on the machine
	// running that same image: a concurrent gate run in CI, a second
	// developer, a stray perfagent-gpu-stub. That is not hypothetical — a run
	// failed with "Records: expected 1000, actual 1128" for exactly this
	// reason. A per-run copy has an inode nobody else can execute, which is
	// what makes the exact count below deterministic.
	stub := privateStubCopy(t, built)

	// A real symbolizer, not nil: without one every capture counts as
	// SymbolizeFailed and no stack ever resolves, failing the gate for a
	// reason that has nothing to do with the transport under test. This is
	// the same construction profile/ uses.
	sym, err := symbolize.NewLocalSymbolizer()
	require.NoError(t, err)
	defer func() { _ = sym.Close() }()

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath:   stub,
		Backend:    gpu.GPUBackendID("stub"),
		Sink:       timeline,
		Symbolizer: sym,
	})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	// 30s, not 15: the gate now holds the producer open until the consumer
	// has counted every sampled launch, and that wait has its own 10s
	// deadline. A context that could expire underneath it would turn a clear
	// "timed out waiting for N sampled launches" into a pile of zero-valued
	// assertions with no cause attached.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// The stub only emits once the semaphore says someone is listening, so it
	// must start after Attach.
	//
	// stderr is captured separately from stdout because the stub reports its
	// own producer-side drop counters there: with a consumer attached the
	// semaphore is non-zero for the whole run, so nothing may be dropped
	// before it ever reaches a probe. A consumer-side counter cannot see that
	// loss — it happens in the producer, upstream of the ringbuf.
	// sample_period=8 explicit (it also happens to be the stub's default):
	// with 500 launches the sampler yields exactly 58 sampled launches.
	//
	// 58, not ceil(500/8)==63, since issue #50: the sampler no longer takes
	// every 8th launch. It draws each gap uniformly from [4,12] -- mean
	// exactly 8, so the RATE is unchanged -- from a hash of (seed, sample
	// point), which is why this count is still an equality and not a
	// tolerance band. The schedule is a deterministic chain from the stub's
	// seed, and 58 is its exact length over 500 launches; it is pinned on
	// both sides of the shim/consumer boundary by
	// shim/core/sampler_test.cc's schedule pin and by
	// TestTheGoReplicaMatchesTheShimSampler, so a change to the sampler
	// fails there with a legible cause before it reaches this gate.
	//
	// The fourth argument is the linger: after flushing, the stub waits for
	// EOF on stdin (up to 10s) before exiting. Start(), not Run(): the
	// consumer symbolizes each sampled stack against /proc/<pid>/maps of the
	// LAUNCHING process, and the kernel destroys those maps the instant the
	// process exits. cmd.Run() blocks until exit, so under it every record
	// still in the ringbuf at that moment symbolizes to bare addresses — the
	// assertion below on the stub's own frame then fails for a reason that
	// has nothing to do with the capture path. Holding the producer open
	// until the consumer has counted every sampled launch removes the race
	// instead of papering over it with a longer sleep.
	// period_us is 1000, not the 200 this gate used through Phase 4a. That
	// was a workaround for the startup window issue #49 has since closed: in
	// system-wide mode (Config.PID == 0, which this gate uses because the
	// producer does not exist at Attach time) the walker's CFI tables used to
	// be compiled on the first batch the consumer saw FROM that process, so
	// every launch sampled during the compile was walked with no tables. At
	// 200us the whole 500-launch run lasted ~100ms against a ~50ms compile
	// and the split was a coin toss; 1000us stretched the run to ~500ms so
	// that most captures landed on the right side of it.
	//
	// The producer now blocks in perfagent_stub_run, before its first launch,
	// until the consumer has installed its tables (shim/core/enroll.h), so
	// the split is no longer timing-dependent at all and StacksWalkedNoTables
	// is asserted zero below. The period is left at 1000 anyway: nothing
	// depends on it - the sampler counts launches, not time, so the 58 below
	// is unchanged either way - and a slower run is the more demanding one
	// for every other counter here.
	cmd := exec.Command(stub, "500", "1000", "8", "10000")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	release, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// Closing the pipe is what lets the stub exit. Deferred as a belt-and-
	// braces release so a t.Fatal between here and the explicit close below
	// cannot leave the stub parked for its full backstop.
	defer func() { _ = release.Close() }()

	// Wait for the consumer to have OBSERVED every sampled launch, rather
	// than sleeping and hoping. Stats is the right handle: SampledLaunches is
	// incremented on the decode path, and stacks are symbolized on that same
	// path, so once it reads 58 every symbolization has already happened —
	// and happened while the stub was alive.
	const wantSampled = 58
	deadline := time.Now().Add(10 * time.Second)
	var lastSampled uint64
	for {
		lastSampled = c.Stats().SampledLaunches
		if lastSampled >= wantSampled {
			break
		}
		if time.Now().After(deadline) {
			// Fail loudly rather than hang: a stub that never emitted and a
			// consumer that never drained look identical from a timeout, so
			// print what the consumer did see.
			_ = release.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			cancel()
			<-done
			t.Fatalf("timed out after 10s waiting for %d sampled launches; consumer saw %d. stats: %+v stderr: %s",
				wantSampled, lastSampled, c.Stats(), stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Every sampled stack is symbolized by now, so the producer may go.
	require.NoError(t, release.Close())
	require.NoError(t, cmd.Wait(), "stdout: %s stderr: %s", stdout.String(), stderr.String())
	stubErr := stderr.String()

	// perfagent_stub_run flushes both batches synchronously before it
	// lingers, so every record is in the ringbuf by this point. This sleep is
	// for our own Run() goroutine to drain the tail of BATCHED launches and
	// execs — none of which needs a live process, because only sampled
	// launches carry a stack to symbolize.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	// Producer-side loss, from the stub's own accounting. With a consumer
	// attached the whole run, every add() and flush() saw an armed semaphore,
	// so both counters must read zero.
	assert.Contains(t, stubErr, "launch_dropped=0",
		"the stub dropped launches: its semaphore read zero while a consumer was attached")
	assert.Contains(t, stubErr, "exec_dropped=0",
		"the stub dropped execs: its semaphore read zero while a consumer was attached")

	// Parse the stub's own sampler accounting so it can be checked against
	// what the consumer counted: producer and consumer must not disagree
	// about how many stacks were taken.
	stubLine := regexp.MustCompile(`observed=(\d+) sampled=(\d+) period=(\d+)`).FindStringSubmatch(stubErr)
	require.Len(t, stubLine, 4, "stub stderr did not match the expected format: %s", stubErr)
	stubObserved, err := strconv.ParseUint(stubLine[1], 10, 64)
	require.NoError(t, err)
	stubSampled, err := strconv.ParseUint(stubLine[2], 10, 64)
	require.NoError(t, err)
	stubPeriod, err := strconv.ParseUint(stubLine[3], 10, 64)
	require.NoError(t, err)
	assert.Equal(t, uint64(500), stubObserved, "the sampler must see every one of the 500 launches")
	assert.Equal(t, uint64(8), stubPeriod, "the stub was invoked with sample_period=8")
	assert.Equal(t, uint64(58), stubSampled,
		"the deterministic jittered schedule at period 8 over 500 launches is exactly 58 samples long")
	assert.Contains(t, stubErr, "seed=0x9e3779b97f4a7c15",
		"the stub ran on a seed other than the shim's default, so the exact count above describes a different schedule than the one asserted")

	stats := c.Stats()
	assert.Zero(t, stats.SequenceGaps, "no batch may be lost silently")
	// Records == 1000 (500 launches + 500 execs) is no longer a safe
	// assertion: kernel-name records also flow now, and their count is
	// non-deterministic - it depends on when the consumer attaches relative
	// to the producer's late-attach replay tick. Assert on the launches and
	// executions specifically (via the deterministic join-count and
	// sampled-launch handles below) instead of a total that can legitimately
	// vary.
	assert.Equal(t, stubSampled, stats.SampledLaunches,
		"the producer's own sampled= count and the consumer's SampledLaunches must not disagree")
	assert.Equal(t, uint64(wantSampled), stats.SampledLaunches,
		"the sampler is deterministic given its seed, so this stays an exact count")
	// Records is incremented *before* the sink call, so it alone does not
	// prove the timeline accepted anything — every other loss counter has to
	// be zero for the count above to mean what it looks like it means.
	assert.Zero(t, stats.SinkRejected,
		"Records counts before the sink call, so a rejection here means the timeline never took the event")
	assert.Zero(t, stats.Malformed,
		"a ringbuf sample that did not decode: a short header, or a payload shorter than the header claims; reasons: %v",
		stats.DecodeFailures)
	assert.Zero(t, stats.Undecoded,
		"the stub emits only launches, execs, sampled launches and kernel names, all of which this phase normalizes; an undecoded record means a kind arrived that it does not")
	assert.Zero(t, stats.KernelDropped,
		"records the BPF program could not deliver: an oversized batch, a full ringbuf, or a faulting read of the producer's buffer")
	assert.Zero(t, stats.ZeroCorrelation,
		"the stub never emits correlation 0, so no record may have been demoted to the heuristic join")
	assert.Zero(t, stats.ZeroCorrelationExecs,
		"spec §6 makes a correlation mandatory on every execution; a non-zero here is the shim breaking its own contract, and the execution can then only be guessed at (issue #52)")
	assert.Zero(t, stats.StacksMissing,
		"StacksMissing is BPF-side capture loss - no scratch, an empty walk, a full or unwritable gpu_stacks - none of which depends on how deep the walk got")
	// StacksMissing says a capture failed; these say how it could have. All
	// four are zero on a healthy run, and each one that is not points at a
	// different part of the walk, which is the whole reason they are apart.
	assert.Zero(t, stats.StackWalkEmpty, "a walk that produced not one frame, not even the probe's own PC")
	assert.Zero(t, stats.StackMapFull, "gpu_stacks fills only if the consumer stopped draining")
	assert.Zero(t, stats.StackMapUpdateFailed, "an insert refused for any reason other than a full map")
	assert.Zero(t, stats.StackWalkScratchFailed, "a per-CPU scratch lookup at key 0 cannot fail on a loaded program")
	assert.Zero(t, stats.StacksEvicted,
		"the parked-stack side table must never overflow at this launch rate and capacity")
	// Issue #67, and the assertion that keeps it from regressing silently.
	// Every sampled record now reaches the ringbuf before the batched twin it
	// belongs to (shim/stub/stub.cc fires the probe before the batched add(),
	// pinned unprivileged by shim/stub/probe_order_test.cc), so every parked
	// stack has a launch still to come and the run is flushed by the time
	// this reads. A stack left parked at rest is a stack whose twin was
	// emitted before it and released stackless: attribution lost, silently
	// except for this gauge. It read 1 of 58 on main at 75ecc513.
	//
	// Checked before StacksAttached below because it is the more specific
	// failure: a shortfall in attached stacks could come from a dozen places,
	// a non-zero PendingStacks names one.
	assert.Zero(t, stats.PendingStacks,
		"a resolved stack is still parked with no launch to join, at rest and after Flush: sampled=%d resolved=%d attached=%d evicted=%d profiler-only=%d uncorrelated=%d",
		stats.SampledLaunches, stats.StacksResolved, stats.StacksAttached,
		stats.StacksEvicted, stats.StacksProfilerOnly, stats.StacksUncorrelated)
	assert.Equal(t, uint64(wantSampled), stats.StacksAttached,
		"every sampled launch must reach the timeline carrying its own stack; resolved=%d pending=%d evicted=%d profiler-only=%d uncorrelated=%d missing=%d",
		stats.StacksResolved, stats.PendingStacks, stats.StacksEvicted,
		stats.StacksProfilerOnly, stats.StacksUncorrelated, stats.StacksMissing)
	// The whole join, on one line, in the shape the accounting identity above
	// StacksResolved is written in: resolved = attached + evicted +
	// profiler-only + pending. Printed whether or not the assertions pass,
	// because "58 = 57 + 0 + 0 + 1" is what told issue #67 apart from silent
	// loss in the first place.
	t.Logf("stack attach: sampled=%d resolved=%d attached=%d evicted=%d profiler-only=%d pending=%d missing=%d uncorrelated=%d",
		stats.SampledLaunches, stats.StacksResolved, stats.StacksAttached,
		stats.StacksEvicted, stats.StacksProfilerOnly, stats.PendingStacks,
		stats.StacksMissing, stats.StacksUncorrelated)
	assert.Zero(t, stats.StackLookupFailed,
		"every resolved stack's gpu_stacks entry must be readable back exactly once")
	assert.Zero(t, stats.StackDeleteFailed, "every gpu_stacks entry read must also be deletable")
	assert.Zero(t, stats.StacksUncorrelated,
		"the stub never emits correlation 0 on a sampled launch")
	// Kept from Phase 4a, and worth being honest about: for THIS producer
	// this assertion cannot fail, and it is not the phase's proof.
	// shimScope disables itself outright when the shim it was pointed at is
	// an ET_EXEC program rather than a shared object (see newShimScope), and
	// perfagent-gpu-fpless is a program - the profiler and the application
	// are the same binary, so "the stack never left the shim" is not a
	// defect there and there is no boundary to police. The guard is only
	// ever armed for the injected-adapter shape, i.e. the CUDA run in Task
	// 5, and that is where "the guard went quiet" becomes evidence.
	// Asserted here anyway because a non-zero value would mean the guard
	// armed itself against a self-contained producer, which is a bug in the
	// classifier.
	assert.Zero(t, stats.StacksProfilerOnly,
		"the producer IS the shim and is an ET_EXEC program, so shimScope must not have armed itself at all; a refusal here means the injected-adapter guard misread a self-contained producer")
	assert.Zero(t, stats.StacksProfilerOnlyUncertain,
		"same guard, the without-proof half: it must be inert for a self-contained producer too")
	assert.Zero(t, stats.SymbolizeFailed,
		"a real Symbolizer is configured; every captured stack must resolve to at least one frame")
	// The counter that would have caught this branch's symbolization failure
	// on its own. SymbolizeFailed stayed at zero through a run in which all
	// 63 stacks resolved to nothing but hex addresses, because blazesym
	// returned a frame per IP and no error — see
	// symbolize.LocalSymbolizer.SymbolizeProcess. A regression here now fails
	// as a number rather than being noticed by a human reading the frame
	// dump in the assertion below.
	assert.Zero(t, stats.StacksUnresolved,
		"every captured stack must resolve at least one real symbol; %d/%d frames came back address-only",
		stats.StackFramesUnresolved, stats.StacksResolved)
	// Deliberately NOT asserted zero: a vDSO frame, or a libc built without
	// a usable symbol table, is a legitimate address-only frame in an
	// otherwise readable stack, and that varies by distro. StacksUnresolved
	// above is the invariant; this is the diagnostic printed alongside it.
	t.Logf("frames that resolved to a bare address: %d", stats.StackFramesUnresolved)
	assert.Zero(t, stats.KernelNamesUnresolved,
		"the stub interns both kernel names before any launch or exec references them; every event must carry a name")

	// ---- the assertions this phase exists for ----------------------------
	//
	// This is the one number that tells this phase from the previous one.
	// WALKER_FLAG_DWARF_USED is set by walk_step only on a frame it
	// classified MODE_FP_LESS and then unwound through a cfi_entry read out
	// of the cfi_rules table - i.e. only when the walker actually read this
	// producer's .eh_frame. The frame-pointer walker Phase 4a shipped could
	// not set it under any input.
	//
	// It cannot pass vacuously: the two FP-less frames are between the probe
	// and main by construction (make check-fpless, run above, fails the test
	// if the toolchain did not omit those frame pointers), so a walk that
	// gets past perfagent_stub_run's frame at all has to cross one.
	assert.Positive(t, stats.StacksWalkedDWARF,
		"no capture used the DWARF path: the walker never unwound an FP-less frame, so this run proves nothing the frame-pointer walker could not. FP-only=%d no-tables=%d cfi-miss=%d abandoned=%d",
		stats.StacksWalkedFPOnly, stats.StacksWalkedNoTables, stats.StacksWalkedCFIMiss, stats.StackWalkAbandoned)
	assert.Equal(t, uint64(wantSampled), stats.StacksWalkedDWARF+stats.StacksWalkedFPOnly,
		"every non-empty capture is counted exactly once as either a DWARF walk or an FP-only walk; a shortfall means captures went uncounted")

	// ---- issue #49: the tables exist before the first probe fires --------
	//
	// This assertion was `Less(NoTables, wantSampled)` through Phase 4b, with
	// a comment calling zero a trap: in system-wide mode the tables were
	// compiled after the consumer saw the producer's first batch, so the
	// first sampled launches were legitimately walked before they existed. On
	// the RTX 3090 that cost ~38% of all sampled stacks (issue #49), and here
	// it was one or two of the 63.
	//
	// It is zero now because the producer no longer races the compile: it
	// blocks in perfagent_stub_run, before its first launch and therefore
	// before its first probe, until this consumer has installed its tables
	// (shim/core/enroll.h, gpuprobe/enroll.go). There is no launch, so no
	// probe, so no walk, until that is done - so a non-zero count here does
	// not mean "the timing was unlucky", it means the rendezvous did not
	// happen or did not do its job, and the counters below say which.
	assert.Zero(t, stats.StacksWalkedNoTables,
		"a capture was walked with no CFI tables even though the producer waits for them before it launches anything. enroll: listening=%v requests=%d confirmed=%d refused=%d failed=%d err=%q; registration err=%q",
		stats.UnwindEnrollListening, stats.UnwindEnrollRequests, stats.UnwindEnrollConfirmed,
		stats.UnwindEnrollRefused, stats.UnwindEnrollFailed, stats.UnwindEnrollLastError,
		stats.UnwindLastError)
	assert.Zero(t, stats.StacksNoTablesAfterEnroll,
		"the producer was released on a promise that its tables were installed, and a later walk found none: the rendezvous is reporting success it did not deliver")
	assert.True(t, stats.UnwindEnrollListening,
		"the rendezvous address could not be bound, so the producer had nothing to wait on and fell back to the pre-#49 lazy path: %q",
		stats.UnwindEnrollLastError)
	assert.Equal(t, uint64(1), stats.UnwindEnrollConfirmed,
		"exactly one producer runs against this private stub copy, and it must have been told its tables are in. requests=%d refused=%d failed=%d err=%q",
		stats.UnwindEnrollRequests, stats.UnwindEnrollRefused, stats.UnwindEnrollFailed,
		stats.UnwindEnrollLastError)
	assert.Zero(t, stats.UnwindEnrollRefused,
		"a connection was turned away: either the peer credentials were unreadable or the producer did not map the inode this consumer attached to, which cannot be true of the process the uprobes fired in. %q",
		stats.UnwindEnrollLastError)
	assert.Zero(t, stats.UnwindEnrollFailed,
		"the rendezvous ran and installed no tables: %q", stats.UnwindEnrollLastError)
	assert.Zero(t, stats.UnwindEnrollThrottled,
		"one producer cannot exceed the rendezvous rate limit; if this fires the limiter is refusing legitimate traffic: %q",
		stats.UnwindEnrollLastError)
	assert.Zero(t, stats.UnwindEnrolledPIDsEvicted,
		"one producer against the default 128-PID bound cannot be evicted, so its tables cannot have been taken back")
	// The producer's own account of the same exchange, from its stderr. A
	// consumer-side counter cannot see a producer that never reached the
	// socket - it would simply never appear - so both ends are checked.
	assert.Contains(t, stubErr, "enroll=confirmed",
		"the producer did not get a confirmation before it started launching; it ran on the pre-#49 lazy path")
	assert.Positive(t, stats.UnwindPIDsRegistered,
		"the consumer never registered the producer's PID with the walker, so no capture could have had tables to use")
	assert.Zero(t, stats.UnwindPIDsFailed,
		"a registration that installed nothing: %q", stats.UnwindLastError)
	assert.Equal(t, uint64(wantSampled), stats.StacksWalkedDWARF,
		"every capture should now be a DWARF walk: the tables were installed before the producer launched anything, and the two FP-less bridge frames force the walker onto the CFI path. fp-only=%d no-tables=%d",
		stats.StacksWalkedFPOnly, stats.StacksWalkedNoTables)

	// StackWalkAbandoned means "the walk stopped because it could not
	// proceed". Through issue #44 it read 62 here alongside dwarf == 62 -
	// every single DWARF walk cut short - and the gate asserted that on
	// purpose, because asserting zero would have been asserting the bug.
	// Issue #45 is the fix and this block is where it is measured.
	//
	// What changed, frame by frame (derived from this binary's own CFI in
	// TestTheCFIForcesTheWalkToReachTheRoot, which runs unprivileged):
	//
	//   - the two FP-less bridges carry no rule for %rbp; ehcompile now
	//     compiles that to SAME_VALUE, as the x86-64 psABI says, instead of
	//     UNDEFINED. walk_step no longer zeroes the frame pointer crossing
	//     them, so main is reached WITH one.
	//   - main, __libc_start_call_main and __libc_start_main_impl are all
	//     FP_SAFE and are walked by frame pointer.
	//   - __libc_start_main_impl's saved-FP slot is zero, because _start does
	//     `xorl %ebp, %ebp`. walk_step now carries the return address stored
	//     beside that zero forward for one step instead of discarding it, so
	//     the walk lands on _start, whose CFI gives no return address:
	//     WALKER_FLAG_RA_UNDEFINED, the genuine root.
	//
	// So the DWARF walks that all ended in StackWalkFPExhausted now all end
	// in StackWalkReachedRoot, and abandonment goes to zero. The FP-only
	// walks (the first capture or two, before the tables land) end at the
	// same zero saved FP with WALKER_FLAG_FP_TERMINATED and no tables to
	// classify _start, which is also a complete walk and also not abandoned.
	assert.Zero(t, stats.StacksWalkedCFIMiss,
		"the tables were consulted and did not cover a frame's PC: a gap in what ehcompile produced for this binary")
	assert.Zero(t, stats.StackWalkFPExhausted,
		"a walk still arrived at an FP_SAFE frame with no frame pointer: the DWARF step below it zeroed %%rbp, which is the issue #45 defect. dwarf=%d fp-only=%d no-tables=%d reached-root=%d",
		stats.StacksWalkedDWARF, stats.StacksWalkedFPOnly, stats.StacksWalkedNoTables, stats.StackWalkReachedRoot)
	assert.Zero(t, stats.StackWalkFPNonMonotonic,
		"a frame's saved frame pointer did not name a caller frame; this producer has no such frame")
	assert.Zero(t, stats.StackWalkRootDisagreement,
		"the frame-pointer chain and the CFI disagreed about where a stack ends. On this producer they agree by construction: _start's own CFI declares the root (TestTheCFIForcesTheWalkToReachTheRoot), so a walk that ended any other way did not go where the derivation says it does")
	assert.Zero(t, stats.StackWalkAbandoned,
		"walks stopped without reaching an end of chain: %d abandoned of %d captures. cfi-miss=%d no-tables=%d dwarf=%d fp-only=%d fp-exhausted=%d nonmonotonic=%d",
		stats.StackWalkAbandoned, wantSampled, stats.StacksWalkedCFIMiss,
		stats.StacksWalkedNoTables, stats.StacksWalkedDWARF, stats.StacksWalkedFPOnly,
		stats.StackWalkFPExhausted, stats.StackWalkFPNonMonotonic+stats.StackWalkRootDisagreement)
	// The measurement itself. This is the assertion issue #44 left inverted
	// with the note "when #45 IS fixed this is the assertion to invert", and
	// it is tied to StacksWalkedDWARF so it cannot pass on a run where no
	// walk used CFI at all: a zero-DWARF run makes the equality vacuous, and
	// the Positive below refuses it outright.
	assert.Positive(t, stats.StackWalkReachedRoot,
		"not one walk reached a frame whose CFI marks it outermost. That was the state issue #45 was filed about (reached-root=0 on the RTX 3090 baseline); if it still reads zero, the fix did not take. dwarf=%d fp-only=%d fp-exhausted=%d abandoned=%d",
		stats.StacksWalkedDWARF, stats.StacksWalkedFPOnly, stats.StackWalkFPExhausted, stats.StackWalkAbandoned)
	assert.Equal(t, stats.StacksWalkedDWARF, stats.StackWalkReachedRoot,
		"a DWARF walk on this producer can only end at _start's RA_UNDEFINED - every frame between the probe and it is derived in TestTheCFIForcesTheWalkToReachTheRoot. A shortfall means some walk found a different ending and that derivation no longer describes the producer: dwarf=%d reached-root=%d fp-exhausted=%d",
		stats.StacksWalkedDWARF, stats.StackWalkReachedRoot, stats.StackWalkFPExhausted)
	t.Logf("walk shape: dwarf=%d fp-only=%d no-tables=%d cfi-miss=%d truncated=%d abandoned=%d fp-exhausted=%d nonmonotonic=%d root-disagree=%d reached-root=%d registered=%d binaries=%d",
		stats.StacksWalkedDWARF, stats.StacksWalkedFPOnly, stats.StacksWalkedNoTables,
		stats.StacksWalkedCFIMiss, stats.StackWalkTruncated, stats.StackWalkAbandoned,
		stats.StackWalkFPExhausted, stats.StackWalkFPNonMonotonic, stats.StackWalkRootDisagreement,
		stats.StackWalkReachedRoot, stats.UnwindPIDsRegistered, stats.UnwindBinariesAttached)

	snap := timeline.Snapshot()
	samples := gpu.ProjectExecutions(snap)
	require.NotEmpty(t, samples, "the gate is pprof samples, not counters")

	// ProjectExecutions emits one sample per execution regardless of join
	// status - a launch that aged out of the cache before its exec arrived
	// still produces a sample, just with no [gpu:launch]-attributed launch
	// underneath it. So sample count alone cannot distinguish "everything
	// joined" from "nothing joined": that has to come from JoinStats
	// directly.
	assert.Zero(t, snap.JoinStats.UnmatchedExecutionCount,
		"an unmatched execution means its launch aged out of the cache or never arrived - the stub emits both sides, so this must be zero")
	assert.Equal(t, uint64(len(snap.Executions)), snap.JoinStats.ExactExecutionJoinCount,
		"every execution must join its launch by exact correlation, not a weaker path")
	assert.Positive(t, snap.JoinStats.ExactExecutionJoinCount,
		"the exact-join and unmatched assertions above are vacuous on an empty snapshot; there must be at least one real join")
	// Every execution joined its launch exactly, because the stub emits a
	// correlation on both sides.
	assert.Zero(t, snap.JoinStats.HeuristicExecutionJoinCount,
		"the stub supplies correlations; no join should need the heuristic")
	assert.Len(t, snap.Executions, 500, "500 launches + 500 execs, exactly, none lost")

	// Every execution must carry a kernel name resolved through interning -
	// KernelNamesUnresolved == 0 above is the aggregate count, this is the
	// same fact checked per event.
	for _, view := range snap.Executions {
		assert.NotEmpty(t, view.Exec.KernelName,
			"execution for correlation %s has no kernel name", view.Exec.Correlation.Value)
	}

	// Sampled-stack accounting: exactly 58 of the 500 launches reached the
	// timeline with a non-empty CPUStack, and every one of those stacks
	// names the function the probe fired in. perfagent_stub_run keeps its
	// frame pointer (see shim/Makefile), so a failure on that one is the
	// capture or symbolization path, not the workload — see
	// resolveStackLocked in consumer.go.
	//
	// sawFPLessCaller is the by-name half of the DWARF assertion, and it is
	// a strictly stronger statement than a counter. perfagent_fpless_caller
	// is reachable ONLY through perfagent_fpless_bridge's frame, which has
	// no frame pointer and does not touch %rbp — so throughout both of them
	// %rbp still holds main's frame pointer, and a saved-RBP walk steps from
	// perfagent_stub_run's frame straight past both of them into main. An
	// FP-only walk therefore yields
	//
	//	perfagent_stub_run, perfagent_fpless_bridge, main, ...
	//
	// (the bridge's PC is the return address stored in perfagent_stub_run's
	// own frame — reaching a frame's return address is not unwinding that
	// frame) and a hybrid walk yields
	//
	//	perfagent_stub_run, perfagent_fpless_bridge,
	//	perfagent_fpless_caller, main
	//
	// so this name in a resolved stack is a positive witness that the
	// walker read .eh_frame and unwound an FP-less frame with it. That is
	// why it is asserted by name rather than by depth: a deeper stack could
	// come from anywhere, this name could not.
	var sampledLaunches, sawFPLessCaller int
	stackedKernels := map[string]int{}
	for _, view := range snap.Executions {
		require.NotNil(t, view.Launch,
			"every execution joined its launch exactly (asserted above), so Launch must never be nil here")
		if len(view.Launch.Launch.CPUStack) == 0 {
			continue
		}
		sampledLaunches++
		stackedKernels[view.Exec.KernelName]++
		var sawStubFrame, sawCaller bool
		for _, f := range view.Launch.Launch.CPUStack {
			if strings.Contains(f.Name, "perfagent_stub_run") {
				sawStubFrame = true
			}
			if strings.Contains(f.Name, "perfagent_fpless_caller") {
				sawCaller = true
			}
		}
		assert.True(t, sawStubFrame,
			"sampled launch's CPUStack does not contain a frame from the function the probe fires in: %+v",
			view.Launch.Launch.CPUStack)
		if sawCaller {
			sawFPLessCaller++
		}
	}
	assert.Equal(t, wantSampled, sampledLaunches,
		"exactly %d launches on the timeline must carry a non-empty CPUStack, matching Stats.SampledLaunches",
		wantSampled)
	// Issue #50, at the pipeline level rather than the unit level. The stub
	// alternates two kernel ids, one per launch, so its launch stream is a
	// 2-cycle -- the exact shape the fixed-stride sampler aliased against. At
	// period 8 every sampled ordinal used to be even, so kernel_1111 took
	// every stack on this producer and kernel_2222 took none, in a run that
	// otherwise passed every assertion above. Both must now be represented.
	assert.Greater(t, len(stackedKernels), 1,
		"every stack-carrying launch on this producer names the same kernel (%v): the sampler locked phase against the stub's alternating 2-kernel stream, which is issue #50",
		stackedKernels)
	t.Logf("stacks per kernel: %v", stackedKernels)
	assert.Positive(t, sawFPLessCaller,
		"not one of the %d sampled stacks names perfagent_fpless_caller, which is only reachable by unwinding an FP-less frame through CFI: the walk never crossed one, or the symbolizer could not name it",
		sampledLaunches)
	// The two halves of the same fact must agree, and in this direction:
	// a stack can only carry that frame if the walk that produced it used
	// CFI, so the by-name count can never exceed the flag count. If it did,
	// the flag is being cleared or lost somewhere between walk_step and
	// Stats — which would make StacksWalkedDWARF an unreliable gate.
	assert.LessOrEqual(t, uint64(sawFPLessCaller), stats.StacksWalkedDWARF,
		"more stacks name perfagent_fpless_caller (%d) than the walker flagged as having used DWARF (%d)",
		sawFPLessCaller, stats.StacksWalkedDWARF)
	t.Logf("sampled stacks naming perfagent_fpless_caller: %d/%d", sawFPLessCaller, sampledLaunches)

	// The two projected populations - stack-attributed and unattributed -
	// must sum to the exact total GPU duration, and no sample's value may
	// exceed its own execution's measured duration. That is what proves
	// nothing was scaled: sampling controls attribution only, never
	// duration - every execution is measured and joined regardless of
	// whether its launch was sampled.
	require.Len(t, samples, len(snap.Executions),
		"the stub emits no PC samples, so ProjectExecutions must emit exactly one sample per execution")
	var totalExecNs, totalSampleValue, attributedValue, unattributedValue uint64
	for i, view := range snap.Executions {
		require.Greater(t, view.Exec.EndNs, view.Exec.StartNs, "the stub's own execs must have a positive duration")
		dur := view.Exec.EndNs - view.Exec.StartNs
		totalExecNs += dur

		val := samples[i].Value
		totalSampleValue += val
		assert.LessOrEqualf(t, val, dur, "sample %d's value exceeds its own execution's measured duration - GPU time was scaled", i)

		var attributed bool
		for _, fr := range samples[i].Stack {
			if fr.Name == gpu.FrameLaunch {
				attributed = true
				break
			}
		}
		if attributed {
			attributedValue += val
		} else {
			unattributedValue += val
		}
	}
	assert.Equal(t, totalExecNs, totalSampleValue,
		"the projected samples must sum to exactly the measured GPU total, never a scaled estimate")
	assert.Equal(t, totalExecNs, attributedValue+unattributedValue,
		"the attributed and unattributed populations must sum to the exact total GPU duration")
	assert.Positive(t, attributedValue, "the stack-attributed population must be non-empty at sample_period=8")
	assert.Positive(t, unattributedValue, "the unattributed population must be non-empty at sample_period=8")
}

// privateStubCopy copies the built stub into a scratch directory NEXT TO THE
// SOURCE and returns the copy's path. The copy is what the test attaches to
// and what it runs, so the uprobe can only ever match this run's executions.
//
// Next to the source, not in t.TempDir(), and that is load-bearing rather than
// tidy. The rendezvous name embeds a device number; stat(2) and
// /proc/<pid>/maps report the SAME device on tmpfs and DIFFERENT devices on
// btrfs (measured here: stat=0:49 vs maps=0:34). TMPDIR is tmpfs on this
// machine, so a gate whose fixture lived there proved the two ends agree on
// the one filesystem where they cannot disagree - and both gates passed while
// the real CUDA run failed with enroll=no-listener. The shim is built in the
// source tree, so that is where the gate's copy of it belongs.
//
// The directory is removed when the test ends. It is on the same filesystem as
// the repository, which is not noexec (the build output next to it is executed
// by every other test here); an exec failure would surface as ENOEXEC/EACCES
// rather than silently degrading.
func privateStubCopy(t *testing.T, src string) string {
	t.Helper()
	info, err := os.Stat(src)
	require.NoError(t, err)
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	dir, err := os.MkdirTemp(filepath.Dir(src), "gate")
	require.NoError(t, err, "scratch dir beside the built shim")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dst := filepath.Join(dir, filepath.Base(src))
	// Preserve the executable bit from the source rather than assuming 0755.
	require.NoError(t, os.WriteFile(dst, data, info.Mode().Perm()))
	require.NotZero(t, info.Mode().Perm()&0o100, "built stub is not executable")

	// A distinct inode is the entire point; assert it rather than trusting the
	// copy. Two paths sharing an inode (a hard link, or a copy that silently
	// became a link) would put the shared image back under the uprobe.
	srcSys, dstInfo := info.Sys(), mustStat(t, dst)
	require.NotEqual(t, inodeOf(t, srcSys), inodeOf(t, dstInfo.Sys()),
		"the copy must have its own inode, or the attach is not private to this run")
	return dst
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info
}

func inodeOf(t *testing.T, sys any) uint64 {
	t.Helper()
	st, ok := sys.(*syscall.Stat_t)
	require.True(t, ok, "stat did not yield a *syscall.Stat_t")
	return st.Ino
}

// requireBuilt builds the producer at path. The make target is the file's own
// base name, not a fixed "perfagent-gpu-stub": shim/Makefile has a target per
// producer, and building the wrong one leaves the caller pointing at a path
// that may not exist at all on a clean tree - which fails later, in
// ehcompile.Compile, where the cause is not legible.
func requireBuilt(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make unavailable")
	}
	// filepath.Dir(path), not a double Dir: path is ".../shim/perfagent-gpu-stub"
	// and the Makefile with that target lives in ".../shim", not one level
	// above it.
	cmd := exec.Command("make", "-C", filepath.Dir(path), filepath.Base(path))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build %s: %s", filepath.Base(path), out)
}

// requireFPLess runs `make -C shim check-fpless`, which disassembles the
// producer and fails if the toolchain did not do what the per-file flags in
// shim/Makefile asked: no frame pointer and no write to %rbp in either
// bridge function, a frame pointer in perfagent_stub_run, and an .eh_frame
// section to read.
//
// It is here rather than left to the build because the failure it catches is
// silent in the worst way. A bridge with a frame pointer is followable by the
// frame-pointer walker this phase replaces, so every new assertion in the
// gate would pass on a run that exercised none of the new code. Fedora ships
// -fno-omit-frame-pointer in its RPM build flags, so a toolchain that
// re-enables frame pointers is a real possibility, not a theoretical one.
func requireFPLess(t *testing.T, built string) {
	t.Helper()
	for _, tool := range []string{"make", "objdump", "readelf"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: cannot prove %s has no frame pointers, and running the gate without that proof would assert nothing",
				tool, filepath.Base(built))
		}
	}
	out, err := exec.Command("make", "-C", filepath.Dir(built), "check-fpless").CombinedOutput()
	require.NoError(t, err, "check-fpless: %s", out)
	t.Logf("check-fpless: %s", bytes.TrimSpace(out))
}

// TestTheProducersBridgeFramesAreFPLessInTheCFI is the same proof as
// `make check-fpless`, one level up: check-fpless reads the instruction
// stream, this reads what the walker actually consults.
//
// walk_step does not look at instructions. It looks up (table_id, rel_pc) in
// cfi_classification and takes the DWARF path only for MODE_FP_LESS, which
// ehcompile derives from one thing: whether the CFA rule at that PC is rooted
// at SP or at FP. So "the function has no `push %rbp; mov %rsp,%rbp`" and
// "the walker will unwind it with CFI" are two different claims, and only the
// second one makes the gate mean anything. This test makes the second claim
// checkable without CAP_BPF - it compiles the producer's .eh_frame with the
// very same ehcompile the consumer uses and asserts the modes directly.
//
// Without it, the only evidence for the walker's behaviour would be a gate
// that a developer without capabilities cannot run.
// Issue #44 asserted that on the RTX 3090 validation run EVERY DWARF walk
// terminated via walk_step's `ctx->fp == 0` arm rather than its
// `ra_type == UNDEFINED` arm, and this test derived that from the producer's
// own CFI without any capability. The live gate then confirmed it exactly:
//
//	dwarf=62 fp-only=1 abandoned=62 fp-exhausted=62 reached-root=0
//
// Issue #45 is the fix, and this test is now the derivation of what the gate
// must print after it. Same method, because the method held: which arm fires
// at each frame is decided entirely by data readable here -
//
//   - the mode of the frame the walk is ON (FP_LESS takes the DWARF path,
//     FP_SAFE and FALLBACK take the frame-pointer path), and
//   - the fp_type of the frame it stepped OUT of (only UNDEFINED or REGISTER
//     zero the frame pointer now; "no rule" means SAME_VALUE),
//
// neither of which depends on memory contents; the arm is reached before any
// read. The whole chain is walked below, frame by frame, from the probe site
// to _start, and the prediction it yields is:
//
//	StackWalkReachedRoot == StacksWalkedDWARF,
//	StackWalkFPExhausted == StackWalkAbandoned == 0.
//
// Cross-checked against gdb on this exact binary, which reports the same
// seven frames (see .superpowers/sdd/issue-45-report.md).
func TestTheCFIForcesTheWalkToReachTheRoot(t *testing.T) {
	built := filepath.Join("..", "shim", "perfagent-gpu-fpless")
	requireBuilt(t, built)

	entries, classes, _, err := ehcompile.Compile(built)
	require.NoError(t, err)

	ef, err := elf.Open(built)
	require.NoError(t, err)
	defer func() { _ = ef.Close() }()
	syms, err := ef.Symbols()
	require.NoError(t, err, "the producer must not be stripped")

	// The midpoint of the body, not the entry PC: at entry the CFA is still
	// SP-rooted even in a function that does establish a frame pointer.
	pcOf := func(name string) uint64 {
		for i := range syms {
			if syms[i].Name == name && syms[i].Size > 0 {
				return syms[i].Value + syms[i].Size/2
			}
		}
		t.Fatalf("symbol %s not found in %s", name, built)
		return 0
	}
	modeOf := func(cls []ehcompile.Classification, pc uint64) ehcompile.Mode {
		for _, c := range cls {
			if pc >= c.PCStart && pc < c.PCStart+uint64(c.PCEndDelta) {
				return c.Mode
			}
		}
		// walk_step's own default for an uncovered PC.
		return ehcompile.ModeFPSafe
	}
	cfiOf := func(ents []ehcompile.CFIEntry, pc uint64) *ehcompile.CFIEntry {
		for i := range ents {
			if pc >= ents[i].PCStart && pc < ents[i].PCStart+uint64(ents[i].PCEndDelta) {
				return &ents[i]
			}
		}
		return nil
	}

	// --- the two FP-less bridge frames, which the walk crosses with CFI.
	//
	// This is issue #45 seen from the outside. Neither function touches
	// %rbp, so its CFI carries no rule for it. ehcompile used to call that
	// UNDEFINED, walk_step turned UNDEFINED into new_fp = 0, and every frame
	// above arrived with no frame pointer. The x86-64 psABI says an
	// unmentioned callee-saved register is UNCHANGED, which is SAME_VALUE -
	// and SAME_VALUE is the one fp_type that leaves ctx->fp alone.
	for _, fn := range []string{"perfagent_fpless_bridge", "perfagent_fpless_caller"} {
		pc := pcOf(fn)
		require.Equal(t, ehcompile.ModeFPLess, modeOf(classes, pc),
			"%s is not FP-less, so walk_step would never take the DWARF path here", fn)
		row := cfiOf(entries, pc)
		require.NotNil(t, row, "no CFI row covers %s", fn)
		assert.Equal(t, ehcompile.FPTypeSameValue, row.FPType,
			"%s: the CFI carries no rule for %%rbp, which the psABI reads as unchanged; "+
				"UNDEFINED here is issue #45 and the walk loses the frame pointer crossing it", fn)
		assert.Equal(t, ehcompile.RATypeOffsetCFA, row.RAType,
			"%s: marked outermost by its own CFI, so the walk would end here rather than reach main", fn)
	}

	// --- main, reached with a LIVE frame pointer, so the FP path works.
	mainPC := pcOf("main")
	require.Equal(t, ehcompile.ModeFPSafe, modeOf(classes, mainPC),
		"main is not FP_SAFE, so the walk would not take the frame-pointer path out of it")
	mainCFI := cfiOf(entries, mainPC)
	require.NotNil(t, mainCFI)
	assert.Equal(t, ehcompile.CFATypeFP, mainCFI.CFAType,
		"main's CFA is not FP-rooted, so its saved-FP slot is not where the walk expects")
	assert.NotEqual(t, ehcompile.RATypeUndefined, mainCFI.RAType,
		"main marks itself outermost, which would end the walk before libc")

	// --- _start, the genuine root, and the reason WALKER_FLAG_RA_UNDEFINED
	// exists. It is FP-less, so the walk classifies it and reads its CFI;
	// its CFI says there is no return address.
	//
	// Reaching it needs walk_step's step past the end of the frame-pointer
	// chain: main's callers (__libc_start_call_main, __libc_start_main_impl)
	// are FP_SAFE and are walked by frame pointer, and the last of them has
	// a saved-FP slot of zero because _start does `xorl %ebp, %ebp`. That
	// zero used to end the walk while discarding the return address stored
	// beside it - which IS _start's PC.
	// TestWalkStepStepsPastTheFramePointerRoot pins the walker half.
	startPC := pcOf("_start")
	require.Equal(t, ehcompile.ModeFPLess, modeOf(classes, startPC),
		"_start is not FP-less, so the walk would take the FP path and never read its ra_type")
	startCFI := cfiOf(entries, startPC)
	require.NotNil(t, startCFI, "no CFI row covers _start")
	assert.Equal(t, ehcompile.RATypeUndefined, startCFI.RAType,
		"_start does not mark itself outermost, so WALKER_FLAG_RA_UNDEFINED would never fire on this binary")

	// The return address the walk actually arrives with is the instruction
	// AFTER `call __libc_start_main`, not the symbol's midpoint, so check
	// that PC specifically: a CFI row that stopped short of it would leave
	// the walk uncovered exactly where it matters.
	var startEnd uint64
	for i := range syms {
		if syms[i].Name == "_start" {
			startEnd = syms[i].Value + syms[i].Size
		}
	}
	require.NotZero(t, startEnd)
	lastRow := cfiOf(entries, startEnd-1)
	require.NotNil(t, lastRow, "the tail of _start, where the return address lands, has no CFI row")
	assert.Equal(t, ehcompile.RATypeUndefined, lastRow.RAType,
		"the row covering the return address into _start does not mark it outermost")

	t.Logf("perfagent_fpless_{bridge,caller}: mode=FP_LESS fp_type=SAME_VALUE -> ctx->fp survives")
	t.Logf("main: mode=FP_SAFE, reached WITH a frame pointer -> FP path continues into libc")
	t.Logf("_start: mode=FP_LESS ra_type=UNDEFINED -> WALKER_FLAG_RA_UNDEFINED, reached-root")
	t.Logf("prediction: reached-root == dwarf, fp-exhausted == abandoned == 0")
}

// TestFramePushRefusalRaisesAFlag pins that both frame_push_native (at
// MAX_FRAMES) and frame_push_python (fewer than two slots left) raise
// WALKER_FLAG_FRAME_PUSH_REFUSED on their refusal path, before returning 1
// — issue #83's constraint that a dropped frame is never silent. Checked by
// source inspection, like TestWalkStepStepsPastTheFramePointerRoot below,
// because the refusal itself is only reachable inside the BPF verifier.
func TestFramePushRefusalRaisesAFlag(t *testing.T) {
	src, err := os.ReadFile("../bpf/unwind_common.h")
	require.NoError(t, err)
	body := string(src)

	require.Contains(t, body, "#define WALKER_FLAG_FRAME_PUSH_REFUSED 0x80",
		"the refusal flag is gone, renamed, or its value changed")

	checkRefusalRaisesFlag := func(t *testing.T, fn string) {
		t.Helper()
		start := strings.Index(body, "static __always_inline int "+fn+"(")
		require.Positive(t, start, "%s not found in the shared header", fn)
		rest := body[start:]
		end := strings.Index(rest, "\n}\n")
		require.Positive(t, end, "%s body not closed", fn)
		fnBody := rest[:end]

		refusal := strings.Index(fnBody, "return 1;")
		require.Positive(t, refusal, "%s has no refusal path", fn)
		flag := strings.Index(fnBody, "WALKER_FLAG_FRAME_PUSH_REFUSED")
		require.Positive(t, flag, "%s's refusal drops the frame without raising a flag", fn)
		require.Less(t, flag, refusal,
			"%s must raise WALKER_FLAG_FRAME_PUSH_REFUSED before returning 1, not after", fn)
	}

	t.Run("frame_push_native", func(t *testing.T) { checkRefusalRaisesFlag(t, "frame_push_native") })
	t.Run("frame_push_python", func(t *testing.T) { checkRefusalRaisesFlag(t, "frame_push_python") })
}

// A bounds check that adds to the value it is checking is not a bounds
// check. frame_push_python used to guard its two-slot write with
// `if (i + 2 > MAX_FRAMES)`, where i is __u32: the addition wraps, so at
// i == 0xFFFFFFFE the expression is 0, the guard passes, and both stores run
// with a wild index. The verifier refused to derive i <= 125 from
// i + 2 <= 127 and rejected perf_dwarf and offcpu_dwarf outright
// ("R1 unbounded memory access" at the tags[] store).
//
// The property is structural, so it is checked structurally: neither pusher's
// guard may contain arithmetic on the checked local. The comparison must be
// against a constant the compiler folds, which is the only form the verifier
// can carry from the branch to the store.
//
// Source inspection, like its neighbours: the refusal path is only reachable
// inside the verifier, and no test on this machine can load a program.
func TestTheFramePushBoundsDoNoArithmeticOnTheCheckedValue(t *testing.T) {
	src, err := os.ReadFile("../bpf/unwind_common.h")
	require.NoError(t, err)
	body := string(src)

	guardOf := func(t *testing.T, fn string) string {
		t.Helper()
		start := strings.Index(body, "static __always_inline int "+fn+"(")
		require.Positive(t, start, "%s not found in the shared header", fn)
		rest := body[start:]
		end := strings.Index(rest, "\n}\n")
		require.Positive(t, end, "%s body not closed", fn)
		fnBody := rest[:end]

		open := strings.Index(fnBody, "if (")
		require.Positive(t, open, "%s has no bounds guard at all", fn)
		close := strings.Index(fnBody[open:], ") {")
		require.Positive(t, close, "%s's guard is not closed", fn)
		return fnBody[open+len("if (") : open+close]
	}

	for _, fn := range []string{"frame_push_native", "frame_push_python"} {
		t.Run(fn, func(t *testing.T) {
			guard := guardOf(t, fn)
			require.Contains(t, guard, "MAX_FRAMES",
				"%s's guard must be expressed against MAX_FRAMES", fn)
			require.NotContains(t, strings.ReplaceAll(guard, " ", ""), "i+",
				"%s adds to the checked value; __u32 arithmetic wraps and the verifier cannot carry the bound to the store", fn)
		})
	}

	// The exact boundary, pinned. frame_push_python writes slots i and i+1,
	// so both must be < MAX_FRAMES and the accept set is i <= MAX_FRAMES-2.
	// One off in either direction is silent: too tight drops the last valid
	// pair on a deep stack, too loose writes one slot past the array.
	require.Contains(t, body, "if (i > MAX_FRAMES - 2) {",
		"frame_push_python's two-slot bound moved; the accept set must stay i <= MAX_FRAMES-2")
	require.Contains(t, body, "if (i >= MAX_FRAMES) {",
		"frame_push_native's one-slot bound moved; the accept set must stay i <= MAX_FRAMES-1")

	// And the subtraction itself must be safe: at MAX_FRAMES < 2 the constant
	// underflows to a huge unsigned and the guard accepts everything.
	require.Contains(t, body, "_Static_assert(MAX_FRAMES >= 2,",
		"nothing stops MAX_FRAMES - 2 underflowing if MAX_FRAMES is ever made tiny")
}

// The walker half of the derivation above, read out of the shared header so
// it needs no capability either.
//
// walk_step used to `return 1` the moment a frame's saved-FP slot read zero,
// throwing away the return address it had ALREADY read out of the adjacent
// slot - so the outermost frame of every stack it produced was dropped, and
// the CFI of that frame (the one place a genuine root is declared) was never
// consulted. On a main-thread stack the dropped frame is `_start`.
//
// Three properties make the replacement safe, and all three are asserted:
// the return address is recorded, the step is taken only when it is non-zero,
// and it is bounded to ONE step by fp_chain_ended.
func TestWalkStepStepsPastTheFramePointerRoot(t *testing.T) {
	src, err := os.ReadFile("../bpf/unwind_common.h")
	require.NoError(t, err)
	body := string(src)
	step := body[strings.Index(body, "static long walk_step("):]
	require.NotEmpty(t, step, "walk_step not found in the shared header")

	guard := strings.Index(step, "if (saved_fp <= ctx->fp) {")
	require.Positive(t, guard,
		"the saved-FP guard is gone or reshaped; the zero and non-monotonic cases must share it")
	arm := step[guard:]

	require.NotContains(t, arm[:strings.Index(arm, "WALKER_FLAG_FP_TERMINATED")], "return 1;",
		"the guard still bails out before classifying the two cases")

	// The zero case continues rather than stopping, carrying the return
	// address it already read.
	zero := arm[strings.Index(arm, "if (saved_fp == 0) {"):]
	require.Positive(t, strings.Index(zero, "ctx->pc = ret_addr;"),
		"the return address read beside the zero saved FP is still discarded (issue #45)")
	require.Positive(t, strings.Index(zero, "return 0;"),
		"the walk still stops at the end of the frame-pointer chain, so it can never reach a frame whose CFI declares the root")

	// And the step is bounded: both arms that can be reached with fp == 0
	// after it consult fp_chain_ended and stop.
	require.Equal(t, 2, strings.Count(step, "fp_chain_ended(ctx)"),
		"the step past the frame-pointer root must be bounded by fp_chain_ended in BOTH the FP path and the DWARF path's RA read")

	// The DWARF-path guard is the one that can stop a walk whose ending the
	// CFI contradicts. WALKER_FLAG_FP_TERMINATED is already set there, so a
	// bare `return 1` would file that walk as a clean success with no
	// counter moving - the #44 defect, recreated by #45's own fix. It must
	// raise a bit of its own.
	dwarfGuard := step[strings.Index(step, "if (e.ra_type == RA_TYPE_OFFSET_CFA) {"):]
	dwarfGuard = dwarfGuard[:strings.Index(dwarfGuard, "bpf_probe_read_user")]
	require.Contains(t, dwarfGuard, "WALKER_FLAG_ROOT_DISAGREEMENT",
		"the FP-chain-says-root / CFI-says-caller disagreement stops the walk silently")

	// The non-monotonic case records the frame too, under its own flag.
	require.Positive(t, strings.Index(arm, "WALKER_FLAG_FP_NONMONOTONIC"),
		"a frame pointer that does not increase is still an unflagged bare `return 1`")
	nonmono := arm[strings.Index(arm, "WALKER_FLAG_FP_NONMONOTONIC"):]
	// Recorded via frame_push_native (issue #83) rather than a raw
	// `ctx->rec->pcs[ctx->n_pcs++] = ret_addr;` write, so this slot's
	// tags[] byte is set too instead of carrying forward whatever a
	// previous sample left in the reused per-CPU scratch buffer. The
	// property this test names — the return address is recorded, not
	// discarded — still holds; only the how changed.
	require.Positive(t, strings.Index(nonmono, "frame_push_native(ctx, ret_addr)"),
		"the non-monotonic arm still discards the return address it already read")
}

func TestTheProducersBridgeFramesAreFPLessInTheCFI(t *testing.T) {
	built := filepath.Join("..", "shim", "perfagent-gpu-fpless")
	requireBuilt(t, built)

	_, classes, ehBytes, err := ehcompile.Compile(built)
	require.NoError(t, err, "the producer's .eh_frame must compile: it is the only way the walker can cross an FP-less frame")
	require.Positive(t, ehBytes, "no .eh_frame bytes: the DWARF walker would have nothing to read")

	ef, err := elf.Open(built)
	require.NoError(t, err)
	defer func() { _ = ef.Close() }()
	syms, err := ef.Symbols()
	require.NoError(t, err, "the producer must not be stripped; the gate asserts on frame names")

	// The midpoint of the function body, not its entry. At entry the CFA is
	// still SP-rooted even in a function that does establish a frame pointer
	// - the prologue has not run yet - so classifying on the entry PC would
	// call every function FP-less and the test would pass for the wrong
	// reason.
	classify := func(t *testing.T, name string) ehcompile.Mode {
		t.Helper()
		var sym *elf.Symbol
		for i := range syms {
			if syms[i].Name == name {
				sym = &syms[i]
				break
			}
		}
		require.NotNilf(t, sym, "symbol %s not found in %s", name, built)
		require.Positivef(t, sym.Size, "symbol %s has no size; cannot pick a PC inside it", name)
		pc := sym.Value + sym.Size/2
		for _, c := range classes {
			if pc >= c.PCStart && pc < c.PCStart+uint64(c.PCEndDelta) {
				return c.Mode
			}
		}
		t.Fatalf("no classification row covers %#x, the midpoint of %s: the walker would treat it as FP_SAFE and never take the DWARF path", pc, name)
		return 0
	}

	// The two frames between the probe and main. MODE_FP_LESS is exactly
	// what makes walk_step read cfi_rules and set WALKER_FLAG_DWARF_USED.
	for _, name := range []string{"perfagent_fpless_bridge", "perfagent_fpless_caller"} {
		assert.Equalf(t, ehcompile.ModeFPLess, classify(t, name),
			"%s classifies as something other than FP_LESS, so walk_step would take the frame-pointer path through it and the gate's StacksWalkedDWARF assertion could not be satisfied by this producer", name)
	}

	// The other half of the hybrid, and not a formality: the walk's FIRST
	// step is an FP step out of perfagent_stub_run's frame, and main is the
	// frame it has to land on afterwards. If either were FP-less the walk
	// would still work but would no longer exercise the FP -> DWARF -> FP
	// handoff this producer exists to reproduce.
	for _, name := range []string{"perfagent_stub_run", "main"} {
		assert.Equalf(t, ehcompile.ModeFPSafe, classify(t, name),
			"%s is not FP_SAFE, so the walk would not cross an FP/DWARF boundary and the producer would not reproduce the CUDA stack shape", name)
	}
}

// The gate hole that let issue #49's first fix ship broken.
//
// That fix passed TestStubDrivesThePipelineToPprofWithoutAGPU with
// no-tables=0, and then did nothing whatsoever on the RTX 3090:
// UnwindEnrollRequests read 0 across three runs and the loss was unchanged at
// ~175 of 500 sampled stacks. The gate could not see it because it drives an
// EXEC'd producer, and the two paths differ in exactly the respect that
// mattered - the kernel arms a uprobe's reference-count semaphore while it
// builds the mm for an exec, so it is already non-zero when main runs, whereas
// a CUPTI adapter is DLOPEN'd and libcuda calls InitializeInjection
// essentially the instant the mapping appears. A rendezvous gated on that
// semaphore therefore ran on one path and not the other, and the gate only
// covered the path where it worked.
//
// So this drives a producer .so loaded with dlopen(3) by a separate host
// (shim/stub/dlopen_host.cc), which puts producer initialisation on the same
// kernel path the CUDA adapter's is on. It is deliberately a second test
// rather than a change to the first: both shapes have to keep working, and a
// single test cannot be both.
//
// It also records the producer's own view of the semaphore at the moment it
// enrolled (sem_at_enroll). That number is the measurement this whole episode
// turned on, and logging it on every run means the next person does not have
// to infer it.
func TestADlopenedProducerEnrollsBeforeItsFirstLaunch(t *testing.T) {
	if !hasGateCaps() {
		t.Skip("needs CAP_BPF, CAP_PERFMON and CAP_CHECKPOINT_RESTORE; " +
			"sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep <test binary>")
	}
	host := filepath.Join("..", "shim", "perfagent-gpu-dlopen-host")
	producer := filepath.Join("..", "shim", "libperfagent-gpu-fpless.so")
	requireBuilt(t, host)
	requireBuilt(t, producer)
	// The same build-time proof the exec gate takes: if the toolchain kept
	// frame pointers in the bridge, the walk below never reaches an FP-less
	// frame, never takes the DWARF path, and the assertion on it would pass
	// while proving nothing.
	requireFPLess(t, producer)

	// Same inode-privacy reasoning as the exec gate: the attach is
	// system-wide, so a shared image would also collect from any other
	// process running it.
	so := privateStubCopy(t, producer)

	sym, err := symbolize.NewLocalSymbolizer()
	require.NoError(t, err)
	defer func() { _ = sym.Close() }()

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath:   so,
		Backend:    gpu.GPUBackendID("stub"),
		Sink:       timeline,
		Symbolizer: sym,
	})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// The host is started after Attach, so the .so is dlopened into a process
	// that did not exist when the uprobe link was created - the CUDA ordering.
	cmd := exec.Command(host, so, "500", "1000", "8", "10000")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	release, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() { _ = release.Close() }()

	// 58, not ceil(500/8)==63: since #50 the sampler jitters its stride, so
	// 500 launches at period 8 yield 58 sampled, not 63. Cross-checked against
	// internal/gpuabi.SampleSchedule(500, 8, DefaultSampleSeed), which replays
	// the shim's schedule exactly. Left as an equality rather than a floor -
	// the count is deterministic given the seed, and a floor would let a
	// silently under-sampling shim pass.
	const wantSampled = 58
	deadline := time.Now().Add(10 * time.Second)
	for c.Stats().SampledLaunches < wantSampled {
		if time.Now().After(deadline) {
			_ = release.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			cancel()
			<-done
			t.Fatalf("timed out waiting for %d sampled launches; consumer saw %d. stats: %+v stderr: %s",
				wantSampled, c.Stats().SampledLaunches, c.Stats(), stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.NoError(t, release.Close())
	require.NoError(t, cmd.Wait(), "stdout: %s stderr: %s", stdout.String(), stderr.String())
	producerErr := stderr.String()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	stats := c.Stats()

	// The measurement, logged whether the test passes or fails. sem_at_enroll
	// is what the producer saw when it ran the rendezvous; if it reads 0 here
	// while the run still confirms, that is direct evidence for why gating on
	// the semaphore was wrong, on a machine with no GPU in it.
	sem := regexp.MustCompile(`sem_at_enroll=(\d+) sem_at_exit=(\d+)`).FindStringSubmatch(producerErr)
	if len(sem) == 3 {
		t.Logf("producer semaphore: at_enroll=%s at_exit=%s (0 then non-zero means the "+
			"kernel had not armed it when the rendezvous ran - the CUDA failure mode, reproduced without a GPU)",
			sem[1], sem[2])
	} else {
		t.Logf("producer did not report semaphore counts; stderr: %s", producerErr)
	}
	t.Logf("walk shape: dwarf=%d fp-only=%d no-tables=%d profiler-only=%d cfi-miss=%d abandoned=%d registered=%d binaries=%d",
		stats.StacksWalkedDWARF, stats.StacksWalkedFPOnly, stats.StacksWalkedNoTables,
		stats.StacksProfilerOnly, stats.StacksWalkedCFIMiss, stats.StackWalkAbandoned,
		stats.UnwindPIDsRegistered, stats.UnwindBinariesAttached)

	assert.Zero(t, stats.SequenceGaps, "no batch may be lost silently")
	assert.Equal(t, uint64(wantSampled), stats.SampledLaunches,
		"ceil(500/8): the sampler is deterministic, and a dlopened producer samples exactly as an exec'd one does")

	// ---- the assertions this test exists for -----------------------------
	//
	// The first of these is the one that was 0 on the RTX 3090 while the exec
	// gate was green. It is asserted BEFORE the no-tables assertion on
	// purpose: "the producer never even tried" and "it tried and the tables
	// still were not there" are different failures, and a bare no-tables
	// check reports them identically.
	assert.True(t, stats.UnwindEnrollListening,
		"the rendezvous address could not be bound, so this test cannot say anything about dlopen-time arming: %q",
		stats.UnwindEnrollLastError)
	assert.Equal(t, uint64(1), stats.UnwindEnrollRequests,
		"the dlopened producer never reached the rendezvous. This is the exact CUDA failure: "+
			"UnwindEnrollRequests was 0 on the RTX 3090 while the exec-driven gate passed. "+
			"refused=%d throttled=%d err=%q producer stderr: %s",
		stats.UnwindEnrollRefused, stats.UnwindEnrollThrottled, stats.UnwindEnrollLastError, producerErr)
	assert.Equal(t, uint64(1), stats.UnwindEnrollConfirmed,
		"the producer reached the rendezvous but was not told its tables were installed: failed=%d err=%q",
		stats.UnwindEnrollFailed, stats.UnwindEnrollLastError)
	assert.Contains(t, producerErr, "enroll=confirmed",
		"the producer's own account disagrees with the consumer's; a producer that never reached the "+
			"socket is invisible to every consumer-side counter, which is why both ends are checked")
	assert.Zero(t, stats.StacksWalkedNoTables,
		"a capture was walked with no CFI tables even though the producer waits for them before it "+
			"launches anything. enroll: requests=%d confirmed=%d refused=%d failed=%d err=%q",
		stats.UnwindEnrollRequests, stats.UnwindEnrollConfirmed, stats.UnwindEnrollRefused,
		stats.UnwindEnrollFailed, stats.UnwindEnrollLastError)
	assert.Zero(t, stats.StacksNoTablesAfterEnroll,
		"the producer was released on a promise its tables were installed and a later walk found none")
	assert.Zero(t, stats.UnwindEnrollRefused,
		"the process the uprobes fired in was refused by the identity check: %q", stats.UnwindEnrollLastError)

	// The tables have to be USED, not merely present.
	//
	// The first version of this test loaded libperfagent-gpu-stub.so, which is
	// built with frame pointers throughout, and reported dwarf=0 fp-only=63
	// with no-tables=0 - the rendezvous fired, the tables were installed, and
	// not one walk ever needed them. That is legitimate for that producer
	// (walk_step only sets WALKER_FLAG_DWARF_USED for a frame it classified
	// FP_LESS) and it is also useless: it proves the rendezvous fired and
	// nothing about the DWARF unwinding the rendezvous exists to feed.
	//
	// This producer puts two -fomit-frame-pointer frames between the probe and
	// the host's main, which is where libcupti and libcudart sit in a real
	// CUDA stack, so a walk that gets out of perfagent_stub_run's frame at all
	// has to cross one. requireFPLess above fails the test if the toolchain
	// did not actually omit them, so this cannot pass vacuously.
	assert.Positive(t, stats.StacksWalkedDWARF,
		"no capture used the DWARF path: the tables were installed before the first launch and then "+
			"never needed, so this run proves nothing about unwinding a CUDA-shaped stack. fp-only=%d no-tables=%d",
		stats.StacksWalkedFPOnly, stats.StacksWalkedNoTables)
	assert.Equal(t, uint64(wantSampled), stats.StacksWalkedDWARF+stats.StacksWalkedFPOnly,
		"every non-empty capture is counted exactly once as either a DWARF walk or an FP-only walk")

	// StacksProfilerOnly is LOGGED above and deliberately not asserted.
	//
	// This producer is an ET_DYN with no DT_SONAME and no PT_INTERP, so
	// shimScope classifies it as an injected library and the #39 guard IS
	// armed - the same shape as the CUDA adapter, and unlike the exec gate
	// where the guard disables itself. That makes the counter meaningful
	// here, and it is why it is worth printing. But its value depends on the
	// walk crossing out of the .so into the host executable, and while the
	// FP-less bridge makes that the DWARF walker's job rather than the frame
	// pointer chain's, this test has not derived the result and could not run
	// it. Asserting an underived number is how a gate goes green while
	// proving nothing, which is the failure this whole test exists to
	// correct; the DWARF assertion above is the derived one.
}

// ---------------------------------------------------------------------------
// Phase 6: the PC-sampling gate
// ---------------------------------------------------------------------------

// gateCRCAbsent is a CRC no cubin is ever stored under, so a PC sample
// carrying it must read gpu_src_status="no-module". The two REAL fixture CRCs
// are not constants here: they are read out of the producer's own report and
// cross-checked against the bytes, so the number the store keys on is the
// number the producer put on the wire rather than one this test invented. See
// gateModuleCRCs.
const gateCRCAbsent uint64 = 0x6A7E0003

// TestStubDrivesPCSamplingToPprofWithoutAGPU is the end-to-end half of the
// Phase 6 phase gate (Task 13, assertions 1-5, 8, 9 and 12), driven by the
// same GPU-free producer TestStubDrivesThePipelineToPprofWithoutAGPU uses.
//
// It is a SECOND test rather than an extension of that one, deliberately. That
// test asserts an exact sampled count, reached-root == dwarf, zero abandoned,
// NoTables == 0 and a dozen other equalities that describe a run with PC
// sampling OFF - `require.Len(samples, len(snap.Executions))` among them, which
// is true precisely because the stub emits no PC samples there. Turning PC
// sampling on inside it would have meant weakening those. So the baseline stays
// exactly as it was and this runs the same producer in the PC-sampling
// configuration beside it.
//
// # What comes off the wire, and what does not
//
// Off the wire, from a real producer through a real uprobe_multi link:
//
//   - 500 launches and 500 executions, 58 of the launches carrying a CPU stack
//     walked through two -fomit-frame-pointer frames using this consumer's own
//     compiled CFI, symbolized against the live process;
//   - two real checked-in cubins, over the cubin channel, as sealed memfds
//     passed by SCM_RIGHTS;
//   - 64 PC-sample records, a stall-reason map, a config record and Tier A
//     sampling windows.
//
// NOT off the wire, and this is a finding rather than a shortcut - see
// .superpowers/sdd/task-13-gate-report.md:
//
//	The stub's PC records cannot be attributed to anything. Their cubin_crc is
//	a pair of synthetic constants (shim/stub/stub.cc kStubCubinCRC =
//	{0xC0FFEE01, 0xC0FFEE02}) unrelated to the cubins the same run delivers
//	over the cubin channel, and their correlation is 0 in every tier. Tier B
//	attribution runs crc -> module -> function name -> the execution's
//	KernelName, and the stub's kernel names are "kernel_1111"/"kernel_2222"
//	while the fixtures' only functions are the CUDA kernels they were compiled
//	from. So neither join path can fire: every one of those 64 records is
//	correctly counted as pending, and the gate asserts that exactly.
//
// Assertions 2, 3, 4 and 9 need a PC sample that DOES reach an execution, so
// the gate supplies those itself at Timeline.EmitPCSample - the same entry
// point the consumer calls - on correlations that a real, wire-delivered,
// stack-carrying launch is known to occupy. Everything downstream of that entry
// point is product code: the join, the module store's four-valued resolution,
// the projection's label set, the cardinality budget. What the injection skips
// is the consumer's decode arm for KIND_PC, and that is asserted separately and
// exactly by the 64 records above.
//
// The MODULES those injected samples resolve against are no longer supplied by
// the gate. Issue #93 - the cubin transport never feeding gpu.ModuleStore - is
// closed, so the store below is handed to gpuprobe.Config.Modules and FILLED BY
// THE PRODUCT from the bytes the producer sent, and this test asserts what it
// holds rather than putting it there. Only the PC records are still injected,
// and only for the reason above.
//
// # Assertion 11 is not here
//
// getcap on the gate binary is asserted in gate_compose_test.go, where it runs
// without capabilities. Asserting "this pipeline needs no cap_sys_admin" only
// on machines that hold enough privilege to run this test would be asserting it
// in the one place it cannot be checked usefully.
func TestStubDrivesPCSamplingToPprofWithoutAGPU(t *testing.T) {
	if !hasGateCaps() {
		t.Skip("needs CAP_BPF, CAP_PERFMON and CAP_CHECKPOINT_RESTORE " +
			"(the last so blazesym can follow /proc/<pid>/map_files/); " +
			"sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep <test binary>. " +
			"gpu/gate_test.go and gpuprobe/gate_compose_test.go assert the same twelve " +
			"points without privilege; this adds the end-to-end run")
	}
	built := filepath.Join("..", "shim", "perfagent-gpu-fpless")
	requireBuilt(t, built)
	requireFPLess(t, built)
	stub := privateStubCopy(t, built)

	// The module store, built here exactly as cmd/gpu-cuda-profile builds it:
	// one instance, handed to gpuprobe.Config.Modules (where the cubin
	// listener writes it), to gpu.TimelineConfig.Modules (where the join reads
	// device function names out of it) and to gpu.ProjectionConfig.Modules
	// (where the source labels are resolved). The defaults are taken rather
	// than restated - 512 modules and 64 MiB - because the sizing belongs in
	// one place.
	//
	// Nothing in this test PUTS anything into it. It is filled by the product,
	// over the cubin channel, from the bytes the producer sends, and what it
	// ends up holding is asserted below against the CRCs the producer itself
	// declared. That is the hop issue #93 was about, and it is why this gate
	// no longer says "as shipped every PC sample reads no-module".
	lineInfo := readFixture(t, "single_lineinfo.cubin")
	noLineInfo := readFixture(t, "single_nolineinfo.cubin")
	store := gpu.NewModuleStore(gpu.ModuleStoreConfig{})
	lineIdx := fixtureSymIndex(t, lineInfo, "addOne")
	noLineIdx := fixtureSymIndex(t, noLineInfo, "addOne")

	sym, err := symbolize.NewLocalSymbolizer()
	require.NoError(t, err)
	defer func() { _ = sym.Close() }()

	// PCSamplingSerialized, not the zero value: the tier is what decides
	// whether an execution with no covering window reads "unknown" or "false",
	// and a Timeline told nothing would answer "false" for every execution in
	// this run - correctly, since nothing would then have been serialized, and
	// uselessly, since the producer IS emitting windows.
	timeline := gpu.NewTimeline(gpu.TimelineConfig{
		PCSampling: gpu.PCSamplingSerialized,
		Modules:    store,
	})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath:   stub,
		Backend:    gpu.GPUBackendID("stub"),
		Sink:       timeline,
		Symbolizer: sym,
		// The same store the Timeline above and the projection below read.
		// Without this the cubins still arrive, are still sealed, verified
		// and counted - and land where nothing reads them, which is what
		// issue #93 was.
		Modules: store,
	})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.True(t, c.Stats().CubinsListening,
		"the cubin channel did not bind, so no module can arrive and every source label would read no-module for a reason that has nothing to do with this phase: %q",
		c.Stats().CubinsLastError)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	const (
		wantSampled  = 58 // gpuabi.SampleSchedule(500, 8, DefaultSampleSeed)
		wantPC       = 64
		wantWindows  = 4
		wantCubins   = 2
		wantDropRecs = 4 // one gpu_dropped_v1 per drop class, from the stub
	)
	cmd := exec.Command(stub, "500", "1000", "8", "10000")
	cmd.Env = append(os.Environ(),
		// The tier is the OUTER gate in the stub exactly as in the CUPTI
		// adapter (shim/core/pctier.h): with it off, none of the four
		// PC-sampling probes fires whatever the knobs below say.
		"PERFAGENT_GPU_PC_SAMPLING=serialized",
		"PERFAGENT_STUB_PC_SAMPLES="+strconv.Itoa(wantPC),
		"PERFAGENT_STUB_SAMPLING_WINDOWS="+strconv.Itoa(wantWindows),
		"PERFAGENT_STUB_CUBINS="+
			mustAbs(t, fixturePath("single_lineinfo.cubin"))+":"+
			mustAbs(t, fixturePath("single_nolineinfo.cubin")),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	release, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() { _ = release.Close() }()

	// Hold the producer open until every sampled stack has been symbolized
	// against its still-live /proc/<pid>/maps, exactly as the baseline gate
	// does. The cubins are waited for too: the stub offers them from its drain
	// thread before its first launch, so they are the earliest thing to
	// arrive, and a run that started asserting before they landed would report
	// "no-module" for a scheduling reason.
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := c.Stats()
		if st.SampledLaunches >= wantSampled && st.CubinsReceived >= wantCubins {
			break
		}
		if time.Now().After(deadline) {
			_ = release.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			cancel()
			<-done
			t.Fatalf("timed out: sampled=%d/%d cubins=%d/%d. stats: %+v stderr: %s",
				st.SampledLaunches, wantSampled, st.CubinsReceived, wantCubins, st, stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	stubPID := cmd.Process.Pid
	require.NoError(t, release.Close())
	require.NoError(t, cmd.Wait(), "stdout: %s stderr: %s", stdout.String(), stderr.String())
	stubErr := stderr.String()

	// The CRC the producer declared for each fixture, from its own report, and
	// the store keyed on those rather than on numbers this test chose.
	//
	// This is the offline half of hardware assertion 13 ("cuptiGetCubinCrc()
	// over the received copy equals the PC records' cubinCrc"). The stub's
	// crc is FNV-1a rather than CUPTI's unpublished polynomial, but the
	// property under test is the same one and it is the one the join needs:
	// ONE number identifies one set of bytes, and the same number reaches both
	// ends. Recomputing it here over the checked-in fixture proves the number
	// is a function of the bytes and not of the load order, the module id or
	// the path.
	crcs := gateModuleCRCs(t, stubErr)
	require.Len(t, crcs, 2, "the producer reported %d captured modules, not 2: %s", len(crcs), stubErr)
	gateCRCLineInfo, gateCRCNoLineInfo := crcs[0], crcs[1]
	assert.Equal(t, stubCubinCRC(lineInfo), gateCRCLineInfo,
		"the CRC the producer declared for single_lineinfo.cubin is not a content hash of those bytes, so it cannot identify them on the wire")
	assert.Equal(t, stubCubinCRC(noLineInfo), gateCRCNoLineInfo,
		"same, for single_nolineinfo.cubin")
	assert.NotEqual(t, gateCRCLineInfo, gateCRCNoLineInfo,
		"two different cubins collided on one CRC, which would make them indistinguishable to the store")

	// And the hop: the store the consumer was handed holds both modules, under
	// those CRCs, because the cubin listener wrote them there. This test put
	// nothing in it.
	//
	// This is issue #93's assertion. Before it was closed the listener's sink
	// was a bounded map with no line table, gpuprobe.Config had no field for a
	// store, and this block could not be written at all - the gate filled the
	// store itself and recorded the gap as a finding.
	ms := store.Stats()
	require.Equal(t, 2, store.Len(),
		"the cubin transport did not feed the module store: %d modules held after %d cubins were received. "+
			"Every PC sample in a real profile would read gpu_src_status=\"no-module\" (issue #93)",
		store.Len(), c.Stats().CubinsReceived)
	assert.Equal(t, uint64(1), ms.ModulesWithLineInfo,
		"the -lineinfo fixture did not arrive as a module with a usable line table: %+v", ms)
	assert.Equal(t, uint64(1), ms.ModulesWithoutLineInfo,
		"the no-lineinfo fixture did not arrive as a module without one: %+v", ms)
	assert.Zero(t, ms.ModulesUnparseable,
		"a cubin crossed the transport and did not survive as an ELF: %+v", ms)
	assert.Zero(t, ms.ModulesEvicted, "two modules cannot pressure a 512-module, 64 MiB store")
	assert.Equal(t, gpu.SrcResolved, store.Resolve(gateCRCLineInfo, lineIdx, 0x10).Status(),
		"the module the PRODUCER declared under %#x does not resolve a source line in the store the product filled",
		gateCRCLineInfo)
	assert.Equal(t, gpu.SrcNoLineInfo, store.Resolve(gateCRCNoLineInfo, noLineIdx, 0x10).Status())

	// The tail: PC batches, the stall map, the config record, the windows and
	// the drop records are all flushed after the last launch, so they are the
	// last things on the wire.
	waitFor(t, 10*time.Second, func() bool {
		st := c.Stats()
		return st.PCSamplesDecoded >= wantPC &&
			st.SamplingWindowsDecoded > 0 &&
			st.ConfigsDecoded > 0 &&
			st.PendingStallSamples == 0
	}, func() string {
		st := c.Stats()
		return fmt.Sprintf("pc=%d/%d windows=%d configs=%d pending-stall=%d stderr: %s",
			st.PCSamplesDecoded, wantPC, st.SamplingWindowsDecoded, st.ConfigsDecoded,
			st.PendingStallSamples, stubErr)
	})
	cancel()
	<-done

	stats := c.Stats()
	t.Logf("stub stderr:\n%s", stubErr)
	t.Logf("pc sampling: pc=%d modules=%d stall-names=%d windows=%d(open=%d) configs=%d cubins=%d/%dB undecoded=%d",
		stats.PCSamplesDecoded, stats.ModulesDecoded, stats.StallNamesLearned,
		stats.SamplingWindowsDecoded, stats.SamplingWindowsOpen, stats.ConfigsDecoded,
		stats.CubinsReceived, stats.CubinBytesReceived, stats.Undecoded)

	// ---- the transport, and the producer's own account of it --------------
	assert.Contains(t, stubErr, "pc_sampling=serialized",
		"the producer did not take the tier it was handed, so nothing below describes Tier A")
	assert.Contains(t, stubErr, "launch_dropped=0")
	assert.Contains(t, stubErr, "exec_dropped=0")
	assert.Zero(t, stats.SequenceGaps, "no batch may be lost silently")
	assert.Zero(t, stats.Malformed, "reasons: %v", stats.DecodeFailures)
	assert.Zero(t, stats.KernelDropped)
	assert.Zero(t, stats.SinkRejected)

	// Assertion 12. Undecoded is now zero for EVERY kind the ABI defines,
	// gpu_dropped_v1 included: issue #94 gave it an applyBatch arm so the
	// CUDA-graph drop class could arm Tier A's refusal. It used to read
	// wantDropRecs here, and that is the assertion this one replaces.
	//
	// The four class records the stub emits are accounted for exactly, and
	// they PARTITION: one graph-exec record acted on, three Tier B loss
	// classes decoded and counted unconsumed until the task that gives each an
	// operator-visible number lands. A fifth record, or a shortfall in the
	// partition, means a class arrived that nothing on this side knows about.
	assert.Zero(t, stats.Undecoded,
		"every kind the ABI defines has a decode arm; a non-zero Undecoded is a KIND_* added on one side of the wire and not the other")
	assert.Equal(t, uint64(wantDropRecs), stats.DropsDecoded,
		"the stub emits exactly one gpu_dropped_v1 record per drop class")
	assert.Equal(t, stats.DropsDecoded, stats.GraphExecReports+stats.DropsUnconsumed,
		"every decoded drop record is acted on or counted unconsumed: %+v", stats)
	assert.Equal(t, uint64(wantDropRecs-1), stats.DropsUnconsumed)
	assert.Equal(t, uint64(wantPC), stats.PCSamplesDecoded,
		"every PC record the stub emitted must have been decoded, not carried")
	assert.Equal(t, uint64(wantCubins), stats.CubinsReceived,
		"both checked-in cubins must have crossed the cubin channel: err=%q", stats.CubinsLastError)
	assert.Equal(t, uint64(2), stats.ModulesDecoded,
		"gpu_module_load_v1 announces the load; the bytes travel separately, and both must arrive")
	assert.Zero(t, stats.CubinsRejectedUnsealed, "a sealed memfd was refused: %q", stats.CubinsLastError)
	assert.Zero(t, stats.CubinsRejectedTooLarge)
	assert.Zero(t, stats.CubinsRejectedMalformed)
	assert.Zero(t, stats.CubinsRejectedUnauthorized)
	assert.Zero(t, stats.CubinsThrottled, "two offers cannot exhaust the cubin bucket")
	// The isolation property, on a live run rather than in the unit test that
	// proves it structurally (gate_compose_test.go assertion 10): real cubin
	// traffic crossed while a real enrolment happened, and the enrolment's own
	// counters are untouched.
	assert.Zero(t, stats.UnwindEnrollThrottled,
		"cubin traffic spent an enrolment admission token: %q", stats.UnwindEnrollLastError)
	assert.Equal(t, uint64(1), stats.UnwindEnrollConfirmed)
	assert.Zero(t, stats.StacksWalkedNoTables,
		"a capture was walked with no CFI tables: enroll requests=%d confirmed=%d err=%q",
		stats.UnwindEnrollRequests, stats.UnwindEnrollConfirmed, stats.UnwindEnrollLastError)

	// Stall names: the stub emits the map AFTER the batches on purpose, so
	// these two zeros are the assertion that the consumer held the samples
	// rather than rendering "stall#17" or dropping them.
	assert.Zero(t, stats.StallNamesMissing,
		"a PC sample carried a stall index the map never named; the label would then be empty")
	assert.Zero(t, stats.PendingStallSamples,
		"PC samples are still parked waiting for a stall name that has arrived")
	// The gauge, not the counter: the stall map replays on the attach edge
	// (spec §6.1's replay contract), so StallNamesLearned counts RECORDS and a
	// replayed map legitimately doubles it. What must be exactly 8 is the
	// number of distinct indexes the table ends up holding.
	assert.Equal(t, 8, stats.KnownStallNames,
		"the stub's synthetic GA102 table has 8 entries; a shortfall means a name was evicted or never interned")
	assert.GreaterOrEqual(t, stats.StallNamesLearned, uint64(8))
	assert.Zero(t, stats.StallNamesEvicted,
		"an 8-entry table cannot overflow the default bound; an eviction here costs PC samples their stall label")

	// Tier B's own signature, kept apart from the aggregate: every PC record
	// in CONTINUOUS collection has correlation 0, so ZeroCorrelation stops
	// being an anomaly signal for that population and this counter is what
	// carries it instead.
	assert.Equal(t, uint64(wantPC), stats.PCSamplesWithoutCorrelation,
		"the stub emits correlation 0 on every PC record, in both tiers")

	// ---- the injected samples ---------------------------------------------
	//
	// See the doc comment: the stub cannot produce a PC record that attributes
	// to anything, so assertions 2, 3, 4 and 9 are driven from records the gate
	// emits into the same Timeline the consumer emits into, on correlations
	// that a wire-delivered, stack-carrying launch occupies.
	//
	// SampleSchedule replays the shim's own sampler exactly (it is pinned
	// against it by TestTheGoReplicaMatchesTheShimSampler), so these
	// correlations are not guesses: launch ordinal N carries correlation N+1
	// and, if N is in the schedule, a CPU stack.
	sched := gpuabi.SampleSchedule(500, 8, gpuabi.DefaultSampleSeed)
	require.Equal(t, wantSampled, len(sched))
	corrOf := func(i int) gpu.CorrelationID {
		return gpu.CorrelationID{
			Backend: gpu.GPUBackendID("stub"),
			PID:     uint32(stubPID),
			Value:   strconv.FormatUint(sched[i]+1, 10),
		}
	}
	inject := func(corr gpu.CorrelationID, crc uint64, fnIndex uint32, pcOffset uint64) {
		t.Helper()
		require.NoError(t, timeline.EmitPCSample(gpu.GPUPCSample{
			Correlation:   corr,
			Module:        gpu.ModuleRef{Backend: gpu.GPUBackendID("stub"), CRC: crc},
			FunctionIndex: fnIndex,
			TimeNs:        1,
			PCOffset:      pcOffset,
			StallReason:   "long_scoreboard",
			Count:         1,
		}))
	}
	// One per gpu_src_status, each on its own stack-carrying execution.
	inject(corrOf(0), gateCRCLineInfo, lineIdx, 0x10)     // resolved
	inject(corrOf(1), gateCRCNoLineInfo, noLineIdx, 0x10) // no-lineinfo
	inject(corrOf(2), gateCRCAbsent, 0, 0x10)             // no-module
	inject(corrOf(3), gateCRCLineInfo, lineIdx, 0x180)    // unmapped: past the function
	// And a spray of distinct offsets for the cardinality cap.
	const capSpray = 40
	for i := range capSpray {
		inject(corrOf(4), gateCRCLineInfo, lineIdx, 0x1000+uint64(i)*16)
	}
	injected := 4 + capSpray

	snap := timeline.Snapshot()
	require.Len(t, snap.Executions, 500, "500 launches + 500 execs, exactly, none lost")

	// The wire's own account of the same two modules: gpu_module_load_v1
	// announces THAT a module loaded, with its CRC and size, while the bytes
	// travel the cubin channel. Both must name the same modules, or the
	// announcement and the payload describe different things and the CRC join
	// is meaningless.
	gotCRCs := map[uint64]uint64{}
	for _, m := range snap.Modules {
		gotCRCs[m.Ref.CRC] = m.SizeBytes
	}
	assert.Equal(t, map[uint64]uint64{
		gateCRCLineInfo:   uint64(len(lineInfo)),
		gateCRCNoLineInfo: uint64(len(noLineInfo)),
	}, gotCRCs,
		"the decoded gpu_module_load_v1 records do not name the same (crc, size) pairs the producer reported for the bytes it sent")

	// ---- assertion 5: reconciliation --------------------------------------
	//
	// Every PC record that reached the sink lands in exactly one of
	// attributed-exact, attributed-kernel, still-pending (in either store) or
	// evicted (from either store). The two pending stores are separate on
	// purpose - a Tier B eviction storm must be distinguishable from a Tier A
	// one - so both terms are in the identity.
	accepted := uint64(wantPC) + uint64(injected)
	assert.Equal(t, accepted,
		snap.AttributedPCSamples+
			uint64(snap.PendingSamples)+snap.Dropped.EvictedPendingSamples+
			uint64(snap.PendingModuleSamples)+snap.Dropped.EvictedPendingModuleSamples,
		"a PC sample went unaccounted for: attributed=%d pending=%d evicted=%d pending-module=%d evicted-module=%d of %d accepted",
		snap.AttributedPCSamples, snap.PendingSamples, snap.Dropped.EvictedPendingSamples,
		snap.PendingModuleSamples, snap.Dropped.EvictedPendingModuleSamples, accepted)
	assert.Equal(t, snap.AttributedPCSamples, snap.PCJoin.AttributedTotal(),
		"AttributedExact + AttributedKernel must account for every attributed sample: %+v", snap.PCJoin)
	assert.Equal(t, uint64(injected), snap.PCJoin.AttributedExact,
		"every injected sample carries a correlation a wire-delivered execution occupies, so all of them take the exact path")
	// And the stub's own 64, which can attribute to nothing - see the doc
	// comment. Asserted as an equality rather than tolerated as a shortfall:
	// if the stub is ever given real CRCs and real kernel names this number
	// changes, and the gate should say so rather than quietly pass.
	assert.Equal(t, wantPC, snap.PendingModuleSamples,
		"the stub's PC records carry synthetic CRCs and kernel names that no cubin can name, so every one of them must remain pending and counted - never attached to a plausible neighbour")
	assert.Positive(t, snap.PCJoin.GroupsUnresolvedName,
		"a group whose (crc, functionIndex) names nothing must be counted as such, not silently skipped")
	assert.Equal(t, snap.PCJoin.GroupsExamined(),
		snap.PCJoin.GroupsJoined+uint64(snap.PendingModuleGroups),
		"every pending group must be joined or left pending for exactly one counted reason: %+v", snap.PCJoin)

	// ---- assertion 8: Tier A disclosure -----------------------------------
	assert.Equal(t, uint64(len(snap.Executions)),
		snap.ExecutionsSerialized+snap.ExecutionsNotSerialized+snap.ExecutionsSerializationUnknown,
		"the three gpu_serialized outcomes must partition the executions exactly")
	assert.Positive(t, snap.SamplingWindowsReceived,
		"Tier A was selected and the producer said it emitted windows, but none reached the disclosure store")
	assert.Positive(t, snap.ExecutionsSerialized,
		"the stub's bursts bracket about half its executions; not one was marked perturbed")
	assert.Positive(t, snap.ExecutionsNotSerialized,
		"not one execution fell in a proven gap between bursts, so \"false\" is unreachable on this run and the three-way split is not being exercised")
	t.Logf("serialization: true=%d false=%d unknown=%d over %d windows (held=%d open=%d)",
		snap.ExecutionsSerialized, snap.ExecutionsNotSerialized, snap.ExecutionsSerializationUnknown,
		snap.SamplingWindowsReceived, snap.SamplingWindowsHeld, snap.SamplingWindowsOpen)

	// The second half of assertion 8, and the half that matters: Tier A
	// selected with NO window arriving must read "unknown" on every execution
	// and "false" on none. Driven off this run's own executions rather than
	// off synthetic ones, so the population is identical and only the evidence
	// differs.
	t.Run("tier A with no windows is unknown, never false", func(t *testing.T) {
		blind := gpu.NewTimeline(gpu.TimelineConfig{PCSampling: gpu.PCSamplingSerialized})
		for _, v := range snap.Executions {
			require.NoError(t, blind.EmitExec(v.Exec))
		}
		bs := blind.Snapshot()
		require.Len(t, bs.Executions, len(snap.Executions))
		assert.Equal(t, uint64(len(bs.Executions)), bs.ExecutionsSerializationUnknown,
			"Tier A ran and no window arrived, so nothing can be shown unperturbed")
		assert.Zero(t, bs.ExecutionsNotSerialized,
			"\"not perturbed\" when the truth is \"cannot tell\" is the one answer that must never be reachable by accident")
		assert.Zero(t, bs.ExecutionsSerialized)
	})

	// Assertion 10b's third clause, on the same population: a window with
	// end_ns == 0 is OPEN, not zero-length. Treating it as zero-length would
	// mark a whole perturbed tail "false".
	t.Run("an open window is unknown from its start, never false", func(t *testing.T) {
		open := gpu.NewTimeline(gpu.TimelineConfig{PCSampling: gpu.PCSamplingSerialized})
		var first, last uint64
		for i, v := range snap.Executions {
			if i == 0 || v.Exec.StartNs < first {
				first = v.Exec.StartNs
			}
			if v.Exec.EndNs > last {
				last = v.Exec.EndNs
			}
		}
		require.Greater(t, last, first)
		require.NoError(t, open.EmitSamplingWindow(gpu.GPUSamplingWindow{
			Backend: gpu.GPUBackendID("stub"),
			PID:     uint32(stubPID),
			StartNs: first,
			EndNs:   0, // the hard-exit shape: cuptiPCSamplingStop never ran
			Mode:    gpu.SamplingModeKernelSerialized,
		}))
		for _, v := range snap.Executions {
			require.NoError(t, open.EmitExec(v.Exec))
		}
		ws := open.Snapshot()
		assert.Zero(t, ws.ExecutionsNotSerialized,
			"an unterminated window read as zero-length would mark the whole perturbed tail \"false\"")
		assert.Equal(t, uint64(len(ws.Executions)),
			ws.ExecutionsSerialized+ws.ExecutionsSerializationUnknown,
			"every execution at or after an open window's start is either perturbed or unknown, and nothing else")
	})

	// ---- assertion 10b, first clause: the CUDA-graph refusal, end to end ---
	//
	// The stub emits one gpu_dropped_v1 record under GPU_DROP_CLASS_GRAPH_EXEC
	// whenever PC sampling is on, so this run - Tier A, from a real producer,
	// across a real uprobe - arms the refusal for real. That is the whole
	// difference between this and the unprivileged half in gpu/gate_test.go:
	// here the class crosses the wire, is decoded by the consumer's own arm,
	// is scoped to the producer's pid from the batch header, and reaches the
	// Timeline through the ordinary sink.
	//
	// Without the refusal this run would report gpu_join="exact" and
	// gpu_pc_attrib="exact" on every one of the injected samples while a graph
	// launch was in play - exact-LOOKING and many-to-one, which is issue #94.
	assert.Equal(t, uint64(2), stats.GraphExecutions,
		"the stub emits {count: 2, GPU_DROP_CLASS_GRAPH_EXEC}; a zero here means the class crossed the wire and nothing consumed it")
	assert.Equal(t, uint64(1), stats.GraphExecReports)
	assert.Equal(t, stats.GraphExecutions, snap.GraphExecutions,
		"every graph report the consumer decoded must have reached the Timeline; a gap is loss between the wire and the join")
	assert.Equal(t, uint64(1), snap.GraphExecProcesses)
	assert.Zero(t, snap.GraphExecUnscoped,
		"the producer named its process on the batch header, so the refusal must be scoped to it")
	assert.Zero(t, snap.GraphExecTrackingCapped)
	assert.True(t, snap.TierAGraphRefused())
	assert.Equal(t, uint64(len(snap.Executions)), snap.ExecutionsGraphRefused,
		"every execution of the one graph-using process in this run must be marked")
	assert.Positive(t, snap.PCJoin.GraphRefusedAttributions,
		"the injected PC samples joined by correlation, so their gpu_pc_attrib must have been withdrawn from \"exact\"")
	for i, v := range snap.Executions {
		require.True(t, v.GraphRefused, "execution %d unmarked", i)
		assert.NotEqual(t, gpu.PCAttribExact, v.PCAttrib,
			"execution %d still claims exact attribution in a graph-using process", i)
	}
	// And the refusal is LOUD: it reaches the operator on the summary line
	// every run prints, not only in an anomaly they have to scroll to.
	graphHealth := gpu.JoinHealth(snap)
	assert.Contains(t, graphHealth[0], "WITHDRAWN")
	assert.Contains(t, strings.Join(graphHealth, "\n"), "launched from CUDA GRAPHS")

	// ---- the projection ---------------------------------------------------
	//
	// Projected TWICE over the SAME snapshot: once with the default budget, for
	// the label assertions, and once with a small ceiling for assertion 9.
	// Snapshot() is consuming, so the snapshot value is reused rather than
	// retaken.
	samples, projStats := gpu.ProjectExecutionsWith(snap, gpu.ProjectionConfig{Modules: store})
	require.NotEmpty(t, samples, "the gate is pprof samples, not counters")
	assert.Zero(t, projStats.PCLabelsSuppressed,
		"the default ceiling is nowhere near %d distinct offsets; a suppression here means the budget shrank",
		projStats.DistinctPCLabels)

	// The walk over every projected sample: assertions 1, 2, 3 and 4, on real
	// output rather than on a hand-built ExecutionView.
	byStatus := map[string]int{}
	byAttrib := map[string]int{}
	var pcDerived, resolvedWithStack int
	si := 0
	for _, view := range snap.Executions {
		// ProjectExecutionsWith emits one sample per PC sample, or one for
		// the execution itself when it carries none, in snapshot order.
		n := max(1, len(view.PCSamples))
		for range n {
			require.Less(t, si, len(samples))
			s := samples[si]
			si++

			// Assertion 1, in its exact form: the frames are the launch's own
			// CPU stack, then the boundary marker, then the kernel - compared
			// as a whole slice rather than scanned for forbidden substrings.
			// Whole-slice comparison is strictly stronger (nothing may be
			// inserted anywhere, not merely appended) and it has no false
			// positives, which a substring scan would: several of the stub's
			// stall reasons are spelt "wait", "barrier" and "membar", and libc
			// frame names contain all three.
			require.NotEmpty(t, view.Exec.KernelName,
				"every execution carries an interned kernel name, so the kernel frame is never omitted")
			var want []string
			if view.Launch != nil && len(view.Launch.Launch.CPUStack) > 0 {
				for _, f := range view.Launch.Launch.CPUStack {
					want = append(want, f.Name)
				}
				want = append(want, gpu.FrameLaunch)
			} else {
				want = append(want, gpu.FrameLaunchUnsampled)
			}
			want = append(want, "[gpu:kernel:"+view.Exec.KernelName+"]")
			require.Equal(t, want, frameNamesOf(s.Stack),
				"frames are exhaustively the CPU stack, the boundary marker and the kernel; this sample's differ")
			// The two frames this package synthesizes are the only ones it
			// could smuggle per-sample detail into, so they take the substring
			// scan the CPU frames cannot safely take.
			for _, name := range want[len(want)-2:] {
				for _, bad := range []string{"gpu:pc", "gpu:src", "gpu:stall", "long_scoreboard", "resolved", "0x"} {
					assert.NotContains(t, name, bad,
						"per-sample detail was promoted to a frame: %q", name)
				}
			}

			if len(view.PCSamples) == 0 {
				assert.NotContains(t, s.Labels, "gpu_src_status",
					"an execution with no PC samples has nothing to say about a source location")
				continue
			}
			pcDerived++
			status := s.Labels["gpu_src_status"]
			require.NotEmpty(t, status,
				"gpu_src_status is unconditional on every PC-derived sample: an absent label reads as \"not sampled\"")
			byStatus[status]++
			byAttrib[s.Labels["gpu_pc_attrib"]]++
			require.NotEmpty(t, s.Labels["gpu_pc_attrib"], "gpu_pc_attrib is unconditional too")
			require.Contains(t, s.Labels, "gpu_serialized",
				"gpu_serialized rides on every execution, PC-bearing or not")

			switch status {
			case "resolved":
				// Assertion 2: a source line reached from a CPU stack.
				assert.Equal(t, "single.cu", s.Labels["gpu_src_file"], "the basename, never the build-host path")
				assert.Equal(t, "addOne", s.Labels["gpu_src_func"])
				require.Contains(t, s.Labels, "gpu_src_line")
				if view.Launch != nil && len(view.Launch.Launch.CPUStack) > 0 {
					resolvedWithStack++
					if resolvedWithStack == 1 {
						t.Logf("assertion 2: %v -> %s:%s (%s, attrib=%s)",
							frameNamesOf(s.Stack), s.Labels["gpu_src_file"], s.Labels["gpu_src_line"],
							s.Labels["gpu_stall"], s.Labels["gpu_pc_attrib"])
					}
				}
			default:
				// Assertion 3, generalized to all three unresolvable statuses:
				// no location is ever invented.
				assert.NotContains(t, s.Labels, "gpu_src_file", "status=%s invented a file", status)
				assert.NotContains(t, s.Labels, "gpu_src_line", "status=%s invented a line", status)
				assert.NotContains(t, s.Labels, "gpu_src_func", "status=%s invented a function", status)
			}
		}
	}
	require.Equal(t, len(samples), si, "every projected sample must belong to an execution")
	t.Logf("pc-derived samples: %d  by status: %v  by attrib: %v", pcDerived, byStatus, byAttrib)

	// Assertion 10b's first clause, at the label. This run's PC samples all
	// joined by vendor correlation, so without the CUDA-graph refusal every
	// one of them would read gpu_pc_attrib="exact" - in a run where the
	// producer reported a graph execution, which makes that word false. There
	// must be no "exact" left, and the withdrawal must be visible on every
	// sample rather than only in a counter.
	assert.Zero(t, byAttrib[string(gpu.PCAttribExact)],
		"a graph execution was reported on this run; no sample may still claim exact attribution: %v", byAttrib)
	assert.Equal(t, pcDerived, byAttrib[string(gpu.PCAttribGraphRefused)],
		"every correlation-joined sample's attribution must have been withdrawn: %v", byAttrib)
	for i := range samples {
		assert.Equal(t, "true", samples[i].Labels["gpu_graph_refused"],
			"gpu_graph_refused rides on EVERY execution of a graph-using process, PC-bearing or not (sample %d)", i)
	}

	// Assertion 2, stated as a number rather than left implicit in the loop.
	assert.Positive(t, resolvedWithStack,
		"no sample carries BOTH a real CPU stack and gpu_src_status=resolved; that conjunction is the Phase 6 exit condition, and either half alone is not it")

	// Assertion 4: all four values reachable, each by the fixture that should
	// produce it.
	for _, want := range []string{"resolved", "no-lineinfo", "no-module", "unmapped"} {
		assert.Positive(t, byStatus[want],
			"gpu_src_status=%q is not reachable from this run: %v", want, byStatus)
	}
	// Assertion 3's own arithmetic: the -lineinfo fixture produced the
	// resolved population, the no-lineinfo fixture produced exactly one
	// no-lineinfo sample, and neither borrowed from the other.
	assert.Equal(t, 1, byStatus["no-lineinfo"],
		"exactly one sample was injected against the no-lineinfo fixture")
	// One, not 65: the stub's own 64 records never reach an execution at all
	// (see the doc comment), so they never project and never carry a label.
	// The single no-module sample here is the injected one whose CRC no cubin
	// was ever stored for - which is exactly the fixture that should produce
	// that status.
	assert.Equal(t, 1, byStatus["no-module"],
		"only the injected absent-CRC sample can read no-module; the stub's records are pending and unprojected")
	assert.Equal(t, 1, byStatus["resolved"])
	assert.Equal(t, 1+capSpray, byStatus["unmapped"],
		"the injected past-the-function offset plus the %d cardinality-spray offsets, all past the line table's last address", capSpray)
	assert.Equal(t, injected, pcDerived,
		"every projected PC-derived sample must be one of the injected ones")

	// ---- assertion 9: the cardinality cap ---------------------------------
	//
	// Past the ceiling gpu_pc is dropped and counted, while gpu_stall and
	// gpu_src_* survive untouched: they are coarser and more actionable, so the
	// label that gives way is the numerous one rather than the useful one.
	const ceiling = 8
	capped, capStats := gpu.ProjectExecutionsWith(snap, gpu.ProjectionConfig{
		Modules:             store,
		MaxDistinctPCLabels: ceiling,
	})
	require.Len(t, capped, len(samples), "the cap changes labels, never the sample population")
	distinct := map[string]bool{}
	var suppressed uint64
	for i, s := range capped {
		if _, isPC := s.Labels["gpu_src_status"]; !isPC {
			continue
		}
		if pc, ok := s.Labels["gpu_pc"]; ok {
			distinct[pc] = true
			continue
		}
		suppressed++
		// Everything else must have survived. This is the half that a cap
		// which simply stopped emitting labels would fail.
		assert.NotEmpty(t, s.Labels["gpu_stall"],
			"the cap dropped gpu_stall, which it must never touch (sample %d)", i)
		assert.NotEmpty(t, s.Labels["gpu_src_status"],
			"the cap dropped gpu_src_status, which is unconditional (sample %d)", i)
		assert.NotEmpty(t, s.Labels["gpu_pc_attrib"], "the cap dropped gpu_pc_attrib (sample %d)", i)
		assert.Equal(t, samples[i].Value, s.Value,
			"a suppressed sample still carries its full share of the execution's duration")
	}
	assert.Positive(t, suppressed,
		"a ceiling of %d over %d distinct injected offsets suppressed nothing, so this proves nothing",
		ceiling, capSpray)
	assert.Equal(t, suppressed, capStats.PCLabelsSuppressed,
		"ProjectionPCLabelsSuppressed must equal the suppressions actually visible in the output, or the counter is decoration")
	assert.Equal(t, uint64(len(distinct)), capStats.DistinctPCLabels)
	assert.LessOrEqual(t, len(distinct), ceiling, "more distinct gpu_pc values were emitted than the ceiling allows")
	assert.Equal(t, uint64(ceiling), capStats.PCLabelCap)
	// And it is visible to the operator: a profile that silently lost its PC
	// labels looks identical to one that never had any.
	health := strings.Join(gpu.JoinHealthWith(snap, capStats), "\n")
	assert.Contains(t, health, "gpu_pc",
		"the suppression is not surfaced in joinhealth output:\n%s", health)
	t.Logf("cardinality cap: distinct=%d cap=%d suppressed=%d", capStats.DistinctPCLabels, capStats.PCLabelCap, capStats.PCLabelsSuppressed)
}

// waitFor polls until cond holds or the deadline passes, failing with what
// describe() reports rather than with a bare timeout. A producer that never
// emitted and a consumer that never drained look identical from a timeout.
func waitFor(t *testing.T, within time.Duration, cond func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %s", within, describe())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func frameNamesOf(frames []pp.Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Name)
	}
	return out
}

func fixturePath(name string) string {
	return filepath.Join("..", "internal", "cubin", "testdata", name)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err, "cubin fixture %s", name)
	require.NotEmpty(t, b)
	return b
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	require.NoError(t, err)
	return abs
}

// fixtureSymIndex reads the .symtab index the module store keys its
// functionIndex table on out of the fixture itself. Whether CUPTI's
// functionIndex IS that index is the design's premise and is measured on
// hardware (Task 6); nothing here depends on the answer, only on the store and
// the sample agreeing.
func fixtureSymIndex(t *testing.T, b []byte, fn string) uint32 {
	t.Helper()
	c, err := cubin.Parse(b)
	require.NoError(t, err)
	for _, f := range c.Functions() {
		if f.Name == fn {
			require.GreaterOrEqual(t, f.SymIndex, 0)
			return uint32(f.SymIndex)
		}
	}
	t.Fatalf("fixture has no function %q", fn)
	return 0
}

// gateModuleCRCs pulls the CRC the producer declared for each captured module
// out of its own stderr, in capture order.
//
// From the producer rather than from a constant, so that the number the module
// store is keyed on is the number that was actually put on the wire. A
// constant would keep this gate green through a change to how the producer
// derives a CRC - which is exactly the change that would break the join on
// hardware.
func gateModuleCRCs(t *testing.T, stubErr string) []uint64 {
	t.Helper()
	re := regexp.MustCompile(`stub: module id=\d+ path=\S+ size=\d+ crc=0x([0-9a-f]{16}) captured=yes`)
	var out []uint64
	for _, m := range re.FindAllStringSubmatch(stubErr, -1) {
		v, err := strconv.ParseUint(m[1], 16, 64)
		require.NoError(t, err)
		out = append(out, v)
	}
	return out
}

// stubCubinCRC is the Go replica of shim/stub/stub.cc's stub_cubin_crc: FNV-1a
// over the module bytes, with zero coerced to one because zero is the ABI's
// "no module".
//
// It exists so the CRC can be recomputed from the checked-in fixture rather
// than read back from the producer that produced it. Reading it back would
// assert only that the producer is self-consistent; recomputing it asserts
// that the identity the join runs on is a function of the BYTES, which is the
// property the whole content-addressed scheme rests on.
func stubCubinCRC(b []byte) uint64 {
	h := uint64(1469598103934665603)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	if h == 0 {
		return 1
	}
	return h
}

// TestGateTheStubsPCRecordsCannotAttributeToAnything pins the second reason
// TestStubDrivesPCSamplingToPprofWithoutAGPU injects PC samples of its own
// instead of using the producer's, and it runs WITHOUT capabilities so the
// reason is checkable on any machine.
//
// # The gap
//
// The stub emits real module loads carrying real checked-in cubins, and real
// PC-sample records. They are unrelated to each other:
//
//   - the PC records' cubin_crc is one of two compile-time constants,
//     kStubCubinCRC = {0xC0FFEE01, 0xC0FFEE02}, while the modules the same run
//     delivers are keyed by a content hash of the fixture bytes (FNV-1a over
//     the file, measured: 0x9d57accad01046eb for single_lineinfo.cubin). No
//     cubin is ever stored under a 0xC0FFEE0n key, so every one of those
//     records resolves as "no-module";
//   - their correlation is 0 in every tier, by design and correctly - that is
//     what CONTINUOUS collection produces - so the exact-correlation path is
//     unavailable to them;
//   - Tier B attribution therefore runs crc -> module -> function name ->
//     the execution's KernelName, and the stub's kernel names are
//     "kernel_1111"/"kernel_2222" while the fixtures' only function is the
//     CUDA kernel they were compiled from ("addOne"). No name can match.
//
// So neither join path can fire for a stub PC record, in either tier. That is
// not a bug in the pipeline - the pipeline correctly counts every one of them
// as pending, which the end-to-end gate asserts as an exact equality - but it
// does mean the producer cannot drive gate assertions 2, 3, 4 or 9, all of
// which need a PC sample that reaches an execution.
//
// Closing it is a small change to shim/stub/stub.cc: record the CRC each
// capture computed and use it on the PC records, and name the kernels after
// the fixtures' own functions. This branch is a test task and may not change
// the shim, so it is pinned here instead.
//
// # Why a passing test
//
// Same shape as the other two outstanding pins: when the stub is fixed, this
// fails by name and the person fixing it drops the injection from the
// end-to-end gate and asserts the real thing.
func TestGateTheStubsPCRecordsCannotAttributeToAnything(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "shim", "stub", "stub.cc"))
	require.NoError(t, err, "the producer's source must be readable; this test is about what it emits")
	body := string(src)

	assert.Contains(t, body, "static const uint64_t kStubCubinCRC[] = {0xC0FFEE01ull, 0xC0FFEE02ull};",
		"the stub's synthetic PC-record CRCs changed. If they now come from the CRC each capture "+
			"computed, gate assertions 2/3/4/9 may be drivable from the producer: drop the PC-sample "+
			"injection from TestStubDrivesPCSamplingToPprofWithoutAGPU and assert them off the wire.")
	assert.Contains(t, body, "r.cubin_crc = kStubCubinCRC[i % 2];",
		"the stub no longer keys its PC records on the synthetic constants - see above")
	assert.Contains(t, body, `snprintf(namebuf, sizeof(namebuf), "kernel_%llx", (unsigned long long)kernel_id);`,
		"the stub's kernel names changed. If they now name functions the checked-in cubins "+
			"actually contain, the Tier B module join can fire for its own records - see above.")

	// And the fixtures really do carry a different name, so the mismatch above
	// is a fact about both ends rather than an assumption about one.
	c, err := cubin.Parse(readFixture(t, "single_lineinfo.cubin"))
	require.NoError(t, err)
	for _, fn := range c.Functions() {
		assert.NotContains(t, fn.Name, "kernel_",
			"a fixture function is now named like a stub kernel; the join might match by accident, which is worse than not matching at all")
	}
	t.Log("stub PC records -> a cubin the agent holds: OUTSTANDING - synthetic CRCs and synthetic " +
		"kernel names; see .superpowers/sdd/task-13-gate-report.md")
}
