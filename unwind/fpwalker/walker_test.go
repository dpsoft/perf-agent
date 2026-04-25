package fpwalker

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
)

// buildStack lays out a synthetic stack image. Each frame contributes 16
// bytes: [savedBP (8), retAddr (8)]. Returns (stackBase, stackBytes).
//
// The first frame in `frames` is the innermost (closest to sample point);
// its savedBP points to the second frame's base, and so on.
type frame struct {
	retAddr uint64
}

func buildStack(base uint64, frames []frame) []byte {
	// 16 bytes per frame. Each frame's savedBP points to the next frame's
	// address. The LAST frame's savedBP is 0 (terminator).
	buf := make([]byte, 16*len(frames))
	for i, f := range frames {
		off := i * 16
		var savedBP uint64
		if i == len(frames)-1 {
			savedBP = 0
		} else {
			savedBP = base + uint64((i+1)*16)
		}
		binary.LittleEndian.PutUint64(buf[off:], savedBP)
		binary.LittleEndian.PutUint64(buf[off+8:], f.retAddr)
	}
	return buf
}

func TestWalkSimpleChain(t *testing.T) {
	const base = uint64(0x1000)
	stack := buildStack(base, []frame{
		{retAddr: 0xAAA}, // innermost frame's caller
		{retAddr: 0xBBB}, // middle
		{retAddr: 0xCCC}, // outermost (savedBP = 0)
	})

	got := Walk(0xDEAD, base, base, stack)
	assert.Equal(t, []uint64{0xDEAD, 0xAAA, 0xBBB, 0xCCC}, got)
}

func TestWalkStopsWhenBPOutOfRange(t *testing.T) {
	const base = uint64(0x1000)
	stack := make([]byte, 32)
	// bp well outside stack range: no frames to walk.
	got := Walk(0xDEAD, 0x9999, base, stack)
	assert.Equal(t, []uint64{0xDEAD}, got)
}

func TestWalkStopsOnNullSavedBP(t *testing.T) {
	const base = uint64(0x1000)
	// Single frame with savedBP=0 terminator.
	stack := buildStack(base, []frame{{retAddr: 0xAAA}})
	got := Walk(0xDEAD, base, base, stack)
	assert.Equal(t, []uint64{0xDEAD, 0xAAA}, got)
}

func TestWalkStopsOnNonMonotonicChain(t *testing.T) {
	const base = uint64(0x1000)
	// Frame 0 points to a bp BELOW itself (invalid — stack grows down,
	// caller frames are above). Walker should emit retAddr then stop.
	stack := make([]byte, 32)
	binary.LittleEndian.PutUint64(stack[0:], base-0x100) // savedBP below current
	binary.LittleEndian.PutUint64(stack[8:], 0xAAA)

	got := Walk(0xDEAD, base, base, stack)
	assert.Equal(t, []uint64{0xDEAD, 0xAAA}, got)
}

func TestWalkHandlesShortStack(t *testing.T) {
	// Only 8 bytes — can't hold a full frame. Walker should emit IP and stop.
	got := Walk(0xDEAD, 0x1000, 0x1000, make([]byte, 8))
	assert.Equal(t, []uint64{0xDEAD}, got)
}

func TestWalkRespectsMaxFrames(t *testing.T) {
	// Build a circular chain: frame N's savedBP points back to frame 0.
	// Walker should not loop forever; MaxFrames bounds it.
	const base = uint64(0x1000)
	stack := make([]byte, 32)
	binary.LittleEndian.PutUint64(stack[0:], base+16)
	binary.LittleEndian.PutUint64(stack[8:], 0xAAA)
	binary.LittleEndian.PutUint64(stack[16:], base) // points back
	binary.LittleEndian.PutUint64(stack[24:], 0xBBB)

	got := Walk(0xDEAD, base, base, stack)
	// The back-pointer (base) is not > current bp (base+16), so the
	// monotonic check should cut the walk short. Assert we didn't loop.
	assert.LessOrEqual(t, len(got), MaxFrames+2)
}
