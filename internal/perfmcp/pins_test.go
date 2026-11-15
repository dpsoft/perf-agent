package perfmcp

import "testing"

// blob is an object of a fixed estimated size.
type blob struct{ n int64 }

func (b *blob) RetainedBytes() int64 { return b.n }

func TestPinsCountEachObjectOnce(t *testing.T) {
	l := newPins()
	a, b, c := &blob{100}, &blob{20}, &blob{3}
	h1, h2 := new(int), new(int)
	l.hold(h1, a, b, a)
	l.hold(h2, a, c)
	if got := l.total; got != 123 {
		t.Errorf("total = %d, want 123: a held twice counts once", got)
	}
	if got := l.holders(a); got != 2 {
		t.Errorf("a has %d holders, want 2 (h1 holding it twice is one)", got)
	}
	for _, tc := range []struct {
		holders []any
		want    int64
	}{
		{[]any{h1}, 120},
		{[]any{h2}, 103},
		{[]any{h1, h2}, 123},
		{[]any{h1, h1}, 120},
		{nil, 0},
	} {
		if got := l.heldBy(tc.holders...); got != tc.want {
			t.Errorf("heldBy(%v) = %d, want %d", tc.holders, got, tc.want)
		}
	}
	if got := l.estimate(a, b, a, &blob{7}); got != 127 {
		t.Errorf("estimate = %d, want 127", got)
	}
	if l.total != 123 {
		t.Errorf("estimate held something: total %d", l.total)
	}

	l.release(h1)
	if l.total != 103 || l.holders(a) != 1 || l.holders(b) != 0 {
		t.Errorf("after releasing h1: total %d, holders a %d b %d; want 103, 1, 0", l.total, l.holders(a), l.holders(b))
	}
	l.release(h1)
	if l.total != 103 {
		t.Errorf("a second release changed the total to %d", l.total)
	}
	l.release(h2)
	if l.total != 0 || len(l.objs) != 0 || len(l.held) != 0 {
		t.Errorf("after releasing everything: total %d, %d objects, %d holders", l.total, len(l.objs), len(l.held))
	}
}

func TestPinsHoldMoreForAHolder(t *testing.T) {
	l := newPins()
	a, b := &blob{5}, &blob{7}
	h := new(int)
	l.hold(h, a)
	l.hold(h, a, b)
	if l.total != 12 || l.holders(a) != 1 || l.heldBy(h) != 12 {
		t.Errorf("total %d, holders a %d, heldBy %d; want 12, 1, 12", l.total, l.holders(a), l.heldBy(h))
	}
}
