package usdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/usdt"
)

// The shim's probe macros must produce notes this parser can read, with a
// register descriptor that does not vary with the compiler's mood. The spike
// observed "8@%rax 8@%rdx 8@%rcx" from an unpinned macro, which a consumer
// reading fixed registers would silently misread.
func TestShimProbeMacrosProduceAParsableNoteWithPinnedRegisters(t *testing.T) {
	if _, err := exec.LookPath("g++"); err != nil {
		t.Skip("g++ not available")
	}
	dir := t.TempDir()
	so := filepath.Join(dir, "libprobeselftest.so")

	cmd := exec.Command("g++", "-O2", "-shared", "-fPIC",
		"-o", so, "shim/core/probe_selftest.cc")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "compile failed: %s", out)

	probes, err := usdt.ParseFile(so)
	require.NoError(t, err)
	// Two probes: probe_selftest.cc declares gpu_launch_v1 and gpu_exec_v1 in
	// the same translation unit. A single-probe self-test cannot catch a
	// regression in the shared .stapsdt.base guard (PERFAGENT_USDT_BASE is
	// expanded once per call site); this shipped broken until shim/stub/stub.cc
	// composed two probes for the first time.
	require.Len(t, probes, 2)

	names := make(map[string]bool)
	for _, p := range probes {
		assert.Equal(t, "perfagent", p.Provider)
		assert.True(t, p.HasSemaphore, "the shim must be able to skip work when nobody listens")
		assert.Equal(t, "8@%rdi 8@%rsi 8@%rdx", p.Args,
			"the ABI pins its argument registers; an unpinned macro lets the compiler choose")
		names[p.Name] = true
	}
	assert.Equal(t, map[string]bool{"gpu_launch_v1": true, "gpu_exec_v1": true}, names)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Dir(filepath.Dir(wd)) // internal/usdt -> repo root
}
