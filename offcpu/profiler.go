package offcpu

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/dpsoft/perf-agent/internal/bpfstack"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// Profiler handles off-CPU profiling with stack traces
type Profiler struct {
	objs       *offcpuObjects
	symbolizer symbolize.Symbolizer
	resolver   *procmap.Resolver
	link       link.Link
	tags       []string
	labels     map[string]string
}

// stackBuilder accumulates symbolized stack frames
type stackBuilder struct {
	stack []pprof.Frame
}

func (s *stackBuilder) append(f pprof.Frame) {
	s.stack = append(s.stack, f)
}

// NewProfiler creates a new off-CPU profiler
func NewProfiler(pid int, systemWide bool, tags []string, labels map[string]string, sym symbolize.Symbolizer) (*Profiler, error) {
	spec, err := loadOffcpu()
	if err != nil {
		return nil, fmt.Errorf("load offcpu spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
		return nil, fmt.Errorf("set system_wide variable: %w", err)
	}

	objs := &offcpuObjects{}
	if err := spec.LoadAndAssign(objs, nil); err != nil {
		return nil, fmt.Errorf("load offcpu objects: %w", err)
	}

	// Configure PID filter only for the targeted mode
	if !systemWide {
		trackValue := uint8(1)
		if err := objs.PidFilter.Update(uint32(pid), &trackValue, ebpf.UpdateAny); err != nil {
			_ = objs.Close()
			return nil, fmt.Errorf("configure PID filter: %w", err)
		}
	}

	// Attach to sched_switch tracepoint
	tp, err := link.AttachTracing(link.TracingOptions{
		Program: objs.OffcpuSchedSwitch,
	})
	if err != nil {
		_ = objs.Close()
		return nil, fmt.Errorf("attach tp_btf sched_switch: %w", err)
	}

	return &Profiler{
		objs:       objs,
		symbolizer: sym,
		resolver:   procmap.NewResolver(),
		link:       tp,
		tags:       tags,
		labels:     labels,
	}, nil
}

// Close releases all resources associated with the profiler.
// The symbolizer is owned by the Agent; we do not close it here.
func (pr *Profiler) Close() {
	pr.resolver.Close()
	_ = pr.link.Close()
	_ = pr.objs.Close()
}

// Collect writes the profile to the provided writer (supports streaming).
// The output is gzip-compressed pprof data.
func (pr *Profiler) Collect(w io.Writer) error {
	m := pr.objs.OffcpuCounts
	mapSize := m.MaxEntries()

	keys := make([]offcpuOffcpuKey, mapSize)
	values := make([]uint64, mapSize)

	opts := &ebpf.BatchOptions{}
	cursor := new(ebpf.MapBatchCursor)

	n, err := m.BatchLookupAndDelete(cursor, keys, values, opts)
	if n > 0 {
		log.Printf("Off-CPU BatchLookupAndDelete: %d samples", n)
	}

	if errors.Is(err, ebpf.ErrKeyNotExist) {
		// Expected when a map is empty or all entries processed
	} else if err != nil {
		log.Printf("Off-CPU BatchLookupAndDelete error: %v", err)
	}

	if n == 0 {
		log.Println("No off-CPU samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    1, // Not used for off-CPU but needed for builder
		PerPIDProfile: false,
		Comments:      pr.tags,
		Resolver:      pr.resolver,
		Labels:        pr.labels,
	})

	for i := 0; i < n; i++ {
		key := keys[i]
		value := values[i]

		// Use PID from a sample key for symbolization
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
		// overhead dominates for short stacks; one batched call is
		// dramatically cheaper than one per IP.
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

		sample := pprof.ProfileSample{
			Pid:         samplePid,
			Aggregation: pprof.SampleAggregated,
			SampleType:  pprof.SampleTypeOffCpu,
			Stack:       sb.stack,
			Value:       value,
		}
		builders.AddSample(&sample)
	}

	// Write profile directly to the provided writer
	for _, builder := range builders.Builders {
		_, err = builder.Write(w)
		if err != nil {
			return fmt.Errorf("write off-CPU profile: %w", err)
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
		return fmt.Errorf("create off-CPU profile file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if err := pr.Collect(file); err != nil {
		return err
	}

	log.Printf("Off-CPU profile written to %s", outputPath)
	return nil
}
