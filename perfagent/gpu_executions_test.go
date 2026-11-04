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
		{"same path", func(c *Config) { c.GPUExecutionsPath = prof }, "own path"},
		{"same file via symlink", func(c *Config) { c.GPUExecutionsPath = link }, "own file"},
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
