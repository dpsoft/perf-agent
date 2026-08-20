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
	"github.com/dpsoft/perf-agent/internal/bpfstack"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/dpsoft/perf-agent/internal/usdt"
	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
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

	// Symbolizer resolves the instruction pointers of a sampled launch's
	// captured stack into frames. Nil disables resolution entirely: the
	// captures still arrive and are still accounted for (each one counts in
	// Stats.SymbolizeFailed), and every launch still reaches the sink -
	// without a stack, which projects as unattributed GPU time rather than
	// as a guess.
	Symbolizer symbolize.Symbolizer

	// SampledStackCapacity bounds the correlation -> stack side table that
	// holds a resolved capture until its batched twin arrives. Zero means
	// defaultSampledStackCapacity. See pendingStacks for why this table
	// exists and why it must be bounded.
	SampledStackCapacity int

	// DeferredLaunchCapacity bounds how many launches may be held at once
	// waiting for a sampled twin that has not arrived yet. Zero means
	// defaultDeferredLaunchCapacity. See deferredLaunches.
	DeferredLaunchCapacity int

	// KernelNameCapacity bounds the kernel_id -> name table. Zero means
	// defaultKernelNameCapacity. A ceiling, not a dial: a workload has one
	// entry per distinct kernel, but nothing in the ABI promises that.
	KernelNameCapacity int

	// PendingNamedEventCapacity bounds how many launches and executions may
	// be held at once waiting for a kernel name that has not arrived yet.
	// Zero means defaultPendingNamedEventCapacity. See pendingNames.
	PendingNamedEventCapacity int
}

const (
	// defaultSampledStackCapacity bounds the parked-stack side table. A
	// parked stack is one whose launch has not arrived (or never will,
	// because the batch carrying it was dropped), so the table has to be
	// bounded or a profiled application feeds it without limit. 1024 is
	// generous next to the real occupancy - a capture normally waits less
	// than one batch - and still trivially small in memory.
	defaultSampledStackCapacity = 1024

	// defaultDeferredLaunchCapacity bounds the launches held for a possible
	// sampled twin. Only launches from the most recent batch are ever held
	// (see Consumer.applyBatch), and a shim batch is 32 records, so this is
	// several times the working set; it exists as a hard ceiling for a
	// producer that batches differently, not as a tuning dial.
	defaultDeferredLaunchCapacity = 256

	// defaultKernelNameCapacity bounds the interned-name table. Real
	// workloads have tens to low hundreds of distinct kernels; 4096 leaves
	// room for a generated or JIT-heavy one while keeping the table's worst
	// case at a few hundred KB.
	defaultKernelNameCapacity = 4096

	// defaultPendingNamedEventCapacity bounds the events held for a name
	// that has not arrived. The window it has to cover is the producer's
	// drain interval (100ms in the shim), so it is sized for a burst of
	// launches and execs rather than for steady state - in steady state the
	// name is already interned and nothing waits at all.
	defaultPendingNamedEventCapacity = 512
)

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
	// does not yet normalize: module loads and PC samples. Sampled launches
	// and interned kernel names are no longer in this class - the first has
	// its stack attached to the batched launch it belongs to, the second
	// names the launches and executions that refer to its kernel id.
	// Counted so the loss is visible rather than silent.
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
	// StacksResolved counts captures that made it all the way to frames:
	// the stackmap entry was there, it held instruction pointers, and the
	// symbolizer turned them into at least one frame. Every sampled launch
	// ends in exactly one of the counters below or in StacksMissing, so
	// they reconcile against SampledLaunches:
	//
	//	SampledLaunches = StacksMissing + StacksUncorrelated +
	//	                  StackLookupFailed + SymbolizeFailed + StacksResolved
	//	StacksResolved  = StacksAttached + StacksEvicted + PendingStacks
	//
	// (the second identity holds at rest; PendingStacks is a gauge). Both
	// are asserted by TestSampledStackAccountingReconciles.
	StacksResolved uint64
	// StackLookupFailed counts captures whose stackmap entry could not be
	// read back: the map lookup failed, or the entry decoded to no
	// instruction pointers.
	//
	// The expected cause is not a broken map. bpf_get_stackid is
	// content-addressed, so two launches from the same call site in flight
	// at once carry the SAME stack id; the consumer resolves the first and
	// deletes the entry (see freeStackLocked), and the second then finds
	// nothing. That is a real, bounded loss of attribution - the launch
	// still ships, without a stack - and it is counted here rather than
	// being papered over with a cached lookup, which could serve the frames
	// of a different stack that reused the id after the delete.
	StackLookupFailed uint64
	// SymbolizeFailed counts captures that were read back but produced no
	// frames: the symbolizer returned an error, returned nothing, or no
	// Symbolizer was configured at all. The launch is never dropped for
	// this - it degrades to no stack, which projects as unattributed GPU
	// time, and the degradation is visible here.
	SymbolizeFailed uint64
	// StackDeleteFailed counts stackmap entries the consumer read but could
	// not delete. See freeStackLocked: deletion is what stops the map
	// filling, so a rising count here is the early warning for capture
	// failures (StacksMissing) that follow.
	StackDeleteFailed uint64
	// StacksUncorrelated counts captures whose sampled launch carried a
	// wire correlation of zero. The stack is real, but the ABI's "no
	// correlation" leaves nothing to pair it with: the batched twin cannot
	// be identified, so the capture cannot be attached to any launch
	// without guessing which one it belongs to.
	StacksUncorrelated uint64
	// StacksAttached counts captures actually attached to a launch that
	// went to the sink - the number that ends up carrying attribution in
	// the profile, from both arrival orders.
	StacksAttached uint64
	// StacksEvicted counts resolved captures pushed out of the bounded
	// side table before their batched twin ever arrived, which happens when
	// the batch carrying that twin was dropped or when the table is too
	// small for the in-flight window. It is attribution loss, not record
	// loss: the launches themselves are unaffected.
	StacksEvicted uint64
	// KernelNamesLearned counts gpu_kernel_name_v1 records interned. The
	// producer replays its whole table on late attach, so a name may be
	// learned more than once for the same kernel; this counts records, not
	// distinct kernels (PendingNames is the gauge for the latter).
	KernelNamesLearned uint64
	// KernelNamesTruncated counts names the producer had to cut off at
	// GPU_KERNEL_NAME_MAX. Those names still resolve - marked, so a
	// truncated name is never presented as complete. See
	// truncatedNameSuffix.
	KernelNamesTruncated uint64
	// KernelNamesEvicted counts names pushed out of the bounded table. Not
	// record loss: events referring to that kernel still flow, unnamed, and
	// count in KernelNamesUnresolved.
	KernelNamesEvicted uint64
	// KernelNamesUnresolved counts launches and executions emitted with no
	// kernel name: the producer never named that kernel, the name was
	// evicted, or the event was released (by the queue's bound, or by a
	// Flush) before its name arrived. Without this counter an unnamed
	// execution in the profile is a mystery rather than a measurement.
	KernelNamesUnresolved uint64
	// PendingStacks, PendingLaunches, PendingNamedEvents and KnownKernelNames
	// are gauges, not counters: what the side tables hold right now.
	// PendingLaunches and PendingNamedEvents are also how much event
	// delivery the consumer is currently holding back - see Consumer.Flush.
	PendingStacks      int
	PendingLaunches    int
	PendingNamedEvents int
	KnownKernelNames   int
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

// stackStore is the slice of *ebpf.Map that sampled-stack resolution uses:
// read one stackmap entry, then delete it. It is a seam for the same reason
// batchReader is - creating a real BPF_MAP_TYPE_STACK_TRACE needs CAP_BPF,
// so without it the whole resolve/delete/attach path would be untestable -
// and *ebpf.Map satisfies it as-is.
type stackStore interface {
	LookupBytes(key any) ([]byte, error)
	Delete(key any) error
}

// Consumer owns the loaded BPF objects, the uprobe_multi link and the
// ringbuf reader for one shim.
type Consumer struct {
	cfg    Config
	objs   gpuusdtObjects
	links  []link.Link
	reader batchReader
	// stacks is c.objs.Stackmap behind the stackStore seam. Nil when no BPF
	// objects were loaded (every unit test), which makes every resolution
	// count as StackLookupFailed rather than panic.
	stacks stackStore

	mu          sync.Mutex
	seqByStream map[seqKey]uint64
	pending     *pendingStacks
	deferred    *deferredLaunches
	names       *kernelNameTable
	unnamed     *pendingNames
	// sawKernelName records whether this producer emits kernel names at
	// all. Holding an event for a name that is never coming would delay
	// every event a producer without the name probe ever sends, so nothing
	// waits until the producer has demonstrated, once, that names exist.
	sawKernelName bool
	stats         Stats
}

// newConsumer builds a Consumer with its side tables sized from cfg. Attach
// and the unit tests share it so a store can never be forgotten in one of
// the two paths.
func newConsumer(cfg Config) *Consumer {
	return &Consumer{
		cfg:         cfg,
		seqByStream: map[seqKey]uint64{},
		pending:     newPendingStacks(cfg.SampledStackCapacity),
		deferred:    newDeferredLaunches(cfg.DeferredLaunchCapacity),
		names:       newKernelNameTable(cfg.KernelNameCapacity),
		unnamed:     newPendingNames(cfg.PendingNamedEventCapacity),
	}
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

	c = newConsumer(cfg)
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

	// The stackmap is read (and each resolved entry deleted) by the sampled
	// stack path; see Consumer.resolveStackLocked.
	c.stacks = c.objs.Stackmap

	if c.reader, err = ringbuf.NewReader(c.objs.Events); err != nil {
		return
	}
	return c, nil
}

// errSampledBatchNotSingular means a gpu_launch_sampled_v1 batch arrived
// carrying anything other than exactly one record.
//
// The stack id lives in the batch header, one per batch. A batch of N sampled
// launches would therefore attribute one captured stack to N unrelated
// launches — silently, since every record still decodes perfectly. The BPF
// program caps this kind at one record per batch (max_records in
// bpf/gpu_usdt.bpf.c) and the decoder refuses anything else, so neither end
// relies on the other, or on the producer, to hold the invariant.
var errSampledBatchNotSingular = errors.New("gpuprobe: sampled-launch batch must carry exactly one record")

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
	// arrives with count == 1. That is enforced at both ends (the BPF
	// program's per-kind cap and decodeBatch), not assumed of the producer;
	// see errSampledBatchNotSingular. It is -1 on every other kind and must
	// only be interpreted for kindLaunchSampled.
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
		// One header, one stack id, one launch. See errSampledBatchNotSingular.
		if count != 1 {
			return batch{}, fmt.Errorf("%w: count=%d", errSampledBatchNotSingular, count)
		}
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
	// Nothing more will arrive once this loop ends, so no held launch can
	// still gain a stack. Releasing here (and not only in Close) is what
	// lets a caller cancel, wait for Run, and take a complete Snapshot
	// without closing the consumer.
	defer c.Flush()

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

	// A held launch is only ever waiting for a sampled record, and the
	// sampled record it is waiting for would be the very next thing off the
	// ringbuf (see sampledstacks.go). Anything else arriving means the wait
	// is over, so the queue is released here rather than left to age: the
	// timeline's join wants launches promptly, and a launch held past the
	// window buys nothing.
	if b.Kind != kindLaunchSampled {
		c.releaseDeferredLocked()
	}

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
					// CPUStack is filled in below if this launch was the one
					// in SamplePeriod the shim captured a stack for.
				},
			}
			c.admitLaunchLocked(ev, l.KernelID)
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
			c.emitExecLocked(ev, e.KernelID)
		}
	case kindLaunchSampled:
		// One capture per batch, counted once per batch: decodeBatch has
		// already refused any count but 1, so the loop below runs exactly
		// once, but counting the capture failure outside it means a future
		// change to that rule degrades into an obvious undercount rather
		// than multiplying one failure by the record count.
		if b.StackID < 0 {
			// A launch whose stack capture failed is still a launch: it is
			// counted here and carried on, never discarded.
			c.stats.StacksMissing++
		}
		for _, sl := range b.SampledLaunches {
			c.stats.Records++
			c.stats.SampledLaunches++
			// Never emitted as a launch of its own: it is the same launch as
			// the batched gpu_launch_v1 record with the same correlation, and
			// the timeline has no dedup that would catch the double. Only its
			// stack travels on, onto that batched twin.
			c.attachSampledStackLocked(b.PID, b.StackID, sl)
		}
	case kindKernelName:
		for _, n := range b.KernelNames {
			c.stats.Records++
			c.learnKernelNameLocked(n)
		}
	default:
		// Carried on the wire, not yet normalized. Counted, never silent.
		c.stats.Undecoded += uint64(b.RawCount)
	}
}

// admitLaunchLocked decides whether a normalized launch goes to the sink now
// or waits briefly for a sampled stack. Caller holds mu.
//
// Three cases, in the order they are cheapest to decide:
//
//   - No correlation. Pairing is by correlation and nothing else - matching
//     on pid/tid/time would be a guess dressed as a fact - so an
//     uncorrelated launch can never gain a stack and goes straight out.
//   - Its stack is already parked (the sampled record arrived first, the
//     common order). Attach and emit.
//   - Otherwise hold it: its sampled record may be the next ringbuf sample.
//     The queue releases on the next batch of any other kind, on Flush, when
//     Run returns and on Close, so "hold" is bounded by arrivals, not by
//     time, and never means "drop".
//
// The first two cases can therefore overtake a launch already held from the
// same batch. That reordering is bounded by one batch and is harmless to the
// consumers that exist: gpu.LaunchCache keys on correlation, orders eviction
// by insertion rather than by timestamp, and ignores a timestamp older than
// the newest it has seen (observeTimestampLocked), so an out-of-order Put
// neither displaces the wrong entry nor moves the horizon.
func (c *Consumer) admitLaunchLocked(ev gpu.GPUKernelLaunch, kernelID uint64) {
	if ev.Correlation == (gpu.CorrelationID{}) {
		c.emitLaunchLocked(ev, kernelID)
		return
	}
	if st, ok := c.pending.take(ev.Correlation); ok {
		c.attachLocked(&ev, st.frames, st.period)
		c.emitLaunchLocked(ev, kernelID)
		return
	}
	if released, ok := c.deferred.push(deferredLaunch{launch: ev, kernelID: kernelID}); ok {
		c.emitLaunchLocked(released.launch, released.kernelID)
	}
}

// attachSampledStackLocked resolves one sampled launch's captured stack and
// gets it onto the launch it belongs to - which is the batched
// gpu_launch_v1 record with the same correlation, not this record. Caller
// holds mu.
func (c *Consumer) attachSampledStackLocked(pid uint32, stackID int32, sl gpuabi.LaunchSampled) {
	if stackID < 0 {
		// bpf_get_stackid failed; already counted as StacksMissing by the
		// caller, and there is no entry to read or free.
		return
	}
	if sl.Correlation == 0 {
		// Unpairable: the ABI's "no correlation" leaves no way to say which
		// launch this stack belongs to, and attaching it to a plausible
		// neighbour would be a fabricated call path. The stackmap slot is
		// still ours to release.
		c.stats.StacksUncorrelated++
		c.freeStackLocked(stackID)
		return
	}
	frames, ok := c.resolveStackLocked(pid, stackID)
	if !ok {
		return // counted inside resolveStackLocked
	}
	c.stats.StacksResolved++

	corr := gpu.CorrelationID{Backend: c.cfg.Backend, Value: strconv.FormatUint(sl.Correlation, 10)}
	// The batched twin arrived first and is still being held: attach and let
	// it go now, since nothing more can arrive for it.
	if held, ok := c.deferred.take(corr); ok {
		c.attachLocked(&held.launch, frames, sl.SamplePeriod)
		c.emitLaunchLocked(held.launch, held.kernelID)
		return
	}
	// Otherwise the twin is still to come; park the stack for it.
	c.stats.StacksEvicted += uint64(c.pending.park(corr, frames, sl.SamplePeriod))
}

// attachLocked puts a resolved stack on a launch. SamplePeriod travels with
// the stack so a consumer can see how much GPU time one sampled call path
// stands for - it is NOT applied to the launch's own time anywhere in this
// pipeline. Caller holds mu.
func (c *Consumer) attachLocked(ev *gpu.GPUKernelLaunch, frames []pp.Frame, period uint32) {
	ev.Launch.CPUStack = frames
	ev.Launch.SamplePeriod = period
	c.stats.StacksAttached++
}

// resolveStackLocked turns a stackmap key into symbolized frames, exactly as
// profile/ does it: read the entry, extract the non-zero instruction
// pointers, symbolize them against the launching process, flatten to pprof
// frames. Caller holds mu.
//
// Two details differ from profile/:
//
//   - Order. ToProfFrames returns leaf-first; the gpu projection nests the
//     [gpu:launch] boundary and the kernel frame *under* the call path, so
//     the frames are reversed to root-first here, the same way profile/
//     reverses before handing frames to the pprof builder.
//   - Deletion. See freeStackLocked.
//
// Every failure path returns ok=false with a counter bumped, and the caller
// then ships the launch without a stack: a launch is never dropped because
// its stack could not be resolved.
func (c *Consumer) resolveStackLocked(pid uint32, stackID int32) ([]pp.Frame, bool) {
	if c.stacks == nil {
		c.stats.StackLookupFailed++
		return nil, false
	}
	raw, err := c.stacks.LookupBytes(uint32(stackID))
	if err != nil || len(raw) == 0 {
		c.stats.StackLookupFailed++
		return nil, false
	}
	ips := bpfstack.ExtractIPs(raw)
	// Free the slot as soon as its contents are in hand, before anything
	// that can fail: the entry is useless to us from here on either way.
	c.freeStackLocked(stackID)
	if len(ips) == 0 {
		c.stats.StackLookupFailed++
		return nil, false
	}
	if c.cfg.Symbolizer == nil {
		c.stats.SymbolizeFailed++
		return nil, false
	}
	// Kernel-range IPs are not split out the way profile/ does it. This
	// capture is taken at a uprobe on a USDT probe in the application's own
	// code, so the walked stack is user context by construction, unlike a
	// perf_event sample that can land mid-syscall. If that assumption ever
	// breaks, the symptom is unresolved frames, not misattributed ones.
	frames, err := c.cfg.Symbolizer.SymbolizeProcess(pid, ips)
	if err != nil {
		c.stats.SymbolizeFailed++
		return nil, false
	}
	out := symbolize.ToProfFrames(frames)
	if len(out) == 0 {
		c.stats.SymbolizeFailed++
		return nil, false
	}
	pp.Reverse(out)
	return out, true
}

// freeStackLocked deletes a stackmap entry once its contents have been read.
//
// This is not housekeeping, it is what keeps capture working. The BPF
// program calls bpf_get_stackid WITHOUT BPF_F_REUSE_STACKID
// (bpf/gpu_usdt.bpf.c), so a hash bucket already holding a live entry
// answers -EEXIST instead of overwriting it. Nothing else ever removes an
// entry, so in a continuously running consumer the map fills up and capture
// failures climb until every sampled launch reports StacksMissing - which
// reads exactly like a broken capture path rather than a full map. Deleting
// each entry as it is consumed keeps the map's occupancy proportional to
// what is in flight instead of to the length of the run.
//
// (profile/ does not need this and does not do it: it drains its stackmap
// once at the end of a fixed-length profiling run. The continuous consumer
// here is the case that does.)
//
// A delete failure is counted, not ignored: it is the leading indicator of
// the map filling. ErrKeyNotExist is possible and harmless - two captures of
// the identical stack share one id, so the first delete may already have
// removed it - and is counted the same way rather than special-cased, since
// it points at the same in-flight-duplicate situation StackLookupFailed
// reports. Caller holds mu.
func (c *Consumer) freeStackLocked(stackID int32) {
	if c.stacks == nil {
		return
	}
	if err := c.stacks.Delete(uint32(stackID)); err != nil {
		c.stats.StackDeleteFailed++
	}
}

// emitLaunchLocked names a launch and hands it to the sink, or holds it if
// its name has not arrived yet. Caller holds mu.
func (c *Consumer) emitLaunchLocked(ev gpu.GPUKernelLaunch, kernelID uint64) {
	name, ok := c.resolveKernelNameLocked(kernelID)
	if !ok && c.waitsForNameLocked(kernelID) {
		held := ev
		c.holdForNameLocked(unnamedEvent{kernelID: kernelID, launch: &held})
		return
	}
	ev.KernelName = name
	c.sinkLaunchLocked(ev, ok)
}

// emitExecLocked is emitLaunchLocked for an execution. Executions need the
// name at least as much as launches do: the projection's
// [gpu:kernel:<name>] frame is built from the *execution's* KernelName, and
// the timeline's heuristic join matches on it. Caller holds mu.
func (c *Consumer) emitExecLocked(ev gpu.GPUKernelExec, kernelID uint64) {
	name, ok := c.resolveKernelNameLocked(kernelID)
	if !ok && c.waitsForNameLocked(kernelID) {
		held := ev
		c.holdForNameLocked(unnamedEvent{kernelID: kernelID, exec: &held})
		return
	}
	ev.KernelName = name
	c.sinkExecLocked(ev, ok)
}

// sinkLaunchLocked and sinkExecLocked are the only two places an event
// reaches the sink. named reports whether a kernel name was resolved; an
// unnamed event is delivered anyway - a missing name must never cost a
// record - and counted so it is visible rather than mysterious. Caller
// holds mu.
func (c *Consumer) sinkLaunchLocked(ev gpu.GPUKernelLaunch, named bool) {
	if !named {
		c.stats.KernelNamesUnresolved++
	}
	if err := c.cfg.Sink.EmitLaunch(ev); err != nil {
		c.stats.SinkRejected++
	}
}

func (c *Consumer) sinkExecLocked(ev gpu.GPUKernelExec, named bool) {
	if !named {
		c.stats.KernelNamesUnresolved++
	}
	if err := c.cfg.Sink.EmitExec(ev); err != nil {
		c.stats.SinkRejected++
	}
}

// resolveKernelNameLocked looks a kernel id up in the interned table. A
// kernel id of zero is the ABI's "no kernel", so it never resolves and
// never waits. Caller holds mu.
func (c *Consumer) resolveKernelNameLocked(kernelID uint64) (string, bool) {
	if kernelID == 0 {
		return "", false
	}
	k, ok := c.names.get(kernelID)
	if !ok {
		return "", false
	}
	return k.resolved(), true
}

// waitsForNameLocked decides whether an event with an unknown kernel id is
// worth holding. Only if this producer emits names at all: otherwise every
// event from a producer without the name probe would be held to its bound
// and released late, buying nothing. Caller holds mu.
func (c *Consumer) waitsForNameLocked(kernelID uint64) bool {
	return kernelID != 0 && c.sawKernelName
}

// holdForNameLocked queues an event until its name arrives, releasing the
// oldest held event if the queue is full. Caller holds mu.
func (c *Consumer) holdForNameLocked(ev unnamedEvent) {
	if released, ok := c.unnamed.push(ev); ok {
		c.releaseUnnamedLocked(released, "")
	}
}

// releaseUnnamedLocked sends one held event on, with the name if one was
// found and without it otherwise. Caller holds mu.
func (c *Consumer) releaseUnnamedLocked(ev unnamedEvent, name string) {
	switch {
	case ev.launch != nil:
		ev.launch.KernelName = name
		c.sinkLaunchLocked(*ev.launch, name != "")
	case ev.exec != nil:
		ev.exec.KernelName = name
		c.sinkExecLocked(*ev.exec, name != "")
	}
}

// learnKernelNameLocked interns one name and releases everything waiting on
// it. Caller holds mu.
func (c *Consumer) learnKernelNameLocked(rec gpuabi.KernelName) {
	c.stats.KernelNamesLearned++
	c.sawKernelName = true
	if rec.Truncated {
		c.stats.KernelNamesTruncated++
	}
	k := kernelName{name: rec.Name, truncated: rec.Truncated}
	c.stats.KernelNamesEvicted += uint64(c.names.put(rec.KernelID, k))
	// A name that arrives empty resolves nothing, so held events would be
	// released as unnamed anyway; releasing them here keeps that decision
	// in one place.
	name := k.resolved()
	for _, waiting := range c.unnamed.takeByKernel(rec.KernelID) {
		c.releaseUnnamedLocked(waiting, name)
	}
}

// releaseDeferredLocked emits every launch being held for a sampled stack,
// oldest first. They may then be held again for a kernel name, which
// releaseUnnamedAllLocked resolves - Flush runs the two in that order.
// Caller holds mu.
func (c *Consumer) releaseDeferredLocked() {
	for _, held := range c.deferred.drain() {
		c.emitLaunchLocked(held.launch, held.kernelID)
	}
}

// releaseUnnamedAllLocked sends on every event still waiting for a name,
// unnamed and counted. Caller holds mu.
func (c *Consumer) releaseUnnamedAllLocked() {
	for _, ev := range c.unnamed.drain() {
		c.releaseUnnamedLocked(ev, "")
	}
}

// Flush releases every launch the consumer is holding back for a possible
// sampled stack, in arrival order.
//
// Run calls it on the way out and Close calls it too, so a consumer that is
// finished never leaves a launch behind. It is exported for the other case:
// a caller taking a Snapshot while the consumer is still running would
// otherwise miss up to DeferredLaunchCapacity of the most recent launches -
// held, not lost, but absent from that snapshot and therefore able to make
// their executions look unmatched. Call Flush immediately before such a
// snapshot.
func (c *Consumer) Flush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Order matters: releasing a held launch can put it straight into the
	// name queue, so the launches go first and the name queue is drained
	// after, leaving nothing behind in either.
	c.releaseDeferredLocked()
	c.releaseUnnamedAllLocked()
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
	// Gauges, read fresh: what the two side tables are holding right now.
	out.PendingStacks = c.pending.len()
	out.PendingLaunches = c.deferred.len()
	out.PendingNamedEvents = c.unnamed.len()
	out.KnownKernelNames = c.names.len()
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
	// Any launch still being held for a sampled stack goes to the sink
	// before the objects come down: the wait is over, and a held launch
	// dropped at teardown would be silent record loss - the one thing this
	// consumer must never do. Flush takes mu itself, so it runs before the
	// lock below rather than inside it.
	c.Flush()
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
