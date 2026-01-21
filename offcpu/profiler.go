package offcpu

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"perf-agent/pprof"

	p "github.com/google/pprof/profile"
	blazesym "github.com/libbpf/blazesym/go"
)

// Profiler handles off-CPU profiling with stack traces
type Profiler struct {
	objs       *OffcpuObjects
	symbolizer *blazesym.Symbolizer
	link       link.Link
	tags       []string
}

// stackBuilder accumulates symbolized stack frames
type stackBuilder struct {
	stack []string
}

func (s *stackBuilder) append(sym string) {
	s.stack = append(s.stack, sym)
}

// NewProfiler creates a new off-CPU profiler
func NewProfiler(pid int, systemWide bool, tags []string) (*Profiler, error) {
	spec, err := LoadOffcpu()
	if err != nil {
		return nil, fmt.Errorf("load offcpu spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.RewriteConstants(map[string]interface{}{
		"system_wide": systemWide,
	}); err != nil {
		return nil, fmt.Errorf("rewrite constants: %w", err)
	}

	objs := &OffcpuObjects{}
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

	symbolizer, err := blazesym.NewSymbolizer(blazesym.SymbolizerWithCodeInfo(true))
	if err != nil {
		_ = tp.Close()
		_ = objs.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	return &Profiler{
		objs:       objs,
		symbolizer: symbolizer,
		link:       tp,
		tags:       tags,
	}, nil
}

// Close releases all resources associated with the profiler
func (pr *Profiler) Close() {
	pr.symbolizer.Close()
	_ = pr.link.Close()
	_ = pr.objs.Close()
}

// CollectAndWrite collects samples and writes the profile to the specified path
func (pr *Profiler) CollectAndWrite(outputPath string) error {
	m := pr.objs.OffcpuCounts
	mapSize := m.MaxEntries()

	keys := make([]OffcpuOffcpuKey, mapSize)
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

		for j := 0; j < 127; j++ {
			instructionPointerBytes := stack[j*8 : j*8+8]
			instructionPointer := binary.LittleEndian.Uint64(instructionPointerBytes)
			if instructionPointer == 0 {
				break
			}

			symbol, err := pr.symbolizer.SymbolizeProcessAbsAddrs(
				[]uint64{instructionPointer},
				samplePid,
				blazesym.ProcessSourceWithPerfMap(true),
				blazesym.ProcessSourceWithDebugSyms(true),
			)
			if err != nil {
				log.Printf("Failed to symbolize: %v", err)
				break
			}

			sb.append(symbol[0].Name)
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

	// Get builder and write profile
	var buf bytes.Buffer
	for _, builder := range builders.Builders {
		_, err = builder.Write(&buf)
		if err != nil {
			return fmt.Errorf("write off-CPU profile: %w", err)
		}
		break // Only need first builder for non-per-PID profile
	}

	if buf.Len() == 0 {
		log.Println("No off-CPU profile data to write")
		return nil
	}

	parsed, err := p.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("parse off-CPU profile: %w", err)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create off-CPU profile file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if err := parsed.Write(file); err != nil {
		return fmt.Errorf("write off-CPU profile to file: %w", err)
	}

	log.Printf("Off-CPU profile written to %s", outputPath)
	return nil
}
