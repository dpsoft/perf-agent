package perfmcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// The settings a capture gets for what it does not say (§4.2), and the
// series and label.
const (
	defaultCaptureDuration = 10 * time.Second
	defaultSampleRate      = 99
	defaultUnwind          = "auto"
	defaultSeries          = "adhoc"
	defaultLabel           = "run"
)

// The settings by their argument names, as Explicit and the messages name
// them.
const (
	argModes        = "modes"
	argDuration     = "duration"
	argSampleRate   = "sample_rate"
	argUnwind       = "unwind"
	argKernelStacks = "kernel_stacks"
)

// startJob admits req, in memory only: no I/O, no lock, never the engine.
// The windows are reserved at a safe upper bound of the duration: the one
// passed, else Capture.MaxDuration when it is to be inherited from a
// baseline, else the default; the job reconciles it once it knows. A busy
// slot or full windows refuse the job; otherwise it runs on its own
// goroutine and every later failure is the job's state.
func (s *Server) startJob(req captureRequest) (*job, *capture.Error) {
	if s.captureDisabled != "" {
		return nil, capture.Errorf(capture.CodeDisabled, "", "%s", s.captureDisabled)
	}
	reserve := cmp.Or(req.Settings.Duration, defaultCaptureDuration)
	if !req.Explicit[argDuration] && req.CompareWith != "" {
		reserve = s.lim.Capture.MaxDuration
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.closed {
		return nil, capture.Errorf(capture.CodeClosing, "start a new perf-agent-mcp to capture", "this server is closing and starts no capture")
	}
	now := s.clock()
	j := newJob(s.registry.newJobID(), now, reserve)
	j.req, j.began = req, time.Now()
	if e := s.registry.admit(j, now); e != nil {
		return nil, e
	}
	s.jobsWG.Add(1)
	go s.runJob(j)
	return j, nil
}

// jobRun is one job's working state, on its own goroutine.
type jobRun struct {
	s *Server
	j *job
	// ctx ends when the job is cancelled or the server closes, which ends
	// its waits for the directory and the engine.
	ctx           context.Context
	series, label string

	launch  *capture.LaunchSpec
	d       *captureDir
	dirHeld bool
	lease   *capture.Lease
	holder  capture.Holder
	eff     capture.Settings
	base    *manifest
	evicted []string
	ser     *os.Root
	id      int
	txn     *captureTxn
	staging *os.Root
	// m is what the job knows of the capture for its manifest.
	m          manifest
	o          *capture.Outcome
	cancelNote string
	// paths are the server's paths scrub replaces, the longest first, as
	// strings.NewReplacer pairs; slack is the longest of them, which Run
	// keeps beyond each tail so a path across the tail's cut is replaced
	// whole.
	paths []string
	slack int
}

// runJob runs j through §7.4's steps, then records how it ended.
func (s *Server) runJob(j *job) {
	defer s.jobsWG.Done()
	ctx, stop := context.WithCancel(s.ctx)
	defer stop()
	go func() {
		select {
		case <-j.cancel:
			stop()
		case <-ctx.Done():
		}
	}()
	r := &jobRun{s: s, j: j, ctx: ctx, series: cmp.Or(j.req.Series, defaultSeries), label: cmp.Or(j.req.Label, defaultLabel)}
	status, e := r.capture()
	r.release()
	// Whatever the end, the capture directory may have been written.
	s.historyChanged()
	switch status {
	case jobDone:
		r.done()
	case jobCommitting:
		r.leaveUndecided(e)
	default:
		r.end(status, e)
	}
}

// capture runs the steps up to the commit and returns the state the job
// ends in: done once committed, committing when the commit is undecided,
// or failed or cancelled with why.
func (r *jobRun) capture() (jobStatus, *capture.Error) {
	s, j := r.s, r.j
	j.setState(jobPreparing, "lease")
	r.m = manifest{Kept: j.req.Keep}
	if err := r.resolve(); err != nil {
		return r.fail(err)
	}
	d, e := s.openCaptureDir()
	if e != nil {
		return r.fail(e)
	}
	r.d = d
	if err := r.boundary(); err != nil {
		return r.fail(err)
	}
	if !s.lockDir(j.cancel) {
		return r.fail(cancelledError())
	}
	r.dirHeld = true
	start, _ := s.startTime()
	r.holder = capture.Holder{ServerPID: os.Getpid(), ServerStart: start, Series: r.series, Deadline: r.deadline(j.reserve)}
	lease, e := capture.TryLock(d.root, r.holder)
	if e != nil {
		return r.fail(e)
	}
	r.lease = lease
	if err := s.fault(faultAfterLease); err != nil {
		return r.fail(err)
	}

	j.setState(jobPreparing, "recovery")
	s.retryStrandedPass(d)
	if _, err := s.recoverCaptures(d); err != nil {
		return r.fail(err)
	}
	if err := s.fault(faultAfterRecovery); err != nil {
		return r.fail(err)
	}
	if err := r.boundary(); err != nil {
		return r.fail(err)
	}

	j.setState(jobPreparing, "retention")
	p, err := r.engineSection()
	if err != nil {
		return r.fail(err)
	}
	if err := r.retain(p); err != nil {
		return r.fail(err)
	}
	if err := s.fault(faultAfterEngine); err != nil {
		return r.fail(err)
	}
	if err := r.boundary(); err != nil {
		return r.fail(err)
	}

	ser, err := d.series(r.series)
	if err != nil {
		return r.fail(err)
	}
	r.ser = ser
	id, err := s.nextID(ser)
	if err != nil {
		return r.fail(err)
	}
	r.id = id
	captureID := captureKey(r.series, formatID(id))
	j.set(func() { j.captureID = captureID })
	r.holder.CaptureID, r.holder.Deadline = captureID, r.deadline(r.eff.Duration)
	if err := lease.Update(r.holder); err != nil {
		s.log.Debug("capture lock holder", "err", err)
	}
	if err := s.fault(faultAfterSeq); err != nil {
		return r.fail(err)
	}
	if err := r.boundary(); err != nil {
		return r.fail(err)
	}

	j.setState(jobPreparing, "staging")
	txn, err := s.beginTxn(d, r.series, id, r.label, toWire(r.eff), capture.ExpectedFiles(r.eff.Modes))
	r.txn = txn
	if err != nil {
		return r.fail(err)
	}
	if err := txn.fault(faultAfterIntent); err != nil {
		return r.fail(err)
	}
	if err := r.spaceCheck(); err != nil {
		return r.fail(err)
	}
	if err := txn.fault(faultAfterSpace); err != nil {
		return r.fail(err)
	}
	if err := txn.makeStaging(); err != nil {
		return r.fail(err)
	}
	if err := txn.fault(faultAfterStaging); err != nil {
		return r.fail(err)
	}
	if err := r.boundary(); err != nil {
		return r.fail(err)
	}

	j.setState(jobPreparing, "launching")
	run, e := r.start()
	if e != nil {
		return r.fail(e)
	}
	j.run.Store(run)
	// KillCaptures stores killed before it loads run, and this loads
	// killed after storing run, so one of the two kills it.
	if s.killed.Load() {
		run.Kill()
	}
	r.recordReaper(run)
	j.setState(jobRunning, "")
	o := r.relay(run)
	r.scrub(&o)
	r.o = &o
	r.m = r.manifestFrom(o)
	if o.Err != nil {
		return r.fail(o.Err)
	}
	return r.commit()
}

// deadline is the latest a capture of duration d started now can end, as
// the lock file records it.
func (r *jobRun) deadline(d time.Duration) time.Time {
	c := r.s.lim.Capture
	return r.s.clock().Add(c.StartTimeout + d + c.FinishTimeout + c.StopGrace)
}

// boundary is a cancel noticed between two steps, before the capture has
// started: the job stops there.
func (r *jobRun) boundary() error {
	if r.j.d.cancelled() {
		return cancelledError()
	}
	if r.s.killed.Load() {
		return capture.Errorf(capture.CodeCancelled, "", "the server is shutting down; the capture was cancelled before it started")
	}
	return nil
}

// cause is err as the failure the job reports.
func (r *jobRun) cause(err error) *capture.Error {
	if e, ok := errors.AsType[*capture.Error](err); ok {
		return e
	}
	if r.d != nil {
		return r.d.cause(err)
	}
	return capture.Errorf(capture.CodeDisk, "", "%v", err)
}

// fail ends the capture after err, before it committed: a failure after
// the intent is rolled back by the transaction, and one after the id but
// before the intent leaves a failure manifest. A simulated crash stops at
// once and cleans nothing up, as a dead process would.
func (r *jobRun) fail(err error) (jobStatus, *capture.Error) {
	if errors.Is(err, errCrash) {
		return r.crashed()
	}
	cause := r.cause(err)
	switch {
	case r.txn != nil:
		rerr := r.txn.rollback(cause, r.m)
		switch {
		case errors.Is(rerr, errCrash):
			return r.crashed()
		case errors.Is(rerr, errAlreadyCommitted):
			return jobDone, nil
		case rerr != nil:
			r.s.log.Warn("capture rollback stopped; it is retried before the next capture", "capture", r.j.captureIDNow(), "err", rerr)
		}
	case r.id > 0:
		if werr := r.s.writeFailure(r.d, r.series, r.id, r.label, toWire(r.eff), cause, r.m); werr != nil {
			if errors.Is(werr, errCrash) {
				return r.crashed()
			}
			r.s.log.Warn("cannot record the capture's failure", "capture", r.j.captureIDNow(), "err", werr)
		}
	}
	return statusOf(cause), cause
}

// failed is the end of a capture its transaction has already rolled back
// with err.
func (r *jobRun) failed(err error) (jobStatus, *capture.Error) {
	if errors.Is(err, errCrash) {
		return r.crashed()
	}
	cause := r.cause(err)
	return statusOf(cause), cause
}

// crashed is a simulated crash: everything stays where it is.
func (r *jobRun) crashed() (jobStatus, *capture.Error) {
	if r.txn != nil {
		r.txn.die()
	}
	return jobFailed, capture.Errorf(capture.CodeDisk, "", "the capture stopped at a simulated crash; recovery finishes it")
}

func statusOf(e *capture.Error) jobStatus {
	if e.Code == capture.CodeCancelled {
		return jobCancelled
	}
	return jobFailed
}

// captureIDNow is j's capture id, "" before it has one.
func (j *job) captureIDNow() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.captureID
}

// resolve checks a command and its directory without side effects: the
// directory must be inside a source root or the profiles root, and the
// program one this server may launch.
func (r *jobRun) resolve() error {
	req := r.j.req
	if len(req.Command) == 0 {
		return nil
	}
	cwd, e := r.s.resolveCwd(req.Cwd)
	if e != nil {
		return e
	}
	path, e := capture.ResolveCommand(req.Command[0], envValue(capture.WorkloadEnv(os.Environ(), ""), "PATH"), r.s.captureCfg.Commands)
	if e != nil {
		return e
	}
	if e := capture.CheckCommandFile(path); e != nil {
		return e
	}
	r.launch = &capture.LaunchSpec{Path: path, Args: slices.Clone(req.Command), Dir: cwd}
	return nil
}

// envValue is key's value in env, "" when it has none.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// resolveCwd is the directory a command runs in: cwd with its symbolic
// links resolved, which must be a directory inside a source root or the
// profiles root; "" is the first source root, else the profiles root. A
// relative cwd is relative to the profiles root.
func (s *Server) resolveCwd(cwd string) (string, *capture.Error) {
	const next = "pass cwd as a directory inside a --source root or the profiles root"
	dirs := s.src.Dirs()
	if cwd == "" {
		if len(dirs) > 0 {
			return dirs[0], nil
		}
		return s.profiles.Name(), nil
	}
	// Messages repeat the agent's cwd, cut, and name the roots by kind:
	// never an absolute path of the server's.
	p := cwd
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.profiles.Name(), p)
	}
	real, err := s.evalSymlinks(p)
	if err != nil {
		return "", capture.Errorf(capture.CodeInvalidArgument, next, "cwd %q cannot be resolved: %v", echo(cwd), pathless(err))
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", capture.Errorf(capture.CodeInvalidArgument, next, "cwd %q is not a directory", echo(cwd))
	}
	for _, dir := range dirs {
		for _, root := range []string{dir, s.resolved(dir)} {
			if rel, err := filepath.Rel(root, real); err == nil && filepath.IsLocal(rel) {
				return real, nil
			}
		}
	}
	if _, err := s.rel(s.profiles, real); err == nil {
		return real, nil
	}
	return "", capture.Errorf(capture.CodeInvalidArgument, next, "cwd %q is not inside a --source root or the profiles root", echo(cwd))
}

// resolved is the server's own directory dir with its symbolic links
// resolved, or dir when that fails.
func (s *Server) resolved(dir string) string {
	if real, err := s.evalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// recordReaper records the capture's reaper in its intent, so recovery
// judges the capture's owner alive while the reaper runs, and in the lock
// file, so a busy answer names it. Neither failure stops the capture: the
// reaper holds the lock, so no recovery runs while it lives.
func (r *jobRun) recordReaper(run *capture.Run) {
	pid, start := run.Reaper()
	if pid <= 0 || start == 0 {
		return
	}
	if err := r.txn.setReaper(pid, start); err != nil {
		r.s.log.Warn("cannot record the capture's reaper in its intent", "capture", r.j.captureIDNow(), "err", causeText(err))
	}
	r.holder.ReaperPID, r.holder.ReaperStart = pid, start
	if err := r.lease.Update(r.holder); err != nil {
		r.s.log.Debug("capture lock holder", "err", causeText(err))
	}
}

// retain runs retention against p, without the engine. Its failure is
// logged, and the space check decides whether the capture can go on.
func (r *jobRun) retain(p protection) error {
	evicted, err := r.s.retain(r.d, p, r.s.clock())
	r.evicted = evicted
	if errors.Is(err, errCrash) {
		return err
	}
	if err != nil {
		r.s.log.Warn("capture retention stopped; the space check decides", "err", causeText(err))
	}
	return nil
}

// engineSection is §7.4 step 3, holding the engine: the baseline is
// resolved and checked, the effective settings are found and checked
// against this server and charged to the windows, keep is checked, and
// what retention must keep is read, the baseline pinned among it. The
// engine is released before retention hashes and removes files.
func (r *jobRun) engineSection() (protection, error) {
	s, j := r.s, r.j
	if err := s.engine.acquire(r.ctx); err != nil {
		return protection{}, cancelledError()
	}
	defer s.engine.release()
	if err := s.fault(faultInEngine); err != nil {
		return protection{}, err
	}
	var (
		ms  []manifest
		bad []string
		ser *os.Root
	)
	if _, err := r.d.root.Lstat(r.series); err == nil {
		if ser, err = r.d.openSeries(r.series); err != nil {
			return protection{}, err
		}
		defer func() { _ = ser.Close() }()
		if ms, bad, err = seriesManifests(ser); err != nil {
			return protection{}, r.d.writeError(err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return protection{}, r.d.writeError(err)
	}
	if j.req.CompareWith != "" {
		base, err := r.baseline(ser, ms, bad)
		if err != nil {
			return protection{}, err
		}
		r.base = base
		r.m.CompareWith = base.ID
	}
	eff, err := r.effective()
	if err != nil {
		return protection{}, err
	}
	r.eff = eff
	j.set(func() { j.duration = eff.Duration })
	if err := s.registry.reconcile(j, eff.Duration, s.clock()); err != nil {
		return protection{}, err
	}
	if j.req.Keep {
		kept := 0
		for _, m := range ms {
			if m.Kept {
				kept++
			}
		}
		if kept >= s.lim.Capture.MaxKeptPerSeries {
			return protection{}, capture.Errorf(capture.CodeInvalidArgument, "pass keep false, or start a new series",
				"series %s already holds %d kept captures, Capture.MaxKeptPerSeries=%d; one more would let keep defeat retention",
				r.series, kept, s.lim.Capture.MaxKeptPerSeries)
		}
	}
	// The baseline is pinned: retention keeps it whatever its age.
	baseline := ""
	if r.base != nil {
		baseline = captureKey(r.series, r.base.ID)
	}
	return s.protected(r.d, baseline), nil
}

// lastOK is how many of a series' ok captures a refused baseline lists.
const lastOK = 5

// baseline is the compare_with capture of the series: it must exist, be
// ok, and its files must still be the ones its manifest records.
func (r *jobRun) baseline(ser *os.Root, ms []manifest, bad []string) (*manifest, error) {
	cw := r.j.req.CompareWith
	n, ok := parseID(cw)
	if !ok {
		return nil, capture.Errorf(capture.CodeInvalidArgument, "pass a capture id of this series, as capture_list shows it, such as 001",
			"compare_with %q is not a capture id", echo(cw))
	}
	id := formatID(n)
	var oks []string
	for _, m := range ms {
		if m.Outcome == captureOK {
			oks = append(oks, m.ID)
		}
	}
	if len(oks) > lastOK {
		oks = oks[len(oks)-lastOK:]
	}
	listed := fmt.Sprintf("; series %s has no ok capture", r.series)
	if len(oks) > 0 {
		listed = fmt.Sprintf("; the last ok captures of series %s are %s", r.series, strings.Join(oks, ", "))
	}
	const next = "pass one of the ok captures capture_list shows, or capture without compare_with"
	refuse := func(format string, args ...any) error {
		return capture.Errorf(capture.CodeUnknownBaseline, next, "%s%s", fmt.Sprintf(format, args...), listed)
	}
	for i := range ms {
		m := ms[i]
		if m.ID != id {
			continue
		}
		switch m.Outcome {
		case captureOK:
		case captureInterrupted:
			return nil, refuse("capture %s/%s was interrupted; it has no profile to compare with", r.series, id)
		default:
			why := "failed"
			if m.Error != nil {
				why = "failed: " + m.Error.Code
			}
			return nil, refuse("capture %s/%s %s; it has no profile to compare with", r.series, id, why)
		}
		if err := verifyFiles(ser, m); err != nil {
			return nil, refuse("capture %s/%s's files are not the ones its manifest records (%v)", r.series, id, pathless(err))
		}
		return &m, nil
	}
	for _, b := range bad {
		if got, _, _, ok := parseName(b); ok && got == n {
			return nil, refuse("the manifest of capture %s/%s cannot be read", r.series, id)
		}
	}
	return nil, refuse("series %s has no capture %s", r.series, id)
}

// effective is the capture's settings: those the agent passed, the rest
// inherited from the baseline (where one passed differs from it, the
// capture is incomparable) or defaulted, then checked against this
// server's limits, shim and capabilities, which may differ from the
// server that captured the baseline.
func (r *jobRun) effective() (capture.Settings, error) {
	req := r.j.req
	eff := req.Settings
	inherited := map[string]bool{}
	if r.base != nil {
		bs, err := fromWire(r.base.Settings)
		if err != nil {
			return eff, capture.Errorf(capture.CodeUnknownBaseline, "compare with another capture of the series, or start a new series",
				"the settings baseline %s/%s records are not ones this server can read (duration %q, sample_rate %q)",
				r.series, r.base.ID, echo(r.base.Settings.Duration), echo(r.base.Settings.SampleRate))
		}
		bw, rw := toWire(bs), toWire(req.Settings)
		fields := []struct {
			name      string
			base, got string
			inherit   func()
		}{
			{argModes, strings.Join(bw.Modes, ","), strings.Join(rw.Modes, ","), func() { eff.Modes = bs.Modes }},
			{argDuration, bw.Duration, rw.Duration, func() { eff.Duration = bs.Duration }},
			{argSampleRate, bw.SampleRate, rw.SampleRate, func() { eff.SampleRate = bs.SampleRate }},
			{argUnwind, bw.Unwind, rw.Unwind, func() { eff.Unwind = bs.Unwind }},
			{argKernelStacks, strconv.FormatBool(bw.KernelStacks), strconv.FormatBool(rw.KernelStacks), func() { eff.KernelStacks = bs.KernelStacks }},
		}
		for _, f := range fields {
			switch {
			case !req.Explicit[f.name]:
				f.inherit()
				inherited[f.name] = true
			case f.got != f.base:
				return eff, capture.Errorf(capture.CodeIncomparable,
					fmt.Sprintf("drop %s to inherit the baseline's %s, or start a new series", f.name, f.base),
					"%s is %s here and %s in baseline %s/%s", f.name, f.got, f.base, r.series, r.base.ID)
			}
		}
	}
	if len(eff.Modes) == 0 {
		eff.Modes = []capture.Mode{capture.ModeCPU}
	}
	eff.Duration = cmp.Or(eff.Duration, defaultCaptureDuration)
	eff.SampleRate = cmp.Or(eff.SampleRate, defaultSampleRate)
	eff.Unwind = cmp.Or(eff.Unwind, defaultUnwind)
	eff = eff.Canonical()
	from := func(name string) string {
		if inherited[name] {
			return fmt.Sprintf(" (inherited from baseline %s/%s)", r.series, r.base.ID)
		}
		return ""
	}
	c := r.s.lim.Capture
	switch {
	case eff.Duration <= 0 || eff.Duration > c.MaxDuration:
		return eff, capture.Errorf(capture.CodeInvalidArgument, "pass a shorter duration, or start a new series",
			"duration %v%s is outside (0, Capture.MaxDuration=%v] of this server", eff.Duration, from(argDuration), c.MaxDuration)
	case eff.SampleRate < 1 || eff.SampleRate > c.MaxSampleRate:
		return eff, capture.Errorf(capture.CodeInvalidArgument, "pass a lower sample_rate, or start a new series",
			"sample_rate %d%s is outside [1, Capture.MaxSampleRate=%d] of this server", eff.SampleRate, from(argSampleRate), c.MaxSampleRate)
	case slices.Contains(eff.Modes, capture.ModeGPU) && r.s.shim.Path == "":
		return eff, capture.Errorf(capture.CodeDisabled, "start perf-agent-mcp with --gpu-shim PATH",
			"mode gpu%s needs the CUPTI adapter, and this server has none", from(argModes))
	case eff.KernelStacks && !r.s.perfAgent.Caps.Has(capture.CapSyslog):
		return eff, capture.Errorf(capture.CodeInvalidArgument, "add cap_syslog to perf-agent's file capabilities: "+capture.SetcapLine,
			"kernel_stacks%s needs cap_syslog in perf-agent's file capabilities, or kernel frames come out unsymbolized", from(argKernelStacks))
	}
	return eff, nil
}

// fromWire is the settings a manifest records.
func fromWire(w wireSettings) (capture.Settings, error) {
	d, err := time.ParseDuration(w.Duration)
	if err != nil {
		return capture.Settings{}, err
	}
	rate, err := strconv.Atoi(w.SampleRate)
	if err != nil {
		return capture.Settings{}, err
	}
	st := capture.Settings{Duration: d, SampleRate: rate, Unwind: w.Unwind, KernelStacks: w.KernelStacks}
	for _, m := range w.Modes {
		st.Modes = append(st.Modes, capture.Mode(m))
	}
	return st.Canonical(), nil
}

// spaceCheck is §7.4 step 6, after retention: the capture directory must
// have room under Capture.MaxStoredBytes for what this capture may write,
// and its filesystem Capture.MinFreeBytes free.
func (r *jobRun) spaceCheck() error {
	s, c := r.s, r.s.lim.Capture
	need := c.captureNeed(r.eff.Modes)
	used, err := dirBytes(r.d.root)
	if err != nil {
		return r.d.writeError(err)
	}
	const next = "remove captures of that series by hand, or raise --limit Capture.MaxStoredBytes"
	if !s.storeHasRoom(used, need) {
		return capture.Errorf(capture.CodeDisk, next,
			"the capture directory %s holds %d bytes after retention, and this capture may write %d more, over Capture.MaxStoredBytes=%d%s",
			r.d.rel, used, need, c.MaxStoredBytes, largestSeries(r.d))
	}
	free, err := r.freeBytes()
	if err != nil {
		return capture.Errorf(capture.CodeDisk, "check that the capture directory's filesystem is mounted and readable, or start perf-agent-mcp with --captures naming another directory inside the profiles root",
			"cannot read the free space of the capture directory %s: %s", r.d.rel, causeText(err))
	}
	if free < c.MinFreeBytes {
		return capture.Errorf(capture.CodeDisk, "free space on that filesystem, or lower --limit Capture.MinFreeBytes",
			"the filesystem of the capture directory %s has %d bytes free, under Capture.MinFreeBytes=%d%s",
			r.d.rel, free, c.MinFreeBytes, largestSeries(r.d))
	}
	return nil
}

// freeBytes is the free space of the capture directory's filesystem, read
// through the directory the job holds open.
func (r *jobRun) freeBytes() (int64, error) {
	f, err := r.d.root.Open(".")
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return capture.FreeBytes(f)
}

// largestSeries names the series using most space, for a disk message.
func largestSeries(d *captureDir) string {
	names, err := seriesDirs(d)
	if err != nil {
		return ""
	}
	best, most := "", int64(-1)
	for _, n := range names {
		ser, err := d.openSeries(n)
		if err != nil {
			continue
		}
		b, err := dirBytes(ser)
		_ = ser.Close()
		if err == nil && b > most {
			best, most = n, b
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("; the series using most space is %s (%d bytes)", best, most)
}

// start starts the capture in its staging directory.
func (r *jobRun) start() (*capture.Run, *capture.Error) {
	s, t := r.s, r.txn
	staging, err := t.ser.OpenRoot(t.in.Staging)
	if err != nil {
		return nil, r.d.writeError(err)
	}
	r.staging = staging
	spec := capture.RunSpec{
		Self:        s.self,
		Binary:      s.perfAgent,
		Proc:        capture.DefaultProc(),
		SelfID:      capture.SelfID{UID: os.Getuid(), GID: os.Getgid(), PID: os.Getpid()},
		PID:         r.j.req.PID,
		Settings:    r.eff,
		Staging:     staging,
		StagingPath: filepath.Join(t.ser.Name(), t.in.Staging),
		CaptureID:   captureKey(r.series, formatID(r.id)),
		Bounds:      r.tailBounds(),
		Lock:        r.lease.File(),
		Log:         s.log,
		FaultPause:  s.reaperPause,
	}
	shim := ""
	if slices.Contains(r.eff.Modes, capture.ModeGPU) {
		spec.GPU = new(s.shim)
		shim = s.shim.Path
	}
	if r.launch != nil {
		l := *r.launch
		l.Env = capture.WorkloadEnv(os.Environ(), shim)
		spec.Launch = &l
		spec.Cgroup = s.cgroupParent()
	}
	return capture.Start(spec)
}

// captureBounds are the bounds a capture runs under.
func (s *Server) captureBounds() capture.Bounds {
	c := s.lim.Capture
	b := capture.DefaultBounds()
	b.StartTimeout, b.FinishTimeout, b.StopGrace = c.StartTimeout, c.FinishTimeout, c.StopGrace
	b.MaxOutputTail = c.MaxOutputTail
	b.MaxProfileBytes, b.MaxExeclogBytes = c.MaxProfileBytes, c.MaxExeclogBytes
	return b
}

// relay waits for the capture's outcome, passing a cancel on to it: one
// requested before the capture existed at once, and one that comes while
// it runs as it comes. The cancel channel is dropped once relayed, so a
// closed one is not received again.
func (r *jobRun) relay(run *capture.Run) capture.Outcome {
	out := make(chan capture.Outcome, 1)
	go func() { out <- run.Wait() }()
	cancel := r.j.cancel
	if r.j.d.cancelled() {
		run.Cancel()
		cancel = nil
	}
	for {
		select {
		case o := <-out:
			return o
		case <-cancel:
			run.Cancel()
			cancel = nil
		}
	}
}

// tailBounds are the capture's bounds, with each output tail kept longer
// by the longest server path scrub replaces.
func (r *jobRun) tailBounds() capture.Bounds {
	staging := filepath.Join(r.txn.ser.Name(), r.txn.in.Staging)
	root := r.s.profiles.Name()
	r.paths, r.slack = nil, 0
	for _, p := range []struct{ path, as string }{{staging, "staging"}, {root, "the profiles root"}} {
		for _, path := range slices.Compact([]string{p.path, r.s.resolved(p.path)}) {
			r.paths = append(r.paths, path, p.as)
			r.slack = max(r.slack, len(path))
		}
	}
	b := r.s.captureBounds()
	b.MaxOutputTail += r.slack
	return b
}

// scrub takes the server's own paths out of what the capture's programs
// printed, before it leaves the server: perf-agent names its output files
// ("profile written to <path>"), and a workload may print its directory.
// The staging directory becomes "staging" and the profiles root "the
// profiles root", each as given and with its links resolved. Run kept
// each tail longer by the longest such path: a tail it cut loses its first
// line (all of it up to the slack when it has no line break), which may
// hold a path cut short; then the paths are replaced and the tail is cut to
// MaxOutputTail. The rest of the text is the programs' own.
func (r *jobRun) scrub(o *capture.Outcome) {
	rep := strings.NewReplacer(r.paths...)
	n := r.s.lim.Capture.MaxOutputTail
	kept := n + r.slack
	tail := func(s string) string {
		if len(s) > kept-utf8.UTFMax {
			if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
				s = s[i+1:]
			} else {
				s = lastBytes(s, len(s)-r.slack)
			}
		}
		return lastBytes(rep.Replace(s), n)
	}
	o.StderrTail = tail(o.StderrTail)
	o.Workload.Stdout, o.Workload.Stderr = tail(o.Workload.Stdout), tail(o.Workload.Stderr)
	if o.Err != nil {
		e := *o.Err
		e.StderrTail, e.Message = tail(e.StderrTail), rep.Replace(e.Message)
		o.Err = &e
	}
}

// lastBytes is the last n bytes of s at most, starting on a rune boundary.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// kernelNote is the note of a capture with kernel stacks.
const kernelNote = "the profile contains kernel addresses (kernel_stacks)"

// manifestFrom is what the manifest records of the capture o: the target
// by its comm and exe basename, never its command line; the command's
// program and argument count, never its arguments.
func (r *jobRun) manifestFrom(o capture.Outcome) manifest {
	s := r.s
	m := r.m
	if o.Target.PID > 0 {
		m.Target = manifestTarget{
			PID: strconv.Itoa(o.Target.PID), Comm: o.Target.Comm, BuildID: o.Target.BuildID,
			StartTime: strconv.FormatUint(o.Target.StartTime, 10), Launched: o.Launched,
		}
		if o.Target.Exe != "" {
			m.Target.Exe = filepath.Base(o.Target.Exe)
		}
	}
	if o.CommandPath != "" {
		m.Command = &manifestCommand{Path: o.CommandPath, Args: strconv.Itoa(o.ArgCount)}
	}
	m.PerfAgent = perfAgentInfo{Path: s.perfAgent.Path, Version: s.perfAgent.Version, Revision: s.perfAgent.Revision, Capabilities: s.perfAgent.Caps.Names()}
	m.Start = o.Started
	if o.AttachLatency > 0 {
		m.AttachLatency = o.AttachLatency.Round(time.Millisecond).String()
	}
	if o.Profiled > 0 {
		m.ProfiledDuration = o.Profiled.Round(time.Millisecond).String()
	}
	m.StoppedBy = o.StoppedBy
	if o.Launched && o.WorkloadExited && o.StoppedBy == capture.StoppedByCommandExit {
		m.Target.Exit = commandExit(o)
	}
	m.Notes = slices.Clone(o.Notes)
	if r.eff.KernelStacks {
		m.Notes = append(m.Notes, kernelNote)
	}
	m.Survivors = toWireSurvivors(o.Survivors)
	if o.Cleanup != "" && !capture.CleanupIsComplete(o.Cleanup) {
		m.Cleanup = o.Cleanup
	}
	return m
}

// commandExit is how a launched command that ended the capture exited:
// its status, or the signal that killed it, and after how long.
func commandExit(o capture.Outcome) string {
	after := o.Profiled.Round(time.Millisecond).String()
	if o.WorkloadSignal != "" {
		return fmt.Sprintf("killed by signal %q after %s", o.WorkloadSignal, after)
	}
	return fmt.Sprintf("exited %d after %s", o.WorkloadStatus, after)
}

// commit is §7.4 steps 8-11, as the transaction's finish runs them, with
// the job's phase at each.
func (r *jobRun) commit() (jobStatus, *capture.Error) {
	t, j := r.txn, r.j
	j.setState(jobCommitting, "validating")
	if err := t.validate(); err != nil {
		return r.failed(t.fail(err, r.m))
	}
	j.setState(jobCommitting, "linking")
	for _, step := range []func() error{t.record, t.link} {
		if err := step(); err != nil {
			return r.failed(t.fail(err, r.m))
		}
	}
	j.setState(jobCommitting, "manifest")
	committed, err := t.commit(r.m, &j.d)
	if committed {
		if err != nil {
			r.s.log.Warn("capture committed; its cleanup is left to recovery", "capture", j.captureIDNow(), "err", err)
		}
		r.cancelNote = t.cancelNote
		return jobDone, nil
	}
	if t.decide != nil {
		return jobCommitting, r.cause(err)
	}
	return r.failed(err)
}

// release lets go of what the capture held: its staging root, the series,
// the lease (the reaper may hold it a little longer) and the directory.
func (r *jobRun) release() {
	for _, root := range []*os.Root{r.staging, r.ser} {
		if root != nil {
			_ = root.Close()
		}
	}
	if r.lease != nil {
		_ = r.lease.Close()
	}
	if r.d != nil {
		r.d.close()
	}
	if r.dirHeld {
		r.s.unlockDir()
	}
}

// profiled is what the capture profiled, for the windows.
func (r *jobRun) profiled() time.Duration {
	if r.o == nil {
		return 0
	}
	return r.o.Profiled
}

// end records that the job failed or was cancelled with e.
//
// The registry frees the slot before the end is visible, so a client that
// sees it can start the next capture at once: until then the job answers a
// cancel from the state it is ending in.
func (r *jobRun) end(status jobStatus, e *capture.Error) {
	j := r.j
	j.mu.Lock()
	j.ending, j.endCode = status, e.Code
	j.mu.Unlock()
	if err := r.s.registry.finish(j, 0, r.profiled()); err != nil {
		r.s.log.Error("capture registry", "job", j.id, "err", err)
	}
	j.set(func() { j.status, j.phase, j.err = status, "", e })
	r.s.logBounded(slog.LevelInfo, "capture ended", "job", j.id, "capture", j.captureIDNow(), "status", status, "code", e.Code, "message", e.Message)
}

// done is a committed capture: it derives the open and compare results
// under the engine, records them, and lets go of its pins.
func (r *jobRun) done() {
	s, j := r.s, r.j
	m := r.txn.manifestOf(r.m, captureOK, nil)
	j.set(func() { j.status, j.phase, j.derived, j.err = jobDone, "deriving", derivedPending, nil })
	var res captureResult
	if err := s.engine.acquire(s.ctx); err != nil {
		res = s.underived(m, r.base, "the server is shutting down")
	} else {
		res = s.derive(j, m, r.base)
		s.pins.release(j)
		s.engine.release()
	}
	if r.o != nil && r.launch != nil {
		res.Capture.WorkloadOutputTail = &outputTail{Stdout: r.o.Workload.Stdout, Stderr: r.o.Workload.Stderr, Untrusted: true}
	}
	res.Capture.RequestedDuration = r.eff.Duration.String()
	res.Evicted, res.Cancel = r.evicted, r.cancelNote
	b, err := json.Marshal(res)
	if err != nil {
		s.log.Error("capture result", "job", j.id, "err", err)
	}
	derived := derivedReady
	if res.Derived == derivedFailed {
		derived = derivedFailed
	}
	// The slot is freed before a wait can see the results.
	if err := s.registry.finish(j, len(b), r.profiled()); err != nil {
		s.log.Error("capture registry", "job", j.id, "err", err)
	}
	rc := s.recomputeFor(m, r.base, "")
	j.set(func() { j.phase, j.derived, j.result, j.recipe = "", derived, b, &rc })
	s.logBounded(slog.LevelInfo, "capture done", "job", j.id, "capture", j.captureIDNow(), "derived", derived)
}
