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
	objs       *perfObjects
	symbolizer symbolize.Symbolizer
	resolver   *procmap.Resolver
	perfSet    *perfevent.Set
	tags       []string
	sampleRate int
	labels     map[string]string
	perfData   *perfdata.Writer // optional, nil when --perf-data-output not set
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
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, labels map[string]string, perfData *perfdata.Writer, eventSpec *perfevent.EventSpec, sym symbolize.Symbolizer) (*Profiler, error) {
	spec, err := loadPerf()
	if err != nil {
		return nil, fmt.Errorf("load profile spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
		return nil, fmt.Errorf("set system_wide variable: %w", err)
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
			CollectKernel: 0,
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

	return &Profiler{
		objs:       objs,
		symbolizer: sym,
		resolver:   procmap.NewResolver(),
		perfSet:    perfSet,
		tags:       tags,
		sampleRate: sampleRate,
		labels:     labels,
		perfData:   perfData,
	}, nil
}

// Close releases all resources associated with the profiler.
// The symbolizer is owned by the Agent; we do not close it here.
func (pr *Profiler) Close() {
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

	for i := range n {
		key := keys[i]
		value := values[i]

		// Use PID from sample key for symbolization
		samplePid := key.Pid

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
		if len(ips) > 0 {
			frames, err := pr.symbolizer.SymbolizeProcess(samplePid, ips)
			if err != nil {
				log.Printf("Failed to symbolize: %v", err)
			} else {
				for _, f := range symbolize.ToProfFrames(frames) {
					sb.append(f)
				}
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
				Callchain: ips,
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

