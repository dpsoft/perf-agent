package perfmcp

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// leaveUndecided records a capture whose commit nobody could read: it may
// have committed, so it is neither done nor failed. It leaves the active
// slot, and the pass that settles it from the disk tells the job.
func (r *jobRun) leaveUndecided(e *capture.Error) {
	s, j := r.s, r.j
	j.txn = r.txn
	j.set(func() { j.phase, j.err = phaseUndecided, e })
	s.capMu.Lock()
	s.undecided = append(s.undecided, j)
	s.capMu.Unlock()
	s.log.Warn("capture undecided: its manifest link had an outcome that could not be read", "job", j.id, "capture", j.captureIDNow(), "err", e)
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
		s.log.Warn("a stranded capture rollback cannot finish yet; other captures go on", "err", err)
	}
	s.settleUndecided(d)
}

// settleUndecided gives each undecided job whose commit a retry settled its
// end, read from its manifest: done (its results are to be recomputed) or
// failed, and corrects its registry entry. It runs under the captures
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
		s.capMu.Lock()
		s.undecided = slices.DeleteFunc(s.undecided, func(u *job) bool { return u == j })
		s.capMu.Unlock()
		m, err := s.readOwnManifest(d, t)
		var b []byte
		switch {
		case err == nil && m.Outcome == captureOK:
			res := captureResult{Capture: s.captureInfo(m), Derived: derivedRecompute, Handles: "reopen", Recompute: new(s.recomputeFor(m, nil, ""))}
			b, _ = json.Marshal(res)
			j.set(func() { j.status, j.phase, j.derived, j.err, j.result = jobDone, "", derivedRecompute, nil, b })
		default:
			e := capture.Errorf(capture.CodeDisk, "", "capture %s/%s was not committed: its manifest link was not made", t.series, formatID(t.id))
			if err == nil && m.Error != nil {
				e = &capture.Error{Code: capture.Code(m.Error.Code), Message: m.Error.Message, Next: m.Error.Next}
			}
			j.set(func() { j.status, j.phase, j.err = jobFailed, "", e })
		}
		j.txn = nil
		s.log.Info("undecided capture settled", "job", j.id, "status", j.view().Status)
		if err := s.registry.update(j, len(b)); err != nil {
			s.log.Error("capture registry", "job", j.id, "err", err)
		}
	}
}

// readOwnManifest reads the manifest of t's capture, which must carry t's
// token.
func (s *Server) readOwnManifest(d *captureDir, t *captureTxn) (manifest, error) {
	ser, err := d.openSeries(t.series)
	if err != nil {
		return manifest{}, err
	}
	defer func() { _ = ser.Close() }()
	m, err := readManifest(ser, manifestName(t.id, t.in.Label))
	if err != nil {
		return manifest{}, err
	}
	if m.Token != t.in.Token {
		return manifest{}, fmt.Errorf("the manifest of %s/%s is another capture's", t.series, formatID(t.id))
	}
	return m, nil
}
