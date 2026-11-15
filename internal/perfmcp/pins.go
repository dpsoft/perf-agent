package perfmcp

import "slices"

// retained is an object whose memory the pin ledger counts: a profile, a
// view, or a publication's model and history. RetainedBytes must
// not change while the object is held.
type retained interface{ RetainedBytes() int64 }

// pin is one held object: how many holders hold it and the bytes it was
// counted at.
type pin struct {
	holders int
	bytes   int64
}

// pins is the ledger of the memory the server keeps reachable on purpose:
// the store's entries and the live page's publications are its holders,
// and each holds the objects it keeps alive. Objects are keyed by pointer,
// so an object held by several holders (a profile both open and
// published, or published twice) counts once. It belongs to one Server
// and is used from its request goroutine only.
type pins struct {
	objs map[retained]*pin
	// held lists what each holder holds, each object once.
	held map[any][]retained
	// total is the bytes of every distinct object held.
	total int64
}

func newPins() *pins {
	return &pins{objs: map[retained]*pin{}, held: map[any][]retained{}}
}

// hold makes holder hold objs, besides what it already holds. An object
// holder already holds, or one listed twice, is held once.
func (l *pins) hold(holder any, objs ...retained) {
	have := l.held[holder]
	for _, o := range objs {
		if slices.Contains(have, o) {
			continue
		}
		have = append(have, o)
		p := l.objs[o]
		if p == nil {
			p = &pin{bytes: o.RetainedBytes()}
			l.objs[o] = p
			l.total += p.bytes
		}
		p.holders++
	}
	l.held[holder] = have
}

// release lets go of everything holder holds. An object no other holder
// holds leaves the ledger.
func (l *pins) release(holder any) {
	for _, o := range l.held[holder] {
		p := l.objs[o]
		if p.holders--; p.holders == 0 {
			delete(l.objs, o)
			l.total -= p.bytes
		}
	}
	delete(l.held, holder)
}

// heldBy is the bytes of the distinct objects the given holders hold.
func (l *pins) heldBy(holders ...any) int64 {
	var objs []retained
	for _, h := range holders {
		objs = append(objs, l.held[h]...)
	}
	return l.estimate(objs...)
}

// estimate is the bytes of the distinct objects in objs, whether held or
// not. It holds nothing, so admission can try a set before keeping it.
func (l *pins) estimate(objs ...retained) int64 {
	seen := make(map[retained]bool, len(objs))
	var n int64
	for _, o := range objs {
		if seen[o] {
			continue
		}
		seen[o] = true
		n += o.RetainedBytes()
	}
	return n
}

// holders is how many holders hold o.
func (l *pins) holders(o retained) int {
	if p := l.objs[o]; p != nil {
		return p.holders
	}
	return 0
}
