package profile

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/internal/bpfstack"
	"github.com/dpsoft/perf-agent/internal/perfdata"
	"github.com/dpsoft/perf-agent/internal/perfevent"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// Profiler handles CPU profiling with stack traces
type Profiler struct {
	objs             *perfObjects
	symbolizer       symbolize.Symbolizer
	kernelSymbolizer symbolize.KernelSymbolizer
	resolver         *procmap.Resolver
	warmer           *procmap.Warmer
	perfSet          *perfevent.Set
	tags             []string
	sampleRate       int
	labels           map[string]string
	perfData         *perfdata.Writer // optional, nil when --perf-data-output not set
}

// stackBuilder accumulates symbolized stack frames
type stackBuilder struct {
	stack []pprof.Frame
}

func (s *stackBuilder) append(f pprof.Frame) {
	s.stack = append(s.stack, f)
}

// NewProfiler creates a new CPU profiler.
//
// eventSpec selects the perf-event source. Pass nil to default to software
// cpu-clock at sampleRate Hz. When non-nil, sampleRate is ignored (the
// caller is responsible for putting the desired rate in eventSpec). Used
// by the agent to keep the in-kernel event and the perf.data attr in sync
// when the output writer is enabled — a divergence would mislead consumers.
//
// kernelStacks gates the BPF program's kernel-stack capture (set from
// cfg.KernelStacks). When false, kernel-stack capture is fully bypassed
// at sample time; the CollectKernel bit on each pid_config entry is a
// no-op. When true, kernel stacks are captured for matched samples.
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, labels map[string]string, perfData *perfdata.Writer, eventSpec *perfevent.EventSpec, sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer, kernelStacks bool) (*Profiler, error) {
	spec, err := loadPerf()
	if err != nil {
		return nil, fmt.Errorf("load profile spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
		return nil, fmt.Errorf("set system_wide variable: %w", err)
	}

	// Set kernel_stacks_enabled before LoadAndAssign so the BPF program's
	// gate evaluates correctly on first sample.
	if err := spec.Variables["kernel_stacks_enabled"].Set(kernelStacks); err != nil {
		return nil, fmt.Errorf("set kernel_stacks_enabled: %w", err)
	}

	objs := &perfObjects{}
	if err := spec.LoadAndAssign(objs, nil); err != nil {
		return nil, fmt.Errorf("load profile objects: %w", err)
	}

	// Only configure PID filter for targeted mode
	if !systemWide {
		config := perfPidConfig{
			Type:          0,
			CollectUser:   1,
			CollectKernel: 1, // gated by BPF kernel_stacks_enabled global
		}

		if err := objs.Pids.Update(uint32(pid), &config, ebpf.UpdateAny); err != nil {
			_ = objs.Close()
			return nil, fmt.Errorf("update pid map: %w", err)
		}
	}

	evSpec := perfevent.EventSpec{
		Type:         perfevent.PerfTypeSoftware,
		Config:       perfevent.PerfCountSWCPUClock,
		SamplePeriod: uint64(sampleRate),
		Frequency:    true,
	}
	if eventSpec != nil {
		evSpec = *eventSpec
	}
	perfSet, err := perfevent.OpenAll(objs.Profile, cpus, evSpec)
	if err != nil {
		_ = objs.Close()
		return nil, err
	}

	resolver := procmap.NewResolver()
	pr := &Profiler{
		objs:             objs,
		symbolizer:       sym,
		kernelSymbolizer: kernelSym,
		resolver:         resolver,
		perfSet:          perfSet,
		tags:             tags,
		sampleRate:       sampleRate,
		labels:           labels,
		perfData:         perfData,
	}

	// Mappings are read at COLLECT time, which is after the capture window has
	// closed — so without this, every process that exited during the window
	// resolves to nothing and its frames land on the default anonymous
	// mapping. Issue #56.
	//
	// System-wide only sweeps all of /proc. A per-PID capture warms exactly
	// its target: the same protection where it matters, without reading the
	// maps of every process on the machine for a profile that can never
	// contain them.
	if systemWide {
		pr.warmer = procmap.NewWarmer(resolver, 0)
		pr.warmer.Start()
	} else if pid > 0 {
		resolver.Warm(uint32(pid))
	}
	return pr, nil
}

// Close releases all resources associated with the profiler.
// The symbolizer is owned by the Agent; we do not close it here.
func (pr *Profiler) Close() {
	if pr.warmer != nil {
		pr.warmer.Stop()
	}
	pr.resolver.Close()
	_ = pr.perfSet.Close()
	_ = pr.objs.Close()
}

// Collect writes the profile to the provided writer (supports streaming).
// The output is gzip-compressed pprof data.
func (pr *Profiler) Collect(w io.Writer) error {
	m := pr.objs.Counts
	mapSize := m.MaxEntries()

	keys := make([]perfSampleKey, mapSize)
	values := make([]uint64, mapSize)

	opts := &ebpf.BatchOptions{}
	cursor := new(ebpf.MapBatchCursor)

	n, err := m.BatchLookupAndDelete(cursor, keys, values, opts)
	if n > 0 {
		log.Printf("BatchLookupAndDelete: %d samples", n)
	}

	if errors.Is(err, ebpf.ErrKeyNotExist) {
		// Expected when map is empty or all entries processed
	} else if err != nil {
		log.Printf("BatchLookupAndDelete error: %v", err)
	}

	if n == 0 {
		log.Println("No profile samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    int64(pr.sampleRate),
		PerPIDProfile: false,
		Comments:      pr.tags,
		Resolver:      pr.resolver,
		Labels:        pr.labels,
	})

	// PIDs already refreshed in this collect pass. A busy system-wide capture
	// has many samples per process and re-reading /proc/<pid>/maps for each
	// would be the most expensive thing in this loop.
	refreshed := make(map[uint32]struct{}, n)

	for i := range n {
		key := keys[i]
		value := values[i]

		// Use PID from sample key for symbolization
		samplePid := key.Pid
		// Re-read this PID's mappings if it is still alive, so a process that
		// loaded a library after the warmer last saw it still resolves. When
		// it is gone, Refresh keeps whatever was warmed rather than dropping
		// it — which is the whole reason this is not Invalidate. Issue #56.
		//
		// Once per PID per collect, not once per sample: refreshed tracks what
		// has already been done in this pass.
		if _, done := refreshed[samplePid]; !done {
			refreshed[samplePid] = struct{}{}
			pr.resolver.Refresh(samplePid)
		}

		// Kernel stack lookup — only when BPF gated a valid stack ID.
		var kernelIPs []uint64
		if key.KernStack >= 0 {
			if kernBytes, err := pr.objs.Stackmap.LookupBytes(uint32(key.KernStack)); err == nil {
				kernelIPs = bpfstack.ExtractIPs(kernBytes)
			}
		}

		stack, err := pr.objs.Stackmap.LookupBytes(uint32(key.UserStack))
		if err != nil {
			log.Printf("Failed to lookup user stack: %v", err)
			continue
		}

		if len(stack) == 0 {
			continue
		}

		sb := new(stackBuilder)
		begin := len(sb.stack)

		// Extract all non-zero IPs first, then batch-symbolize in a
		// single call through the symbolize.Symbolizer interface. Per-call
		// overhead (CGO boundary + perf-map / debug-syms bookkeeping)
		// dominates for short stacks; one batched call is dramatically
		// cheaper than one call per IP.
		ips := bpfstack.ExtractIPs(stack)
		// Split kernel-range IPs out of the user-stack walk. When the
		// sampled task is in kernel context (syscall, irq, fault),
		// bpf_get_stackid with BPF_F_USER_STACK can leak kernel
		// addresses into the user-stack buffer — they appear in
		// the high half (≥ 0xffff_8000_0000_0000 on x86_64). Without
		// this split the user symbolizer sees kernel IPs (which it
		// can't resolve) and the kernel symbolizer never sees them.
		// Bug discovered via bench-self iteration 2: top 5 hot
		// "user" addresses in perf-agent's self-profile were all
		// kernel-range.
		ips, strayKernelIPs := bpfstack.SplitUserKernelIPs(ips)
		if len(strayKernelIPs) > 0 {
			kernelIPs = append(strayKernelIPs, kernelIPs...)
		}
		if len(ips) > 0 || len(kernelIPs) > 0 {
			var userFrames, kernelFrames []symbolize.Frame
			if len(ips) > 0 {
				userFrames, err = pr.symbolizer.SymbolizeProcess(samplePid, ips)
				if err != nil {
					log.Printf("Failed to symbolize user: %v", err)
				}
			}
			if len(kernelIPs) > 0 {
				kernelFrames, err = pr.kernelSymbolizer.SymbolizeKernel(kernelIPs)
				if err != nil {
					log.Printf("Failed to symbolize kernel: %v", err)
				}
			}
			// Kernel frames are leaf-side: they go first so that after
			// Reverse() the call chain reads root→kernel→user (outermost
			// first), which matches pprof convention.
			for _, f := range symbolize.ToProfFramesKernel(kernelFrames) {
				sb.append(f)
			}
			for _, f := range symbolize.ToProfFrames(userFrames) {
				sb.append(f)
			}
		}

		end := len(sb.stack)
		pprof.Reverse(sb.stack[begin:end])

		sample := pr.createSample(sb, value, int(samplePid))
		builders.AddSample(&sample)

		if pr.perfData != nil && len(ips) > 0 {
			pr.perfData.AddSample(perfdata.SampleRecord{
				IP:        ips[0],
				Pid:       samplePid,
				Tid:       samplePid,
				Period:    value,
				UserIPs:   ips,
				KernelIPs: kernelIPs,
			})
		}
	}

	// Write profile directly to the provided writer
	for _, builder := range builders.Builders {
		_, err = builder.Write(w)
		if err != nil {
			return fmt.Errorf("write profile: %w", err)
		}
		break // Only need first builder for non-per-PID profile
	}

	return nil
}

// CollectAndWrite collects samples and writes the profile to the specified path.
// This is a convenience wrapper around Collect for file-based output.
func (pr *Profiler) CollectAndWrite(outputPath string) error {
	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if err := pr.Collect(file); err != nil {
		return err
	}

	log.Printf("Profile written to %s", outputPath)
	return nil
}

func (pr *Profiler) createSample(sb *stackBuilder, value uint64, pid int) pprof.ProfileSample {
	return pprof.ProfileSample{
		Pid:         uint32(pid),
		Aggregation: pprof.SampleAggregated,
		SampleType:  pprof.SampleTypeCpu,
		Stack:       sb.stack,
		Value:       value,
	}
}
