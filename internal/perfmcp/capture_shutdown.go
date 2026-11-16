package perfmcp

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// strandedEvery is how often maintenance retries this server's stranded
// rollbacks and settles its undecided commits; maintenanceHold is the end a
// maintenance pass records in the lock file, which a busy answer's
// retry_after is counted from.
const (
	strandedEvery   = 30 * time.Second
	maintenanceHold = 5 * time.Second
)

// beforeMaintenance, when set, runs on a new server before its maintenance
// starts; a seam for tests, which arm their fault seams there.
var beforeMaintenance func(*Server)

// lockDir takes the in-process captures directory slot, which orders
// maintenance and the jobs; a wait ends with cancel or the server's close.
func (s *Server) lockDir(cancel <-chan struct{}) bool {
	select {
	case s.dirMu <- struct{}{}:
		return true
	case <-cancel:
		return false
	case <-s.ctx.Done():
		return false
	}
}

func (s *Server) unlockDir() { <-s.dirMu }

// cancelAllJobs asks every job to stop: a CAS and a channel close each,
// no I/O. It is what the end of the stdio session does, since nobody would
// collect the jobs.
func (s *Server) cancelAllJobs() {
	if s.registry == nil {
		return
	}
	for _, j := range s.registry.list() {
		j.requestCancel()
	}
}

// KillCaptures kills the running capture at once: it reads the job's Run
// without a lock and closes its reaper's command pipe, which the reaper
// answers by killing the children and cleaning up before it exits. It
// takes no lock a job holds, so it never waits on one stuck in I/O, and
// is safe from any goroutine. Later jobs do not start their capture.
func (s *Server) KillCaptures() {
	s.killed.Store(true)
	if s.registry == nil {
		return
	}
	if j := s.registry.active.Load(); j != nil {
		if r := j.run.Load(); r != nil {
			r.Kill()
		}
	}
}

// shutdownLogWait bounds how long a job's last log line, or Close's, waits
// for the log: stderr may be full, its client gone.
const shutdownLogWait = 100 * time.Millisecond

// logBounded logs one line at level without waiting for the log more than
// shutdownLogWait, so a blocked stderr never keeps a job from ending or the
// server from closing. The line may then be written later, or never.
func (s *Server) logBounded(level slog.Level, msg string, args ...any) {
	logged := make(chan struct{})
	go func() {
		s.log.Log(context.Background(), level, msg, args...)
		close(logged)
	}()
	t := time.NewTimer(shutdownLogWait)
	defer t.Stop()
	select {
	case <-logged:
	case <-t.C:
	}
}

// waitJobs waits up to d for the job goroutines to end.
func (s *Server) waitJobs(d time.Duration) {
	done := make(chan struct{})
	go func() {
		s.jobsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		s.logBounded(slog.LevelWarn, "capture jobs still running at close", "waited", d)
	}
}

// maintenanceJoin bounds Close's wait for the maintenance goroutine: a
// pass ends its waits at once when the server closes, and its file writes
// (each one temporary and its link) are short, but a stuck filesystem must
// not hold Close for good.
const maintenanceJoin = 2 * time.Second

// waitMaintenance waits up to d for the maintenance goroutine to return.
func (s *Server) waitMaintenance(d time.Duration) {
	if s.maintained == nil {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.maintained:
	case <-t.C:
		s.logBounded(slog.LevelWarn, "capture maintenance still running at close", "waited", d)
	}
}

// recoveryRetry is how soon a full maintenance pass is run again when one
// could not do its work: the captures lock was held (a dead server's
// reaper still ending its capture, say), or recovery left an intent whose
// owner it could not judge yet, or a wait found a capture in progress whose
// owner is gone. It is the heartbeat's stale bound, so a dead owner is
// judged gone by the next pass.
const recoveryRetry = staleAfter

// maintain is the background maintenance: a full pass (recovery and
// retention) at start, run again while it is owed (see recoveryRetry),
// and the stranded rollbacks every strandedEvery.
func (s *Server) maintain() {
	defer close(s.maintained)
	var due time.Time
	owe := func(d time.Duration) {
		if d <= 0 {
			return
		}
		if at := time.Now().Add(d); due.IsZero() || at.Before(due) {
			due = at
		}
	}
	owe(s.maintenance(true))
	tick := time.NewTicker(strandedEvery)
	defer tick.Stop()
	for {
		var retry <-chan time.Time
		if !due.IsZero() {
			retry = time.After(time.Until(due))
		}
		select {
		case <-s.ctx.Done():
			return
		case <-retry:
			due = time.Time{}
			owe(s.maintenance(true))
		case <-s.recoveryWanted:
			owe(recoveryRetry)
		case <-tick.C:
			s.maintenance(false)
		}
	}
}

// wantRecovery asks maintenance for a full pass within recoveryRetry,
// without waiting: a capture in progress whose owner is gone is for
// recovery to settle.
func (s *Server) wantRecovery() {
	select {
	case s.recoveryWanted <- struct{}{}:
	default:
	}
}

// startPassOver records that the start-up pass is over: it ran, or found
// nothing to recover.
func (s *Server) startPassOver() {
	s.recovered.Store(true)
	s.startOnce.Do(func() { close(s.startPassDone) })
}

// maintenance is one pass: under the directory slot, then the captures
// lock, it retries the stranded rollbacks and, when full, recovers and
// runs retention: what it must keep is snapshotted under the engine, and
// the evictions run outside it. A full pass waits for a job holding the
// slot; a retry pass leaves the directory to a running job, which retries
// before it captures. A full pass returns how soon it is owed again (0:
// not owed): when the captures lock was held, at the holder's busy
// retry_after, and when recovery left an intent it could not judge, after
// recoveryRetry.
func (s *Server) maintenance(full bool) time.Duration {
	if full {
		if !s.captureDirExists() {
			s.startPassOver()
			return 0
		}
		if !s.lockDir(nil) {
			return 0
		}
	} else {
		s.capMu.Lock()
		idle := len(s.stranded) == 0 && len(s.undecided) == 0
		s.capMu.Unlock()
		if idle {
			return 0
		}
		select {
		case s.dirMu <- struct{}{}:
		default:
			return 0
		}
	}
	defer s.unlockDir()
	d, e := s.openCaptureDir()
	if e != nil {
		s.log.Warn("capture maintenance", "err", e)
		if full {
			// Nothing this server can recover there; a capture says why.
			s.startPassOver()
		}
		return 0
	}
	defer d.close()
	start, _ := s.startTime()
	lease, e := capture.TryLock(d.root, capture.Holder{ServerPID: os.Getpid(), ServerStart: start, Deadline: s.clock().Add(maintenanceHold)})
	if e != nil {
		s.log.Info("capture maintenance skipped", "why", e)
		if !full {
			return 0
		}
		if e.Code == capture.CodeBusy {
			return min(max(e.RetryAfter, time.Second), recoveryRetry)
		}
		return recoveryRetry
	}
	defer func() { _ = lease.Close() }()
	defer s.historyChanged()
	s.retryStrandedPass(d)
	if !full {
		return 0
	}
	notes, err := s.recoverCaptures(d)
	if err != nil {
		s.log.Warn("capture recovery stopped", "err", causeText(err), "notes", len(notes))
	}
	s.startPassOver()
	again := time.Duration(0)
	if s.recoveryLeft.Load() {
		again = recoveryRetry
	}
	// What retention keeps is read under the engine; the hashing and the
	// removals run without it.
	if err := s.engine.acquire(s.ctx); err != nil {
		return again
	}
	p := s.protected(d, "")
	s.engine.release()
	if evicted, err := s.retain(d, p, s.clock()); err != nil {
		s.log.Warn("capture retention", "err", causeText(err), "evicted", len(evicted))
	}
	return again
}

// captureDirExists reports whether the capture directory was created, so
// a start-up pass does not create it before the first capture.
func (s *Server) captureDirExists() bool {
	rel, err := s.rel(s.profiles, s.captureDirName())
	if err != nil {
		return false
	}
	fi, err := s.profiles.Lstat(rel)
	return err == nil && fi.IsDir()
}
