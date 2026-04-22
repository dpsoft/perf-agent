package ehcompile

import (
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

type goldenFile struct {
	Entries         []CFIEntry       `json:"entries"`
	Classifications []Classification `json:"classifications"`
}

func runGolden(t *testing.T, elfPath, goldenPath string) {
	t.Helper()
	if _, err := os.Stat(elfPath); err != nil {
		t.Skipf("fixture missing: %s", elfPath)
	}
	entries, classes, err := Compile(elfPath)
	require.NoError(t, err)
	got := goldenFile{Entries: entries, Classifications: classes}

	if *updateGolden {
		f, err := os.Create(goldenPath)
		require.NoError(t, err)
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		require.NoError(t, enc.Encode(got))
		t.Logf("golden file updated: %s", goldenPath)
		return
	}

	raw, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing — regenerate with -update")
	var want goldenFile
	require.NoError(t, json.Unmarshal(raw, &want))
	assert.Equal(t, want, got)
}

func TestCompile_NotImplemented(t *testing.T) {
	_, _, err := Compile("/dev/null")
	require.Error(t, err)
}

func TestCompile_SystemBinary(t *testing.T) {
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skip("/bin/true not found")
	}
	entries, classes, err := Compile("/bin/true")
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
	assert.NotEmpty(t, classes)
	for i := 1; i < len(entries); i++ {
		assert.LessOrEqual(t, entries[i-1].PCStart, entries[i].PCStart,
			"entry %d out of order", i)
	}
}

func TestCompile_GoldenFile_x86(t *testing.T) {
	runGolden(t, "testdata/hello", "testdata/hello.golden")
}

func TestCompile_GoldenFile_arm64(t *testing.T) {
	// hello_arm64.o is a relocatable object file (not a linked binary) —
	// we can build it without arm64 libc headers. PCs are placeholders
	// until link time, so the snapshot captures the CFI structure, not
	// absolute addresses.
	runGolden(t, "testdata/hello_arm64.o", "testdata/hello_arm64.golden")
}
