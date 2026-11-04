package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONLRotationBoundsDiskAndPreservesRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	w, err := NewRotatingWriter(path, 12)
	require.NoError(t, err)
	for _, line := range []string{"11111\n", "22222\n", "33333\n", "44444\n", "55555\n", "66666\n", "77777\n"} {
		_, err = w.Write([]byte(line))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	for suffix, want := range map[string]string{"": "77777\n", ".1": "55555\n66666\n", ".2": "33333\n44444\n"} {
		got, err := os.ReadFile(path + suffix)
		require.NoError(t, err)
		require.Equal(t, want, string(got))
		require.LessOrEqual(t, len(got), 12)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, files, 3)
}
func TestRotationRejectsOversizedRecordAndPropagatesRenameFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	w, err := NewRotatingWriter(path, 4)
	require.NoError(t, err)
	defer func() { _ = w.Close() }()
	_, err = w.Write([]byte("12345"))
	require.ErrorContains(t, err, "exceeds")
	_, err = w.Write([]byte("1234"))
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(path+".1", 0700))
	require.NoError(t, os.Mkdir(path+".2", 0700))
	require.NoError(t, os.WriteFile(path+".2/block", []byte("x"), 0600))
	_, err = w.Write([]byte("x"))
	require.Error(t, err)
}

func TestRotationRejectsOversizedExistingFiles(t *testing.T) {
	for _, suffix := range []string{"", ".1", ".2"} {
		t.Run("file"+suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			require.NoError(t, os.WriteFile(path+suffix, []byte("12345"), 0600))
			_, err := NewRotatingWriter(path, 4)
			require.ErrorContains(t, err, "existing JSONL file exceeds")
			got, err := os.ReadFile(path + suffix)
			require.NoError(t, err)
			require.Equal(t, "12345", string(got))
		})
	}
}
