// Package gpuprobe consumes the perfagent GPU USDT ABI from a shim library
// and feeds the normalized events into a gpu.EventSink.
//
// The shim emits fixed-size records (shim/core/usdt_abi.h, mirrored by
// internal/gpuabi) in batches. Each probe fire hands the BPF program a
// pointer, a record count and a per-probe monotonic sequence number; the
// program copies the batch out of user memory into a ringbuf, and this
// package decodes it and normalizes it into gpu events.
//
// Nothing is lost silently (spec §6.1). Batches the kernel could not deliver
// are counted in the BPF-side `dropped` map and surfaced as
// Stats.KernelDropped; gaps in a probe's sequence numbers are counted as
// Stats.SequenceGaps; record kinds this phase carries but does not yet
// normalize are counted as Stats.Undecoded.
package gpuprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/dpsoft/perf-agent/internal/usdt"
)

const (
	kindLaunch = 1
	kindExec   = 2
	kindModule = 3
	kindPC     = 4

	// kindMax mirrors KIND_MAX in bpf/gpu_usdt.bpf.c: the number of slots in
	// the BPF-side `dropped` array.
	kindMax = 8

	// batchHdrSize mirrors struct batch_hdr in bpf/gpu_usdt.bpf.c.
	batchHdrSize = 32

	// usdtProvider is the only provider whose probes this consumer attaches.
	usdtProvider = "perfagent"
)

// cookieFor maps a USDT probe name to the BPF attach cookie that tells the
// program which record kind fired. The values are the KIND_* constants in
// bpf/gpu_usdt.bpf.c; a mismatch would decode garbage silently, so the pair
// is pinned by a unit test. Zero means "not a probe we know", and such
// probes are not attached at all.
func cookieFor(probeName string) uint64 {
	switch probeName {
	case "gpu_launch_v1":
		return kindLaunch
	case "gpu_exec_v1":
		return kindExec
	case "gpu_module_load_v1":
		return kindModule
	case "gpu_pc_sample_batch_v1":
		return kindPC
	}
	return 0
}

// kernelVersionCode supplies what BPF_PROG_TYPE_KPROBE requires. cilium/ebpf
// would otherwise read the vDSO through /proc/self/mem, which a setcap'd
// binary cannot do because file capabilities make the process non-dumpable —
// and it fails with an error that names neither capabilities nor uprobes.
func kernelVersionCode() uint32 {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return 0
	}
	release := unix.ByteSliceToString(u.Release[:])
	var a, b, c uint32
	// A release like "6.19.10-300.fc44.x86_64" parses as 6.19.10; a two-part
	// release like "6.19" leaves c at zero, which is what LINUX_VERSION_CODE
	// would say too.
	if _, err := fmt.Sscanf(release, "%d.%d.%d", &a, &b, &c); err != nil {
		if _, err := fmt.Sscanf(release, "%d.%d", &a, &b); err != nil {
			return 0
		}
	}
	if c > 255 {
		c = 255
	}
	return a<<16 | b<<8 | c
}

// Config describes what to attach to and where the normalized events go.
type Config struct {
	// ShimPath is the ELF carrying the perfagent USDT probes.
	ShimPath string
	// PID restricts the attachment to one process; zero is system-wide.
	PID int
	// Backend labels the correlation IDs the consumer produces.
	Backend gpu.GPUBackendID
	// Sink receives the normalized events.
	Sink gpu.EventSink
}

// Stats is the consumer's loss record. Every discard has a counter here or
// in KernelDropped; none of them is silent.
type Stats struct {
	// Batches and Records count what arrived and was normalized.
	Batches uint64
	Records uint64
	// SequenceGaps counts records implied by a jump in a probe's per-probe
	// monotonic sequence number: batches the consumer never saw.
	SequenceGaps uint64
	// SinkRejected counts events the sink refused (full, or invalid).
	SinkRejected uint64
	// Undecoded counts records of a kind this phase carries on the wire but
	// does not yet normalize (module loads, PC samples). Counted so the loss
	// is visible rather than silent.
	Undecoded uint64
	// Malformed counts ringbuf samples that did not decode: a short header,
	// a payload shorter than the header claims, or a truncated record.
	Malformed uint64
	// KernelDropped counts records the BPF program itself could not deliver:
	// a batch bigger than one ringbuf reservation, a full ringbuf, or a
	// faulting read of the producer's buffer. Read from the BPF `dropped`
	// map on each Stats call.
	KernelDropped uint64
}

// Consumer owns the loaded BPF objects, the uprobe_multi link and the
// ringbuf reader for one shim.
type Consumer struct {
	cfg    Config
	objs   gpuusdtObjects
	links  []link.Link
	reader *ringbuf.Reader

	mu        sync.Mutex
	seqByKind map[uint32]uint64
	stats     Stats
}

// Attach discovers the shim's probes and attaches them all in one
// uprobe_multi link.
//
// It must be uprobe_multi, not link.Uprobe: the perf_uprobe PMU path requires
// CAP_SYS_ADMIN, measured, while the BPF-link path works with CAP_BPF +
// CAP_PERFMON alone (spec §11). Using the obvious API silently undoes the
// capability reduction Phase 1 delivered. Requires Linux 6.6+.
func Attach(cfg Config) (c *Consumer, err error) {
	if cfg.Sink == nil {
		return nil, errors.New("gpuprobe: Config.Sink is required")
	}

	probes, err := usdt.ParseFile(cfg.ShimPath)
	if err != nil {
		return nil, fmt.Errorf("parse usdt notes: %w", err)
	}

	var addrs, refCtrs, cookies []uint64
	for _, p := range probes {
		cookie := cookieFor(p.Name)
		if cookie == 0 || p.Provider != usdtProvider {
			continue
		}
		if !p.HasSemaphore {
			return nil, fmt.Errorf("probe %s has no semaphore; the shim cannot tell when to emit", p.Name)
		}
		addrs = append(addrs, p.Offset)
		refCtrs = append(refCtrs, p.SemaphoreOffset)
		cookies = append(cookies, cookie)
	}
	if len(addrs) == 0 {
		return nil, errors.New("no perfagent probes found in shim")
	}

	spec, err := loadGpuusdt()
	if err != nil {
		return nil, err
	}
	// See kernelVersionCode: without this cilium/ebpf discovers the version
	// through the vDSO, which a setcap'd (non-dumpable) process cannot read.
	kv := kernelVersionCode()
	for _, p := range spec.Programs {
		p.KernelVersion = kv
	}

	c = &Consumer{cfg: cfg, seqByKind: map[uint32]uint64{}}
	defer func() {
		if err != nil {
			c.Close()
			c = nil
		}
	}()

	if err = spec.LoadAndAssign(&c.objs, nil); err != nil {
		return nil, fmt.Errorf("load gpu usdt objects: %w", err)
	}

	ex, err := link.OpenExecutable(cfg.ShimPath)
	if err != nil {
		return nil, err
	}
	l, err := ex.UprobeMulti(nil, c.objs.GpuUsdtBatch, &link.UprobeMultiOptions{
		Addresses:     addrs,
		RefCtrOffsets: refCtrs,
		Cookies:       cookies,
		PID:           uint32(cfg.PID),
	})
	if err != nil {
		return nil, fmt.Errorf("uprobe_multi attach (needs Linux 6.6+): %w", err)
	}
	c.links = append(c.links, l)

	c.reader, err = ringbuf.NewReader(c.objs.Events)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// batch is one decoded ringbuf sample: a header plus the records it carried.
type batch struct {
	Kind     uint32
	Seq      uint64
	PID, TID uint32
	RawCount uint32
	Launches []gpuabi.Launch
	Execs    []gpuabi.Exec
}

func decodeBatch(b []byte) (batch, error) {
	if len(b) < batchHdrSize {
		return batch{}, gpuabi.ErrShortRecord
	}
	le := binary.LittleEndian
	out := batch{
		Kind: le.Uint32(b[0:]),
		Seq:  le.Uint64(b[8:]),
		PID:  le.Uint32(b[16:]),
		TID:  le.Uint32(b[20:]),
	}
	count := int(le.Uint32(b[4:]))
	out.RawCount = uint32(count)
	nbytes := int(le.Uint64(b[24:]))
	payload := b[batchHdrSize:]
	if nbytes < 0 || nbytes > len(payload) {
		return batch{}, fmt.Errorf("batch claims %d payload bytes, has %d", nbytes, len(payload))
	}
	payload = payload[:nbytes]

	switch out.Kind {
	case kindLaunch:
		out.Launches = make([]gpuabi.Launch, 0, count)
		for i := 0; i < count; i++ {
			off := i * gpuabi.SizeLaunch
			if off > len(payload) {
				return batch{}, gpuabi.ErrShortRecord
			}
			rec, err := gpuabi.DecodeLaunch(payload[off:])
			if err != nil {
				return batch{}, err
			}
			out.Launches = append(out.Launches, rec)
		}
	case kindExec:
		out.Execs = make([]gpuabi.Exec, 0, count)
		for i := 0; i < count; i++ {
			off := i * gpuabi.SizeExec
			if off > len(payload) {
				return batch{}, gpuabi.ErrShortRecord
			}
			rec, err := gpuabi.DecodeExec(payload[off:])
			if err != nil {
				return batch{}, err
			}
			out.Execs = append(out.Execs, rec)
		}
	}
	return out, nil
}

// noteSeq counts records lost between batches. A gap is loss the consumer did
// not observe and must never be silent (spec §6.1). Caller holds mu.
func (c *Consumer) noteSeq(kind uint32, seq uint64) {
	prev, seen := c.seqByKind[kind]
	if seen && seq > prev+1 {
		c.stats.SequenceGaps += seq - prev - 1
	}
	c.seqByKind[kind] = seq
}

func correlationOf(backend gpu.GPUBackendID, v uint64) gpu.CorrelationID {
	return gpu.CorrelationID{Backend: backend, Value: strconv.FormatUint(v, 10)}
}

// Run reads batches until the context is cancelled or the consumer is
// closed. It is not safe to call Run more than once concurrently.
func (c *Consumer) Run(ctx context.Context) error {
	// ringbuf.Reader.Read blocks; a deadline in the past is how it is woken
	// on cancellation without closing the reader out from under Close.
	stop := context.AfterFunc(ctx, func() { c.reader.SetDeadline(time.Now()) })
	defer stop()

	for {
		rec, err := c.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		b, err := decodeBatch(rec.RawSample)
		if err != nil {
			c.mu.Lock()
			c.stats.Malformed++
			c.mu.Unlock()
			continue
		}
		c.applyBatch(b)
	}
}

func (c *Consumer) applyBatch(b batch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stats.Batches++
	c.noteSeq(b.Kind, b.Seq)

	switch b.Kind {
	case kindLaunch:
		for _, l := range b.Launches {
			c.stats.Records++
			ev := gpu.GPUKernelLaunch{
				Correlation: correlationOf(c.cfg.Backend, l.Correlation),
				// The shim stamps CLOCK_MONOTONIC, which is the only domain
				// the core accepts; say so rather than leaning on
				// NormalizeClockDomain's zero-value default.
				ClockDomain: gpu.ClockDomainCPUMonotonic,
				TimeNs:      l.TimeNs,
				Launch: gpu.LaunchContext{
					PID:    b.PID,
					TID:    l.TID,
					TimeNs: l.TimeNs,
					// CPUStack is deliberately empty: capturing the launch
					// stack is a later phase.
				},
			}
			if err := c.cfg.Sink.EmitLaunch(ev); err != nil {
				c.stats.SinkRejected++
			}
		}
	case kindExec:
		for _, e := range b.Execs {
			c.stats.Records++
			ev := gpu.GPUKernelExec{
				Correlation: correlationOf(c.cfg.Backend, e.Correlation),
				ClockDomain: gpu.ClockDomainCPUMonotonic,
				StartNs:     e.StartNs,
				EndNs:       e.EndNs,
			}
			if err := c.cfg.Sink.EmitExec(ev); err != nil {
				c.stats.SinkRejected++
			}
		}
	default:
		// Carried on the wire, not yet normalized. Counted, never silent.
		c.stats.Undecoded += uint64(b.RawCount)
	}
}

// Stats returns the loss record, including the BPF-side drop counters read
// fresh from the `dropped` map.
func (c *Consumer) Stats() Stats {
	c.mu.Lock()
	out := c.stats
	c.mu.Unlock()
	out.KernelDropped = c.kernelDropped()
	return out
}

// kernelDropped sums the per-kind drop counters the BPF program maintains.
// A read failure leaves the total at zero rather than panicking; the map is
// nil in tests that construct a Consumer directly.
func (c *Consumer) kernelDropped() uint64 {
	if c.objs.Dropped == nil {
		return 0
	}
	var total uint64
	for key := uint32(0); key < kindMax; key++ {
		var v uint64
		if err := c.objs.Dropped.Lookup(&key, &v); err != nil {
			continue
		}
		total += v
	}
	return total
}

// Close releases the ringbuf reader, the links and the BPF objects. It is
// safe to call on a partially constructed Consumer.
func (c *Consumer) Close() error {
	var errs []error
	if c.reader != nil {
		errs = append(errs, c.reader.Close())
		c.reader = nil
	}
	for _, l := range c.links {
		errs = append(errs, l.Close())
	}
	c.links = nil
	errs = append(errs, c.objs.Close())
	return errors.Join(errs...)
}
