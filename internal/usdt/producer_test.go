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
	require.Len(t, probes, 1)

	p := probes[0]
	assert.Equal(t, "perfagent", p.Provider)
	assert.Equal(t, "gpu_launch_v1", p.Name)
	assert.True(t, p.HasSemaphore, "the shim must be able to skip work when nobody listens")
	assert.Equal(t, "8@%rdi 8@%rsi 8@%rdx", p.Args,
		"the ABI pins its argument registers; an unpinned macro lets the compiler choose")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Dir(filepath.Dir(wd)) // internal/usdt -> repo root
}
