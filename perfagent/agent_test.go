package perfagent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- resolveTarget tests ---
// All use config.PID = 0 (system-wide path) to avoid nspid.Translate
// touching /proc; hostPID stays 0, and the default enricher short-circuits
// on pid <= 0.

func TestResolveTarget_EnricherOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PID = 0 // system-wide → no nspid translation
	WithLabelEnricher(func(int) map[string]string {
		return map[string]string{"pod_uid": "from-enricher", "extra": "set"}
	})(cfg)
	a := &Agent{config: cfg}

	hostPID, labels, err := a.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if hostPID != 0 {
		t.Errorf("hostPID = %d, want 0", hostPID)
	}
	if labels["pod_uid"] != "from-enricher" {
		t.Errorf("pod_uid = %q", labels["pod_uid"])
	}
	if labels["extra"] != "set" {
		t.Errorf("extra = %q", labels["extra"])
	}
}

func TestResolveTarget_StaticLabelsWinOnCollision(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PID = 0
	WithLabelEnricher(func(int) map[string]string {
		return map[string]string{"pod_uid": "from-enricher"}
	})(cfg)
	WithLabels(map[string]string{"pod_uid": "static-override"})(cfg)
	a := &Agent{config: cfg}

	_, labels, err := a.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if labels["pod_uid"] != "static-override" {
		t.Errorf("pod_uid = %q, want static-override (WithLabels must win on collision)", labels["pod_uid"])
	}
}

func TestResolveTarget_NilEnricherDisablesDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PID = 0
	WithLabelEnricher(nil)(cfg) // explicit nil disables k8slabels.FromPID default
	WithLabels(map[string]string{"only": "static"})(cfg)
	a := &Agent{config: cfg}

	_, labels, err := a.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if labels["only"] != "static" {
		t.Errorf("only = %q", labels["only"])
	}
	// Default enricher would have run k8slabels.FromPID; with WithLabelEnricher(nil),
	// it must NOT run. Confirm no cgroup_path label appears.
	if _, has := labels["cgroup_path"]; has {
		t.Errorf("cgroup_path was set despite WithLabelEnricher(nil): %q", labels["cgroup_path"])
	}
}

func TestResolveTarget_DefaultEnricherWhenNotSet(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PID = 0 // hostPID = 0; default enricher returns nil for pid <= 0
	a := &Agent{config: cfg}

	_, labels, err := a.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if len(labels) != 0 {
		t.Errorf("default enricher should produce no labels for pid=0; got %v", labels)
	}
}

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
