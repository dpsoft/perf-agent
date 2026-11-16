package perfmcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// faultPoint names a boundary of the capture protocol (§7.4) where a test
// makes the step fail, or simulates a crash.
type faultPoint int

const (
	faultAfterLease faultPoint = iota + 1
	faultAfterRecovery
	faultInEngine
	faultAfterEngine
	faultAfterSeq
	faultAfterIntent
	faultAfterSpace
	faultAfterStaging
	faultAfterValidate
	faultAfterRecord
	faultBetweenLinks
	faultAfterLinks
	faultAfterManifestTemp
	faultInManifestLink
	faultAfterManifestLink
	faultAfterStagingRemoval
	faultInRollback
	faultInRecovery
	faultHash
	faultRetentionHash
	faultFsync
	faultBeforeOpen
	faultBeforeDiff
)

// faultInfo lists a fault point and whether it lies after the manifest
// link, the commit point.
type faultInfo struct {
	p        faultPoint
	name     string
	postLink bool
}

// faultPoints are every fault point, in protocol order.
var faultPoints = []faultInfo{
	{faultAfterLease, "after-lease", false},
	{faultAfterRecovery, "after-recovery", false},
	{faultInEngine, "in-engine", false},
	{faultAfterEngine, "after-engine", false},
	{faultAfterSeq, "after-seq", false},
	{faultAfterIntent, "after-intent", false},
	{faultAfterSpace, "after-space", false},
	{faultAfterStaging, "after-staging", false},
	{faultAfterValidate, "after-validate", false},
	{faultAfterRecord, "after-record", false},
	{faultBetweenLinks, "between-links", false},
	{faultAfterLinks, "after-links", false},
	{faultAfterManifestTemp, "after-manifest-temp", false},
	{faultInManifestLink, "in-manifest-link", false},
	{faultAfterManifestLink, "after-manifest-link", true},
	{faultAfterStagingRemoval, "after-staging-removal", true},
	{faultInRollback, "in-rollback", false},
	{faultInRecovery, "in-recovery", false},
	{faultHash, "hash", false},
	{faultRetentionHash, "retention-hash", false},
	{faultFsync, "fsync", false},
	{faultBeforeOpen, "before-open", false},
	{faultBeforeDiff, "before-diff", false},
}

func (p faultPoint) String() string {
	if i := slices.IndexFunc(faultPoints, func(f faultInfo) bool { return f.p == p }); i >= 0 {
		return faultPoints[i].name
	}
	return "fault-" + strconv.Itoa(int(p))
}

// errCrash, returned by a seam, simulates the process dying at that
// point: the protocol stops at once and cleans nothing up, as a killed
// server would, and leaves the rest to recovery.
var errCrash = errors.New("simulated crash")

// fault runs the captureFault seam at p.
func (s *Server) fault(p faultPoint) error {
	if s.captureFault == nil {
		return nil
	}
	return s.captureFault(p)
}

// wErr runs the writeErr seam for the write op.
func (s *Server) wErr(op string) error {
	if s.writeErr == nil {
		return nil
	}
	return s.writeErr(op)
}

// clock is the time, through the now seam.
func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// The heartbeat: an intent's owner refreshes its .intent.hb every
// heartbeatEvery, and recovery takes a heartbeat older than staleAfter
// for one whose owner no longer runs.
const (
	heartbeatEvery = 2 * time.Second
	staleAfter     = 10 * time.Second
)

// selfStart is this process's start time, as the intent records it.
var selfStart = sync.OnceValues(func() (uint64, error) {
	return capture.StartTime(capture.DefaultProc(), os.Getpid())
})

// ownerIsAlive reports whether in's server or reaper, by its PID and start
// time, still runs (a zombie or an exiting task does not), through the
// ownerAlive seam.
func (s *Server) ownerIsAlive(in intent) bool {
	if s.ownerAlive != nil {
		return s.ownerAlive(in)
	}
	p := capture.DefaultProc()
	return in.ServerPID > 0 && capture.Alive(p, in.ServerPID, in.ServerStart) ||
		in.ReaperPID > 0 && capture.Alive(p, in.ReaperPID, in.ReaperStart)
}

// captureEUID is the user a capture's outputs must belong to; a seam for
// tests.
var captureEUID = os.Geteuid

// The intent's states.
const (
	statePrepared    = "prepared"
	stateLinking     = "linking"
	stateRollingBack = "rolling_back"
)

// Names inside a staging directory besides perf-agent's outputs: the link
// probe and its second name.
const (
	stagingPrefix = ".staging-"
	probeName     = ".probe"
	probeLinkName = ".probe-link"
)

// intent is NNN-label.intent.json: what a capture in flight may create,
// and the identity of everything it did create, so that its owner can
// roll it back and recovery can finish or undo it after a crash, deleting
// only what it can prove (§7.4).
type intent struct {
	Format   int          `json:"format"`
	State    string       `json:"state"`
	ID       string       `json:"id"`
	Series   string       `json:"series"`
	Label    string       `json:"label"`
	Settings wireSettings `json:"settings"`
	// Outputs are the names perf-agent writes in staging; Finals the
	// names each is published as.
	Outputs []string `json:"outputs"`
	Finals  []string `json:"finals"`
	// Staging is the staging directory, and its device and inode once it
	// exists.
	Staging    string `json:"staging"`
	StagingDev uint64 `json:"staging_dev,string,omitzero"`
	StagingIno uint64 `json:"staging_ino,string,omitzero"`
	// Token (128 random bits, hex) names the intent's temporaries.
	Token       string `json:"token"`
	ServerPID   int    `json:"server_pid,string"`
	ServerStart uint64 `json:"server_start,string"`
	ReaperPID   int    `json:"reaper_pid,string,omitzero"`
	ReaperStart uint64 `json:"reaper_start,string,omitzero"`
	// Heartbeat is the heartbeat file, Files the capture files once
	// validated, and Temps the manifest temporaries written.
	Heartbeat *fileIdentity  `json:"heartbeat,omitzero"`
	Files     []fileIdentity `json:"files,omitzero"`
	Temps     []fileIdentity `json:"temps,omitzero"`
	WrittenBy string         `json:"written_by"`
}

// captureTxn is one capture's file transaction, from its intent to its
// manifest. Its methods run on one goroutine: the job's, or recovery's
// for an intent it adopted.
type captureTxn struct {
	s      *Server
	d      *captureDir
	ser    *os.Root
	series string
	id     int
	in     intent
	// self is the intent file as this transaction last wrote or read it.
	self fileIdentity
	// owner says this process runs the transaction and its heartbeat;
	// borrowed that ser belongs to the caller (recovery).
	owner, borrowed bool
	// validated are the files validate checked, before record.
	validated []fileIdentity
	// pending is a failure manifest a stranded rollback still has to
	// commit.
	pending *manifest
	// cancelNote is what to report about a cancel that met the commit.
	cancelNote string
	// notes say what the transaction left where it was, and why.
	notes []string

	hbStop, hbDone chan struct{}
	stopOnce       sync.Once
	dead, ended    bool
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// beginTxn writes the intent of capture id in series (step 5), without
// replacing anything, then its heartbeat, which a rewrite records, and
// starts the heartbeat. outputs are the files perf-agent will write in
// staging. A nil transaction means nothing was left to roll back: the
// caller writes the failure manifest itself. Once the intent exists the
// transaction is returned even with an error, which the caller rolls back.
// A crash between the intent and its heartbeat leaves an intent that
// recovery judges by its owner's liveness alone.
func (s *Server) beginTxn(d *captureDir, series string, id int, label string, settings wireSettings, outputs []string) (*captureTxn, error) {
	if !validLabel(label) {
		return nil, capture.Errorf(capture.CodeInvalidArgument, "pass a label of [A-Za-z0-9._-]",
			"label %q is not 1-%d bytes of [A-Za-z0-9._-] not starting with '.'", echo(label), maxLabel)
	}
	start, err := s.startTime()
	if err != nil || start == 0 {
		return nil, capture.Errorf(capture.CodeDisabled, "",
			"cannot read this server's start time, which a capture's intent records so that recovery can tell its owner is alive: %v", err)
	}
	ser, err := d.series(series)
	if err != nil {
		return nil, err
	}
	t := &captureTxn{s: s, d: d, ser: ser, series: series, id: id, owner: true}
	t.in = intent{
		Format: 1, State: statePrepared, ID: formatID(id), Series: series, Label: label, Settings: settings,
		Outputs: slices.Clone(outputs), Staging: stagingPrefix + randHex(8), Token: randHex(16),
		ServerPID: os.Getpid(), ServerStart: start, WrittenBy: writtenBy,
	}
	for _, o := range outputs {
		t.in.Finals = append(t.in.Finals, finalName(id, label, o))
	}
	name := intentName(id, label)
	b, _ := json.Marshal(t.in)
	tmp, err := t.writeTemp(name, b, "intent")
	if err == nil {
		err = s.linkNoReplace(ser, rootCaptures, tmp.Name, name)
		if err != nil && !errors.Is(err, errCrash) && landed(ser, name, tmp) {
			err = nil
		}
		if err != nil && !errors.Is(err, errCrash) {
			_, _, _ = removeByIdentity(ser, tmp.Name, tmp)
		}
	}
	if err != nil {
		return nil, t.abort(err)
	}
	t.self = tmp
	t.self.Name = name
	// The temporary is a second name of the intent now. If it cannot go,
	// it is recorded with the intent's next rewrite and removed with the
	// rest; a crash right here leaves it for recovery, which proves it by
	// the intent's own identity.
	if err := s.wErr("remove intent temp"); errors.Is(err, errCrash) {
		return t, t.crashed(err)
	} else if err != nil {
		t.in.Temps = append(t.in.Temps, tmp)
	} else if _, left, err := t.remove(ser, tmp.Name, tmp); errors.Is(err, errCrash) {
		return t, t.crashed(err)
	} else if err != nil || left != "" {
		t.in.Temps = append(t.in.Temps, tmp)
	}
	if err := s.syncDir(ser, ".", "fsync intent"); err != nil {
		return t, t.crashed(err)
	}
	hb, err := s.writeNewFile(ser, heartbeatName(id, label), []byte(t.in.Token+"\n"), "heartbeat")
	if err != nil {
		return t, t.crashed(err)
	}
	t.in.Heartbeat = &hb
	t.startHeartbeat()
	return t, t.rewrite()
}

// startTime is this process's start time, through the selfStartTime seam.
func (s *Server) startTime() (uint64, error) {
	if s.selfStartTime != nil {
		return s.selfStartTime()
	}
	return selfStart()
}

// landed reports whether name now is the file tmp records: a link that
// answered an error may still have been made (a retransmitted NFS request
// answers EEXIST for the link it made itself).
func landed(root *os.Root, name string, tmp fileIdentity) bool {
	fi, err := root.Lstat(name)
	if err != nil {
		return false
	}
	st, _ := statOf(fi)
	return fi.Mode().IsRegular() && st.dev == tmp.Dev && st.ino == tmp.Ino
}

// abort ends a transaction whose intent was never written.
func (t *captureTxn) abort(err error) error {
	if errors.Is(err, errCrash) {
		t.die()
		return err
	}
	t.end()
	return err
}

// crashed is err, after ending the transaction where it stands when err
// is a simulated crash.
func (t *captureTxn) crashed(err error) error {
	if errors.Is(err, errCrash) {
		t.die()
	}
	return err
}

// fault runs the fault seam at p for this transaction.
func (t *captureTxn) fault(p faultPoint) error { return t.crashed(t.s.fault(p)) }

// cause is err as the capture failure it reports.
func (t *captureTxn) cause(err error) *capture.Error { return t.d.cause(err) }

// die stops the transaction as a crash would: the heartbeat stops and
// nothing is cleaned up.
func (t *captureTxn) die() {
	if t.dead {
		return
	}
	t.dead = true
	t.end()
}

// end stops the heartbeat and lets the series go.
func (t *captureTxn) end() {
	t.stopHeartbeat()
	if !t.ended && !t.borrowed {
		_ = t.ser.Close()
	}
	t.ended = true
}

func (t *captureTxn) startHeartbeat() {
	name := t.in.Heartbeat.Name
	now := t.s.clock()
	_ = t.ser.Chtimes(name, now, now)
	stop, done := make(chan struct{}), make(chan struct{})
	t.hbStop, t.hbDone = stop, done
	ser, s := t.ser, t.s
	go func() {
		defer close(done)
		tick := time.NewTicker(heartbeatEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				now := s.clock()
				if err := ser.Chtimes(name, now, now); err != nil {
					s.log.Debug("capture heartbeat", "file", name, "err", err)
				}
			}
		}
	}()
}

// stopHeartbeat stops the heartbeat, once; it may run on any goroutine.
func (t *captureTxn) stopHeartbeat() {
	t.stopOnce.Do(func() {
		if t.hbStop != nil {
			close(t.hbStop)
			<-t.hbDone
		}
	})
}

// writeNewFile creates name inside root (mode 0600, never replacing
// anything), writes b and fsyncs it, and returns its identity, digest
// included. op names the write for the seam. A failure removes the file
// again, by the identity of what it holds then; a name that exists already
// is an error wrapping fs.ErrExist.
func (s *Server) writeNewFile(root *os.Root, name string, b []byte, op string) (fileIdentity, error) {
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fileIdentity{}, fmt.Errorf("cannot create %s in %s: %w", name, root.Name(), pathless(err))
	}
	err = s.wErr(op)
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	var id fileIdentity
	if fi, serr := f.Stat(); serr == nil {
		id = identityOf(name, fi)
		id.SHA256, serr = hashFile(f)
		if err == nil {
			err = serr
		}
	} else if err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if !errors.Is(err, errCrash) && id.SHA256 != "" {
			_, _, _ = removeByIdentity(root, name, id)
		}
		return fileIdentity{}, fmt.Errorf("cannot write %s in %s: %w", name, root.Name(), pathless(err))
	}
	return id, nil
}

// writeTemp writes b to a temporary for final, ".<final>.tmp-<token[:16]>",
// created exclusively; when that name is taken (by a temporary a crash left
// and nothing can prove), a numbered one after it.
func (t *captureTxn) writeTemp(final string, b []byte, op string) (fileIdentity, error) {
	base := "." + final + ".tmp-" + t.in.Token[:16]
	for i := range maxTempTries {
		name := base
		if i > 0 {
			name += "-" + strconv.Itoa(i)
		}
		id, err := t.s.writeNewFile(t.ser, name, b, op)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return id, err
	}
	return fileIdentity{}, fmt.Errorf("cannot create a temporary for %s in %s: every name tried exists", final, t.ser.Name())
}

// rewrite replaces the intent with t.in: through a temporary, renamed over
// the intent only while the intent is still the file this transaction
// wrote, then fsynced.
func (t *captureTxn) rewrite() error {
	b, _ := json.Marshal(t.in)
	name := intentName(t.id, t.in.Label)
	tmp, err := t.writeTemp(name, b, "intent rewrite")
	if err != nil {
		return t.crashed(err)
	}
	if why, err := matches(t.ser, name, t.self); err != nil || why != "" {
		_, _, _ = removeByIdentity(t.ser, tmp.Name, tmp)
		return fmt.Errorf("the intent %s in %s is no longer the one this capture wrote (%s); it is left as it is", name, t.ser.Name(), cmpOrErr(why, err))
	}
	if err := t.ser.Rename(tmp.Name, name); err != nil {
		_, _, _ = removeByIdentity(t.ser, tmp.Name, tmp)
		return err
	}
	t.self = tmp
	t.self.Name = name
	return t.crashed(t.s.syncDir(t.ser, ".", "fsync intent"))
}

// makeStaging creates the staging directory (step 7), records its device
// and inode in the intent, and probes it for hard links: a filesystem that
// refuses them could never publish the capture, so the capture fails here,
// before perf-agent starts.
func (t *captureTxn) makeStaging() error {
	if t.dead {
		return errCrash
	}
	if err := t.s.wErr("staging"); err != nil {
		return t.crashed(err)
	}
	if err := t.ser.Mkdir(t.in.Staging, 0o700); err != nil {
		return err
	}
	fi, err := t.ser.Lstat(t.in.Staging)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("staging %s in %s is %s after it was created", t.in.Staging, t.ser.Name(), describeType(fi.Mode()))
	}
	st, _ := statOf(fi)
	t.in.StagingDev, t.in.StagingIno = st.dev, st.ino
	if err := t.rewrite(); err != nil {
		return err
	}
	return t.probe()
}

// probe makes one no-replace hard link inside staging and removes both
// names.
func (t *captureTxn) probe() error {
	p := filepath.Join(t.in.Staging, probeName)
	q := filepath.Join(t.in.Staging, probeLinkName)
	id, err := t.s.writeNewFile(t.ser, p, []byte(t.in.Token+"\n"), "probe")
	if err != nil {
		return t.crashed(err)
	}
	lerr := t.s.linkNoReplace(t.ser, rootCaptures, p, q)
	for _, n := range []string{q, p} {
		if _, _, err := t.remove(t.ser, n, id); errors.Is(err, errCrash) {
			return t.crashed(err)
		}
	}
	if lerr == nil {
		return nil
	}
	where := fmt.Sprintf("the capture directory %s is inside the profiles root %s", t.d.rel, t.d.profiles)
	errno, _ := errors.AsType[syscall.Errno](lerr)
	switch {
	case errno == syscall.EPERM || errno == syscall.ENOTSUP || errno == syscall.EOPNOTSUPP || errno == syscall.EXDEV:
		return capture.Errorf(capture.CodeDisabled,
			"start perf-agent-mcp with --captures naming a directory inside the profiles root on a filesystem that supports hard links",
			"%v (%s)", lerr, where)
	case errno == syscall.ENOSPC || errno == syscall.EDQUOT:
		return capture.Errorf(capture.CodeDisk,
			"free space there, or start perf-agent-mcp with --captures naming another directory inside the profiles root",
			"there is no room to make a hard link in the capture directory: %s (%s)", errnoText(lerr), where)
	case errno == syscall.EMLINK:
		return capture.Errorf(capture.CodeDisk,
			"start perf-agent-mcp with --captures naming a directory inside the profiles root on another filesystem",
			"the filesystem refused a second link to a new file, too many links: %s (%s)", errnoText(lerr), where)
	}
	return t.d.writeError(lerr)
}

// setReaper records the reaper's PID and start time once it is known, so
// recovery sees the capture's owner alive while either process is.
func (t *captureTxn) setReaper(pid int, start uint64) error {
	if t.dead {
		return errCrash
	}
	t.in.ReaperPID, t.in.ReaperStart = pid, start
	return t.rewrite()
}

// finish commits the capture's outputs (steps 8-11): validate, record,
// link and commit, rolling back to a failure manifest when any of them
// fails before the commit point. m is what the job knows of the capture;
// commit fills in the rest.
func (t *captureTxn) finish(m manifest, d *decisionWord) (committed bool, err error) {
	for _, step := range []func() error{t.validate, t.record, t.link} {
		if err := step(); err != nil {
			return false, t.fail(err, m)
		}
	}
	return t.commit(m, d)
}

// fail rolls the capture back after err and returns the failure to
// report: err itself as a capture error, or errCrash.
func (t *captureTxn) fail(err error, m manifest) error {
	if errors.Is(err, errCrash) {
		t.die()
		return errCrash
	}
	cause := t.cause(err)
	if rerr := t.rollback(cause, m); errors.Is(rerr, errCrash) {
		return errCrash
	}
	return cause
}

// validate checks each output in staging (step 8): a regular file with
// one link, the server's own, not empty and within its size limit; then
// makes it 0600, fsyncs it and computes its SHA-256.
func (t *captureTxn) validate() error {
	if t.dead {
		return errCrash
	}
	t.validated = nil
	for i, out := range t.in.Outputs {
		id, err := t.validateOne(out, t.in.Finals[i])
		if err != nil {
			return err
		}
		t.validated = append(t.validated, id)
		if err := t.fault(faultAfterValidate); err != nil {
			return err
		}
	}
	return nil
}

func (t *captureTxn) validateOne(out, final string) (fileIdentity, error) {
	name := filepath.Join(t.in.Staging, out)
	notOne := capture.Errorf(capture.CodePerfAgentFailed, "", "output %s was not a regular file with one link", out)
	f, fi, err := openNoFollow(t.ser, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fileIdentity{}, capture.Errorf(capture.CodeNoOutput, "", "perf-agent ended without writing its %s output", out)
	case errors.Is(err, errNotRegular):
		return fileIdentity{}, notOne
	case err != nil:
		return fileIdentity{}, err
	}
	defer func() { _ = f.Close() }()
	st, _ := statOf(fi)
	limit := t.s.lim.Capture.MaxProfileBytes
	if out == capture.ExeclogFile {
		limit = t.s.lim.Capture.MaxExeclogBytes
	}
	switch {
	case st.nlink != 1:
		return fileIdentity{}, notOne
	case st.uid != uint64(captureEUID()):
		return fileIdentity{}, capture.Errorf(capture.CodePerfAgentFailed, "", "output %s is not owned by this server's user", out)
	case fi.Size() == 0:
		return fileIdentity{}, capture.Errorf(capture.CodeNoOutput, "", "perf-agent wrote an empty %s", out)
	case fi.Size() > limit:
		return fileIdentity{}, capture.Errorf(capture.CodeTooLarge, "capture a shorter duration or at a lower sample rate",
			"output %s is %d bytes, over its limit of %d", out, fi.Size(), limit)
	}
	if err := f.Chmod(0o600); err != nil {
		return fileIdentity{}, err
	}
	if err := t.s.fault(faultFsync); err != nil {
		return fileIdentity{}, t.crashed(err)
	}
	if err := t.s.wErr("fsync file"); err != nil {
		return fileIdentity{}, t.crashed(err)
	}
	if err := f.Sync(); err != nil {
		return fileIdentity{}, err
	}
	if err := t.fault(faultHash); err != nil {
		return fileIdentity{}, err
	}
	sum, err := hashFile(f)
	if err != nil {
		return fileIdentity{}, err
	}
	if after, err := f.Stat(); err != nil || after.Size() != fi.Size() {
		return fileIdentity{}, capture.Errorf(capture.CodePerfAgentFailed, "", "output %s changed while it was validated", out)
	}
	id := identityOf(final, fi)
	id.Staged, id.SHA256 = out, sum
	return id, nil
}

// record writes the validated files' identities into the intent before
// any is linked (step 9).
func (t *captureTxn) record() error {
	if t.dead {
		return errCrash
	}
	t.in.State = stateLinking
	t.in.Files = slices.Clone(t.validated)
	if err := t.rewrite(); err != nil {
		return err
	}
	return t.fault(faultAfterRecord)
}

// link publishes each file under its final name with a no-replace hard
// link (step 10). A name taken meanwhile is not this capture's: it is
// exists, and is never removed.
func (t *captureTxn) link() error {
	if t.dead {
		return errCrash
	}
	for i, f := range t.in.Files {
		if i > 0 {
			if err := t.fault(faultBetweenLinks); err != nil {
				return err
			}
		}
		if err := t.s.linkNoReplace(t.ser, rootCaptures, filepath.Join(t.in.Staging, f.Staged), f.Name); err != nil && !landed(t.ser, f.Name, f) {
			if errors.Is(err, fs.ErrExist) {
				return capture.Errorf(capture.CodeExists, "pass another label", "%v", err)
			}
			return err
		}
	}
	if err := t.s.syncDir(t.ser, ".", "fsync links"); err != nil {
		return t.crashed(err)
	}
	return t.fault(faultAfterLinks)
}

// manifestOf is m completed as this capture's manifest with outcome. A
// failed or interrupted manifest lists no files.
func (t *captureTxn) manifestOf(m manifest, outcome string, e *wireError) manifest {
	m.Format, m.ID, m.Series, m.Label = manifestFormat, formatID(t.id), t.series, t.in.Label
	m.Outcome, m.Error = outcome, e
	m.Settings, m.SettingsDigest = t.in.Settings, settingsDigest(t.in.Settings)
	m.WrittenBy = writtenBy
	m.Files = nil
	if outcome == captureOK {
		for _, f := range t.in.Files {
			f.Staged = ""
			m.Files = append(m.Files, f)
		}
	}
	return m
}

// linkCause is a failed manifest link as the failure it reports: a name
// taken by another file is exists (the manifest is never written over),
// anything else a write failure.
func (t *captureTxn) linkCause(err error) *capture.Error {
	if errors.Is(err, fs.ErrExist) {
		return capture.Errorf(capture.CodeExists, "pass another label", "%v", err)
	}
	return t.cause(err)
}

func cancelledError() *capture.Error {
	return capture.Errorf(capture.CodeCancelled, "", "the capture was cancelled before it was committed")
}

// commit writes the manifest (step 11): its temporary, recorded in the
// intent, then the terminal decision rule, then the no-replace link that
// is the commit point, then the cleanup. A cancel that came first rolls
// the capture back to a cancelled failure manifest; so does a link that
// fails while a cancel is pending. Once the link has succeeded, committed
// is true whatever follows: an error then is a cleanup the next recovery
// finishes.
func (t *captureTxn) commit(m manifest, d *decisionWord) (committed bool, err error) {
	if t.dead {
		return false, errCrash
	}
	name := manifestName(t.id, t.in.Label)
	b, _ := json.Marshal(t.manifestOf(m, captureOK, nil))
	if err := t.removeTemps(); err != nil {
		return false, t.fail(err, m)
	}
	tmp, err := t.writeTemp(name, append(b, '\n'), "manifest temp")
	if err != nil {
		return false, t.fail(err, m)
	}
	t.in.Temps = append(t.in.Temps, tmp)
	if err := t.rewrite(); err != nil {
		return false, t.fail(err, m)
	}
	if err := t.fault(faultAfterManifestTemp); err != nil {
		return false, t.fail(err, m)
	}
	if !d.beginManifest() {
		return false, t.fail(cancelledError(), m)
	}
	err = t.fault(faultInManifestLink)
	if err == nil {
		err = t.s.wErr("manifest link")
	}
	if err == nil {
		err = t.s.linkNoReplace(t.ser, rootCaptures, tmp.Name, name)
		if err != nil && landed(t.ser, name, tmp) {
			t.s.log.Warn("the manifest link answered an error but was made; the capture is committed", "manifest", name, "err", err)
			err = nil
		}
	}
	if err != nil {
		if errors.Is(err, errCrash) {
			return false, t.crashed(err)
		}
		var cause error = t.linkCause(err)
		if d.settle(false) == cancelAnswerCancelled {
			cause = cancelledError()
		}
		return false, t.fail(cause, m)
	}
	t.cancelNote = d.settle(true)
	return true, t.afterCommit()
}

// afterCommit removes what the committed capture no longer needs:
// staging, which leaves each capture file with one link, the temporary,
// the intent and the heartbeat. It stops at the first failure and leaves
// the rest to the next recovery, which finds the manifest.
func (t *captureTxn) afterCommit() error {
	steps := []func() error{
		func() error { return t.s.syncDir(t.ser, ".", "fsync manifest") },
		func() error { return t.s.fault(faultAfterManifestLink) },
		t.removeStaging,
		func() error { return t.s.fault(faultAfterStagingRemoval) },
		t.cleanup,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return t.leaveToRecovery(err)
		}
	}
	return nil
}

// leaveToRecovery ends a transaction whose manifest is linked but whose
// cleanup failed with err.
func (t *captureTxn) leaveToRecovery(err error) error {
	if errors.Is(err, errCrash) {
		t.die()
		return err
	}
	t.s.log.Warn("capture committed; the next recovery removes what it left", "series", t.series, "id", formatID(t.id), "err", err)
	t.end()
	return fmt.Errorf("capture %s/%s is committed, but its cleanup stopped: %w", t.series, formatID(t.id), err)
}

// cleanup removes the manifest temporaries, the intent and the heartbeat,
// each by its identity, then fsyncs the series and ends the transaction.
func (t *captureTxn) cleanup() error {
	if err := t.removeTemps(); err != nil {
		return err
	}
	t.stopHeartbeat()
	if hb := t.in.Heartbeat; hb != nil {
		if err := t.removeRecorded(*hb, "the heartbeat", "remove heartbeat"); err != nil {
			return err
		}
	}
	if err := t.removeRecorded(t.self, "the intent", "remove intent"); err != nil {
		return err
	}
	if err := t.s.syncDir(t.ser, ".", "fsync cleanup"); err != nil {
		return err
	}
	t.end()
	return nil
}

// tag is the quarantine tag of this transaction's removals.
func (t *captureTxn) tag() string {
	if len(t.in.Token) < 16 {
		return ""
	}
	return t.in.Token[:16] + "-"
}

// remove is removeByIdentity inside root, tagged with this capture.
func (t *captureTxn) remove(root *os.Root, name string, want fileIdentity) (bool, string, error) {
	return removeTagged(root, name, want, t.tag())
}

// removeRecorded removes the file id records, noting it when it is not
// that file any more; op names the removal for the write seam.
func (t *captureTxn) removeRecorded(id fileIdentity, what, op string) error {
	if err := t.s.wErr(op); err != nil {
		return err
	}
	_, left, err := t.remove(t.ser, id.Name, id)
	if left != "" {
		t.note("left", id.Name, what+" is now "+left)
	}
	return err
}

// note records that the transaction left name, and why, and logs it.
func (t *captureTxn) note(kind, name, why string) {
	n := fmt.Sprintf("%s %s/%s: %s", kind, t.series, name, why)
	t.notes = append(t.notes, n)
	t.s.log.Warn("capture file left in place", "note", n)
}

// removeTemps removes the recorded manifest temporaries by identity.
func (t *captureTxn) removeTemps() error {
	for _, tmp := range t.in.Temps {
		_, left, err := t.remove(t.ser, tmp.Name, tmp)
		if err != nil {
			return err
		}
		if left != "" {
			t.note("left", tmp.Name, "a temporary that is now "+left)
		}
	}
	t.in.Temps = nil
	return nil
}

// removeFinals removes each recorded capture file's final name, only
// where it still is that file with its size and digest.
func (t *captureTxn) removeFinals() error {
	for _, f := range t.in.Files {
		_, left, err := t.remove(t.ser, f.Name, f)
		if err != nil {
			return err
		}
		if left != "" {
			t.note("left", f.Name, "not this capture's file: "+left)
		}
	}
	return nil
}

// removeStaging removes the staging directory, only when it is the one
// recorded, and in it only what this capture put there: the recorded
// files by identity, and perf-agent's outputs and the link probe by their
// names. The directory is opened as a root and proven to be the recorded
// one (device and inode) before anything in it is removed by name, so no
// path is walked after the check; its identity proves it is the capture's
// private 0700 workspace. Anything else stays, and so does the directory
// then. The empty directory is moved aside, checked again and removed.
func (t *captureTxn) removeStaging() error { return t.removeStagingDir(t.in.Staging) }

// removeStagingDir is removeStaging for the staging directory under the
// name dir: its own, or a quarantine name a crash left it under.
func (t *captureTxn) removeStagingDir(dir string) error {
	if t.in.StagingDev == 0 && t.in.StagingIno == 0 {
		return nil
	}
	isStaging := func(fi fs.FileInfo) bool {
		st, _ := statOf(fi)
		return fi.IsDir() && st.dev == t.in.StagingDev && st.ino == t.in.StagingIno
	}
	lfi, err := t.ser.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !isStaging(lfi):
		t.note("left", dir, "not the staging directory this capture created")
		return nil
	}
	if openStagingHook != nil {
		openStagingHook(t.ser, dir)
	}
	sr, err := t.ser.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = sr.Close() }()
	if fi, err := sr.Stat("."); err != nil || !isStaging(fi) {
		t.note("left", dir, "not the staging directory this capture created")
		return err
	}
	kept, err := t.emptyStaging(sr)
	if err != nil || kept {
		return err
	}
	q := quarantinePrefix + t.tag() + randHex(8)
	if err := t.ser.Rename(dir, q); err != nil {
		return err
	}
	if quarantineAfterHook != nil {
		if err := quarantineAfterHook(); err != nil {
			return err
		}
	}
	// A failure to remove the emptied directory is returned once it is back
	// under its name, so the intent that records it stays for the next
	// recovery; one that is not empty any more, or not the staging
	// directory, is left and noted.
	var rmErr error
	if fi, err := t.ser.Lstat(q); err == nil && isStaging(fi) {
		rmErr = t.s.wErr("remove staging")
		if rmErr == nil {
			rmErr = t.ser.Remove(q)
		}
		if rmErr == nil {
			return nil
		}
		if errors.Is(rmErr, errCrash) {
			return rmErr
		}
		if errors.Is(rmErr, syscall.ENOTEMPTY) || errors.Is(rmErr, syscall.EEXIST) {
			t.note("left", dir, "the staging directory is not empty any more")
			rmErr = nil
		}
	} else {
		t.note("left", dir, "not the staging directory this capture created")
	}
	if why := restore(t.ser, q, dir); why != "" {
		t.note("left", q, "moved aside from "+dir+why)
	}
	return rmErr
}

// openStagingHook, when set, runs between the staging directory's Lstat
// and its opening; a seam for tests.
var openStagingHook func(ser *os.Root, dir string)

// emptyStaging removes from the opened staging root what this capture
// put there, and reports whether anything had to stay.
func (t *captureTxn) emptyStaging(sr *os.Root) (kept bool, err error) {
	names, err := readDirNames(sr, ".")
	if err != nil {
		return false, err
	}
	recorded := map[string]fileIdentity{}
	for _, f := range t.in.Files {
		recorded[f.Staged] = f
	}
	keep := func(n, why string) {
		kept = true
		t.note("left", filepath.Join(t.in.Staging, n), why)
	}
	for _, n := range names {
		if f, ok := recorded[n]; ok {
			_, left, err := t.remove(sr, n, f)
			if err != nil {
				return kept, err
			}
			if left != "" {
				keep(n, "not the file this capture recorded: "+left)
			}
			continue
		}
		if t.tag() != "" && strings.HasPrefix(n, quarantinePrefix+t.tag()) {
			if id, ok := t.proveQuarantined(sr, n); ok {
				if _, left, err := t.remove(sr, n, id); err != nil {
					return kept, err
				} else if left != "" {
					keep(n, left)
				}
				continue
			}
		}
		if !slices.Contains(t.in.Outputs, n) && n != probeName && n != probeLinkName {
			keep(n, "not a file this capture writes")
			continue
		}
		fi, err := sr.Lstat(n)
		if err != nil {
			return kept, err
		}
		if !fi.Mode().IsRegular() && fi.Mode()&fs.ModeSymlink == 0 {
			keep(n, describeType(fi.Mode())+", not a file perf-agent writes")
			continue
		}
		if err := sr.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return kept, err
		}
	}
	return kept, nil
}

// rollback undoes a failed capture live (§7.4): the intent says
// rolling_back, the linked files and staging are removed by identity, a
// failure manifest with no files is committed, and the intent and
// heartbeat go. It returns nil once the failure manifest is committed. If
// that cannot be done (ENOSPC), the intent stays rolling_back, the
// heartbeat keeps running, and retryStranded tries again before the next
// capture.
func (t *captureTxn) rollback(cause *capture.Error, m manifest) error {
	if t.dead {
		return errCrash
	}
	fm := t.manifestOf(m, captureFailed, toWireError(cause))
	if cause.Code == capture.CodeCancelled {
		fm.Cancel = cancelAnswerCancelled
	}
	return t.unwind(fm, faultInRollback)
}

// unwind commits fm, a manifest with no files, after removing the
// capture's files: the shared path of live rollback and of recovery,
// whose fault point is p. The manifest's temporary is recorded in the
// intent with the rolling_back state, before anything is removed.
func (t *captureTxn) unwind(fm manifest, p faultPoint) error {
	t.pending = &fm
	name := manifestName(t.id, t.in.Label)
	if err := t.removeTemps(); err != nil {
		return t.strand(err)
	}
	b, _ := json.Marshal(fm)
	tmp, terr := t.writeTemp(name, append(b, '\n'), "manifest temp")
	if errors.Is(terr, errCrash) {
		return t.crashed(terr)
	}
	if terr == nil {
		t.in.Temps = append(t.in.Temps, tmp)
	}
	t.in.State = stateRollingBack
	if err := t.rewrite(); err != nil {
		if errors.Is(err, errCrash) {
			return err
		}
		t.s.log.Warn("cannot record the rollback in the intent; going on", "series", t.series, "id", formatID(t.id), "err", err)
	}
	if err := t.s.fault(p); err != nil {
		return t.strand(err)
	}
	if err := t.removeFinals(); err != nil {
		return t.strand(err)
	}
	if err := t.removeStaging(); err != nil {
		return t.strand(err)
	}
	if terr != nil {
		return t.strand(terr)
	}
	err := t.s.wErr("manifest link")
	if err == nil {
		err = t.s.linkNoReplace(t.ser, rootCaptures, tmp.Name, name)
		if err != nil && landed(t.ser, name, tmp) {
			err = nil
		}
	}
	if err != nil {
		return t.strand(t.linkCause(err))
	}
	t.pending = nil
	t.unstrand()
	if err := t.s.syncDir(t.ser, ".", "fsync manifest"); err != nil {
		return t.afterFailure(err)
	}
	return t.afterFailure(t.cleanup())
}

// afterFailure ends a transaction whose failure manifest is linked; a
// cleanup error is logged and left to the next recovery.
func (t *captureTxn) afterFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errCrash) {
		t.die()
		return err
	}
	t.s.log.Warn("capture failure recorded; the next recovery removes what it left", "series", t.series, "id", formatID(t.id), "err", err)
	t.end()
	return nil
}

// strand records that the rollback stopped on err. A live owner keeps the
// intent and its heartbeat and retries later; recovery stops and leaves
// the intent for the next pass.
func (t *captureTxn) strand(err error) error {
	if errors.Is(err, errCrash) {
		t.die()
		return err
	}
	if t.owner {
		t.s.capMu.Lock()
		if !slices.Contains(t.s.stranded, t) {
			t.s.stranded = append(t.s.stranded, t)
		}
		t.s.capMu.Unlock()
		t.s.log.Warn("capture rollback stranded; it is retried before the next capture", "series", t.series, "id", formatID(t.id), "err", err)
	}
	return fmt.Errorf("the rollback of capture %s/%s stopped: %w", t.series, formatID(t.id), err)
}

func (t *captureTxn) unstrand() {
	t.s.capMu.Lock()
	t.s.stranded = slices.DeleteFunc(t.s.stranded, func(u *captureTxn) bool { return u == t })
	t.s.capMu.Unlock()
}

// retryStranded retries this server's stranded rollbacks. It runs under
// the captures lock, before each capture and from maintenance.
func (s *Server) retryStranded() error {
	s.capMu.Lock()
	list := slices.Clone(s.stranded)
	s.capMu.Unlock()
	var errs []error
	for _, t := range list {
		if t.pending == nil || t.dead {
			t.unstrand()
			continue
		}
		if err := t.unwind(*t.pending, faultInRollback); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopStranded stops the heartbeats of the stranded rollbacks, at Close:
// their intents stay for the next server's recovery.
func (s *Server) stopStranded() {
	s.capMu.Lock()
	list := slices.Clone(s.stranded)
	s.capMu.Unlock()
	for _, t := range list {
		t.stopHeartbeat()
	}
}

// writeFailure commits the failure manifest of capture id when it has no
// intent: a failure after the id was handed out and before the intent was
// written. m is what the caller knows of the capture.
func (s *Server) writeFailure(d *captureDir, series string, id int, label string, settings wireSettings, cause *capture.Error, m manifest) error {
	ser, err := d.series(series)
	if err != nil {
		return err
	}
	defer func() { _ = ser.Close() }()
	t := &captureTxn{s: s, d: d, ser: ser, series: series, id: id, in: intent{Label: label, Settings: settings}, borrowed: true}
	fm := t.manifestOf(m, captureFailed, toWireError(cause))
	if cause.Code == capture.CodeCancelled {
		fm.Cancel = cancelAnswerCancelled
	}
	b, _ := json.Marshal(fm)
	err = s.wErr("manifest temp")
	if err == nil {
		err = s.wErr("manifest link")
	}
	if err == nil {
		_, err = s.publishNew(ser, rootCaptures, ".", manifestName(id, label), append(b, '\n'), 0o600)
	}
	if err == nil {
		err = s.syncDir(ser, ".", "fsync manifest")
	}
	return err
}

// readIntent reads and checks the intent name inside a series: this
// server's format, and only names the protocol uses, so recovery acting
// on it touches nothing else. It returns the intent and the file's
// identity.
func readIntent(ser *os.Root, name string) (intent, fileIdentity, error) {
	var in intent
	b, fi, err := readSmall(ser, name, maxManifestBytes)
	if err == nil {
		err = json.Unmarshal(b, &in)
	}
	if err != nil {
		return intent{}, fileIdentity{}, fmt.Errorf("intent %s: %w", name, err)
	}
	if err := in.check(name); err != nil {
		return intent{}, fileIdentity{}, fmt.Errorf("intent %s: %w", name, err)
	}
	id := identityOf(name, fi)
	sum := sha256.Sum256(b)
	id.SHA256 = hex.EncodeToString(sum[:])
	return in, id, nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// check refuses an intent that is not this server's or names anything the
// protocol never creates.
func (in intent) check(name string) error {
	id, ok := parseID(in.ID)
	switch {
	case in.Format != 1 || in.WrittenBy != writtenBy:
		return errors.New("not written by " + writtenBy)
	case !ok || !validLabel(in.Label) || intentName(id, in.Label) != name || !validSeries(in.Series):
		return fmt.Errorf("names capture %s-%s", echo(in.ID), echo(in.Label))
	case !isHex(in.Token, 32):
		return errors.New("has no owner token")
	case !strings.HasPrefix(in.Staging, stagingPrefix) || !isHex(strings.TrimPrefix(in.Staging, stagingPrefix), 16):
		return errors.New("names no staging directory of its own")
	case len(in.Outputs) != len(in.Finals):
		return errors.New("lists outputs and final names that differ in number")
	case in.Heartbeat != nil && in.Heartbeat.Name != heartbeatName(id, in.Label):
		return errors.New("names another heartbeat")
	}
	for i, o := range in.Outputs {
		if !validName(o, maxFileName) || in.Finals[i] != finalName(id, in.Label, o) {
			return fmt.Errorf("lists output %s", echo(o))
		}
	}
	for _, f := range in.Files {
		i := slices.Index(in.Finals, f.Name)
		if i < 0 || in.Outputs[i] != f.Staged {
			return fmt.Errorf("records file %s", echo(f.Name))
		}
	}
	for _, f := range in.Files {
		if !validName(f.Staged, maxFileName) {
			return fmt.Errorf("records staged file %s", echo(f.Staged))
		}
	}
	for _, tmp := range in.Temps {
		if !in.isTemp(id, tmp.Name) {
			return fmt.Errorf("records temporary %s", echo(tmp.Name))
		}
	}
	return nil
}

// isTemp reports whether name is one of the intent's own temporaries:
// ".<intent or manifest>.tmp-<token[:16]>", or that with "-<n>" after it
// (see writeTemp).
func (in intent) isTemp(id int, name string) bool {
	for _, final := range []string{intentName(id, in.Label), manifestName(id, in.Label)} {
		rest, ok := strings.CutPrefix(name, "."+final+".tmp-"+in.Token[:16])
		if !ok {
			continue
		}
		if rest == "" {
			return true
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(rest, "-")); err == nil && strings.HasPrefix(rest, "-") && n >= 1 && n < maxTempTries && "-"+strconv.Itoa(n) == rest {
			return true
		}
	}
	return false
}
