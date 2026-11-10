package gpuprobe

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func arrive(s *seqStream, seqs ...uint64) {
	for _, q := range seqs {
		s.arrive(q)
	}
}

// Sampled launches take their sequence number with fetch_add before they are
// emitted, so two producer threads can deliver 2 before 1. Nothing was lost.
func TestSeqStreamReorderedArrivalsAreNotGaps(t *testing.T) {
	var s seqStream
	arrive(&s, 0, 2, 1, 3)
	assert.Zero(t, s.gaps)
	assert.Zero(t, s.unresolved())
	assert.Equal(t, uint64(1), s.reordered)
	assert.Zero(t, s.stale)
}

// The late arrival must not move the stream backwards: with "last arrival"
// tracking, 5 after 1 was charged again for 2..4 even though 2..4 had
// already been seen.
func TestSeqStreamLateArrivalDoesNotRecountTheRegionAfterIt(t *testing.T) {
	var s seqStream
	arrive(&s, 0, 4, 2, 3, 1, 5)
	assert.Zero(t, s.gaps)
	assert.Zero(t, s.unresolved())
	assert.Equal(t, uint64(3), s.reordered)
}

func TestSeqStreamHoleIsUnresolvedUntilItArrivesOrFallsOutOfTheWindow(t *testing.T) {
	var s seqStream
	arrive(&s, 0, 1, 5)
	assert.Zero(t, s.gaps, "2..4 may still be in flight")
	assert.Equal(t, uint64(3), s.unresolved())

	arrive(&s, 3)
	assert.Equal(t, uint64(2), s.unresolved())

	// Moving the window past 2 and 4 confirms them.
	s.arrive(5 + seqWindow)
	assert.Equal(t, uint64(2), s.gaps)
	assert.Equal(t, uint64(seqWindow-1), s.unresolved(),
		"everything between 5 and the new arrival is now the open hole")
}

func TestSeqStreamDuplicateIsStaleNotReordered(t *testing.T) {
	var s seqStream
	arrive(&s, 0, 1, 3, 3, 1)
	assert.Equal(t, uint64(2), s.stale, "3 repeated inside the window, 1 below it")
	assert.Zero(t, s.reordered)
	assert.Equal(t, uint64(1), s.unresolved())
}

// A record older than the window cannot be told from a duplicate. It is
// counted, never treated as a new stream start, and it cannot un-confirm the
// gap already charged for it.
func TestSeqStreamArrivalBelowTheWindowIsStale(t *testing.T) {
	var s seqStream
	arrive(&s, 0, 2, 2+seqWindow)
	require.Equal(t, uint64(1), s.gaps)
	arrive(&s, 1)
	assert.Equal(t, uint64(1), s.stale)
	assert.Equal(t, uint64(1), s.gaps)
}

// A jump far beyond the window must be counted without walking every skipped
// position.
func TestSeqStreamHugeJumpIsCountedInConstantWork(t *testing.T) {
	var s seqStream
	arrive(&s, 0, 1, 3)
	s.arrive(1 << 40)
	// Skipped: 2 and 4..2^40-1. The last seqWindow-1 of them are still
	// inside the window and stay open; the rest are confirmed.
	assert.Equal(t, uint64(seqWindow-1), s.unresolved())
	assert.Equal(t, uint64(1<<40)-3, s.gaps+s.unresolved(), "every skipped position is accounted once")
}

func TestSeqStreamFirstArrivalStartsTheStreamWherever(t *testing.T) {
	var s seqStream
	arrive(&s, 1000, 1001)
	assert.Zero(t, s.gaps)
	assert.Zero(t, s.unresolved())
}

// The forward-jump size is what the sampling-window store needs: a hole in the
// history must restart coverage when it is first seen, not once it is proven,
// so that an unproven interval is never reported as a proven gap. A late
// arrival filling a hole does not extend the frontier and reports nothing.
func TestSeqStreamArriveReportsTheForwardJump(t *testing.T) {
	var s seqStream
	assert.Zero(t, s.arrive(0))
	assert.Equal(t, uint64(2), s.arrive(3))
	assert.Zero(t, s.arrive(1))
	assert.Zero(t, s.arrive(2))
	assert.Zero(t, s.arrive(4))
}

// Every position is eventually exactly one of: arrived, a confirmed gap or an
// open hole. Checked against a random permutation with drops and bounded
// displacement, which is what concurrent producers deliver.
func TestSeqStreamConservesPositionsUnderBoundedReordering(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	const n = 50_000
	// Each position is delivered at a time jittered by up to 64 slots.
	type slot struct{ seq, at uint64 }
	slots := make([]slot, 0, n)
	for i := range uint64(n) {
		slots = append(slots, slot{seq: i, at: i + r.Uint64N(64)})
	}
	slices.SortStableFunc(slots, func(a, b slot) int { return cmp.Compare(a.at, b.at) })
	seqs := make([]uint64, 0, n)
	for _, sl := range slots {
		seqs = append(seqs, sl.seq)
	}
	var s seqStream
	dropped := map[uint64]bool{}
	for _, q := range seqs {
		if q >= 64 && q != n-1 && r.IntN(100) == 0 {
			dropped[q] = true
			continue
		}
		s.arrive(q)
	}
	assert.Equal(t, uint64(len(dropped)), s.gaps+s.unresolved())
	assert.Zero(t, s.stale)
}

// Attaching mid-stream, the first record seen need not be the lowest in
// flight. Earlier ones arriving next are reordering, not stale, and positions
// before the first arrival are never owed: they may predate the attach.
func TestSeqStreamArrivalsBeforeTheFirstOneAreReorderedNotStale(t *testing.T) {
	var s seqStream
	arrive(&s, 10, 8, 11, 9)
	assert.Zero(t, s.stale)
	assert.Equal(t, uint64(2), s.reordered)
	assert.Zero(t, s.gaps)
	assert.Zero(t, s.unresolved())
}

// Retained state per stream is fixed, however the stream behaves.
func TestSeqStreamStateIsBounded(t *testing.T) {
	assert.LessOrEqual(t, unsafe.Sizeof(seqStream{}), uintptr(seqWindow/8+64))
}

// Through the Consumer: the issue's reproduction, and the reordered arrivals
// visible as a diagnostic.
func TestConsumerReorderedSampledLaunchesAreNotSequenceGaps(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	for _, seq := range []uint64{0, 2, 1, 3} {
		c.noteSeq(kindLaunchSampled, 1234, seq)
	}
	st := c.Stats()
	assert.Zero(t, st.SequenceGaps)
	assert.Equal(t, uint64(1), st.SequenceReordered)
}

// A hole still open when the stats are read is reported: a final snapshot
// after the drain has nothing left in flight to fill it.
func TestConsumerOpenHoleIsReportedAsAGap(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	c.noteSeq(kindLaunch, 1, 0)
	c.noteSeq(kindLaunch, 1, 3)
	c.noteSeq(kindExec, 2, 0)
	c.noteSeq(kindExec, 2, 2)
	assert.Equal(t, uint64(3), c.Stats().SequenceGaps)
	c.noteSeq(kindLaunch, 1, 1)
	assert.Equal(t, uint64(2), c.Stats().SequenceGaps)
}
