package perfmcp

import (
	"sync"
	"sync/atomic"
)

// stateBus joins the live page's two states into the envelope every page
// is sent, {"engine":<state>,"inbox":<inbox state>}: the engine's state,
// which commit writes under the engine, and the inbox's, which the inbox
// writes under its own lock. Each part carries its own revision, seq and
// rev, and a part that is not newer than the one held is ignored, so
// neither part of the envelope ever goes back.
//
// A writer replaces its part, builds the envelope and hands it to notify
// under the bus's lock, so envelopes reach notify in the order they were
// built. Nothing is waited for under that lock, notify never blocks, and
// the bus takes no other lock: neither writer ever waits for the other's.
type stateBus struct {
	mu     sync.Mutex
	seq    int64
	engine []byte
	rev    int64
	inbox  []byte
	notify func([]byte)
	// env is the newest envelope, which readers load without the lock.
	env atomic.Pointer[[]byte]
}

// newStateBus is a bus holding engine at seq 0 and inbox at rev 0, whose
// envelopes go to notify (nil drops them). The first envelope is not
// notified: no writer has changed anything yet.
func newStateBus(engine, inbox []byte, notify func([]byte)) *stateBus {
	b := &stateBus{engine: engine, inbox: inbox, notify: notify}
	b.build()
	return b
}

// setEngine makes b, the engine's state at seq, the envelope's engine
// part, unless the part held is as new.
func (b *stateBus) setEngine(seq int64, part []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seq <= b.seq {
		return
	}
	b.seq, b.engine = seq, part
	b.publish()
}

// setInbox makes b, the inbox's state at rev, the envelope's inbox part,
// unless the part held is as new.
func (b *stateBus) setInbox(rev int64, part []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rev <= b.rev {
		return
	}
	b.rev, b.inbox = rev, part
	b.publish()
}

// envelope is the newest envelope. Any goroutine may call it; the bytes
// never change and must not be modified.
func (b *stateBus) envelope() []byte { return *b.env.Load() }

// publish builds the envelope and hands it to notify. The caller holds
// b.mu.
func (b *stateBus) publish() {
	env := b.build()
	// Both parts are compact JSON from encoding/json, so the envelope is
	// one line, as the events hub requires.
	if b.notify != nil {
		b.notify(env)
	}
}

// build stores the envelope of the parts held and returns it.
func (b *stateBus) build() []byte {
	env := make([]byte, 0, len(`{"engine":,"inbox":}`)+len(b.engine)+len(b.inbox))
	env = append(env, `{"engine":`...)
	env = append(env, b.engine...)
	env = append(env, `,"inbox":`...)
	env = append(env, b.inbox...)
	env = append(env, '}')
	b.env.Store(&env)
	return env
}
