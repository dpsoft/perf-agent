package perfmcp

import (
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
		s.log.Warn("capture jobs still running at close", "waited", d)
	}
}

// maintain is the background maintenance: one full pass (recovery and
// retention) at start, then the stranded rollbacks every strandedEvery.
func (s *Server) maintain() {
	s.maintenance(true)
	tick := time.NewTicker(strandedEvery)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			s.maintenance(false)
		}
	}
}

// maintenance is one pass: under the directory slot, then the captures
// lock (skipped when another capture holds it), it retries the stranded
// rollbacks and, when full, recovers and runs retention under the engine.
// A full pass waits for a job holding the slot; a retry pass leaves the
// directory to a running job, which retries before it captures.
func (s *Server) maintenance(full bool) {
	if full {
		if !s.captureDirExists() {
			return
		}
		if !s.lockDir(nil) {
			return
		}
	} else {
		s.capMu.Lock()
		idle := len(s.stranded) == 0 && len(s.undecided) == 0
		s.capMu.Unlock()
		if idle {
			return
		}
		select {
		case s.dirMu <- struct{}{}:
		default:
			return
		}
	}
	defer s.unlockDir()
	d, e := s.openCaptureDir()
	if e != nil {
		s.log.Warn("capture maintenance", "err", e)
		return
	}
	defer d.close()
	start, _ := s.startTime()
	lease, e := capture.TryLock(d.root, capture.Holder{ServerPID: os.Getpid(), ServerStart: start, Deadline: s.clock().Add(maintenanceHold)})
	if e != nil {
		s.log.Info("capture maintenance skipped", "why", e)
		return
	}
	defer func() { _ = lease.Close() }()
	defer s.historyChanged()
	s.retryStrandedPass(d)
	if !full {
		return
	}
	if notes, err := s.recoverCaptures(d); err != nil {
		s.log.Warn("capture recovery stopped", "err", causeText(err), "notes", len(notes))
	}
	// What retention keeps is read under the engine; the hashing and the
	// removals run without it.
	if err := s.engine.acquire(s.ctx); err != nil {
		return
	}
	p := s.protected(d, "")
	s.engine.release()
	if evicted, err := s.retain(d, p, s.clock()); err != nil {
		s.log.Warn("capture retention", "err", causeText(err), "evicted", len(evicted))
	}
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
