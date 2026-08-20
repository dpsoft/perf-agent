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

	// GPUKernelNameMax mirrors GPU_KERNEL_NAME_MAX in shim/core/usdt_abi.h.
	GPUKernelNameMax = 256
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
