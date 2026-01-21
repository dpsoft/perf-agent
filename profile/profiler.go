package profile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/elastic/go-perf"
	blazesym "github.com/libbpf/blazesym/go"
	"golang.org/x/sys/unix"

	p "github.com/google/pprof/profile"
	"perf-agent/pprof"
)

// Profiler handles CPU profiling with stack traces
type Profiler struct {
	objs       *PerfObjects
	symbolizer *blazesym.Symbolizer
	perfEvents []*perfEvent
	tags       []string
}

// perfEvent wraps a Linux perf event for CPU sampling
type perfEvent struct {
	fd   int
	link *link.RawLink
	p    *perf.Event
}

// stackBuilder accumulates symbolized stack frames
type stackBuilder struct {
	stack []string
}

func (s *stackBuilder) append(sym string) {
	s.stack = append(s.stack, sym)
}

// NewProfiler creates a new CPU profiler
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string) (*Profiler, error) {
	spec, err := LoadPerf()
	if err != nil {
		return nil, fmt.Errorf("load profile spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.RewriteConstants(map[string]interface{}{
		"system_wide": systemWide,
	}); err != nil {
		return nil, fmt.Errorf("rewrite constants: %w", err)
	}

	objs := &PerfObjects{}
	if err := spec.LoadAndAssign(objs, nil); err != nil {
		return nil, fmt.Errorf("load profile objects: %w", err)
	}

	// Only configure PID filter for targeted mode
	if !systemWide {
		config := PerfPidConfig{
			Type:          0,
			CollectUser:   1,
			CollectKernel: 0,
		}

		if err := objs.Pids.Update(uint32(pid), &config, ebpf.UpdateAny); err != nil {
			_ = objs.Close()
			return nil, fmt.Errorf("update pid map: %w", err)
		}
	}

	var perfEvents []*perfEvent
	for _, id := range cpus {
		pe, err := newPerfEvent(int(id), 10000)
		if err != nil {
			// Cleanup already created perf events
			for _, pe := range perfEvents {
				_ = pe.Close()
			}
			_ = objs.Close()
			return nil, fmt.Errorf("create perf event on CPU %d: %w", id, err)
		}

		if err := pe.attachPerfEvent(objs.Profile); err != nil {
			_ = pe.Close()
			for _, pe := range perfEvents {
				_ = pe.Close()
			}
			_ = objs.Close()
			return nil, fmt.Errorf("attach eBPF to perf event on CPU %d: %w", id, err)
		}

		perfEvents = append(perfEvents, pe)
	}

	symbolizer, err := blazesym.NewSymbolizer(blazesym.SymbolizerWithCodeInfo(true))
	if err != nil {
		for _, pe := range perfEvents {
			_ = pe.Close()
		}
		_ = objs.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	return &Profiler{
		objs:       objs,
		symbolizer: symbolizer,
		perfEvents: perfEvents,
		tags:       tags,
	}, nil
}

// Close releases all resources associated with the profiler
func (pr *Profiler) Close() {
	pr.symbolizer.Close()
	for _, pe := range pr.perfEvents {
		_ = pe.Close()
	}
	_ = pr.objs.Close()
}

// CollectAndWrite collects samples and writes the profile to the specified path
func (pr *Profiler) CollectAndWrite(outputPath string) error {
	m := pr.objs.Counts
	mapSize := m.MaxEntries()

	keys := make([]PerfSampleKey, mapSize)
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
		SampleRate:    int64(97),
		PerPIDProfile: false,
		Comments:      pr.tags,
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

		sample := pr.createSample(sb, value, int(samplePid))
		builders.AddSample(&sample)
	}

	// Get builder and write profile
	var buf bytes.Buffer
	for _, builder := range builders.Builders {
		_, err = builder.Write(&buf)
		if err != nil {
			return fmt.Errorf("write profile: %w", err)
		}
		break // Only need first builder for non-per-PID profile
	}

	if buf.Len() == 0 {
		log.Println("No profile data to write")
		return nil
	}

	parsed, err := p.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("parse profile: %w", err)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if err := parsed.Write(file); err != nil {
		return fmt.Errorf("write profile to file: %w", err)
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

// perfEvent helpers

func newPerfEvent(cpu int, sampleRate int) (*perfEvent, error) {
	perfAttribute := new(perf.Attr)
	perfAttribute.SetSampleFreq(uint64(sampleRate))
	perfAttribute.Type = unix.PERF_TYPE_SOFTWARE
	perfAttribute.Config = unix.PERF_COUNT_SW_CPU_CLOCK

	p, err := perf.Open(perfAttribute, -1, cpu, nil)
	if err != nil {
		return nil, fmt.Errorf("open perf event: %w", err)
	}

	fd, err := p.FD()
	if err != nil {
		return nil, fmt.Errorf("get perf event fd: %w", err)
	}

	return &perfEvent{fd: fd, p: p}, nil
}

func (pe *perfEvent) Close() error {
	_ = syscall.Close(pe.fd)
	if pe.link != nil {
		_ = pe.link.Close()
	}
	return nil
}

func (pe *perfEvent) attachPerfEvent(prog *ebpf.Program) error {
	return pe.p.SetBPF(uint32(prog.FD()))
}
