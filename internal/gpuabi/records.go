// Package gpuabi mirrors the frozen USDT record layouts in
// shim/core/usdt_abi.h. The two must agree byte for byte; the sizes are
// asserted on both sides.
package gpuabi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Version = 1

	SizeLaunch        = 48
	SizeExec          = 48
	SizeModuleLoad    = 40
	SizePCSample      = 40
	SizeConfig        = 24
	SizeDropped       = 16
	SizeLaunchSampled = 56
	SizeKernelName    = 272

	// SizeStallReason and SizeSamplingWindow are the two probes added for PC
	// sampling. Neither mutates a frozen record; both are new probes.
	SizeStallReason    = 136
	SizeSamplingWindow = 24

	// GPUKernelNameMax mirrors GPU_KERNEL_NAME_MAX in shim/core/usdt_abi.h.
	GPUKernelNameMax = 256

	// GPUStallNameMax mirrors GPU_STALL_NAME_MAX in shim/core/usdt_abi.h,
	// which is CUPTI's CUPTI_STALL_REASON_STRING_SIZE.
	GPUStallNameMax = 128
)

// Sampling modes for SamplingWindow.Mode, mirroring GPU_SAMPLING_MODE_* in
// shim/core/usdt_abi.h. Zero is not a mode: it means the producer left the
// field unset, which the consumer must treat as unknown rather than as
// continuous.
const (
	SamplingModeContinuous       uint8 = 1
	SamplingModeKernelSerialized uint8 = 2
)

// ErrShortRecord means the buffer is smaller than the record it must hold.
var ErrShortRecord = errors.New("gpuabi: buffer shorter than record")

// ErrInvalidSamplePeriod means a sampled launch declared a zero denominator.
// Scaling by it would divide by zero, so it is rejected at the boundary.
var ErrInvalidSamplePeriod = errors.New("gpuabi: sampled launch has zero sample_period")

type Launch struct {
	Correlation uint64
	KernelID    uint64
	QueueID     uint64
	ContextID   uint64
	TimeNs      uint64
	TID         uint32
}

type Exec struct {
	Correlation uint64
	KernelID    uint64
	QueueID     uint64
	DeviceID    uint64
	StartNs     uint64
	EndNs       uint64
}

type ModuleLoad struct {
	CubinCRC  uint64
	ModuleID  uint64
	SizeBytes uint64
	LoadNs    uint64
	BytesPtr  uint64
}

type PCSample struct {
	CubinCRC      uint64
	Correlation   uint64
	PCOffset      uint64
	FunctionIndex uint32
	StallIndex    uint32
	Count         uint32
}

type Dropped struct {
	Count uint64
	Class uint8
}

// The classes Dropped.Class may carry, mirroring GPU_DROP_CLASS_* in
// shim/core/usdt_abi.h. Wire constants: append only, never renumber.
//
// gpu_dropped_v1 has been in the ABI header since Phase 3 with no enum, no
// kind and no probe, so no producer-side loss has ever reached a consumer.
// These are the first four classes, and they exist because Tier B PC sampling
// loses records in three distinguishable ways plus one that is not loss at
// all — see DropClassPCNonUserKernel.
const (
	// DropClassInvalid is what a producer that memset a record and forgot to
	// set the class lands on. It is deliberately not a real class, so such a
	// record cannot be filed under somebody else's heading.
	DropClassInvalid uint8 = 0
	// DropClassPCDroppedHW is CUpti_PCSamplingData.droppedSamples: samples the
	// hardware discarded under backpressure. The action is to lower the
	// sampling frequency.
	DropClassPCDroppedHW uint8 = 1
	// DropClassPCBufferFull is CUpti_PCSamplingData.hardwareBufferFull, and
	// also the CUPTI_ERROR_OUT_OF_MEMORY return from cuptiPCSamplingGetData.
	// The action is to raise the hardware buffer size or drain more often.
	DropClassPCBufferFull uint8 = 2
	// DropClassPCNonUserKernel is CUpti_PCSamplingData.nonUsrKernelsTotalSamples.
	//
	// It is NOT loss in the sense the other two are: nothing was dropped,
	// CUPTI simply "does not provide PC records for non-user kernels". It
	// rides here because it is the SIZE of a structural omission, and a reader
	// who does not know how much of the device's sampled time this mechanism
	// cannot see would read the rest as more complete than it is. No action
	// follows from it; it is a completeness bound.
	DropClassPCNonUserKernel uint8 = 3
	// DropClassGraphExec counts executions launched from a CUDA graph. One
	// graph launch fires one launch callback for N kernels, and gpu_exec_v1
	// has no field for graphId, so the exact-correlation join silently becomes
	// one-to-many. Nothing else in the pipeline detects it.
	DropClassGraphExec uint8 = 4
)

// DropClassName renders a class for an operator. An unknown class is rendered
// with its number rather than as a blank or a guess: a producer newer than
// this consumer is a real case, and dropping its losses on the floor would be
// the one thing this record exists to prevent.
func DropClassName(c uint8) string {
	switch c {
	case DropClassInvalid:
		return "unset"
	case DropClassPCDroppedHW:
		return "pc-dropped-hw"
	case DropClassPCBufferFull:
		return "pc-buffer-full"
	case DropClassPCNonUserKernel:
		return "pc-non-user-kernel"
	case DropClassGraphExec:
		return "graph-exec"
	}
	return fmt.Sprintf("unknown-class-%d", c)
}

func DecodeLaunch(b []byte) (Launch, error) {
	if len(b) < SizeLaunch {
		return Launch{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return Launch{
		Correlation: le.Uint64(b[0:]),
		KernelID:    le.Uint64(b[8:]),
		QueueID:     le.Uint64(b[16:]),
		ContextID:   le.Uint64(b[24:]),
		TimeNs:      le.Uint64(b[32:]),
		TID:         le.Uint32(b[40:]),
	}, nil
}

func DecodeExec(b []byte) (Exec, error) {
	if len(b) < SizeExec {
		return Exec{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return Exec{
		Correlation: le.Uint64(b[0:]),
		KernelID:    le.Uint64(b[8:]),
		QueueID:     le.Uint64(b[16:]),
		DeviceID:    le.Uint64(b[24:]),
		StartNs:     le.Uint64(b[32:]),
		EndNs:       le.Uint64(b[40:]),
	}, nil
}

func DecodeModuleLoad(b []byte) (ModuleLoad, error) {
	if len(b) < SizeModuleLoad {
		return ModuleLoad{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return ModuleLoad{
		CubinCRC:  le.Uint64(b[0:]),
		ModuleID:  le.Uint64(b[8:]),
		SizeBytes: le.Uint64(b[16:]),
		LoadNs:    le.Uint64(b[24:]),
		BytesPtr:  le.Uint64(b[32:]),
	}, nil
}

func DecodePCSample(b []byte) (PCSample, error) {
	if len(b) < SizePCSample {
		return PCSample{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return PCSample{
		CubinCRC:      le.Uint64(b[0:]),
		Correlation:   le.Uint64(b[8:]),
		PCOffset:      le.Uint64(b[16:]),
		FunctionIndex: le.Uint32(b[24:]),
		StallIndex:    le.Uint32(b[28:]),
		Count:         le.Uint32(b[32:]),
	}, nil
}

// Config is the producer's sampling configuration, emitted once per process.
// It has been on the wire since Phase 3 with no decoder; nothing reads its
// fields yet.
type Config struct {
	ClockHz        uint64
	SamplingFactor uint32
	SMCount        uint32
	Vendor         uint8
}

// StallReason is one entry of the device's index -> name stall table.
//
// Index is the vendor's own and is not stable across devices or driver
// versions. Name is the only portable identity a stall reason has, which is
// why an unresolved index must never be rendered into a label as
// "stall#<n>": that would put an unstable internal number in front of a
// human as though it meant something.
type StallReason struct {
	Index     uint32
	Name      string
	Truncated bool
}

// SamplingWindow is one PC-sampling burst.
//
// EndNs == 0 means the window was still open when the producer stopped
// reporting — a hard exit mid-burst — and is NOT a zero-length window. See
// Open.
type SamplingWindow struct {
	StartNs uint64
	EndNs   uint64
	Mode    uint8

	// NextStartDeltaMs, on a closed window, is how long after EndNs the
	// producer guarantees no further burst can open. Zero means the producer
	// did not say — which is what an older producer's zero padding decodes to,
	// so it is also the safe reading. QuietNever means never again.
	//
	// It is what lets the tail of a run be answered "not serialized" rather
	// than "unknown": without it a missing open record and a genuine gap are
	// indistinguishable, so everything after the last known burst has to be
	// conceded. See the windowStore's coverage logic.
	NextStartDeltaMs uint32
}

// QuietNever is NextStartDeltaMs meaning the producer has refused all further
// bursts for the life of the process — teardown, or the CUDA-graph refusal.
const QuietNever uint32 = 0xFFFFFFFF

// Open reports whether the window never closed. An execution at or after an
// open window's StartNs is "unknown", never "not serialized": the producer
// stopped reporting, it did not report that sampling had stopped.
func (w SamplingWindow) Open() bool { return w.EndNs == 0 }

// ErrWindowInverted means a sampling window ended before it started. It is a
// producer contract violation rather than a short buffer, so it is a distinct
// error: silently accepting it would let a negative duration reach the
// serialization disclosure, where it would mark an arbitrary set of
// executions perturbed.
var ErrWindowInverted = errors.New("gpuabi: sampling window ends before it starts")

func DecodeConfig(b []byte) (Config, error) {
	if len(b) < SizeConfig {
		return Config{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return Config{
		ClockHz:        le.Uint64(b[0:]),
		SamplingFactor: le.Uint32(b[8:]),
		SMCount:        le.Uint32(b[12:]),
		Vendor:         b[16],
	}, nil
}

// DecodeStallReason reads one index -> name entry.
//
// name_len is authoritative but producer-supplied, so it is range-checked
// before it indexes the fixed-size buffer. Without the check a hostile or
// merely buggy producer sets name_len to 65535 and the slice expression
// panics inside the ringbuf drain goroutine, taking the consumer down — the
// record is 136 bytes on the wire whatever the field says.
func DecodeStallReason(b []byte) (StallReason, error) {
	if len(b) < SizeStallReason {
		return StallReason{}, ErrShortRecord
	}
	le := binary.LittleEndian
	n := int(le.Uint16(b[4:]))
	if n > GPUStallNameMax {
		return StallReason{}, fmt.Errorf("gpuabi: stall name length %d exceeds %d", n, GPUStallNameMax)
	}
	return StallReason{
		Index:     le.Uint32(b[0:]),
		Name:      string(b[8 : 8+n]),
		Truncated: b[6] != 0,
	}, nil
}

func DecodeSamplingWindow(b []byte) (SamplingWindow, error) {
	if len(b) < SizeSamplingWindow {
		return SamplingWindow{}, ErrShortRecord
	}
	le := binary.LittleEndian
	out := SamplingWindow{
		StartNs: le.Uint64(b[0:]),
		EndNs:   le.Uint64(b[8:]),
		Mode:    b[16],
		// b[17:20] is padding. The field rides in the tail of the record's
		// existing 7 padding bytes, so the wire size is unchanged at 24 and
		// REC_SAMPLING_WINDOW in the BPF side needs no version bump.
		NextStartDeltaMs: le.Uint32(b[20:]),
	}
	// Meaningless on an open window, and a producer that sets it there is
	// confused rather than informative. Dropped rather than trusted.
	if out.EndNs == 0 {
		out.NextStartDeltaMs = 0
	}
	// EndNs == 0 is the encoded "still open" case, not an inversion.
	if out.EndNs != 0 && out.EndNs < out.StartNs {
		return SamplingWindow{}, fmt.Errorf("%w: start=%d end=%d", ErrWindowInverted, out.StartNs, out.EndNs)
	}
	return out, nil
}

func DecodeDropped(b []byte) (Dropped, error) {
	if len(b) < SizeDropped {
		return Dropped{}, ErrShortRecord
	}
	return Dropped{Count: binary.LittleEndian.Uint64(b[0:]), Class: b[8]}, nil
}

type LaunchSampled struct {
	Correlation  uint64
	KernelID     uint64
	QueueID      uint64
	ContextID    uint64
	TimeNs       uint64
	TID          uint32
	SamplePeriod uint32
	LaunchSeq    uint64
}

type KernelName struct {
	KernelID  uint64
	Name      string
	Truncated bool
}

func DecodeLaunchSampled(b []byte) (LaunchSampled, error) {
	if len(b) < SizeLaunchSampled {
		return LaunchSampled{}, ErrShortRecord
	}
	le := binary.LittleEndian
	out := LaunchSampled{
		Correlation:  le.Uint64(b[0:]),
		KernelID:     le.Uint64(b[8:]),
		QueueID:      le.Uint64(b[16:]),
		ContextID:    le.Uint64(b[24:]),
		TimeNs:       le.Uint64(b[32:]),
		TID:          le.Uint32(b[40:]),
		SamplePeriod: le.Uint32(b[44:]),
		LaunchSeq:    le.Uint64(b[48:]),
	}
	if out.SamplePeriod == 0 {
		return LaunchSampled{}, ErrInvalidSamplePeriod
	}
	return out, nil
}

func DecodeKernelName(b []byte) (KernelName, error) {
	if len(b) < SizeKernelName {
		return KernelName{}, ErrShortRecord
	}
	le := binary.LittleEndian
	n := int(le.Uint16(b[8:]))
	if n > GPUKernelNameMax {
		return KernelName{}, fmt.Errorf("gpuabi: kernel name length %d exceeds %d", n, GPUKernelNameMax)
	}
	return KernelName{
		KernelID:  le.Uint64(b[0:]),
		Name:      string(b[16 : 16+n]),
		Truncated: b[10] != 0,
	}, nil
}
