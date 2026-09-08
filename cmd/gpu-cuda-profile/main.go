// Attaches to the NVIDIA CUPTI adapter and writes gpu-cuda.pb.gz through the
// existing pprof builder. This is cmd/gpu-stub-profile with the synthetic
// producer replaced by an actual GPU.
//
// The adapter is never executed. It is a shared object the CUDA driver loads
// into a process through CUDA_INJECTION64_PATH, so this command attaches its
// uprobes to the .so by path and the probes arm themselves in whichever
// processes map it.
//
// Two shapes, and the second is the one real deployments need:
//
//   - Launch (the default). This command starts the workload itself, with
//     CUDA_INJECTION64_PATH and the sampling configuration in its
//     environment, and attaches system-wide beforehand because the process
//     it will be mapped into does not exist yet. It knows exactly how many
//     sampled launches to expect, and it holds the workload's stdin open
//     until it has them so the stacks can still be symbolized.
//
//   - Attach (-pid). The process is already running and was started by
//     somebody else -- a kubelet, a job runner, an operator -- with the
//     injection variable already in its environment. Nothing here can
//     configure it, so every flag that would have is refused rather than
//     ignored; the run is bounded by -duration instead of by an expected
//     count; and it ends the moment the target exits, because a stack
//     outlives the /proc maps it is symbolized against by nothing at all.
//
// Attach mode is what a sidecar and a node collector both need (issue #124):
// neither can launch what it profiles. It is also strictly weaker today in
// one respect, reported at the end of every attached run -- the adapter
// captures module bytes only while a consumer is present, so modules loaded
// before the attach are missing and their PC samples carry no source line.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/symbolize/nvsym"

	// Registering the interpreter unwinders is a BINARY's decision, not a
	// library's: which languages this build can walk is a property of what was
	// linked, and a library that registered them behind the caller's back would
	// make that invisible. unwind/interp/modules is the single place that names
	// any of them; see unwind/interp for where a new one goes.
	_ "github.com/dpsoft/perf-agent/unwind/interp/modules"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// options is every flag this command has, in one place, so that
// launchOnlyInAttachMode below can be checked for TOTALITY rather than
// trusted. A flag added here and not classified there would be silently
// ignored in attach mode, which is the exact failure the classification
// exists to prevent; a test enumerates the registered set and fails until
// every name is on one side or the other.
type options struct {
	shim        *string
	workload    *string
	iters       *int
	sleepUs     *int
	period      *int
	linger      *int
	out         *string
	pid         *int
	duration    *time.Duration
	waitForShim *time.Duration
	nvSymbols   *string
	pcSampling  *string
	pcAck       *bool
}

func defineFlags(fs *flag.FlagSet) *options {
	return &options{
		shim:     fs.String("shim", "./shim/libperfagent-gpu-nvidia.so", "the CUPTI adapter .so carrying the perfagent USDT probes"),
		workload: fs.String("workload", "./shim/nvidia/testdata/cuda_workload", "CUDA program to run under the adapter"),
		iters:    fs.Int("iters", 2000, "workload iterations; it launches two kernels per iteration"),
		sleepUs:  fs.Int("sleep-us", 200, "workload sleep between iterations, in microseconds"),
		period:   fs.Int("period", 8, "one-in-N launch sampling period (PERFAGENT_GPU_SAMPLE_PERIOD)"),
		linger:   fs.Int("linger-ms", 30000, "how long the workload may wait to be released after it finishes"),
		out:      fs.String("out", "gpu-cuda.pb.gz", "output pprof profile"),

		// Attach mode. Non-zero switches this command from "launch a workload
		// under the adapter" to "profile a process that is already running",
		// which is the shape a sidecar or a node collector has to have: the
		// kubelet started the workload, and nothing we do can have been in its
		// environment.
		pid: fs.Int("pid", 0,
			"profile this already-running process instead of launching a workload. It must "+
				"have been started with CUDA_INJECTION64_PATH pointing at -shim; injection "+
				"happens during cuInit and cannot be added to a live process"),
		duration: fs.Duration("duration", 30*time.Second,
			"how long to profile in -pid mode. The run also ends early on SIGINT or when the "+
				"target exits"),
		waitForShim: fs.Duration("wait-for-shim", 5*time.Second,
			"in -pid mode, how long to wait for the shim to appear in the target's mappings "+
				"before giving up. Non-zero because a process attached to moments after it "+
				"started may not have reached cuInit yet"),
		nvSymbols: fs.String("nvidia-symbols", "",
			"cache directory for NVIDIA's CUDA Toolkit Symbol Server; enables fetching "+
				"symbols for libcuda/libcupti/libcuBLAS, which ship stripped. Off unless set: "+
				"it reaches the network and discloses the build-ids of the CUDA libraries the "+
				"target has mapped."),

		// One setting, three values, and no way to ask for two. The default
		// is the empty string rather than "off" so that an unspecified flag
		// DEFERS to an inherited PERFAGENT_GPU_PC_SAMPLING instead of
		// contradicting it — an explicit --gpu-pc-sampling=off against an
		// exported "serialized" is a disagreement and is refused, but not
		// setting the flag at all is not.
		pcSampling: fs.String("gpu-pc-sampling", "",
			"GPU PC-sampling tier: "+strings.Join(gpu.PCSamplingTierNames, " | ")+
				" (default off; also read from "+gpu.PCSamplingEnvVar+"). "+
				"\"continuous\" does not serialize kernels; \"serialized\" does, and requires "+
				"-gpu-pc-sampling-acknowledge-perturbation"),
		pcAck: fs.Bool("gpu-pc-sampling-acknowledge-perturbation", false,
			"acknowledge that the \"serialized\" tier perturbs the workload: it inflates GPU "+
				"kernel durations inside a burst, it distorts any CPU and off-CPU profile taken "+
				"alongside it with no marking in those profiles at all, and it is unavailable "+
				"where CUDA graphs are in use"),
	}
}

// launchOnlyInAttachMode names every flag that configures the process this
// command STARTS, mapped to why it cannot mean anything when it does not
// start one.
//
// These are refused in attach mode rather than ignored, and the difference
// matters more than it looks. The whole of the producer's configuration --
// its sampling period, its PC-sampling tier -- travels in the target's
// environment and is fixed at cuInit. A -period accepted here would change
// nothing about the target and would then be read off the command line
// afterwards as though it had: the profile would be interpreted at a
// sampling rate it was not taken at, with nothing anywhere saying otherwise.
// A refusal costs one restart; a silently-ignored flag costs a wrong
// conclusion.
//
// Totality is enforced by a test rather than by care: every flag defineFlags
// registers must appear here or in attachSafeFlags, so a flag added later
// cannot default into being silently ignored.
var launchOnlyInAttachMode = map[string]string{

	"workload":  "attach mode profiles a process that is already running",
	"iters":     "the target's iteration count is its own",
	"sleep-us":  "the target's pacing is its own",
	"linger-ms": "nothing is being released; -duration bounds an attached run",
	"period": "the launch sampling period is read by the ADAPTER from " +
		"PERFAGENT_GPU_SAMPLE_PERIOD in the target's environment, which was fixed " +
		"when the target started",
	"gpu-pc-sampling": "the PC-sampling tier is the producer's, selected from " +
		gpu.PCSamplingEnvVar + " in the target's environment at cuInit",
	"gpu-pc-sampling-acknowledge-perturbation": "there is no tier for this run to " +
		"acknowledge; see -gpu-pc-sampling",
}

// attachSafeFlags are the flags that mean the same thing in both modes: they
// configure THIS process -- where the shim is, where the profile goes, how
// long to run, how symbols are resolved -- rather than the profiled one.
var attachSafeFlags = map[string]bool{
	"shim":           true,
	"out":            true,
	"nvidia-symbols": true,
	"pid":            true,
	"duration":       true,
	"wait-for-shim":  true,
}

// refusedLaunchFlags reports the launch-only flags the operator actually set,
// each with its reason. Only flags that were SET are refused: a default that
// happens to sit in the table is not a request.
func refusedLaunchFlags(fs *flag.FlagSet) []string {
	var refused []string
	fs.Visit(func(f *flag.Flag) {
		if why, ok := launchOnlyInAttachMode[f.Name]; ok {
			refused = append(refused, fmt.Sprintf("-%s: %s", f.Name, why))
		}
	})
	sort.Strings(refused)
	return refused
}

func main() {
	opt := defineFlags(flag.CommandLine)
	flag.Parse()

	// Which of the two shapes this run has, and the refusal of every flag
	// that belongs to the other one.
	//
	// Attach mode exists because the deployments that matter cannot launch
	// what they profile. A sidecar profiles a container the kubelet started;
	// a node collector profiles processes that were running before it was
	// scheduled. Neither can put anything in the target's environment, which
	// is where the whole of the producer's configuration lives.
	//
	// That is the reason the launch-only flags are REFUSED here rather than
	// ignored. -period sets PERFAGENT_GPU_SAMPLE_PERIOD in the child this
	// command starts; in attach mode there is no child, the target's period
	// is whatever it was started with, and a -period that silently did
	// nothing would be read off the command line afterwards as though it had
	// applied. The profile would then be interpreted at a sampling rate it
	// was not taken at. Every flag below has that shape: it configures a
	// process this mode does not create.
	attach := *opt.pid != 0
	if attach {
		if refused := refusedLaunchFlags(flag.CommandLine); len(refused) > 0 {
			log.Fatalf("-pid was given, so these flags cannot take effect and are refused "+
				"rather than ignored:\n  %s", strings.Join(refused, "\n  "))
		}
		if *opt.duration <= 0 {
			log.Fatalf("-duration must be positive in -pid mode, got %s", *opt.duration)
		}
	}

	// Tier selection, and it happens BEFORE anything is attached or launched.
	// Every refusal here is a startup error: an unknown value, a value naming
	// two tiers, the flag and the environment naming two tiers, or Tier A
	// without its acknowledgement. None of them is resolved to a tier — a
	// profile produced under a tier nobody chose is worse than no profile,
	// because nothing in it says which one ran.
	//
	// In attach mode there is no tier to select. The tier is the PRODUCER's:
	// the adapter reads it out of the target's environment during cuInit and
	// configures CUPTI accordingly, long before this command existed. Reading
	// our own environment here would let a stale export in the operator's
	// shell decide what this consumer believes the target is doing, and the
	// tier gates the ANSWER — whether an execution may be claimed
	// gpu_serialized="true" (gpu/timeline.go). A consumer that guessed the
	// producer's tier from its own shell would be making that claim on
	// nothing.
	//
	// So attach mode takes the zero value, which is the branch that claims
	// nothing: PC samples still arrive and are still attributed through the
	// module, and executions are marked "unknown" rather than "false". That
	// is correct-but-weaker for a target running Tier A, which is the right
	// direction to be wrong in. Recovering the producer's real tier means
	// replaying gpu_config_v1 on attach, which is the same missing machinery
	// that costs this mode its module bytes; see the report at the end of a
	// run.
	var tier gpu.PCSamplingTier
	if !attach {
		var terr error
		tier, terr = gpu.PCSamplingRequest{
			Flag:                    *opt.pcSampling,
			Env:                     os.Getenv(gpu.PCSamplingEnvVar),
			AcknowledgePerturbation: *opt.pcAck,
		}.Select()
		if terr != nil {
			log.Fatalf("gpu pc sampling: %v", terr)
		}
	}
	// Printed at startup as well as standing in every JoinHealth render
	// below. The startup copy is for the operator who is watching the run
	// begin; the standing copy is for the one who reads the profile an hour
	// later, which is the reader the warning is actually for.
	for _, line := range gpu.PCSamplingStandingWarning(tier) {
		log.Print(line)
	}

	shimPath, err := filepath.Abs(*opt.shim)
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

	// Attach mode's one hard precondition, and it is checked FIRST -- before
	// the module store, the symbolizer, the BPF objects, or anything that
	// needs a capability.
	//
	// A wrong -pid is the most likely thing an operator gets wrong, and it
	// must not surface as a complaint about CAP_CHECKPOINT_RESTORE from
	// setup that a correct -pid would have needed anyway. The cheapest and
	// most decisive question goes first.
	//
	// The shim reaches a process exactly once, when the driver dlopens it
	// during cuInit. There is no way to inject it into a process that is past
	// that point: not with -pid, not with ptrace, not by any flag this
	// command could grow. So a target that does not map the shim now will
	// never map it, and every probe this run attaches would sit on a file
	// that process does not have.
	//
	// Refused rather than warned about, because the alternative is a full
	// -duration of waiting followed by an empty profile — which is precisely
	// the fails-open-and-silent failure the injection check exists to end.
	// The launch path can only diagnose this after the fact; here it is
	// knowable up front, so it is an error at startup.
	//
	// The wait is for one case only: a process attached to in the seconds
	// after it started, which has not reached cuInit yet. It is not a retry
	// loop for a target that will never load the shim.
	if attach {
		if err := waitForShimIn(*opt.pid, shimPath, *opt.waitForShim); err != nil {
			log.Fatalf("attach to pid %d: %v", *opt.pid, err)
		}
		log.Printf("pid %d maps %s; attaching", *opt.pid, shimPath)
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
	symOpts := []symbolize.LocalOption{symbolize.WithModuleIndex(modules)}
	if *opt.nvSymbols != "" {
		// Last resort only, after the library's own file has failed. NVIDIA
		// exports 0.16%-3.2% of .text in these libraries, so almost every
		// address in them is unnameable locally; the server has a
		// symbols-only ELF per build-id that named 13 of 13 such frames in a
		// real capture.
		symOpts = append(symOpts, symbolize.WithNVIDIASymbols(&nvsym.Store{Dir: *opt.nvSymbols}))
		log.Printf("nvidia symbols: enabled, cache %s", *opt.nvSymbols)
	}
	sym, err := symbolize.NewLocalSymbolizer(symOpts...)
	if err != nil {
		log.Fatalf("symbolizer: %v", err)
	}
	defer func() { _ = sym.Close() }()

	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: shimPath,
		// Launch mode passes 0 and attach mode passes the target.
		//
		// Zero is system-wide, and it is what the launch path needs: the
		// process that will map the adapter has not been started yet, so
		// there is nothing to filter on. The semaphore the uprobe refcount
		// maintains is what arms the probes in it once the driver maps the
		// .so.
		//
		// Attach also binds the startup rendezvous before it creates the
		// uprobe link, which is what keeps the workload's first sampled
		// launches from being walked before their CFI tables exist: the
		// adapter blocks in InitializeInjection - during cuInit, after
		// libcuda is mapped and before any kernel can be launched - until
		// this consumer has installed them. See gpuprobe/enroll.go. Nothing
		// is needed here for that, and nothing here may set
		// PERFAGENT_GPU_ENROLL_TIMEOUT_MS to 0, which turns it off.
		//
		// A non-zero PID does two things, and the second is the one that
		// makes late attach viable at all. It narrows the uprobe_multi link
		// to that process, and it takes the EAGER registration path in
		// gpuprobe: Attach compiles that PID's CFI tables synchronously,
		// before the link exists and therefore before any probe can fire.
		// The rendezvous cannot help a process that is already running -- it
		// passed cuInit before this command was started -- so without the
		// eager path a late attach would walk its first stacks with no
		// tables, which is exactly the ~38% loss issue #49 measured. Here
		// the window is not merely narrowed but absent: the tables are in
		// before the first probe exists.
		PID:        *opt.pid,
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

	// The two shapes diverge here and nowhere else. Everything above --
	// the store, the timeline, the symbolizer, the attach -- is identical,
	// and everything below (snapshot, projection, profile, health) is too.
	// What differs is only how the run is bounded: launch mode knows exactly
	// how many sampled launches to expect and waits for them; attach mode
	// cannot know, because it did not start the workload and has no idea what
	// it is doing.
	var launches, wantSampled int
	if attach {
		profileAttached(c, *opt.pid, *opt.duration)
	} else {
		// The sampler jitters each gap around the period so it cannot lock phase
		// against the workload's alternating axpy/scale pair (issue #50), but the
		// schedule is still a deterministic chain from (seed, period): replaying
		// it gives the EXACT sampled count, not an estimate. The workload
		// launches exactly two kernels per iteration and the adapter samples on
		// every launch, attached or not, so this number is what the consumer must
		// see before the workload may be released.
		if *opt.iters <= 0 || *opt.period <= 0 {
			log.Fatalf("iters and period must both be positive, got iters=%d period=%d", *opt.iters, *opt.period)
		}
		launches = *opt.iters * 2
		wantSampled = int(gpuabi.SampledCount(uint64(launches), uint32(*opt.period), gpuabi.DefaultSampleSeed)) //nolint:gosec // both bounds-checked positive above

		cmd := exec.Command(*opt.workload,
			fmt.Sprint(*opt.iters), fmt.Sprint(*opt.sleepUs), fmt.Sprint(*opt.linger))
		cmd.Env = append(os.Environ(),
			"CUDA_INJECTION64_PATH="+shimPath,
			fmt.Sprintf("PERFAGENT_GPU_SAMPLE_PERIOD=%d", *opt.period),
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

		// Did the injection actually happen?
		//
		// CUDA_INJECTION64_PATH fails OPEN and SILENT: a driver that cannot load
		// the library carries on as though the variable were unset, so a broken
		// shim and a workload that launched no kernels produce identical output --
		// an empty profile and no error. The mapping is the one observable that
		// separates them. Watched here, while the workload is alive, because
		// /proc/<pid>/maps is gone the moment it is not.
		injected := waitForInjection(cmd.Process.Pid, shimPath, 10*time.Second)

		deadline := time.Now().Add(time.Duration(*opt.linger) * time.Millisecond)
		for c.Stats().SampledLaunches < uint64(wantSampled) {
			if time.Now().After(deadline) {
				log.Printf("WARNING: only %d/%d sampled launches observed before the workload was released; "+
					"stacks that arrive after it exits cannot be symbolized",
					c.Stats().SampledLaunches, wantSampled)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		reportInjection(injected, shimPath, cmd.Process.Pid, c.Stats().SampledLaunches)
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
	}
	// The adapter's atexit handler runs cuptiActivityFlushAll and flushes
	// both batches before the process leaves, so everything is in the ringbuf
	// by now in launch mode. In attach mode the target is still running and
	// its 100 ms drain timer is still firing, so this is the tail of the last
	// tick rather than of an exit -- either way it is batched launches and
	// executions, none of which carries a stack, so none of it needs a live
	// process.
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
		// In launch mode an empty pipeline is a bug: this command started a
		// workload it knows launches kernels. In attach mode it is most
		// often a fact about the target -- a process that was idle for the
		// whole window launches nothing, and there is no defect to report.
		if attach {
			log.Fatalf("no samples projected: pid %d launched no kernels in %s. The shim is "+
				"mapped (checked at startup), so the probes were live; the target was "+
				"idle, or its CUDA work happens in a different process. stats=%+v",
				*opt.pid, *opt.duration, c.Stats())
		}
		log.Fatal("no samples projected; the pipeline produced nothing")
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 1})
	for i := range samples {
		builders.AddSample(&samples[i])
	}

	f, err := os.Create(*opt.out)
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
		log.Fatalf("close %s: %v", *opt.out, err)
	}
	st := c.Stats()
	// launches/expected_sampled are LAUNCH-MODE facts and are printed only
	// there. They come from replaying the sampler's deterministic schedule
	// over a workload this command started with a known iteration count, and
	// none of those three things is true in attach mode: printing
	// expected_sampled=0 beside a healthy attached run would read as a
	// shortfall rather than as an inapplicable number.
	if attach {
		log.Printf("wrote %s: %d samples from pid %d, stats=%+v", *opt.out, len(samples), *opt.pid, st)
	} else {
		log.Printf("wrote %s: %d samples, launches=%d expected_sampled=%d stats=%+v",
			*opt.out, len(samples), launches, wantSampled, st)
	}
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
	reportLateAttachModuleGap(attach, store.Stats().ModulesStored, st.CubinsReceived)
}

// reportLateAttachModuleGap names the one way an attached profile is
// systematically weaker than a launched one, so it is not mistaken for a
// broken cubin transport.
//
// The adapter captures a module's bytes only while a consumer is already
// present -- capture_enabled() in shim/nvidia/cupti_adapter.cc is
// g_consumer_enrolled || gpu_module_load_v1_enabled(), and in a late attach
// neither holds at the moment the modules load. A CUDA process loads
// essentially all of its modules during startup, so attaching afterwards
// misses essentially all of them, and every PC sample then resolves
// gpu_src_status="no-module".
//
// This is a real gap and not a misconfiguration, which is exactly why it is
// worth a line: the operator's next move is to read the issue, not to go
// looking for a socket that is working perfectly. shim/core/drain.h already
// carries a complete ReplayLog for this transition; nothing calls it yet.
func reportLateAttachModuleGap(attach bool, modulesStored, cubinsReceived uint64) {
	if !attach || modulesStored > 0 || cubinsReceived > 0 {
		return
	}
	log.Printf("no modules were captured, which is expected for a late attach and is NOT a "+
		"transport failure: the adapter only captures a module's bytes while a consumer is "+
		"already attached, and this target loaded its modules before we arrived. Every PC "+
		"sample in %s therefore reads gpu_src_status=\"no-module\" and carries no source "+
		"line. Launch-mode runs do not have this gap. Tracking: issue #124.",
		"this profile")
}

// waitForInjection watches for the shim appearing in the workload's mappings.
//
// Bounded, and a miss is not an error: the driver loads the library during
// cuInit, which a workload may not reach for some time, and a workload started
// through a wrapper script does its CUDA work in a CHILD whose mappings this
// does not inspect. The answer feeds a diagnostic, never a failure.
func waitForInjection(pid int, shimPath string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok, err := gpuprobe.ShimIsMappedIn(pid, shimPath); err == nil && ok {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// reportInjection turns "an empty profile" into a statement about which of the
// two possible causes it was.
//
// Says nothing when there is nothing to say: a run that sampled launches
// plainly injected, whatever the mapping check saw, and a line confirming the
// obvious on every successful run is noise that trains people to skip the
// output.
func reportInjection(mapped bool, shimPath string, pid int, sampled uint64) {
	if sampled > 0 {
		return
	}
	if mapped {
		log.Printf("no launches were sampled, but the shim IS mapped into the workload: " +
			"injection worked and the adapter did not report launches. Look at the " +
			"perfagent-cupti lines above for why -- a missing CUPTI is reported there.")
		return
	}
	log.Printf("no launches were sampled and %s is NOT mapped into pid %d. "+
		"CUDA_INJECTION64_PATH fails open and silent, so this is exactly what a shim the "+
		"driver could not load looks like: check that the workload is a CUDA program that "+
		"reached cuInit, and that the shim loads in ITS environment "+
		"(`make -C shim nvidia-portable` builds one that does). If the workload is a wrapper "+
		"script the CUDA process is a child, and this check does not see it.",
		shimPath, pid)
}

// waitForShimIn is attach mode's precondition, and it answers a question the
// launch path cannot ask: is this process one the probes can ever fire in?
//
// The launch path's waitForInjection above is a diagnostic — it watches a
// process it started, after the fact, and a miss there is ambiguous because
// the workload may simply not have reached cuInit. Here the answer is
// decisive in one direction: the driver dlopens the shim during cuInit and
// never again, so a process past that point either maps it or never will.
// There is no operation — no flag, no ptrace, no second attach — that adds
// the shim to a running process.
//
// The wait therefore covers exactly one legitimate case: a target attached to
// in the seconds after it started, whose cuInit has not happened yet. It is
// bounded and then it is an error, because the alternative is to attach to a
// process nothing can be observed in and say so only after a full -duration.
func waitForShimIn(pid int, shimPath string, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last error
	for {
		mapped, err := gpuprobe.ShimIsMappedIn(pid, shimPath)
		switch {
		case err == nil && mapped:
			return nil
		case err != nil:
			// Distinguish "cannot look" from "looked and it is not there".
			// A pid that does not exist, or one this process may not read,
			// is a different fix from a target that never loaded the shim,
			// and reporting the second for the first sends the reader to
			// the driver instead of to their own command line.
			last = err
		}
		if !time.Now().Before(deadline) {
			if last != nil {
				return fmt.Errorf("could not read the target's mappings: %w", last)
			}
			return fmt.Errorf(
				"%s is not mapped into pid %d after %s. The CUDA driver loads the shim "+
					"during cuInit, from CUDA_INJECTION64_PATH in the process's own "+
					"environment, and nothing can add it afterwards -- so this process "+
					"cannot be profiled by this shim. Check that the target was started "+
					"with CUDA_INJECTION64_PATH=%s (cat /proc/%d/environ | tr '\\0' '\\n' "+
					"| grep CUDA_INJECTION64_PATH), that the file is the same INODE the "+
					"target was given (a rebuilt or copied shim is a different file to a "+
					"uprobe), and that it loads in the target's environment "+
					"(`make -C shim nvidia-portable` builds one that does). If the target "+
					"is a wrapper script, the CUDA process is a child of it and has a "+
					"different pid.",
				shimPath, pid, within, shimPath, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// profileAttached bounds a run this command did not start.
//
// Three things can end it, and the third is the one that matters for
// correctness rather than convenience. The duration is the operator's budget.
// SIGINT is the operator changing their mind, and it must produce the profile
// collected so far rather than discarding it -- a collector killed at the end
// of a scrape window that wrote nothing would be worse than useless.
//
// The target exiting ends the run IMMEDIATELY, and not as a courtesy: a
// sampled launch's stack is symbolized against /proc/<pid>/maps, which the
// kernel destroys the instant the process leaves. Every second spent
// collecting after that point produces records whose stacks can no longer be
// resolved. Launch mode solves this by holding the workload's stdin open;
// attach mode has no such handle on a process it did not create, so the best
// it can do is notice and stop.
//
// A pidfd rather than polling /proc/<pid>: the pid is not ours, so it can be
// reaped and reused by an unrelated process while we watch. A pidfd is bound
// to the process, not to the number, and it becomes readable exactly once,
// when that process dies.
func profileAttached(c *gpuprobe.Consumer, pid int, d time.Duration) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	gone := make(chan struct{})
	if fd, err := unix.PidfdOpen(pid, 0); err == nil {
		go func() {
			defer func() { _ = unix.Close(fd) }()
			defer close(gone)
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // a pidfd is well inside int32
			for {
				n, err := unix.Poll(fds, -1)
				// Poll is interruptible by any signal the runtime delivers,
				// and the Go runtime delivers plenty. EINTR here means
				// "nothing happened yet", not "the process is gone", and
				// treating it as the latter would end every attached run at
				// the first GC-related signal.
				if errors.Is(err, unix.EINTR) {
					continue
				}
				if n > 0 || err != nil {
					return
				}
			}
		}()
	} else {
		// No pidfd (pre-5.3, or the process left between the mapping check
		// and here). The run still works; it just cannot shorten itself when
		// the target exits, so stacks arriving after that point fail to
		// symbolize and are counted as such.
		log.Printf("cannot watch pid %d for exit (%v); the run will not end early if it exits, "+
			"and stacks captured after that point cannot be symbolized", pid, err)
	}

	log.Printf("profiling pid %d for %s (ctrl-c to stop early)", pid, d)
	// A progress line, because the alternative is an operator watching an
	// idle terminal for the length of the budget with no way to tell a
	// working attach from a dead one. Coarse on purpose: it is a sign of
	// life, not a metric.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	deadline := time.After(d)
	for {
		select {
		case <-deadline:
			log.Printf("-duration elapsed")
			return
		case s := <-sig:
			log.Printf("%s: stopping early and writing what was collected", s)
			return
		case <-gone:
			st := c.Stats()
			log.Printf("pid %d exited after %d sampled launches; stopping now, because its "+
				"/proc maps are gone and any stack arriving from here on cannot be "+
				"symbolized", pid, st.SampledLaunches)
			return
		case <-ticker.C:
			st := c.Stats()
			log.Printf("... sampled_launches=%d batches=%d", st.SampledLaunches, st.Batches)
		}
	}
}
