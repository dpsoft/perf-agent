package perfagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The executions log needs GPU profiling, describes only the aggregated
// profile, and can never be the GPU profile's own file.
func TestGPUExecutionsLogOptionsAreValidated(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "gpu.pb.gz")
	if err := os.WriteFile(prof, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.exec")
	if err := os.Symlink(prof, link); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatal(err)
	}
	base := func() Config {
		return Config{EnableGPU: true, GPUProfilePath: prof, GPUExecutionsPath: filepath.Join(dir, "gpu.exec")}
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string // substring; empty means valid
	}{
		{"valid", func(*Config) {}, ""},
		{"without GPU", func(c *Config) { c.EnableGPU = false }, "requires GPU profiling"},
		{"with per-execution samples", func(c *Config) { c.GPUPerExecutionSamples = true }, "cannot be combined"},
		{"same path", func(c *Config) { c.GPUExecutionsPath = prof }, "also an output"},
		{"same file via symlink", func(c *Config) { c.GPUExecutionsPath = link }, "also an output"},
		{"aliased parent, profile not written yet", func(c *Config) {
			c.GPUProfilePath = filepath.Join(realDir, "later.pb.gz")
			c.GPUExecutionsPath = filepath.Join(aliasDir, "later.pb.gz")
		}, "also an output"},
		{"the CPU profile", func(c *Config) {
			c.EnableCPUProfile, c.CPUProfilePath = true, filepath.Join(dir, "cpu.pb.gz")
			c.GPUExecutionsPath = c.CPUProfilePath
		}, "also an output"},
		{"the off-CPU profile", func(c *Config) {
			c.EnableOffCPUProfile, c.OffCPUProfilePath = true, filepath.Join(dir, "off.pb.gz")
			c.GPUExecutionsPath = c.OffCPUProfilePath
		}, "also an output"},
		{"perf.data", func(c *Config) {
			c.PerfDataOutput = filepath.Join(dir, "perf.data")
			c.GPUExecutionsPath = c.PerfDataOutput
		}, "also an output"},
		{"a flame graph", func(c *Config) {
			c.GPUFlamegraphPath = filepath.Join(dir, "gpu.html")
			c.GPUExecutionsPath = c.GPUFlamegraphPath
		}, "also an output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mut(&c)
			err := c.validateGPUExecutions()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
