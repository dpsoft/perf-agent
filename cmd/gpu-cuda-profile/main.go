// Attaches to the NVIDIA CUPTI adapter, runs a real CUDA workload under it,
// and writes gpu-cuda.pb.gz through the existing pprof builder.
//
// This is cmd/gpu-stub-profile with the synthetic producer replaced by an
// actual GPU. The adapter is not executed: it is a shared object the CUDA
// driver loads into the workload through CUDA_INJECTION64_PATH, so this
// command attaches its uprobes to the .so by path (system-wide, since the
// process it will be mapped into does not exist yet) and then starts the
// workload with that environment variable set.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"

	// Registering the interpreter unwinders is a BINARY's decision, not a
	// library's: which languages this build can walk is a property of what was
	// linked, and a library that registered them behind the caller's back would
	// make that invisible. unwind/interp/modules is the single place that names
	// any of them; see unwind/interp for where a new one goes.
	_ "github.com/dpsoft/perf-agent/unwind/interp/modules"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func main() {
	var (
		shim     = flag.String("shim", "./shim/libperfagent-gpu-nvidia.so", "the CUPTI adapter .so carrying the perfagent USDT probes")
		workload = flag.String("workload", "./shim/nvidia/testdata/cuda_workload", "CUDA program to run under the adapter")
		iters    = flag.Int("iters", 2000, "workload iterations; it launches two kernels per iteration")
		sleepUs  = flag.Int("sleep-us", 200, "workload sleep between iterations, in microseconds")
		period   = flag.Int("period", 8, "one-in-N launch sampling period (PERFAGENT_GPU_SAMPLE_PERIOD)")
		linger   = flag.Int("linger-ms", 30000, "how long the workload may wait to be released after it finishes")
		out      = flag.String("out", "gpu-cuda.pb.gz", "output pprof profile")

		// One setting, three values, and no way to ask for two. The default
		// is the empty string rather than "off" so that an unspecified flag
		// DEFERS to an inherited PERFAGENT_GPU_PC_SAMPLING instead of
		// contradicting it — an explicit --gpu-pc-sampling=off against an
		// exported "serialized" is a disagreement and is refused, but not
		// setting the flag at all is not.
		pcSampling = flag.String("gpu-pc-sampling", "",
			"GPU PC-sampling tier: "+strings.Join(gpu.PCSamplingTierNames, " | ")+
				" (default off; also read from "+gpu.PCSamplingEnvVar+"). "+
				"\"continuous\" does not serialize kernels; \"serialized\" does, and requires "+
				"-gpu-pc-sampling-acknowledge-perturbation")
		pcAck = flag.Bool("gpu-pc-sampling-acknowledge-perturbation", false,
			"acknowledge that the \"serialized\" tier perturbs the workload: it inflates GPU "+
				"kernel durations inside a burst, it distorts any CPU and off-CPU profile taken "+
				"alongside it with no marking in those profiles at all, and it is unavailable "+
				"where CUDA graphs are in use")
	)
	flag.Parse()

	// Tier selection, and it happens BEFORE anything is attached or launched.
	// Every refusal here is a startup error: an unknown value, a value naming
	// two tiers, the flag and the environment naming two tiers, or Tier A
	// without its acknowledgement. None of them is resolved to a tier — a
	// profile produced under a tier nobody chose is worse than no profile,
	// because nothing in it says which one ran.
	tier, err := gpu.PCSamplingRequest{
		Flag:                    *pcSampling,
		Env:                     os.Getenv(gpu.PCSamplingEnvVar),
		AcknowledgePerturbation: *pcAck,
	}.Select()
	if err != nil {
		log.Fatalf("gpu pc sampling: %v", err)
	}
	// Printed at startup as well as standing in every JoinHealth render
	// below. The startup copy is for the operator who is watching the run
	// begin; the standing copy is for the one who reads the profile an hour
	// later, which is the reader the warning is actually for.
	for _, line := range gpu.PCSamplingStandingWarning(tier) {
		log.Print(line)
	}

	shimPath, err := filepath.Abs(*shim)
	if err != nil {
		log.Fatalf("shim path: %v", err)
	}
	// The driver dlopens this by the exact string in CUDA_INJECTION64_PATH,
	// and the uprobes are attached to the path resolved here. If the two ever
	// disagreed the probes would sit on a file nothing maps and the run would
	// silently produce an empty profile, so both come from one variable.
	if _, err := os.Stat(shimPath); err != nil {
		log.Fatalf("adapter %s: %v (build it with: make -C shim nvidia)", shimPath, err)
	}

	// The module store, built HERE because it has three readers and no owner
	// among them: the cubin listener writes every arriving cubin into it
	// (gpuprobe.Config.Modules), the Timeline's join resolves a pending PC
	// group's (cubin_crc, functionIndex) to a device function name through it
	// (gpu.TimelineConfig.Modules), and the projection resolves the source
	// location against it (gpu.ProjectionConfig.Modules). It is ONE instance
	// in all three places on purpose: a second store would hold a second copy
	// of every cubin and answer "no-module" for modules the first one holds.
	//
	// The bounds are gpu.ModuleStoreConfig's defaults - 512 modules and
	// 64 MiB - and they are taken rather than restated. 512 distinct cubins is
	// already the JIT/template-explosion case rather than a normal workload,
	// and 64 MiB is a tighter resident bound than anything the transport
	// enforces (8 MiB per cubin, 256 MiB total offered), so the store is what
	// actually caps what this process holds. Writing the numbers again here
	// would put a second copy of the sizing where drift is invisible.
	//
	// Both bounds evict least-recently-used, and eviction is honest: a PC
	// sample for a module that was dropped resolves "no-module", never a stale
	// line from a module that is no longer here.
	store := gpu.NewModuleStore(gpu.ModuleStoreConfig{})

	// The selected tier reaches the agent's own join here and the producer's
	// environment below, from ONE variable. Two copies that could disagree
	// about which tier ran is how a profile ends up disclosing one thing and
	// doing another.
	timeline := gpu.NewTimeline(gpu.TimelineConfig{PCSampling: tier, Modules: store})
	// Without a symbolizer the sampled launch stacks still arrive and are
	// still accounted for, but every one of them degrades to no stack — the
	// profile would then be honest and useless, all GPU time unattributed.
	// The maps index the symbolizer falls back on for addresses blazesym
	// cannot name. It is consulted DURING the run, while the workload is
	// alive; by the time this tool builds the profile the workload has
	// exited and /proc/<pid>/maps is gone, so a lookup at build time would
	// find nothing. Without this, every frame inside a stripped vendor
	// library (libcuda, libcupti - NVIDIA ships no symbols for their
	// internals) renders as a bare ASLR'd address.
	modules := procmap.NewResolver()
	defer modules.Close()
	sym, err := symbolize.NewLocalSymbolizer(symbolize.WithModuleIndex(modules))
	if err != nil {
		log.Fatalf("symbolizer: %v", err)
	}
	defer func() { _ = sym.Close() }()

	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: shimPath,
		// PID 0: the process that will map the adapter has not been started
		// yet, so the attachment has to be system-wide. The semaphore the
		// uprobe refcount maintains is what arms the probes in it once the
		// driver maps the .so.
		//
		// Attach also binds the startup rendezvous before it creates the
		// uprobe link, which is what keeps the workload's first sampled
		// launches from being walked before their CFI tables exist: the
		// adapter blocks in InitializeInjection - during cuInit, after
		// libcuda is mapped and before any kernel can be launched - until
		// this consumer has installed them. See gpuprobe/enroll.go. Nothing
		// is needed here for that, and nothing here may set
		// PERFAGENT_GPU_ENROLL_TIMEOUT_MS to 0, which turns it off.
		PID:        0,
		Backend:    gpu.BackendCUPTI,
		Sink:       timeline,
		Symbolizer: sym,
		// Where the cubins land. Without this the bytes cross the socket,
		// are sealed, verified and stored where nothing reads them, and
		// every PC sample in this profile says gpu_src_status="no-module".
		Modules: store,
	})
	if err != nil {
		log.Fatalf("attach: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := c.Run(ctx); err != nil {
			log.Printf("consumer: %v", err)
		}
	}()

	// The sampler jitters each gap around the period so it cannot lock phase
	// against the workload's alternating axpy/scale pair (issue #50), but the
	// schedule is still a deterministic chain from (seed, period): replaying
	// it gives the EXACT sampled count, not an estimate. The workload
	// launches exactly two kernels per iteration and the adapter samples on
	// every launch, attached or not, so this number is what the consumer must
	// see before the workload may be released.
	if *iters <= 0 || *period <= 0 {
		log.Fatalf("iters and period must both be positive, got iters=%d period=%d", *iters, *period)
	}
	launches := *iters * 2
	wantSampled := int(gpuabi.SampledCount(uint64(launches), uint32(*period), gpuabi.DefaultSampleSeed)) //nolint:gosec // both bounds-checked positive above

	cmd := exec.Command(*workload,
		fmt.Sprint(*iters), fmt.Sprint(*sleepUs), fmt.Sprint(*linger))
	cmd.Env = append(os.Environ(),
		"CUDA_INJECTION64_PATH="+shimPath,
		fmt.Sprintf("PERFAGENT_GPU_SAMPLE_PERIOD=%d", *period),
		"PERFAGENT_GPU_LOG=stderr",
		// Set EXPLICITLY on every run including an off one, never left to be
		// inherited. os.Environ() may already carry this variable from the
		// operator's shell; appending the resolved value last is what keeps a
		// stale export from turning a run this agent believes is off into a
		// producer that serializes the workload's kernels.
		gpu.PCSamplingEnvVar+"="+tier.EnvValue(),
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Same release protocol as the stub: the workload's CPU stacks are
	// symbolized against /proc/<pid>/maps, which the kernel destroys the
	// instant it exits, so we hold its stdin open until the consumer has
	// counted what it needs.
	release, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("workload stdin: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("workload: %v", err)
	}

	deadline := time.Now().Add(time.Duration(*linger) * time.Millisecond)
	for c.Stats().SampledLaunches < uint64(wantSampled) {
		if time.Now().After(deadline) {
			log.Printf("WARNING: only %d/%d sampled launches observed before the workload was released; "+
				"stacks that arrive after it exits cannot be symbolized",
				c.Stats().SampledLaunches, wantSampled)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := release.Close(); err != nil {
		log.Fatalf("release workload: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		log.Fatalf("workload: %v", err)
	}
	// The adapter's atexit handler runs cuptiActivityFlushAll and flushes
	// both batches before the process leaves, so everything is in the ringbuf
	// by now. This sleep is for the consumer goroutine to drain the tail of
	// batched launches and executions — none of which carries a stack, so
	// none of it needs the workload alive.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done
	// Release any launch still held for a sampled twin before the snapshot.
	c.Flush()

	snap := timeline.Snapshot()
	// ProjectExecutionsWith rather than ProjectExecutions so the projection's
	// own losses reach the operator: gpu_pc labels dropped at the cardinality
	// ceiling are invisible in the profile itself, and JoinHealthWith below is
	// the only place they are reported.
	samples, projStats := gpu.ProjectExecutionsWith(snap, gpu.ProjectionConfig{Modules: store})
	if len(samples) == 0 {
		log.Fatal("no samples projected; the pipeline produced nothing")
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 1})
	for i := range samples {
		builders.AddSample(&samples[i])
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	for _, b := range builders.Builders {
		if _, err := b.Write(f); err != nil {
			log.Fatalf("write profile: %v", err)
		}
		break
	}
	if err := f.Close(); err != nil {
		log.Fatalf("close %s: %v", *out, err)
	}
	st := c.Stats()
	log.Printf("wrote %s: %d samples, launches=%d expected_sampled=%d stats=%+v",
		*out, len(samples), launches, wantSampled, st)
	// c.Stats() above is ingestion: what arrived off the ringbuf. This is
	// attribution: what the timeline could join it to, and what it evicted
	// trying. A run can be perfect on the first and quietly useless on the
	// second, so both are printed - one line when the join is clean, one
	// extra line per anomaly when it is not (see gpu.JoinHealthWith).
	for _, line := range gpu.JoinHealthWith(snap, projStats) {
		log.Print(line)
	}
	// And the store's own account, which is neither of the above: what
	// arrived, what it could read, and what it evicted trying. A run where
	// every sample says "no-module" is a different problem depending on
	// whether this line reads modules_stored=0 (nothing arrived - look at the
	// Cubins* counters above) or modules_stored>0 with resolve_no_module high
	// (the CRCs the PC records join on are not the CRCs the cubins arrived
	// under, which is hardware assertion 13).
	log.Printf("module store: %+v", store.Stats())
}
