package gpuabi

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeLaunchMatchesTheFrozenLayout(t *testing.T) {
	// correlation, kernel_id, queue_id, context_id, time_ns, tid, _pad
	require.Equal(t, 48, SizeLaunch, "gpu_launch_v1 is 48 bytes; changing it is an ABI break")

	b := make([]byte, SizeLaunch)
	binary.LittleEndian.PutUint64(b[0:], 0x1122334455667788)
	binary.LittleEndian.PutUint64(b[8:], 0xAAAA)
	binary.LittleEndian.PutUint64(b[16:], 7)
	binary.LittleEndian.PutUint64(b[24:], 3)
	binary.LittleEndian.PutUint64(b[32:], 1_000_000_000)
	binary.LittleEndian.PutUint32(b[40:], 4242)

	got, err := DecodeLaunch(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x1122334455667788), got.Correlation)
	assert.Equal(t, uint64(0xAAAA), got.KernelID)
	assert.Equal(t, uint64(7), got.QueueID)
	assert.Equal(t, uint64(3), got.ContextID)
	assert.Equal(t, uint64(1_000_000_000), got.TimeNs)
	assert.Equal(t, uint32(4242), got.TID)
}

func TestDecodeLaunchRejectsShortBuffer(t *testing.T) {
	_, err := DecodeLaunch(make([]byte, SizeLaunch-1))
	require.ErrorIs(t, err, ErrShortRecord)
}

func TestDecodeExecMatchesTheFrozenLayout(t *testing.T) {
	require.Equal(t, 48, SizeExec)

	b := make([]byte, SizeExec)
	binary.LittleEndian.PutUint64(b[0:], 9)
	binary.LittleEndian.PutUint64(b[8:], 0xBBBB)
	binary.LittleEndian.PutUint64(b[16:], 2)
	binary.LittleEndian.PutUint64(b[24:], 1)
	binary.LittleEndian.PutUint64(b[32:], 100)
	binary.LittleEndian.PutUint64(b[40:], 250)

	got, err := DecodeExec(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(9), got.Correlation)
	assert.Equal(t, uint64(1), got.DeviceID)
	assert.Equal(t, uint64(100), got.StartNs)
	assert.Equal(t, uint64(250), got.EndNs)
}

// The spike settled that PC samples key on the cubin CRC, not a module id,
// and carry no correlation in continuous collection (spec §6.3 finding 3).
func TestDecodePCSampleKeysOnCubinCRC(t *testing.T) {
	require.Equal(t, 40, SizePCSample)

	b := make([]byte, SizePCSample)
	binary.LittleEndian.PutUint64(b[0:], 0x45dfed61)
	binary.LittleEndian.PutUint64(b[8:], 0) // correlation unknown in continuous mode
	binary.LittleEndian.PutUint64(b[16:], 0x2f0)
	binary.LittleEndian.PutUint32(b[24:], 11)
	binary.LittleEndian.PutUint32(b[28:], 28)
	binary.LittleEndian.PutUint32(b[32:], 60)

	got, err := DecodePCSample(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x45dfed61), got.CubinCRC)
	assert.Zero(t, got.Correlation, "zero correlation is legal on a PC sample")
	assert.Equal(t, uint64(0x2f0), got.PCOffset)
	assert.Equal(t, uint32(11), got.FunctionIndex)
	assert.Equal(t, uint32(28), got.StallIndex)
	assert.Equal(t, uint32(60), got.Count)
}

func TestDecodeModuleLoadCarriesCubinCRCFirst(t *testing.T) {
	require.Equal(t, 40, SizeModuleLoad)

	b := make([]byte, SizeModuleLoad)
	binary.LittleEndian.PutUint64(b[0:], 0x45dfed61)
	binary.LittleEndian.PutUint64(b[8:], 22)
	got, err := DecodeModuleLoad(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x45dfed61), got.CubinCRC, "cubin_crc leads: PC samples join on it, not module_id")
	assert.Equal(t, uint64(22), got.ModuleID)
}

func TestDecodeLaunchSampledCarriesTheSamplingDenominator(t *testing.T) {
	require.Equal(t, 56, SizeLaunchSampled, "gpu_launch_sampled_v1 is 56 bytes")

	b := make([]byte, SizeLaunchSampled)
	le := binary.LittleEndian
	le.PutUint64(b[0:], 7)      // correlation
	le.PutUint64(b[8:], 0xAAAA) // kernel_id
	le.PutUint64(b[16:], 3)     // queue_id
	le.PutUint64(b[24:], 1)     // context_id
	le.PutUint64(b[32:], 500)   // time_ns
	le.PutUint32(b[40:], 4242)  // tid
	le.PutUint32(b[44:], 64)    // sample_period
	le.PutUint64(b[48:], 99)    // launch_seq

	got, err := DecodeLaunchSampled(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), got.Correlation)
	assert.Equal(t, uint32(4242), got.TID)
	assert.Equal(t, uint32(64), got.SamplePeriod, "the N in one-in-N, per record")
	assert.Equal(t, uint64(99), got.LaunchSeq)
}

func TestDecodeLaunchSampledRejectsZeroSamplePeriod(t *testing.T) {
	b := make([]byte, SizeLaunchSampled)
	binary.LittleEndian.PutUint64(b[0:], 7)
	// sample_period left zero
	_, err := DecodeLaunchSampled(b)
	require.ErrorIs(t, err, ErrInvalidSamplePeriod,
		"a zero denominator would make the scale factor a division by zero")
}

func TestDecodeKernelNameTruncatesAtTheDeclaredLength(t *testing.T) {
	require.Equal(t, 272, SizeKernelName)

	b := make([]byte, SizeKernelName)
	binary.LittleEndian.PutUint64(b[0:], 0xAAAA) // kernel_id
	binary.LittleEndian.PutUint16(b[8:], 5)      // name_len
	copy(b[16:], "_Z4kAddPfi")                   // 10 bytes present, 5 declared

	got, err := DecodeKernelName(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0xAAAA), got.KernelID)
	assert.Equal(t, "_Z4kA", got.Name, "name_len is authoritative, not the NUL")
	assert.False(t, got.Truncated)
}

func TestDecodeKernelNameFlagsTruncation(t *testing.T) {
	b := make([]byte, SizeKernelName)
	binary.LittleEndian.PutUint16(b[8:], uint16(GPUKernelNameMax))
	b[10] = 1 // truncated flag
	for i := range GPUKernelNameMax {
		b[16+i] = 'x'
	}
	got, err := DecodeKernelName(b)
	require.NoError(t, err)
	assert.True(t, got.Truncated, "a truncated name must be visible, not silently short")
	assert.Len(t, got.Name, GPUKernelNameMax)
}

func TestDecodeKernelNameRejectsLengthPastTheBuffer(t *testing.T) {
	b := make([]byte, SizeKernelName)
	binary.LittleEndian.PutUint16(b[8:], uint16(GPUKernelNameMax+1))
	_, err := DecodeKernelName(b)
	require.Error(t, err, "a length past the fixed array must not read out of bounds")
}

// ----- The PC-sampling additions: gpu_stall_reason_map_v1,
// gpu_sampling_window_v1, and gpu_config_v1's first decoder.

func TestDecodeStallReasonMatchesTheWireLayout(t *testing.T) {
	require.Equal(t, 136, SizeStallReason, "gpu_stall_reason_map_v1 is 136 bytes")

	b := make([]byte, SizeStallReason)
	binary.LittleEndian.PutUint32(b[0:], 17) // index
	binary.LittleEndian.PutUint16(b[4:], 16) // name_len
	copy(b[8:], "long_scoreboardEXTRA")      // 20 bytes present, 16 declared

	got, err := DecodeStallReason(b)
	require.NoError(t, err)
	assert.Equal(t, uint32(17), got.Index)
	assert.Equal(t, "long_scoreboard", got.Name[:15])
	assert.Len(t, got.Name, 16, "name_len is authoritative, not the NUL")
	assert.False(t, got.Truncated)
}

func TestDecodeStallReasonFlagsTruncation(t *testing.T) {
	b := make([]byte, SizeStallReason)
	binary.LittleEndian.PutUint16(b[4:], uint16(GPUStallNameMax))
	b[6] = 1 // truncated
	for i := range GPUStallNameMax {
		b[8+i] = 'x'
	}
	got, err := DecodeStallReason(b)
	require.NoError(t, err)
	assert.True(t, got.Truncated, "a truncated stall name must be visible, not silently short")
	assert.Len(t, got.Name, GPUStallNameMax)
}

// The decoder reads name_len straight out of a producer-supplied field and
// then uses it to slice a fixed 128-byte array. A length past the end must
// come back as an error: a panic here happens on the consumer's ringbuf drain
// goroutine and takes the whole consumer down, which is a far worse outcome
// than one refused record. The record is 136 bytes on the wire whatever the
// field claims, so there is never anything past the array to read.
func TestDecodeStallReasonRejectsLengthPastTheBuffer(t *testing.T) {
	for _, n := range []uint16{uint16(GPUStallNameMax) + 1, 200, 0xFFFF} {
		b := make([]byte, SizeStallReason)
		binary.LittleEndian.PutUint16(b[4:], n)
		require.NotPanicsf(t, func() {
			_, err := DecodeStallReason(b)
			require.Errorf(t, err, "name_len=%d must be refused", n)
		}, "name_len=%d must error, never slice out of range", n)
	}

	// The boundary itself is legal: exactly GPUStallNameMax fills the array.
	b := make([]byte, SizeStallReason)
	binary.LittleEndian.PutUint16(b[4:], uint16(GPUStallNameMax))
	_, err := DecodeStallReason(b)
	require.NoError(t, err, "a full-length name is not an overrun")
}

func TestDecodeStallReasonRejectsShortBuffer(t *testing.T) {
	_, err := DecodeStallReason(make([]byte, SizeStallReason-1))
	require.ErrorIs(t, err, ErrShortRecord)
}

func TestDecodeSamplingWindowMatchesTheWireLayout(t *testing.T) {
	require.Equal(t, 24, SizeSamplingWindow, "gpu_sampling_window_v1 is 24 bytes")

	b := make([]byte, SizeSamplingWindow)
	binary.LittleEndian.PutUint64(b[0:], 1_000_000_000)
	binary.LittleEndian.PutUint64(b[8:], 1_050_000_000)
	b[16] = SamplingModeKernelSerialized

	got, err := DecodeSamplingWindow(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(1_000_000_000), got.StartNs)
	assert.Equal(t, uint64(1_050_000_000), got.EndNs)
	assert.Equal(t, SamplingModeKernelSerialized, got.Mode)
	assert.False(t, got.Open(), "a window with an end is closed")
}

// end_ns == 0 encodes "the producer stopped reporting mid-burst", which is a
// different fact from a zero-length window and must survive decode as such.
// Everything at or after such a window's start is "unknown", never "not
// serialized", so collapsing the two would let perturbed executions be
// reported as clean ones.
func TestDecodeSamplingWindowKeepsAnOpenWindowOpen(t *testing.T) {
	b := make([]byte, SizeSamplingWindow)
	binary.LittleEndian.PutUint64(b[0:], 1_000_000_000)
	binary.LittleEndian.PutUint64(b[8:], 0)
	b[16] = SamplingModeContinuous

	got, err := DecodeSamplingWindow(b)
	require.NoError(t, err, "an open window is a valid record, not a malformed one")
	assert.True(t, got.Open())
	assert.Equal(t, uint64(0), got.EndNs)
	assert.Equal(t, uint64(1_000_000_000), got.StartNs)
}

// An inverted window would produce a negative duration in the serialization
// disclosure and mark an arbitrary set of executions perturbed. It is a
// producer contract violation and gets its own error rather than being
// quietly normalized.
func TestDecodeSamplingWindowRejectsAnInvertedWindow(t *testing.T) {
	b := make([]byte, SizeSamplingWindow)
	binary.LittleEndian.PutUint64(b[0:], 2_000)
	binary.LittleEndian.PutUint64(b[8:], 1_000)
	_, err := DecodeSamplingWindow(b)
	require.ErrorIs(t, err, ErrWindowInverted)
}

func TestDecodeSamplingWindowRejectsShortBuffer(t *testing.T) {
	_, err := DecodeSamplingWindow(make([]byte, SizeSamplingWindow-1))
	require.ErrorIs(t, err, ErrShortRecord)
}

// Mode 0 is not a mode. The producer leaving the field unset must stay
// distinguishable from it declaring continuous collection, or an unconfigured
// burst reads as a non-serializing one.
func TestSamplingModesAreNonZeroAndDistinct(t *testing.T) {
	assert.NotZero(t, SamplingModeContinuous)
	assert.NotZero(t, SamplingModeKernelSerialized)
	assert.NotEqual(t, SamplingModeContinuous, SamplingModeKernelSerialized)
}

func TestDecodeConfigMatchesTheWireLayout(t *testing.T) {
	require.Equal(t, 24, SizeConfig)

	b := make([]byte, SizeConfig)
	binary.LittleEndian.PutUint64(b[0:], 1_695_000_000)
	binary.LittleEndian.PutUint32(b[8:], 5)
	binary.LittleEndian.PutUint32(b[12:], 82) // GA102 SM count
	b[16] = 1

	got, err := DecodeConfig(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(1_695_000_000), got.ClockHz)
	assert.Equal(t, uint32(5), got.SamplingFactor)
	assert.Equal(t, uint32(82), got.SMCount)
	assert.Equal(t, uint8(1), got.Vendor)
}

func TestDecodeConfigRejectsShortBuffer(t *testing.T) {
	_, err := DecodeConfig(make([]byte, SizeConfig-1))
	require.ErrorIs(t, err, ErrShortRecord)
}

// gpu_dropped_v1 reaches a consumer for the first time in this phase. Its two
// fields are read by hard-coded offset, so a klass that moved would decode as
// part of the count: a drop storm would read as a plausible number under class
// 0 rather than as an error anywhere.
func TestDecodeDroppedMatchesTheWireLayout(t *testing.T) {
	b := make([]byte, SizeDropped)
	binary.LittleEndian.PutUint64(b[0:], 0x1122334455667788)
	b[8] = DropClassPCNonUserKernel

	got, err := DecodeDropped(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x1122334455667788), got.Count)
	assert.Equal(t, DropClassPCNonUserKernel, got.Class)
}

func TestDecodeDroppedRejectsShortBuffer(t *testing.T) {
	_, err := DecodeDropped(make([]byte, SizeDropped-1))
	assert.ErrorIs(t, err, ErrShortRecord)
}

// Every class is distinct and non-zero, and zero stays reserved. A producer
// that memsets a record and forgets to set the class must not land on a real
// class and have its loss filed under somebody else's heading.
func TestDropClassesAreDistinctAndNonZero(t *testing.T) {
	seen := map[uint8]string{}
	for _, c := range []uint8{
		DropClassPCDroppedHW, DropClassPCBufferFull,
		DropClassPCNonUserKernel, DropClassGraphExec,
	} {
		require.NotZero(t, c, "zero is reserved for an unset class")
		prev, dup := seen[c]
		require.Falsef(t, dup, "class %d is shared with %s", c, prev)
		seen[c] = DropClassName(c)
	}
	assert.Equal(t, "unset", DropClassName(DropClassInvalid))
	// A producer newer than this consumer is a real case; its losses must
	// still render as losses rather than vanish or be guessed at.
	assert.Equal(t, "unknown-class-200", DropClassName(200))
}
