package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCorrelationIDUsableAsMapKey(t *testing.T) {
	m := map[CorrelationID]int{}
	a := CorrelationID{Backend: BackendCUPTI, Value: "1234"}
	b := CorrelationID{Backend: BackendCUPTI, Value: "1234"}
	c := CorrelationID{Backend: BackendHIP, Value: "1234"}

	m[a] = 1
	m[c] = 2

	assert.Equal(t, 1, m[b], "equal correlation IDs must hash to the same bucket")
	assert.Len(t, m, 2, "same value under a different backend is a different key")
}

func TestNormalizeClockDomainDefaultsToCPUMonotonic(t *testing.T) {
	assert.Equal(t, ClockDomainCPUMonotonic, NormalizeClockDomain(ClockDomain(0)))
	assert.Equal(t, ClockDomainCPUMonotonic, NormalizeClockDomain(ClockDomainCPUMonotonic))
}

func TestValidateSupportedClockDomainRejectsDeviceClocks(t *testing.T) {
	require.NoError(t, ValidateSupportedClockDomain(ClockDomainCPUMonotonic))
	require.NoError(t, ValidateSupportedClockDomain(ClockDomain(0)),
		"zero value normalizes to cpu-monotonic and must be accepted")
	assert.Error(t, ValidateSupportedClockDomain(ClockDomainGPUDevice),
		"producers must convert device clocks before emitting")
	assert.Error(t, ValidateSupportedClockDomain(ClockDomainSynced))
}

func TestPCSamplesAreSeparateFromExecutions(t *testing.T) {
	// GPUPCSample must not be reachable by widening GPUKernelExec: PC data is
	// capability-gated and only CUPTI-class backends emit it.
	s := GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "7"},
		Module:      ModuleRef{Backend: BackendCUPTI, CRC: 0xdeadbeef},
		PCOffset:    0x1a40,
		StallReason: "long_scoreboard",
		Count:       3,
	}
	assert.Equal(t, uint64(0x1a40), s.PCOffset)
	assert.Equal(t, uint64(3), s.Count)
}
