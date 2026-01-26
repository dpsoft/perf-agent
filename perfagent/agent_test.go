package perfagent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		opts    []Option
		wantErr string
	}{
		{
			name:    "requires at least one mode",
			opts:    []Option{WithSystemWide()},
			wantErr: "at least one of",
		},
		{
			name:    "requires pid or system-wide",
			opts:    []Option{WithPMU()},
			wantErr: "either PID or system-wide",
		},
		{
			name: "last option wins - system-wide after pid",
			opts: []Option{WithPID(123), WithSystemWide(), WithPMU()},
			// No error: WithSystemWide() resets PID to 0, so config is valid
		},
		{
			name: "valid system-wide PMU",
			opts: []Option{WithSystemWide(), WithPMU()},
		},
		{
			name: "valid PID profile",
			opts: []Option{WithPID(1), WithCPUProfile("")},
		},
		{
			name: "valid system-wide CPU profile",
			opts: []Option{WithSystemWide(), WithCPUProfile("")},
		},
		{
			name: "valid system-wide off-CPU profile",
			opts: []Option{WithSystemWide(), WithOffCPUProfile("")},
		},
		{
			name:    "per-pid requires system-wide",
			opts:    []Option{WithPID(1), WithPMU(), WithPerPID()},
			wantErr: "per-PID requires system-wide",
		},
		{
			name:    "per-pid requires PMU",
			opts:    []Option{WithSystemWide(), WithCPUProfile(""), WithPerPID()},
			wantErr: "per-PID is only valid with PMU",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts...)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, 99, cfg.SampleRate)
	assert.Equal(t, "profile.pb.gz", cfg.CPUProfilePath)
	assert.Equal(t, "offcpu.pb.gz", cfg.OffCPUProfilePath)
	assert.Nil(t, cfg.CPUProfileWriter)
	assert.Nil(t, cfg.OffCPUProfileWriter)
}

func TestOptionsApply(t *testing.T) {
	cfg := DefaultConfig()

	opts := []Option{
		WithPID(1234),
		WithSampleRate(199),
		WithTags("key=value", "env=test"),
	}

	for _, opt := range opts {
		opt(cfg)
	}

	assert.Equal(t, 1234, cfg.PID)
	assert.Equal(t, 199, cfg.SampleRate)
	assert.Equal(t, []string{"key=value", "env=test"}, cfg.Tags)
}

func TestWithCPUProfileWriter(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()

	WithCPUProfileWriter(&buf)(cfg)

	assert.True(t, cfg.EnableCPUProfile)
	assert.Equal(t, &buf, cfg.CPUProfileWriter)
}

func TestWithOffCPUProfileWriter(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()

	WithOffCPUProfileWriter(&buf)(cfg)

	assert.True(t, cfg.EnableOffCPUProfile)
	assert.Equal(t, &buf, cfg.OffCPUProfileWriter)
}

func TestWithPIDDisablesSystemWide(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SystemWide = true

	WithPID(123)(cfg)

	assert.Equal(t, 123, cfg.PID)
	assert.False(t, cfg.SystemWide)
}

func TestWithSystemWideDisablesPID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PID = 123

	WithSystemWide()(cfg)

	assert.Equal(t, 0, cfg.PID)
	assert.True(t, cfg.SystemWide)
}

func TestWithCPUProfileSetsPath(t *testing.T) {
	cfg := DefaultConfig()

	WithCPUProfile("/custom/path.pb.gz")(cfg)

	assert.True(t, cfg.EnableCPUProfile)
	assert.Equal(t, "/custom/path.pb.gz", cfg.CPUProfilePath)
}

func TestWithCPUProfileEmptyPathUsesDefault(t *testing.T) {
	cfg := DefaultConfig()
	defaultPath := cfg.CPUProfilePath

	WithCPUProfile("")(cfg)

	assert.True(t, cfg.EnableCPUProfile)
	assert.Equal(t, defaultPath, cfg.CPUProfilePath)
}

func TestWithOffCPUProfileSetsPath(t *testing.T) {
	cfg := DefaultConfig()

	WithOffCPUProfile("/custom/offcpu.pb.gz")(cfg)

	assert.True(t, cfg.EnableOffCPUProfile)
	assert.Equal(t, "/custom/offcpu.pb.gz", cfg.OffCPUProfilePath)
}

func TestWithCPUs(t *testing.T) {
	cfg := DefaultConfig()

	WithCPUs([]uint{0, 2, 4})(cfg)

	assert.Equal(t, []uint{0, 2, 4}, cfg.CPUs)
}

func TestAgentConfigCopy(t *testing.T) {
	agent, err := New(
		WithSystemWide(),
		WithPMU(),
		WithSampleRate(49),
	)
	require.NoError(t, err)

	cfg := agent.Config()

	assert.True(t, cfg.SystemWide)
	assert.True(t, cfg.EnablePMU)
	assert.Equal(t, 49, cfg.SampleRate)

	// Verify it's a copy (modifying doesn't affect original)
	cfg.SampleRate = 100
	originalCfg := agent.Config()
	assert.Equal(t, 49, originalCfg.SampleRate)
}
