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
