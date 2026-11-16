package perfmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// leaveUndecided records a capture whose commit nobody could read: it may
// have committed, so it is neither done nor failed, and holds no error yet.
// It leaves the active slot, and the pass that settles it from the disk
// tells the job; a cancel meanwhile is recorded on the decision word and
// reported then. e is only logged.
func (r *jobRun) leaveUndecided(e *capture.Error) {
	s, j := r.s, r.j
	j.txn, j.base = r.txn, r.base
	j.setState(jobCommitting, phaseUndecided)
	s.capMu.Lock()
	s.undecided = append(s.undecided, j)
	s.capMu.Unlock()
	s.log.Warn("capture undecided: its manifest link had an outcome that could not be read", "job", j.id, "capture", j.captureIDNow(), "why", e.Message)
	if err := s.registry.finish(j, 0, r.profiled()); err != nil {
		s.log.Error("capture registry", "job", j.id, "err", err)
	}
}

// retryStrandedPass retries the stranded rollbacks, which never stops the
// caller: a rollback that cannot finish (a foreign file at its manifest
// name, say) is logged once per pass, and capture_list shows it. Then it
// tells the undecided jobs a retry has settled.
func (s *Server) retryStrandedPass(d *captureDir) {
	if err := s.retryStranded(); err != nil {
		s.log.Warn("a stranded capture rollback cannot finish yet; other captures go on", "err", causeText(err))
	}
	s.settleUndecided(d)
}

// settleUndecided gives each undecided job whose commit a retry settled its
// end, read from its manifest: done (its results are to be recomputed from
// its files, its baseline included), or what its own failure manifest
// says, or failed when no manifest was written. A cancel that came while
// it was undecided is reported as §7.4 says: too late for a commit that
// was made, cancelled for one that was not. A manifest that cannot be read
// for any other reason leaves the job undecided for the next pass. Each
// settled job's registry entry is corrected. It runs under the captures
// lock and the directory slot, as retryStranded does.
func (s *Server) settleUndecided(d *captureDir) {
	s.capMu.Lock()
	list := slices.Clone(s.undecided)
	s.capMu.Unlock()
	for _, j := range list {
		t := j.txn
		if t.decide != nil && !t.dead {
			continue
		}
		m, err := s.readOwnManifest(d, t)
		var e *capture.Error
		switch {
		case err == nil && m.Outcome == captureOK:
		case err == nil:
			if e = errorFromWire(m.Error); e == nil {
				e = capture.Errorf(capture.CodeDisk, "", "capture %s/%s failed", t.series, formatID(t.id))
			}
		case errors.Is(err, fs.ErrNotExist):
			if e = t.settled; e == nil {
				e = capture.Errorf(capture.CodeDisk, "", "capture %s/%s was not committed: its manifest link was not made", t.series, formatID(t.id))
			}
		default:
			s.log.Warn("an undecided capture's manifest cannot be read; it stays undecided", "job", j.id, "err", causeText(err))
			continue
		}
		var b []byte
		// The decision word is settled under the job's mutex, in the same
		// step as the final status, so a cancel is either seen here or
		// answered from that status: none is lost.
		j.set(func() {
			if e == nil {
				res := captureResult{
					Capture: s.captureInfo(m), Notes: m.Notes, Cancel: j.d.settle(true),
					Derived: derivedRecompute, Handles: "reopen", Recompute: new(s.recomputeFor(m, j.base, "")),
				}
				b, _ = json.Marshal(res)
				j.status, j.phase, j.derived, j.err, j.result = jobDone, "", derivedRecompute, nil, b
				return
			}
			if j.d.settle(false) == cancelAnswerCancelled && e.Code != capture.CodeCancelled {
				e = cancelledError()
			}
			j.status, j.phase, j.err = statusOf(e), "", e
		})
		s.capMu.Lock()
		s.undecided = slices.DeleteFunc(s.undecided, func(u *job) bool { return u == j })
		s.capMu.Unlock()
		j.txn, j.base = nil, nil
		s.log.Info("undecided capture settled", "job", j.id, "status", j.view().Status)
		if err := s.registry.update(j, len(b)); err != nil {
			s.log.Error("capture registry", "job", j.id, "err", err)
		}
	}
}

// readOwnManifest reads the manifest of t's capture, which must carry t's
// token. An error wrapping fs.ErrNotExist means there is none.
func (s *Server) readOwnManifest(d *captureDir, t *captureTxn) (manifest, error) {
	if _, err := d.root.Lstat(t.series); err != nil {
		return manifest{}, err
	}
	ser, err := d.openSeries(t.series)
	if err != nil {
		return manifest{}, err
	}
	defer func() { _ = ser.Close() }()
	m, err := settleRead(ser, manifestName(t.id, t.in.Label))
	if err != nil {
		return manifest{}, err
	}
	if m.Token != t.in.Token {
		return manifest{}, fmt.Errorf("the manifest of %s/%s is another capture's", t.series, formatID(t.id))
	}
	return m, nil
}

// settleRead reads the manifest a settle judges; a seam for tests.
var settleRead = readManifest

// errorFromWire is the failure a manifest records as a capture error, nil
// for nil: what an agent acts on survives the round trip (retryable and
// when, what changed, who survived).
func errorFromWire(w *wireError) *capture.Error {
	if w == nil {
		return nil
	}
	e := capture.Errorf(capture.Code(w.Code), w.Next, "%s", w.Message)
	e.Retryable = w.Retryable
	if d, err := time.ParseDuration(w.RetryAfter); err == nil {
		e.RetryAfter = d
	}
	e.PerfAgentExit, e.StderrTail, e.Changed = w.PerfAgentExit, w.StderrTail, w.Changed
	for _, sv := range w.Survivors {
		pid, _ := strconv.Atoi(sv.PID)
		start, _ := strconv.ParseUint(sv.StartTime, 10, 64)
		e.Survivors = append(e.Survivors, capture.Survivor{PID: pid, Comm: sv.Comm, StartTime: start, State: sv.State})
	}
	return e
}
