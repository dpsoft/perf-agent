//go:build linux

package capture

import (
	"math"
	"os"
	"testing"
)

// FreeBytes reads the filesystem of an open directory.
func TestFreeBytesOfAnOpenDirectory(t *testing.T) {
	f, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	n, err := FreeBytes(f)
	if err != nil || n <= 0 {
		t.Fatalf("FreeBytes: %d, %v", n, err)
	}
}

// The free space counts fragments of the fragment size when the
// filesystem gives one, and saturates rather than overflows.
func TestFreeBytesArithmetic(t *testing.T) {
	cases := []struct {
		avail        uint64
		frsize, size int64
		want         int64
	}{
		{10, 1024, 4096, 10240},
		{10, 0, 4096, 40960},
		{math.MaxUint64, 4096, 4096, math.MaxInt64},
		{1 << 62, 4, 4, math.MaxInt64},
		{0, 4096, 4096, 0},
		{10, -1, 4096, 0},
	}
	for _, c := range cases {
		if got := freeBytes(c.avail, c.frsize, c.size); got != c.want {
			t.Errorf("freeBytes(%d, %d, %d) = %d, want %d", c.avail, c.frsize, c.size, got, c.want)
		}
	}
}
