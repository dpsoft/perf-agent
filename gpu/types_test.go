package gpu

import (
	"reflect"
	"strings"
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

func TestPCSamplingFieldsDoNotWidenExecution(t *testing.T) {
	// PC data is capability-gated: only backends advertising
	// CapabilityPCSampling emit it. Keeping it off GPUKernelExec is what stops
	// every backend paying for CUPTI's richness. Adding any of these fields to
	// the execution type should fail here.
	forbidden := []string{"pc", "stall", "module", "cubin", "sass"}

	execType := reflect.TypeOf(GPUKernelExec{})
	for i := range execType.NumField() {
		name := strings.ToLower(execType.Field(i).Name)
		for _, f := range forbidden {
			assert.NotContains(t, name, f,
				"GPUKernelExec must not carry PC-sampling fields; that data belongs on GPUPCSample")
		}
	}

	// And the fields really do live on GPUPCSample.
	sampleType := reflect.TypeOf(GPUPCSample{})
	_, hasPC := sampleType.FieldByName("PCOffset")
	_, hasStall := sampleType.FieldByName("StallReason")
	_, hasModule := sampleType.FieldByName("Module")
	assert.True(t, hasPC && hasStall && hasModule,
		"GPUPCSample must carry the PC, stall reason and module reference")
}
