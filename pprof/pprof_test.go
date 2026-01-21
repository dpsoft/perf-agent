package pprof

import (
	"bytes"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// === EXISTING FUNCTIONALITY TESTS ===

func TestNewProfileBuilders(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate:    99,
		PerPIDProfile: false,
	})

	assert.NotNil(t, builders.Builders)
	assert.Empty(t, builders.Builders)
}

func TestSampleTypes(t *testing.T) {
	tests := []struct {
		name       string
		sampleType SampleType
		wantType   string
		wantUnit   string
	}{
		{"CPU", SampleTypeCpu, "cpu", "nanoseconds"},
		{"OffCPU", SampleTypeOffCpu, "offcpu", "nanoseconds"},
		{"Memory", SampleTypeMem, "alloc_objects", "count"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
			builders.AddSample(&ProfileSample{
				SampleType: tt.sampleType,
				Stack:      []string{"main"},
				Value:      100,
			})

			for _, b := range builders.Builders {
				assert.Equal(t, tt.wantType, b.Profile.SampleType[0].Type)
				assert.Equal(t, tt.wantUnit, b.Profile.SampleType[0].Unit)
			}
		})
	}
}

func TestAddSampleCreatesLocationsAndFunctions(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       []string{"main", "foo", "bar"},
		Value:       100,
	})

	for _, b := range builders.Builders {
		assert.Len(t, b.Profile.Location, 3)
		assert.Len(t, b.Profile.Function, 3)
		assert.Len(t, b.Profile.Sample, 1)

		// Verify function names
		funcNames := make([]string, len(b.Profile.Function))
		for i, f := range b.Profile.Function {
			funcNames[i] = f.Name
		}
		assert.Contains(t, funcNames, "main")
		assert.Contains(t, funcNames, "foo")
		assert.Contains(t, funcNames, "bar")
	}
}

func TestSampleAggregation(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	// Add same stack twice with SampleAggregated=false (should dedup)
	for i := 0; i < 3; i++ {
		builders.AddSample(&ProfileSample{
			SampleType:  SampleTypeCpu,
			Aggregation: false, // Not aggregated - should merge
			Stack:       []string{"main", "handler"},
			Value:       100,
		})
	}

	for _, b := range builders.Builders {
		// Should have only 1 sample (merged)
		assert.Len(t, b.Profile.Sample, 1)
		// Value should be accumulated
		assert.Equal(t, int64(300*b.Profile.Period), b.Profile.Sample[0].Value[0])
	}
}

func TestPerPIDProfile(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate:    99,
		PerPIDProfile: true,
	})

	// Add samples from different PIDs
	builders.AddSample(&ProfileSample{
		Pid:        1000,
		SampleType: SampleTypeCpu,
		Stack:      []string{"main"},
		Value:      100,
	})
	builders.AddSample(&ProfileSample{
		Pid:        2000,
		SampleType: SampleTypeCpu,
		Stack:      []string{"worker"},
		Value:      200,
	})

	// Should have 2 separate builders
	assert.Len(t, builders.Builders, 2)
}

func TestProfileWriteAndParse(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       []string{"main", "processRequest", "doWork"},
		Value:       500,
	})

	var buf bytes.Buffer
	for _, b := range builders.Builders {
		_, err := b.Write(&buf)
		require.NoError(t, err)
	}

	// Parse the profile back
	parsed, err := profile.Parse(&buf)
	require.NoError(t, err)

	assert.Len(t, parsed.Sample, 1)
	assert.Len(t, parsed.Location, 3)
	assert.Len(t, parsed.Function, 3)
	assert.Equal(t, "cpu", parsed.SampleType[0].Type)
}

func TestOffCpuValueNotScaled(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	// Off-CPU values should NOT be multiplied by period
	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeOffCpu,
		Aggregation: SampleAggregated,
		Stack:       []string{"main"},
		Value:       1000000, // 1ms in nanoseconds
	})

	for _, b := range builders.Builders {
		// Value should be exactly what we passed (not scaled)
		assert.Equal(t, int64(1000000), b.Profile.Sample[0].Value[0])
	}
}

// === NEW COMMENTS/TAGS TESTS ===

func TestProfileComments(t *testing.T) {
	comments := []string{"env=prod", "version=1.2.3", "service=api"}

	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Comments:   comments,
	})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      []string{"main"},
		Value:      100,
	})

	for _, b := range builders.Builders {
		assert.Equal(t, comments, b.Profile.Comments)
	}
}

func TestProfileCommentsEmpty(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Comments:   nil,
	})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      []string{"main"},
		Value:      100,
	})

	for _, b := range builders.Builders {
		assert.Empty(t, b.Profile.Comments)
	}
}

func TestProfileWriteWithComments(t *testing.T) {
	comments := []string{"env=staging", "commit=abc123"}

	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Comments:   comments,
	})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      []string{"main"},
		Value:      100,
	})

	var buf bytes.Buffer
	for _, b := range builders.Builders {
		_, err := b.Write(&buf)
		require.NoError(t, err)
	}

	// Parse and verify comments survived round-trip
	parsed, err := profile.Parse(&buf)
	require.NoError(t, err)
	assert.Equal(t, comments, parsed.Comments)
}
