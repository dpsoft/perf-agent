package profile

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/cilium/ebpf"

	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/internal/bpfstack"
	"github.com/dpsoft/perf-agent/internal/perfevent"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// Profiler handles CPU profiling with stack traces
type Profiler struct {
	objs       *perfObjects
	symbolizer *blazesym.Symbolizer
	resolver   *procmap.Resolver
	perfSet    *perfevent.Set
	tags       []string
	sampleRate int
	labels     map[string]string
}

// stackBuilder accumulates symbolized stack frames
type stackBuilder struct {
	stack []pprof.Frame
}

func (s *stackBuilder) append(f pprof.Frame) {
	s.stack = append(s.stack, f)
}

// blazeSymToFrames converts a blazesym.Sym into one or more pprof.Frames.
// addr is the absolute PC from the BPF stack — it is copied onto every
// frame (inlined chain + outer real function) so pprof Locations stay
// distinguishable when two PCs symbolize to the same (file, line, func).
//
// blazesym reports Inlined in outer→inner order (see
// blazesym/src/symbolize/mod.rs:408), so we walk it in reverse to get
// leaf-first output.
func blazeSymToFrames(s blazesym.Sym, addr uint64) []pprof.Frame {
	out := make([]pprof.Frame, 0, 1+len(s.Inlined))
	for i := len(s.Inlined) - 1; i >= 0; i-- {
		in := s.Inlined[i]
		f := pprof.Frame{Name: in.Name, Module: s.Module, Address: addr}
		if in.CodeInfo != nil {
			f.File = in.CodeInfo.File
			f.Line = in.CodeInfo.Line
		}
		out = append(out, f)
	}
	outer := pprof.Frame{Name: s.Name, Module: s.Module, Address: addr}
	if s.CodeInfo != nil {
		outer.File = s.CodeInfo.File
		outer.Line = s.CodeInfo.Line
	}
	out = append(out, outer)
	return out
}

// NewProfiler creates a new CPU profiler with the specified sample rate in Hz
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, labels map[string]string) (*Profiler, error) {
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

	perfSet, err := perfevent.OpenAll(objs.Profile, cpus, sampleRate)
	if err != nil {
		_ = objs.Close()
		return nil, err
	}

	symbolizer, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		_ = perfSet.Close()
		_ = objs.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	return &Profiler{
		objs:       objs,
		symbolizer: symbolizer,
		resolver:   procmap.NewResolver(),
		perfSet:    perfSet,
		tags:       tags,
		sampleRate: sampleRate,
		labels:     labels,
	}, nil
}

// Close releases all resources associated with the profiler
func (pr *Profiler) Close() {
	pr.symbolizer.Close()
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

	for i := 0; i < n; i++ {
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
		// single blazesym call. Per-call overhead (CGO boundary +
		// perf-map / debug-syms bookkeeping) dominates for short stacks;
		// one batched call is dramatically cheaper than one call per IP.
		ips := bpfstack.ExtractIPs(stack)
		if len(ips) > 0 {
			symbols, err := pr.symbolizer.SymbolizeProcessAbsAddrs(
				ips,
				samplePid,
				blazesym.ProcessSourceWithPerfMap(true),
				blazesym.ProcessSourceWithDebugSyms(true),
			)
			if err != nil {
				log.Printf("Failed to symbolize: %v", err)
			} else {
				// symbols and ips are parallel — one Sym per IP.
				for i, s := range symbols {
					if i >= len(ips) {
						break
					}
					for _, f := range blazeSymToFrames(s, ips[i]) {
						sb.append(f)
					}
				}
			}
		}

		end := len(sb.stack)
		pprof.Reverse(sb.stack[begin:end])

		sample := pr.createSample(sb, value, int(samplePid))
		builders.AddSample(&sample)
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

