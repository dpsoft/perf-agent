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
	"regexp"
	"sort"
	"strings"
	"sync"
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
	shim            *string
	workload        *string
	iters           *int
	sleepUs         *int
	period          *int
	linger          *int
	out             *string
	pid             *int
	discover        *bool
	rediscoverEvery *time.Duration
	duration        *time.Duration
	drainEvery      *time.Duration
	waitForShim     *time.Duration
	nvSymbols       *string
	pcSampling      *string
	pcAck           *bool
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
		discover: fs.Bool("discover", false,
			"profile EVERY process that maps -shim, found by scanning /proc, instead of "+
				"being handed one with -pid. This is the shape a sidecar and a node "+
				"collector need: neither is the application's parent and neither is told "+
				"its pid"),
		rediscoverEvery: fs.Duration("rediscover-every", 5*time.Second,
			"in -discover mode, how often to rescan for targets that appeared or left. "+
				"Processes that start normally enrol themselves during cuInit and do not "+
				"need this; the rescan is for ones already past that point, and for "+
				"noticing when every target has gone"),
		duration: fs.Duration("duration", 30*time.Second,
			"how long to profile in -pid mode. The run also ends early on SIGINT or when the "+
				"target exits"),
		drainEvery: fs.Duration("drain-every", 2*time.Second,
			"in -pid mode, how often to drain the timeline into the profile. The "+
				"timeline's rings hold ONE interval, not the whole run: a long attach to a "+
				"busy process overruns them and loses GPU time outright. Larger intervals "+
				"cost memory; smaller ones cost a little CPU"),
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
	"shim":             true,
	"out":              true,
	"nvidia-symbols":   true,
	"pid":              true,
	"duration":         true,
	"wait-for-shim":    true,
	"drain-every":      true,
	"discover":         true,
	"rediscover-every": true,
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

// anomalyDigits collapses a health line to its KIND by removing the numbers
// in it, so "evicted 10198 launches" and "evicted 20824 launches" are
// recognised as one ongoing condition rather than two events.
var anomalyDigits = regexp.MustCompile(`[0-9]+`)

// describeTargets names what a run covered, in the singular or the plural as
// the run actually was. "pid 1234" and "3 pids [12 34 56]" are different
// facts, and a message that said "pid 12" for a three-process collector would
// misreport the scope of everything else on the line.
func describeTargets(targets []int) string {
	if len(targets) == 1 {
		return fmt.Sprintf("pid %d", targets[0])
	}
	return fmt.Sprintf("%d pids %v", len(targets), targets)
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
	attach := *opt.pid != 0 || *opt.discover
	if *opt.pid != 0 && *opt.discover {
		log.Fatalf("-pid and -discover both name what to profile and disagree about how: " +
			"-pid is one process you already know, -discover is every process that maps " +
			"the shim. Pick one")
	}
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
	var targets []int
	switch {
	case *opt.discover:
		var derr error
		targets, derr = waitForTargets(shimPath, *opt.waitForShim)
		if derr != nil {
			log.Fatalf("discover targets: %v", derr)
		}
		log.Printf("discovered %d process(es) mapping %s: %v", len(targets), shimPath, targets)
	case *opt.pid != 0:
		if err := waitForShimIn(*opt.pid, shimPath, *opt.waitForShim); err != nil {
			log.Fatalf("attach to pid %d: %v", *opt.pid, err)
		}
		targets = []int{*opt.pid}
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
		//
		// Discovery leaves this ZERO on purpose. PID 0 is not a weaker
		// attachment than a specific pid, it is the broader one: the
		// uprobe_multi link then fires in EVERY process mapping the file,
		// which is precisely the set discovery just enumerated -- and it
		// keeps covering processes that map it later, which a pid filter
		// fixed at startup could never do. The narrowing that a pid does buy
		// is not wanted here; what a specific pid ALSO does -- eager CFI
		// registration -- is, and that is what EagerPIDs carries.
		PID: *opt.pid,
		// Every already-running target, whose tables must exist before the
		// link does. None of them can use the startup rendezvous: all of them
		// went past cuInit before this process started.
		EagerPIDs:  targets,
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

	// The profile accumulates ACROSS snapshots, which is what lets an
	// attached run drain the timeline while it is still collecting.
	//
	// Timeline.Snapshot drains: it swaps in fresh rings and leaves anything
	// it could not join eligible for a later call. It was built to be called
	// repeatedly. Launch mode calls it once because it can -- it profiles a
	// bounded workload and then stops -- and that simplification is exactly
	// what breaks under a long attach. Measured on an RTX 3090: a 20 s attach
	// to a workload issuing ~5,500 launches/s filled the 65,536-entry
	// execution ring at about twelve seconds, and from there on every
	// snapshot-less second evicted both executions and the launches waiting
	// to join them -- 45,032 executions and 45,315 launches gone, GPU time
	// missing from the profile entirely. The bound is not the problem;
	// holding a whole run inside it is. Raising it only moves the cliff and
	// makes the agent's memory grow with the length of the run, which for a
	// collector is unbounded by construction.
	//
	// So the ring holds a drain interval rather than a run, and the profile
	// -- not the timeline -- is what accumulates.
	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 1})
	totalSamples := 0
	var lastSnap gpu.Snapshot
	var lastProj gpu.ProjectionStats
	snapshots := 0
	seenAnomaly := map[string]bool{}
	collect := func() {
		snap := timeline.Snapshot()
		// ProjectExecutionsWith rather than ProjectExecutions so the
		// projection's own losses reach the operator: gpu_pc labels dropped
		// at the cardinality ceiling are invisible in the profile itself, and
		// JoinHealthWith is the only place they are reported.
		samples, projStats := gpu.ProjectExecutionsWith(snap, gpu.ProjectionConfig{Modules: store})
		for i := range samples {
			builders.AddSample(&samples[i])
		}
		totalSamples += len(samples)
		snapshots++
		lastSnap, lastProj = snap, projStats
		// Health is per-snapshot, and printing all of it would put one clean
		// line per interval into a collector's log forever. JoinHealthWith
		// returns exactly one summary line when there is nothing wrong, so
		// anything past the first is a warning or an anomaly -- which is
		// precisely what must not wait for the end of a long run to be seen.
		// The final snapshot prints in full below either way.
		//
		// Reported ONCE PER KIND, because not every counter behind these
		// lines is per-snapshot. Snapshot drains the rings but not the launch
		// cache, so LaunchCacheStats is cumulative for the life of the run:
		// its eviction total is re-reported, larger, in every subsequent
		// snapshot. Printed naively that is one ongoing condition wearing the
		// costume of a fresh anomaly every interval -- and an operator who
		// sees the same alarm ten times learns to skip it, which is the exact
		// harm the anomaly exists to prevent. The kind is the line with its
		// numbers removed, so a condition that persists is announced when it
		// starts and then stays quiet; the final health block below carries
		// the totals.
		if lines := gpu.JoinHealthWith(snap, projStats); len(lines) > 1 {
			for _, line := range lines[1:] {
				kind := anomalyDigits.ReplaceAllString(line, "#")
				if seenAnomaly[kind] {
					continue
				}
				seenAnomaly[kind] = true
				log.Printf("snapshot %d: %s", snapshots, line)
			}
		}
	}

	// The two shapes diverge here and nowhere else. Everything above --
	// the store, the timeline, the symbolizer, the attach -- is identical,
	// and everything below (snapshot, projection, profile, health) is too.
	// What differs is only how the run is bounded: launch mode knows exactly
	// how many sampled launches to expect and waits for them; attach mode
	// cannot know, because it did not start the workload and has no idea what
	// it is doing.
	var launches, wantSampled int
	if attach {
		targets = profileAttached(c, targets, attachConfig{
			duration:   *opt.duration,
			drainEvery: *opt.drainEvery,
			rediscover: rediscoverInterval(*opt.discover, *opt.rediscoverEvery),
			shimPath:   shimPath,
		}, collect)
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

	// The last drain interval, and in launch mode the only one.
	collect()
	if totalSamples == 0 {
		// In launch mode an empty pipeline is a bug: this command started a
		// workload it knows launches kernels. In attach mode it is most
		// often a fact about the target -- a process that was idle for the
		// whole window launches nothing, and there is no defect to report.
		if attach {
			log.Fatalf("no samples projected: %v launched no kernels in %s. The shim is "+
				"mapped (checked at startup), so the probes were live; the targets were "+
				"idle, or the CUDA work happens in a process that does not map this shim. "+
				"stats=%+v",
				describeTargets(targets), *opt.duration, c.Stats())
		}
		log.Fatal("no samples projected; the pipeline produced nothing")
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
		log.Printf("wrote %s: %d samples from %v over %d drain intervals, stats=%+v",
			*opt.out, totalSamples, describeTargets(targets), snapshots, st)
	} else {
		log.Printf("wrote %s: %d samples, launches=%d expected_sampled=%d stats=%+v",
			*opt.out, totalSamples, launches, wantSampled, st)
	}
	// c.Stats() above is ingestion: what arrived off the ringbuf. This is
	// attribution: what the timeline could join it to, and what it evicted
	// trying. A run can be perfect on the first and quietly useless on the
	// second, so both are printed - one line when the join is clean, one
	// extra line per anomaly when it is not (see gpu.JoinHealthWith).
	for _, line := range gpu.JoinHealthWith(lastSnap, lastProj) {
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
					"different pid",
				shimPath, pid, within, shimPath, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// attachConfig is how an attached run is bounded and, in discovery mode, kept
// current. Grouped rather than passed as five positional arguments, because
// four of them are durations and a transposition would compile.
type attachConfig struct {
	duration   time.Duration
	drainEvery time.Duration
	// rediscover is zero when there is nothing to rediscover: a -pid run
	// profiles the one process it was given, and rescanning /proc for it would
	// be work in service of an answer that cannot change.
	rediscover time.Duration
	shimPath   string
}

// rediscoverInterval is zero unless discovery is actually in use.
func rediscoverInterval(discover bool, every time.Duration) time.Duration {
	if !discover || every <= 0 {
		return 0
	}
	return every
}

// waitForTargets is discovery's precondition, and it is the same refusal
// waitForShimIn makes, asked of a set rather than of one process.
//
// Bounded and then an error, for the same reason: a collector that attaches to
// nothing and says nothing spends its whole -duration producing an empty
// profile, which is indistinguishable from a workload that launched no
// kernels. The wait covers the one legitimate case -- starting alongside an
// application that has not reached cuInit yet, which is the NORMAL case for a
// sidecar, since both containers start together.
func waitForTargets(shimPath string, within time.Duration) ([]int, error) {
	deadline := time.Now().Add(within)
	for {
		found, err := gpuprobe.ProcessesMappingShim(shimPath)
		if err != nil {
			return nil, err
		}
		if len(found) > 0 {
			return found, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf(
				"no process on this host maps %s after %s. The CUDA driver loads the shim "+
					"during cuInit, from CUDA_INJECTION64_PATH in each process's own "+
					"environment, so a process not started with it pointing at THIS FILE "+
					"can never be a target -- and a copy of the shim at another path is a "+
					"different file to a uprobe, however identical its bytes. Check that "+
					"the application was started with CUDA_INJECTION64_PATH=%s, that it "+
					"has reached cuInit, and that this process can see it (a sidecar needs "+
					"shareProcessNamespace, a node agent needs hostPID)",
				shimPath, within, shimPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// profileAttached bounds a run this command did not start.
//
// Four things can end it, and the last is the one that matters for correctness
// rather than convenience. The duration is the operator's budget. SIGINT is
// the operator changing their mind, and it must produce the profile collected
// so far rather than discarding it -- a collector killed at the end of a
// scrape window that wrote nothing would be worse than useless.
//
// Every target exiting ends the run, and not as a courtesy: a sampled launch's
// stack is symbolized against /proc/<pid>/maps, which the kernel destroys the
// instant the process leaves. Every second spent collecting after the last one
// has gone produces records whose stacks can no longer be resolved. Launch
// mode solves this by holding the workload's stdin open; attach mode has no
// such handle on processes it did not create, so the best it can do is notice.
//
// EVERY target, not any: with several processes under one attachment, one
// exiting is an ordinary event -- pods restart -- and ending the run there
// would throw away the profiling of everything still running.
//
// A pidfd per target rather than polling /proc/<pid>: the pids are not ours,
// so they can be reaped and reused by unrelated processes while we watch. A
// pidfd is bound to the process, not to the number.
func profileAttached(c *gpuprobe.Consumer, targets []int, cfg attachConfig, collect func()) []int {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	live := newLiveSet(targets)
	defer live.close()
	// Every return below reports what was ACTUALLY covered, which under
	// rediscovery is not what the run started with.
	covered := func() []int { return live.all() }

	log.Printf("profiling %d process(es) for %s (ctrl-c to stop early)", len(targets), cfg.duration)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// The drain is what keeps the timeline's rings holding an interval rather
	// than a run. It is deliberately separate from the progress ticker above:
	// one is for the operator watching, the other is load-bearing.
	drain := time.NewTicker(cfg.drainEvery)
	defer drain.Stop()

	// A ticker that never fires when rediscovery is off, so the select below
	// does not need a second shape for the -pid case.
	rescan := make(<-chan time.Time)
	if cfg.rediscover > 0 {
		t := time.NewTicker(cfg.rediscover)
		defer t.Stop()
		rescan = t.C
	}

	deadline := time.After(cfg.duration)
	for {
		select {
		case <-deadline:
			log.Printf("-duration elapsed")
			return covered()
		case s := <-sig:
			log.Printf("%s: stopping early and writing what was collected", s)
			return covered()
		case <-live.allGone:
			st := c.Stats()
			log.Printf("every target has exited after %d sampled launches; stopping now, "+
				"because their /proc maps are gone and any stack arriving from here on "+
				"cannot be symbolized", st.SampledLaunches)
			return covered()
		case <-drain.C:
			// Deliberately NOT c.Flush() here, though the end-of-run path
			// does exactly that. Flush releases every launch being held for a
			// sampled twin that has not arrived yet, and mid-run most of
			// those twins are simply still in flight -- a batch or two away.
			// Flushing on a timer would release them stackless, converting
			// launches that were about to be attributed into
			// [gpu:launch unsampled]. Held launches are not lost by being
			// skipped: they reach the sink when their twin arrives and land
			// in the NEXT snapshot. Only the last one, where nothing more is
			// coming, has to force the issue.
			collect()
		case <-rescan:
			found, err := gpuprobe.ProcessesMappingShim(cfg.shimPath)
			if err != nil {
				// Not fatal: the run in progress is unaffected by a failed
				// rescan, and ending it would throw away a working profile
				// over a transient /proc read.
				log.Printf("rediscovery failed (continuing with the targets already found): %v", err)
				continue
			}
			for _, pid := range found {
				if !live.add(pid) {
					continue
				}
				// New target, and it is here rather than in the rendezvous
				// because the rendezvous already had its chance: a process
				// that could use it never reaches this line.
				n, rerr := c.RegisterTarget(pid)
				log.Printf("new target pid %d (%d binaries registered, err=%v)", pid, n, rerr)
			}
		case <-ticker.C:
			st := c.Stats()
			log.Printf("... targets=%d sampled_launches=%d batches=%d",
				live.count(), st.SampledLaunches, st.Batches)
		}
	}
}

// liveSet watches a set of processes and closes allGone when the last one
// leaves.
//
// The edge is ALL-GONE rather than any-gone, and that distinction is the whole
// reason this is a type instead of a channel. Under discovery a run covers
// several processes; one of them exiting is an ordinary event -- pods restart,
// jobs finish -- and treating it as the end of the run would discard the
// profiling of everything still running. Under -pid the set has one member and
// all-gone is any-gone, so the single-target case falls out rather than being
// special-cased.
//
// A pidfd per target rather than polling /proc/<pid>: these pids belong to
// other people, so the kernel may reap and reuse the number while we watch. A
// pidfd is bound to the process itself and becomes readable exactly once, when
// that process dies. A pid that cannot be opened is treated as ALREADY GONE
// rather than as live-forever: it exited between the scan and here, or we may
// not see it, and either way waiting for it to die would hold the run open on
// something we can never observe.
type liveSet struct {
	allGone chan struct{}

	mu      sync.Mutex
	watched map[int]bool
	alive   int
	closed  bool
	fds     []int
}

func newLiveSet(pids []int) *liveSet {
	// alive starts at 1 for a CONSTRUCTION TOKEN, released once every member
	// has been added. Without it the set can report all-gone before it is
	// fully built: the first target's watcher goroutine is running the moment
	// add returns, so a process that exits during construction takes alive
	// from 1 to 0 and closes allGone while the remaining pids have not been
	// added yet. The run would end immediately, having profiled nothing,
	// on a set where all but one process was perfectly healthy.
	l := &liveSet{allGone: make(chan struct{}), watched: map[int]bool{}, alive: 1}
	for _, p := range pids {
		l.add(p)
	}
	// Releasing the token is itself a death for counting purposes, which
	// gives the degenerate cases the right answer for free: a set where every
	// pidfd failed to open drops straight to zero and reports all-gone, rather
	// than holding the run open on processes nothing can observe.
	l.died()
	return l
}

// add starts watching pid and reports whether it was new to this set.
func (l *liveSet) add(pid int) bool {
	l.mu.Lock()
	if l.closed || l.watched[pid] {
		l.mu.Unlock()
		return false
	}
	l.watched[pid] = true
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		// Not watchable: gone already, or invisible to us. Recorded as seen so
		// rediscovery does not retry it every interval, but never counted as
		// alive -- a target we cannot observe dying must not be able to hold
		// the run open forever.
		l.mu.Unlock()
		return true
	}
	l.alive++
	l.fds = append(l.fds, fd)
	l.mu.Unlock()

	go func() {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // a pidfd is well inside int32
		for {
			n, err := unix.Poll(fds, -1)
			// Poll is interruptible by any signal the runtime delivers, and
			// the Go runtime delivers plenty. EINTR means "nothing happened
			// yet", not "the process is gone", and treating it as the latter
			// would end every attached run at the first GC-related signal.
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if n > 0 || err != nil {
				break
			}
		}
		l.died()
	}()
	return true
}

func (l *liveSet) died() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.alive--
	if l.alive <= 0 {
		l.closed = true
		close(l.allGone)
	}
}

// all returns every pid this set ever watched, including ones that have since
// exited and ones found by rediscovery.
//
// It is what the end-of-run summary must report, and NOT the set discovery
// started with. Under -discover the covered set grows during the run, so a
// summary built from the startup list understates what is in the profile --
// it would name two processes for a profile containing four, which is a
// false statement about the scope of every other number on the line.
func (l *liveSet) all() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]int, 0, len(l.watched))
	for pid := range l.watched {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func (l *liveSet) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alive
}

func (l *liveSet) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, fd := range l.fds {
		_ = unix.Close(fd)
	}
	l.fds = nil
}
