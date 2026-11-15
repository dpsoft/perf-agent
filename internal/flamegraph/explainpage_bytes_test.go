package flamegraph

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/explain"
)

// ExplainedPage is what WriteExplainedPage writes, for every budget that
// fits, rendered once into memory.
func TestExplainedPageIsWhatWriteExplainedPageWrites(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	for _, maxBytes := range []int{1 << 20, 64 << 10, 36 << 10} {
		opts := exOpts()
		opts.MaxBytes = maxBytes
		var want bytes.Buffer
		require.NoError(t, WriteExplainedPage(&want, v, r, opts))
		got, err := ExplainedPage(v, r, opts)
		require.NoError(t, err, "MaxBytes %d", maxBytes)
		assert.Equal(t, want.Bytes(), got, "MaxBytes %d", maxBytes)
		assert.LessOrEqual(t, len(got), maxBytes)
	}
	_, err := ExplainedPage(v, r, exOpts())
	assert.ErrorContains(t, err, "MaxBytes", "an unbounded page is WriteExplainedPage's alone")
	_, err = ExplainedPage(v, explain.Result{}, ExplainedPageOptions{MaxBytes: 1 << 20})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrPageTooLarge), "a failed check is not a size error")
}

// A page or live model over its budget is ErrPageTooLarge, with the text
// it always had; nothing else is.
func TestOverTheBudgetIsErrPageTooLarge(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	opts := exOpts()
	opts.MaxBytes = 1024
	_, err := ExplainedPage(v, r, opts)
	require.ErrorIs(t, err, ErrPageTooLarge)
	assert.Regexp(t, `^flamegraph: the page needs \d+ bytes with only the explained and referenced paths drawn, over the budget of 1024 bytes \(MaxBytes\)$`, err.Error())
	var w bytes.Buffer
	assert.ErrorIs(t, WriteExplainedPage(&w, v, r, opts), ErrPageTooLarge)
	assert.Zero(t, w.Len())

	_, err = ExplainedLiveModel(v, r, LiveOptions{MaxBytes: 64})
	require.ErrorIs(t, err, ErrPageTooLarge)
	assert.Regexp(t, `^flamegraph: the live model needs \d+ bytes .* over the budget of 64 bytes \(MaxBytes\)$`, err.Error())
	_, err = ExplainedLiveModel(nil, r, LiveOptions{MaxBytes: 1 << 20})
	assert.False(t, errors.Is(err, ErrPageTooLarge))
}
