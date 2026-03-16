package profile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/internal/blazesym"

	"github.com/dpsoft/perf-agent/pprof"
)

// Profiler handles CPU profiling with stack traces
type Profiler struct {
	objs       *perfObjects
	symbolizer *blazesym.Symbolizer
	perfEvents []*perfEvent
	tags       []string
	sampleRate int
}

// perfEvent wraps a Linux perf event for CPU sampling
type perfEvent struct {
	fd   int
	link *link.RawLink
}

// stackBuilder accumulates symbolized stack frames
type stackBuilder struct {
	stack []string
}

func (s *stackBuilder) append(sym string) {
	s.stack = append(s.stack, sym)
}

// NewProfiler creates a new CPU profiler with the specified sample rate in Hz
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	spec, err := loadPerf()
	if err != nil {
		return nil, fmt.Errorf("load profile spec: %w", err)
	}

	// Build eBPF constants
	constants := map[string]interface{}{
		"system_wide": systemWide,
	}

	// Add PID namespace info for namespace-aware PID resolution in containers.
	// When running inside a container (e.g. K8s sidecar), the PID seen in /proc
	// differs from the host PID that bpf_get_current_pid_tgid() returns.
	// By passing the pidns dev/ino, the eBPF program can use
	// bpf_get_ns_current_pid_tgid() to resolve PIDs in the caller's namespace.
	if !systemWide {
		if dev, ino, err := getPIDNamespaceInfo(); err != nil {
			log.Printf("WARNING: failed to get PID namespace info, eBPF will use host PIDs: %v", err)
		} else {
			constants["pidns_dev"] = dev
			constants["pidns_ino"] = ino
		}
	}

	if err := spec.RewriteConstants(constants); err != nil {
		return nil, fmt.Errorf("rewrite constants: %w", err)
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

	var perfEvents []*perfEvent
	for _, id := range cpus {
		pe, err := newPerfEvent(int(id), sampleRate)
		if err != nil {
			// Clean up already created perf events
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
		sampleRate: sampleRate,
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

// getPIDNamespaceInfo returns the device and inode of the current process's
// PID namespace. These values are passed to the eBPF program so it can use
// bpf_get_ns_current_pid_tgid() for namespace-aware PID resolution.
// We need the stat of the namespace file itself (following the symlink),
// not the symlink entry in procfs.
func getPIDNamespaceInfo() (uint64, uint64, error) {
	// Open the ns file to get a real fd, then fstat it.
	// This gives us the nsfs device and inode that the kernel expects.
	fd, err := unix.Open("/proc/self/ns/pid", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("open pidns: %w", err)
	}
	defer unix.Close(fd)

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, 0, fmt.Errorf("fstat pidns: %w", err)
	}
	log.Printf("PID namespace info: dev=%d, ino=%d", stat.Dev, stat.Ino)
	return stat.Dev, stat.Ino, nil
}

// perfEvent helpers

func newPerfEvent(cpu int, sampleRate int) (*perfEvent, error) {
	attr := unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Sample: uint64(sampleRate),
		Bits:   unix.PerfBitFreq, // Enable frequency mode
	}

	fd, err := unix.PerfEventOpen(&attr, -1, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, os.NewSyscallError("perf_event_open", err)
	}

	return &perfEvent{fd: fd}, nil
}

func (pe *perfEvent) Close() error {
	if pe.link != nil {
		_ = pe.link.Close()
	}
	if pe.fd >= 0 {
		return unix.Close(pe.fd)
	}
	return nil
}

func (pe *perfEvent) attachPerfEvent(prog *ebpf.Program) error {
	rawLink, err := link.AttachRawLink(link.RawLinkOptions{
		Target:  pe.fd,
		Program: prog,
		Attach:  ebpf.AttachPerfEvent,
	})
	if err != nil {
		return fmt.Errorf("attach raw link: %w", err)
	}
	pe.link = rawLink
	return nil
}
