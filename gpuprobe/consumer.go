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
// normalize are counted as Stats.Undecoded; records whose wire correlation is
// zero — the ABI's "no correlation", which demotes them to the timeline's
// heuristic join — are counted as Stats.ZeroCorrelation.
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

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/dpsoft/perf-agent/internal/usdt"
)

const (
	kindLaunch        = 1
	kindExec          = 2
	kindModule        = 3
	kindPC            = 4
	kindLaunchSampled = 5
	kindKernelName    = 6

	// kindMax mirrors KIND_MAX in bpf/gpu_usdt.bpf.c: the number of slots in
	// the BPF-side `dropped` and `stacks_missing` arrays.
	kindMax = 8

	// batchHdrSize mirrors struct batch_hdr in bpf/gpu_usdt.bpf.c, which is
	// 40 bytes since Phase 4a appended stack_id and its padding:
	//
	//   0 kind  4 count  8 seq  16 pid  20 tid  24 bytes  32 stack_id  36 _pad
	//
	// The C side carries a _Static_assert on the same number. The two must
	// move together in one commit: a mismatch does not error anywhere, it
	// just decodes every field from the wrong offset.
	batchHdrSize = 40

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
	case "gpu_launch_sampled_v1":
		return kindLaunchSampled
	case "gpu_kernel_name_v1":
		return kindKernelName
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
	// SequenceGaps counts *batches* the consumer never saw, implied by a jump
	// in a probe's monotonic sequence number. The shim increments seq_ once
	// per batch (shim/core/batch.h), so one gap is one whole batch of up to
	// MAX_RECORDS_PER_BATCH records, not one record. It is therefore not
	// additive with KernelDropped or Records, which are both in records:
	// a gap is loss the consumer cannot size, only detect.
	SequenceGaps uint64
	// SinkRejected counts events the sink refused (full, or invalid).
	SinkRejected uint64
	// Undecoded counts records of a kind this phase carries on the wire but
	// does not yet normalize (module loads, PC samples, and interned kernel
	// names, which Task 5 turns into KernelName attributes). Counted so the
	// loss is visible rather than silent.
	Undecoded uint64
	// Malformed counts ringbuf samples that did not decode: a short header,
	// a payload shorter than the header claims, or a truncated record.
	Malformed uint64
	// ZeroCorrelation counts records that arrived carrying a wire correlation
	// of zero, which the ABI defines as "no correlation" (shim/core/usdt_abi.h;
	// spec §6.3 finding 3 makes it the normal case for PC samples in the
	// continuous collection mode this project ships). Those records are NOT
	// lost — they are normalized with the zero gpu.CorrelationID, which routes
	// them to the timeline's heuristic join instead of the exact one. The
	// counter exists because that demotion changes how confidently the join
	// can be read, and a silent demotion is as bad as silent loss.
	ZeroCorrelation uint64
	// KernelDropped counts records the BPF program itself could not deliver:
	// a batch bigger than one ringbuf reservation, a full ringbuf, or a
	// faulting read of the producer's buffer. Read from the BPF `dropped`
	// map on each Stats call.
	KernelDropped uint64
	// SampledLaunches counts gpu_launch_sampled_v1 records decoded. It is the
	// denominator the sampling period is checked against: the producer prints
	// how many launches it sampled, and the two must agree.
	SampledLaunches uint64
	// StacksMissing counts sampled launches that arrived with a negative
	// batch_hdr.stack_id — bpf_get_stackid failed because the stack was
	// deeper than PERF_MAX_STACK_DEPTH, the stackmap was full, or the
	// launching binary has no frame pointers.
	//
	// This is NOT loss: the launch record is delivered and normalized
	// anyway, it simply carries no CPU stack. Folding it into KernelDropped
	// would report a record loss that did not happen, which is its own kind
	// of dishonesty.
	StacksMissing uint64
	// KernelStacksMissing is the same event counted on the BPF side, read
	// from the `stacks_missing` map. It is kept separate from StacksMissing
	// rather than replacing it because the two disagree exactly when a batch
	// carrying a failed capture was itself lost: the kernel counted it, the
	// consumer never saw it. KernelStacksMissing > StacksMissing therefore
	// localizes loss that SequenceGaps can only detect.
	KernelStacksMissing uint64
}

// batchReader is the slice of *ringbuf.Reader that Run and Close use. It
// exists so the Run/Close lifecycle — in particular Close racing a blocked
// Read — is testable without CAP_BPF, which creating a real ringbuf map
// requires. *ringbuf.Reader satisfies it.
//
// SetDeadline is part of the surface but is deliberately never used to
// interrupt a read: see Run for why it cannot work. It stays here so a fake
// can model its real locking and prove that Run does not reach for it.
type batchReader interface {
	Read() (ringbuf.Record, error)
	SetDeadline(t time.Time)
	Close() error
}

// Consumer owns the loaded BPF objects, the uprobe_multi link and the
// ringbuf reader for one shim.
type Consumer struct {
	cfg    Config
	objs   gpuusdtObjects
	links  []link.Link
	reader batchReader

	mu          sync.Mutex
	seqByStream map[seqKey]uint64
	stats       Stats
}

// seqKey identifies one sequence-number stream. The shim's seq_ counter is
// per-process (shim/core/batch.h), so a system-wide attach (Config.PID == 0)
// interleaves one independent stream per profiled process. Keying on kind
// alone would read those interleavings as enormous gaps — manufacturing
// exactly the loss the counter exists to detect.
type seqKey struct {
	kind uint32
	pid  uint32
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

	c = &Consumer{cfg: cfg, seqByStream: map[seqKey]uint64{}}
	// Every failure below this point must leave through a *bare* return that
	// only assigns err. `return nil, err` would set the named result c to nil
	// before the deferred cleanup runs, so the defer would call Close on a nil
	// receiver — a panic in the caller instead of an error, and a leak of the
	// programs, maps and links Close never got to release.
	defer func() {
		if err != nil {
			// Cleanup path: the caller sees the original failure, not
			// whatever the teardown of a half-built consumer reports.
			_ = c.Close()
			c = nil
		}
	}()

	if err = spec.LoadAndAssign(&c.objs, nil); err != nil {
		err = fmt.Errorf("load gpu usdt objects: %w", err)
		return
	}

	var ex *link.Executable
	if ex, err = link.OpenExecutable(cfg.ShimPath); err != nil {
		return
	}
	var l link.Link
	l, err = ex.UprobeMulti(nil, c.objs.GpuUsdtBatch, &link.UprobeMultiOptions{
		Addresses:     addrs,
		RefCtrOffsets: refCtrs,
		Cookies:       cookies,
		PID:           uint32(cfg.PID),
	})
	if err != nil {
		err = fmt.Errorf("uprobe_multi attach (needs Linux 6.6+): %w", err)
		return
	}
	c.links = append(c.links, l)

	if c.reader, err = ringbuf.NewReader(c.objs.Events); err != nil {
		return
	}
	return c, nil
}

// batch is one decoded ringbuf sample: a header plus the records it carried.
type batch struct {
	Kind     uint32
	Seq      uint64
	PID, TID uint32
	RawCount uint32
	// StackID is the BPF stackmap key for the launching thread's user stack,
	// or negative when the capture failed. It is a property of the whole
	// batch because the BPF program stores it in the header — sound only
	// because the one kind that carries a stack, kindLaunchSampled, always
	// arrives with count == 1 (the shim emits it unbatched). It is -1 on
	// every other kind and must only be interpreted for kindLaunchSampled.
	StackID         int32
	Launches        []gpuabi.Launch
	Execs           []gpuabi.Exec
	SampledLaunches []gpuabi.LaunchSampled
	KernelNames     []gpuabi.KernelName
}

func decodeBatch(b []byte) (batch, error) {
	if len(b) < batchHdrSize {
		return batch{}, gpuabi.ErrShortRecord
	}
	le := binary.LittleEndian
	out := batch{
		Kind:    le.Uint32(b[0:]),
		Seq:     le.Uint64(b[8:]),
		PID:     le.Uint32(b[16:]),
		TID:     le.Uint32(b[20:]),
		StackID: int32(le.Uint32(b[32:])),
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
		// Division, not multiplication: count comes from a uint32 field and
		// count*SizeLaunch could overflow int on a 32-bit build.
		if count > len(payload)/gpuabi.SizeLaunch {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.Launches = make([]gpuabi.Launch, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeLaunch(payload[i*gpuabi.SizeLaunch:])
			if err != nil {
				return batch{}, err
			}
			out.Launches = append(out.Launches, rec)
		}
	case kindExec:
		if count > len(payload)/gpuabi.SizeExec {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.Execs = make([]gpuabi.Exec, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeExec(payload[i*gpuabi.SizeExec:])
			if err != nil {
				return batch{}, err
			}
			out.Execs = append(out.Execs, rec)
		}
	case kindLaunchSampled:
		if count > len(payload)/gpuabi.SizeLaunchSampled {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.SampledLaunches = make([]gpuabi.LaunchSampled, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeLaunchSampled(payload[i*gpuabi.SizeLaunchSampled:])
			if err != nil {
				return batch{}, err
			}
			out.SampledLaunches = append(out.SampledLaunches, rec)
		}
	case kindKernelName:
		if count > len(payload)/gpuabi.SizeKernelName {
			return batch{}, gpuabi.ErrShortRecord
		}
		out.KernelNames = make([]gpuabi.KernelName, 0, count)
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeKernelName(payload[i*gpuabi.SizeKernelName:])
			if err != nil {
				return batch{}, err
			}
			out.KernelNames = append(out.KernelNames, rec)
		}
	}
	return out, nil
}

// noteSeq counts batches lost between the ones that arrived. A gap is loss
// the consumer did not observe and must never be silent (spec §6.1). The
// stream is identified by (kind, pid): see seqKey. Caller holds mu.
func (c *Consumer) noteSeq(kind, pid uint32, seq uint64) {
	key := seqKey{kind: kind, pid: pid}
	prev, seen := c.seqByStream[key]
	if seen && seq > prev+1 {
		c.stats.SequenceGaps += seq - prev - 1
	}
	c.seqByStream[key] = seq
}

// correlationOf converts a wire correlation into the core's CorrelationID.
//
// A wire value of zero means "this record carries no correlation" — the ABI
// says so explicitly for PC samples in continuous collection, the mode this
// project ships (shim/core/usdt_abi.h, spec §6.3 finding 3). It must map to
// the *zero* gpu.CorrelationID, because that is the value gpu/timeline.go
// tests to decide a record needs the heuristic join. Formatting it as the
// string "0" would produce a perfectly valid-looking ID that every
// uncorrelated record shares, collapsing them into one exact-join bucket and
// yielding confident, wrong joins with nothing counted.
//
// Caller holds mu: the zero case bumps a counter so the demotion is visible.
func (c *Consumer) correlationOf(v uint64) gpu.CorrelationID {
	if v == 0 {
		c.stats.ZeroCorrelation++
		return gpu.CorrelationID{}
	}
	return gpu.CorrelationID{Backend: c.cfg.Backend, Value: strconv.FormatUint(v, 10)}
}

// Run reads batches until the context is cancelled or the consumer is
// closed. It is not safe to call Run more than once concurrently.
func (c *Consumer) Run(ctx context.Context) error {
	// Cancellation closes the reader. It cannot use SetDeadline:
	// ringbuf.(*Reader).ReadInto takes the reader's own mutex and holds it
	// for the entire blocking loop, epoll wait included, while SetDeadline
	// wants that same mutex — so a deadline set on a *blocked* reader parks
	// forever behind the read it was supposed to interrupt, and nothing is
	// ever woken. Close is the documented interrupt ("It interrupts calls to
	// Read"), and it works precisely because it closes the poller BEFORE
	// acquiring the mutex. It is also idempotent: once the poller is closed a
	// second Close returns nil, so Consumer.Close remains safe afterwards.
	// The read then fails with ringbuf.ErrClosed, which the loop below treats
	// as a clean exit.
	stop := context.AfterFunc(ctx, func() { _ = c.reader.Close() })
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
	c.noteSeq(b.Kind, b.PID, b.Seq)

	switch b.Kind {
	case kindLaunch:
		for _, l := range b.Launches {
			c.stats.Records++
			ev := gpu.GPUKernelLaunch{
				Correlation: c.correlationOf(l.Correlation),
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
				Correlation: c.correlationOf(e.Correlation),
				ClockDomain: gpu.ClockDomainCPUMonotonic,
				StartNs:     e.StartNs,
				EndNs:       e.EndNs,
			}
			if err := c.cfg.Sink.EmitExec(ev); err != nil {
				c.stats.SinkRejected++
			}
		}
	case kindLaunchSampled:
		// The stack is a property of the batch, not of a record, and that is
		// only sound because this kind rides unbatched — the shim emits one
		// probe fire per sampled launch precisely so a captured stack belongs
		// to exactly one launch (shim/stub/stub.cc). Counting it per batch
		// would be wrong the moment that changed, so count it per record and
		// let the RawCount == 1 invariant hold it together.
		for range b.SampledLaunches {
			c.stats.Records++
			c.stats.SampledLaunches++
			if b.StackID < 0 {
				// A launch whose stack capture failed is still a launch: it
				// is counted here and carried on, never discarded.
				c.stats.StacksMissing++
			}
		}
		// Deliberately not emitted to the sink in this task. A sampled launch
		// shares its correlation with the plain gpu_launch_v1 record for the
		// same launch, so emitting both would hand the timeline two launches
		// for one. Task 5 resolves the stack ID through the stackmap and
		// attaches it to the launch that is already flowing.
	default:
		// Carried on the wire, not yet normalized. Counted, never silent.
		c.stats.Undecoded += uint64(b.RawCount)
	}
}

// Stats returns the loss record, including the BPF-side drop counters read
// fresh from the `dropped` map.
// The `dropped` map is read under mu, not outside it: Close tears c.objs down
// under the same lock. Reading the map while another goroutine closes the map
// fd does not crash — the lookup just fails — so the symptom would have been a
// silent KernelDropped == 0, which is precisely the silent loss §6.1 forbids.
func (c *Consumer) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stats
	out.KernelDropped = c.sumPerKind(c.objs.Dropped)
	out.KernelStacksMissing = c.sumPerKind(c.objs.StacksMissing)
	return out
}

// sumPerKind totals one of the BPF program's per-kind counter arrays
// (`dropped`, `stacks_missing`). A read failure leaves the total at zero
// rather than panicking; the map is nil in tests that construct a Consumer
// directly. Caller holds mu.
func (c *Consumer) sumPerKind(m *ebpf.Map) uint64 {
	if m == nil {
		return 0
	}
	var total uint64
	for key := uint32(0); key < kindMax; key++ {
		var v uint64
		if err := m.Lookup(&key, &v); err != nil {
			continue
		}
		total += v
	}
	return total
}

// Close releases the ringbuf reader, the links and the BPF objects. It is
// safe to call on a partially constructed Consumer, on a nil one, and more
// than once.
func (c *Consumer) Close() error {
	// Nil-receiver safe: Attach's cleanup defer runs on whatever the named
	// result holds, and a caller that kept a nil *Consumer from a failed
	// Attach must get an error path, not a panic.
	if c == nil {
		return nil
	}
	var errs []error
	// c.reader is closed first and outside mu, because closing it is what
	// wakes a Run blocked in Read, and Run takes mu to apply each batch. It is
	// deliberately NOT set to nil: Run reads the field without holding mu.
	// ringbuf.Reader's Close, Read and SetDeadline are safe to call
	// concurrently and a second Close is a no-op, so leaving the pointer in
	// place is both race-free and idempotent.
	if c.reader != nil {
		errs = append(errs, c.reader.Close())
	}
	// The links and objects come down under mu so Stats, which reads the
	// `dropped` map, cannot be looking at c.objs while it is being closed.
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.links {
		errs = append(errs, l.Close())
	}
	c.links = nil
	errs = append(errs, c.objs.Close())
	return errors.Join(errs...)
}
