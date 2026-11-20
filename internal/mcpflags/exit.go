package mcpflags

import (
	"log/slog"
	"sync"
	"time"
)

// BlockedWait bounds how long a server whose output is blocked waits for
// Close and its last log line before it exits.
const BlockedWait = 100 * time.Millisecond

// LogBounded logs msg as an error without waiting on the handler for more
// than BlockedWait: a client may have stopped reading what it writes to.
func LogBounded(log *slog.Logger, msg string, args ...any) {
	Bounded(BlockedWait, func() { log.Error(msg, args...) })
}

// Bounded runs each step on its own goroutine and returns when all are
// done or d has passed, whichever is first. A step that never returns is
// left behind.
func Bounded(d time.Duration, steps ...func()) {
	var wg sync.WaitGroup
	for _, step := range steps {
		wg.Go(step)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}
