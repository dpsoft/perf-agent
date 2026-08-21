package gpuprobe_test

import (
	"bytes"
	"context"
	"debug/elf"
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
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// hasCaps mirrors perfagent/agent.go's hasCapSysPtrace: check Permitted as
// well as Effective, because a setcap'd binary has not promoted Permitted
// yet, and never gate on Getuid alone.
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
	// with 500 launches the sampler is deterministic and yields exactly 63
	// sampled launches (n%8==0 for n in 0..499, i.e. ceil(500/8)).
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
	// period_us is 1000, not the 200 this gate used through Phase 4a. In
	// system-wide mode (Config.PID == 0, which this gate uses because the
	// producer does not exist at Attach time) the walker's CFI tables for a
	// process are compiled on the first batch the consumer sees FROM that
	// process, on a worker goroutine, and that compile takes tens of
	// milliseconds - it reads .eh_frame out of the producer, libstdc++, libm
	// and libc. Sampled launches taken before it finishes are walked with no
	// tables at all and land in StacksWalkedNoTables, which is correct
	// behaviour and NOT asserted zero below.
	//
	// At 200us the whole 500-launch run lasted ~100ms, the same order as the
	// compile, so the share of launches that got a DWARF walk was a coin
	// toss. At 1000us the run lasts ~500ms against a ~50ms compile, so the
	// large majority of the 63 sampled launches are walked with tables
	// present. Nothing else depends on the period: the sampler is counting
	// launches, not time, so the 63 below is unchanged.
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
	// path, so once it reads 63 every symbolization has already happened —
	// and happened while the stub was alive.
	const wantSampled = 63
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
	assert.Equal(t, uint64(63), stubSampled, "ceil(500/8): the sampler is deterministic at period 8 over 500 launches")

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
	assert.Equal(t, uint64(63), stats.SampledLaunches,
		"ceil(500/8) sampled launches, deterministic because the sampler is")
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

	// Deliberately NOT asserted zero, and this is the trap in the shape of
	// an obvious assertion: in system-wide mode the CFI tables for a process
	// are compiled after the consumer sees that process's first batch, so
	// the first sampled launches from a newly-seen producer are legitimately
	// walked before its tables exist. A small non-zero count here is correct
	// behaviour. What would be wrong is ALL of them, which is what the
	// bound below says - and which StacksWalkedDWARF > 0 already implies,
	// stated separately so the intent survives a future edit.
	assert.Less(t, stats.StacksWalkedNoTables, uint64(wantSampled),
		"every single capture was walked without CFI tables: registration never completed for the producer. last registration error: %q",
		stats.UnwindLastError)
	assert.Positive(t, stats.UnwindPIDsRegistered,
		"the consumer never registered the producer's PID with the walker, so no capture could have had tables to use")
	assert.Zero(t, stats.UnwindPIDsFailed,
		"a registration that installed nothing: %q", stats.UnwindLastError)

	// StackWalkAbandoned means "the walk stopped because it could not
	// proceed". Every walk in this run stops for one of exactly two reasons,
	// and issue #44 is the story of them having been indistinguishable:
	//
	//   - a DWARF walk crosses the two FP-less bridges and arrives at main
	//     with no caller frame pointer, because their CFI carries no rule
	//     for %rbp and ehcompile reads that as UNDEFINED (issue #45) where
	//     the psABI says unchanged. walk_step reports it as
	//     WALKER_FLAG_FP_EXHAUSTED and it is a real truncation: main's
	//     callers - __libc_start_call_main, __libc_start_main_impl, _start -
	//     are on the stack and are missing from the capture.
	//   - an FP-only walk (the first capture or two, before the tables land)
	//     follows the frame-pointer chain out through
	//     __libc_start_call_main and __libc_start_main_impl into _start,
	//     whose saved frame pointer is zero - WALKER_FLAG_FP_TERMINATED.
	//     Verified under gdb on this exact producer.
	//
	// So abandoned is NOT zero here, and asserting that it were would be
	// asserting the bug. What it must be is fully explained: every
	// abandonment accounted for by the one cause this producer has, with no
	// unexplained remainder and no CFI misses. That is the instrument issue
	// #45 will be measured against - when "no rule for %rbp" starts meaning
	// unchanged, these walks reach _start instead, and the counts move from
	// StackWalkFPExhausted to StackWalkReachedRoot with abandoned falling to
	// zero.
	//
	// The first run of this gate reported abandoned == StacksWalkedDWARF ==
	// 62 for an unrelated reason: WALKER_FLAG_FP_TERMINATED was then the
	// only end-of-chain signal, and a hybrid walk that crossed an FP-less
	// frame can never set it, so the counter fired on successful walks too.
	// The equality below looks the same and means something different -
	// these walks really are cut short - so the two must not be confused.
	assert.Zero(t, stats.StacksWalkedCFIMiss,
		"the tables were consulted and did not cover a frame's PC: a gap in what ehcompile produced for this binary, not the known #45 truncation")
	assert.Equal(t, stats.StackWalkFPExhausted, stats.StackWalkAbandoned,
		"walks stopped for a reason other than the known lost-frame-pointer one: %d abandoned, %d of them fp-exhausted, of %d captures. cfi-miss=%d no-tables=%d dwarf=%d fp-only=%d",
		stats.StackWalkAbandoned, stats.StackWalkFPExhausted, wantSampled, stats.StacksWalkedCFIMiss,
		stats.StacksWalkedNoTables, stats.StacksWalkedDWARF, stats.StacksWalkedFPOnly)
	// The measurement itself, and the reason this gate is also the
	// instrument for #45: on this producer a DWARF walk CANNOT end any other
	// way, because every FP-less bridge frame loses %rbp and main above it
	// is FP_SAFE - see TestTheReportedTerminationArmIsTheOneTheCFIForces,
	// which derives that from the binary's own CFI without any capability.
	// If this ever reads less than StacksWalkedDWARF, some walk found a
	// different ending and the CFI-level derivation no longer describes the
	// producer.
	assert.Equal(t, stats.StacksWalkedDWARF, stats.StackWalkFPExhausted,
		"a DWARF walk on this producer ended some way other than running out of frame pointer: dwarf=%d fp-exhausted=%d reached-root=%d",
		stats.StacksWalkedDWARF, stats.StackWalkFPExhausted, stats.StackWalkReachedRoot)
	assert.Zero(t, stats.StackWalkReachedRoot,
		"a walk reached a frame whose CFI marks it outermost: with %%rbp lost at every bridge that should be unreachable until issue #45 is fixed, and when it IS fixed this is the assertion to invert")
	t.Logf("walk shape: dwarf=%d fp-only=%d no-tables=%d cfi-miss=%d truncated=%d abandoned=%d fp-exhausted=%d reached-root=%d registered=%d binaries=%d",
		stats.StacksWalkedDWARF, stats.StacksWalkedFPOnly, stats.StacksWalkedNoTables,
		stats.StacksWalkedCFIMiss, stats.StackWalkTruncated, stats.StackWalkAbandoned,
		stats.StackWalkFPExhausted, stats.StackWalkReachedRoot,
		stats.UnwindPIDsRegistered, stats.UnwindBinariesAttached)

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

	// Sampled-stack accounting: exactly 63 of the 500 launches reached the
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
	for _, view := range snap.Executions {
		require.NotNil(t, view.Launch,
			"every execution joined its launch exactly (asserted above), so Launch must never be nil here")
		if len(view.Launch.Launch.CPUStack) == 0 {
			continue
		}
		sampledLaunches++
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
	assert.Equal(t, 63, sampledLaunches,
		"exactly 63 launches on the timeline must carry a non-empty CPUStack, matching Stats.SampledLaunches")
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

// privateStubCopy copies the built stub into this test's own temp directory
// and returns the copy's path. The copy is what the test attaches to and what
// it runs, so the uprobe can only ever match this run's executions.
//
// t.TempDir() is removed when the test ends, taking the copy with it. It lives
// under TMPDIR (/tmp here, mounted rw,nosuid,nodev — not noexec, so the copy
// is runnable); nosuid is irrelevant because the stub is an ordinary
// unprivileged producer and carries no file capabilities. If TMPDIR were ever
// noexec the exec below fails loudly with ENOEXEC/EACCES rather than
// silently degrading.
func privateStubCopy(t *testing.T, src string) string {
	t.Helper()
	info, err := os.Stat(src)
	require.NoError(t, err)
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
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
// Issue #44 asserts that on the RTX 3090 validation run EVERY DWARF walk
// terminated via walk_step's `ctx->fp == 0` arm rather than its
// `ra_type == UNDEFINED` arm. That assertion came out of a report, and it is
// the justification for splitting WALKER_FLAG_UNWIND_TERMINATED in two, so it
// deserves a measurement rather than a citation.
//
// The GPU is not reachable from here, but the MECHANISM is: which arm fires
// is decided entirely by data this test can read without any capability at
// all. walk_step's choice at each frame comes from two facts and nothing
// else:
//
//   - the mode of the frame it is ON (FP_LESS takes the DWARF path, FP_SAFE
//     and FALLBACK take the frame-pointer path), and
//   - the fp_type of the frame it stepped OUT of (UNDEFINED or REGISTER set
//     new_fp = 0, and the FP path then finds ctx->fp == 0).
//
// Both come straight out of ehcompile, from a real binary, and the memory
// contents cannot change the answer: the arm is reached before any read.
//
// The binary is the gate's own producer, whose stack is built to reproduce
// the CUDA shape - an FP-less bridge between the probe frame and main. If the
// issue's claim holds anywhere it holds here.
func TestTheReportedTerminationArmIsTheOneTheCFIForces(t *testing.T) {
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
	modeOf := func(pc uint64) ehcompile.Mode {
		for _, c := range classes {
			if pc >= c.PCStart && pc < c.PCStart+uint64(c.PCEndDelta) {
				return c.Mode
			}
		}
		// walk_step's own default for an uncovered PC.
		return ehcompile.ModeFPSafe
	}
	cfiOf := func(pc uint64) *ehcompile.CFIEntry {
		for i := range entries {
			if pc >= entries[i].PCStart && pc < entries[i].PCStart+uint64(entries[i].PCEndDelta) {
				return &entries[i]
			}
		}
		return nil
	}

	// The frame the walk steps OUT of last: the outermost FP-less bridge,
	// which walk_step unwinds with CFI.
	caller := cfiOf(pcOf("perfagent_fpless_caller"))
	require.NotNil(t, caller, "no CFI row covers perfagent_fpless_caller")
	require.Equal(t, ehcompile.ModeFPLess, modeOf(pcOf("perfagent_fpless_caller")),
		"not FP-less, so walk_step would never take the DWARF path here")

	// This is the whole mechanism, and it is issue #45 seen from the outside:
	// the function never touches %rbp, its CFI therefore carries no rule for
	// %rbp, and ehcompile calls that UNDEFINED where the x86-64 psABI says
	// unchanged. walk_step turns UNDEFINED into new_fp = 0.
	require.Equal(t, ehcompile.FPTypeUndefined, caller.FPType,
		"the FP-less frame preserves a frame pointer after all; the walk would not lose it and the claim under test would not arise here")
	require.Equal(t, ehcompile.RATypeOffsetCFA, caller.RAType,
		"the FP-less frame is already the outermost per its own CFI; the walk would end before reaching main")

	// The frame the walk lands ON next, with ctx->fp now zero.
	mainPC := pcOf("main")
	require.Equal(t, ehcompile.ModeFPSafe, modeOf(mainPC),
		"main is not FP_SAFE, so walk_step would take the DWARF path and never reach the ctx->fp == 0 check")

	// Therefore: FP path, ctx->fp == 0, WALKER_FLAG_FP_EXHAUSTED. The other
	// arm is not merely unfired here - it is unreachable, because reaching
	// it needs a frame whose CFI marks it outermost, and the only such frame
	// on this stack is _start, three FP_SAFE frames further up
	// (__libc_start_call_main, __libc_start_main, then _start) that the walk
	// can no longer reach with a zeroed frame pointer.
	require.NotEqual(t, ehcompile.RATypeUndefined, caller.RAType)
	mainCFI := cfiOf(mainPC)
	require.NotNil(t, mainCFI)
	assert.NotEqual(t, ehcompile.RATypeUndefined, mainCFI.RAType,
		"main marks itself outermost, which would fire the RA_UNDEFINED arm instead")

	// And the arm that SHOULD have fired is real, not hypothetical: _start in
	// this very binary is marked outermost exactly as the issue describes.
	// It is simply unreachable once the frame pointer is gone. That is the
	// whole of issue #44's claim, reproduced locally.
	startCFI := cfiOf(pcOf("_start"))
	require.NotNil(t, startCFI, "no CFI row covers _start")
	assert.Equal(t, ehcompile.RATypeUndefined, startCFI.RAType,
		"_start does not mark itself outermost, so the RA_UNDEFINED arm would never fire for ANY walk on this binary")

	t.Logf("perfagent_fpless_caller: mode=FP_LESS fp_type=UNDEFINED -> new_fp = 0")
	t.Logf("main: mode=FP_SAFE, reached with ctx->fp == 0 -> WALKER_FLAG_FP_EXHAUSTED")
	t.Logf("_start: ra_type=UNDEFINED (the WALKER_FLAG_RA_UNDEFINED arm), unreachable from main with no frame pointer")
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
