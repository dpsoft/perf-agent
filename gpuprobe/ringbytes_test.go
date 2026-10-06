package gpuprobe

import (
	"os"
	"testing"
)

// TestRoundRingBytes pins the two properties BPF_MAP_TYPE_RINGBUF requires of
// max_entries -- a power of two, and a multiple of the page size -- plus the
// bounds. A size that satisfies neither is rejected by the verifier with a
// bare EINVAL far from the knob that set it, which is what this rounding is
// for.
func TestRoundRingBytes(t *testing.T) {
	page := os.Getpagesize()

	t.Run("rounds up to a power-of-two page multiple", func(t *testing.T) {
		for _, in := range []int{1, page - 1, page, page + 1, 4 << 20, (4 << 20) + 1, 100 << 20} {
			got, err := roundRingBytes(in)
			if err != nil {
				t.Fatalf("roundRingBytes(%d): unexpected error %v", in, err)
			}
			n := int(got)
			if n < in {
				t.Errorf("roundRingBytes(%d) = %d, rounded DOWN", in, n)
			}
			if n&(n-1) != 0 {
				t.Errorf("roundRingBytes(%d) = %d, not a power of two", in, n)
			}
			if n%page != 0 {
				t.Errorf("roundRingBytes(%d) = %d, not a multiple of page %d", in, n, page)
			}
			// Rounding up must not overshoot past the next power of two.
			if in > page && n >= in*2 {
				t.Errorf("roundRingBytes(%d) = %d, overshot", in, n)
			}
		}
	})

	t.Run("rejects non-positive", func(t *testing.T) {
		for _, in := range []int{0, -1, -(4 << 20)} {
			if _, err := roundRingBytes(in); err == nil {
				t.Errorf("roundRingBytes(%d): want error, got nil", in)
			}
		}
	})

	t.Run("rejects absurd", func(t *testing.T) {
		if _, err := roundRingBytes((1 << 30) + 1); err == nil {
			t.Error("roundRingBytes(1GiB+1): want error, got nil")
		}
		if _, err := roundRingBytes(1 << 30); err != nil {
			t.Errorf("roundRingBytes(1GiB): want ok, got %v", err)
		}
	})
}
