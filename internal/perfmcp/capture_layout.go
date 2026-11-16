package perfmcp

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// rootCaptures is how messages name the capture directory and its series.
const rootCaptures = "captures"

// The two kinds of server that write captures, and the directory each
// uses by default inside the profiles root. They never share one.
const (
	ownerStdio = "stdio"
	ownerServe = "serve"

	defaultStdioCaptures = "captures"
	defaultServeCaptures = "captures-serve"
)

// Names inside the capture directory and its series.
const (
	ownerFile = ".owner"
	seqFile   = ".seq"
	// maxSmallFile bounds .owner and .seq when they are read back.
	maxSmallFile = 4 << 10
	// maxSeriesName and maxLabel bound a series' and a label's bytes.
	maxSeriesName = 64
	maxLabel      = 32
)

// captureDir is the open capture directory: root is its sub-root of the
// profiles root, rel its name inside the profiles root, owner the kind of
// server that owns it, and profiles the profiles root's own directory,
// which messages name.
type captureDir struct {
	root     *os.Root
	rel      string
	owner    string
	profiles string
}

func (d *captureDir) close() { _ = d.root.Close() }

// ownerRecord is .owner's content.
type ownerRecord struct {
	Kind   string `json:"kind"`
	Format int    `json:"format"`
}

// ownerKind is the kind this server writes as.
func (s *Server) ownerKind() string { return cmp.Or(s.captureCfg.Owner, ownerStdio) }

// captureDirName is the capture directory as configured, or the default
// for this server's kind.
func (s *Server) captureDirName() string {
	switch {
	case s.captureCfg.Dir != "":
		return s.captureCfg.Dir
	case s.ownerKind() == ownerServe:
		return defaultServeCaptures
	}
	return defaultStdioCaptures
}

// openCaptureDir opens the capture directory, creating it (mode 0700) and
// its .owner record on first use. .owner is written without replacing
// anything; when it exists already it is read back, and a directory that
// belongs to the other kind of server is owner_mismatch. The kind read is
// cached, so later calls can refuse a mismatch without I/O
// (ownerMismatch).
func (s *Server) openCaptureDir() (*captureDir, *capture.Error) {
	kind := s.ownerKind()
	if kind != ownerStdio && kind != ownerServe {
		return nil, capture.Errorf(capture.CodeDisabled, "", "the capture owner %q is neither %s nor %s", kind, ownerStdio, ownerServe)
	}
	dir := s.captureDirName()
	profiles := s.profiles.Name()
	next := "start perf-agent-mcp with --captures naming a directory inside the profiles root " + profiles
	rel, err := s.rel(s.profiles, dir)
	if err == nil {
		rel = filepath.Clean(rel)
		if !filepath.IsLocal(rel) || rel == "." {
			err = fmt.Errorf("the capture directory %s is not a directory inside the profiles root %s", dir, profiles)
		}
	}
	if err != nil {
		return nil, capture.Errorf(capture.CodeDisabled, next, "%v", err)
	}
	if err := s.profiles.MkdirAll(rel, 0o700); err != nil {
		return nil, capture.Errorf(capture.CodeDisk, next, "cannot create the capture directory %s inside the profiles root %s: %v", rel, profiles, pathless(err))
	}
	root, err := s.profiles.OpenRoot(rel)
	if err != nil {
		return nil, capture.Errorf(capture.CodeDisk, next, "cannot open the capture directory %s inside the profiles root %s: %v", rel, profiles, pathless(err))
	}
	d := &captureDir{root: root, rel: rel, owner: kind, profiles: profiles}
	if e := s.claimOwner(d); e != nil {
		d.close()
		return nil, e
	}
	return d, nil
}

// claimOwner writes d's .owner as this server's kind, or checks the one
// there, and caches the kind it found.
func (s *Server) claimOwner(d *captureDir) *capture.Error {
	b, _ := json.Marshal(ownerRecord{Kind: d.owner, Format: 1})
	_, err := s.publishNew(d.root, rootCaptures, ".", ownerFile, append(b, '\n'), 0o600)
	switch {
	case err == nil:
		s.ownerSeen.Store(&d.owner)
		return nil
	case !errors.Is(err, fs.ErrExist):
		return d.writeError(err)
	}
	abs := d.root.Name()
	b, _, err = readSmall(d.root, ownerFile, maxSmallFile)
	var rec ownerRecord
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil || rec.Format != 1 || (rec.Kind != ownerStdio && rec.Kind != ownerServe) {
		return capture.Errorf(capture.CodeOwnerMismatch, "give this server its own directory with --captures",
			"the capture directory %s has an %s that perf-agent-mcp did not write", abs, ownerFile)
	}
	s.ownerSeen.Store(&rec.Kind)
	return s.ownerMismatch()
}

// ownerMismatch is owner_mismatch when the capture directory is known to
// belong to the other kind of server, and nil when it is this kind's or
// has not been read yet. It does no I/O.
func (s *Server) ownerMismatch() *capture.Error {
	seen := s.ownerSeen.Load()
	if seen == nil || *seen == s.ownerKind() {
		return nil
	}
	dir := s.captureDirName()
	if s.profiles != nil && !filepath.IsAbs(dir) {
		dir = filepath.Join(s.profiles.Name(), dir)
	}
	return capture.Errorf(capture.CodeOwnerMismatch,
		"a stdio server and a serve server never share a capture directory; start this one with --captures naming another directory",
		"the capture directory %s belongs to a perf-agent-mcp %s server, and this one is %s", dir, *seen, s.ownerKind())
}

// series opens the series directory name, creating it (mode 0700) when it
// does not exist. A name that is not a directory is refused.
func (d *captureDir) series(name string) (*os.Root, error) {
	if !validSeries(name) {
		return nil, fmt.Errorf("series %q is not 1-%d bytes of [A-Za-z0-9._-] not starting with '.'", echo(name), maxSeriesName)
	}
	if err := d.root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, d.writeError(err)
	}
	return d.openSeries(name)
}

// openSeries opens an existing series directory, refusing a symbolic link
// or anything but a directory there.
func (d *captureDir) openSeries(name string) (*os.Root, error) {
	fi, err := d.root.Lstat(name)
	if err != nil {
		return nil, d.writeError(err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("series %s in the capture directory %s is %s, not a directory", name, d.root.Name(), describeType(fi.Mode()))
	}
	r, err := d.root.OpenRoot(name)
	if err != nil {
		return nil, d.writeError(err)
	}
	if got, err := r.Stat("."); err != nil || !os.SameFile(fi, got) {
		_ = r.Close()
		return nil, fmt.Errorf("series %s in the capture directory %s changed while it was opened", name, d.root.Name())
	}
	return r, nil
}

// writeError is a failure to write in the capture directory, naming it
// inside the profiles root and the flag that moves it.
func (d *captureDir) writeError(err error) *capture.Error {
	return capture.Errorf(capture.CodeDisk,
		"free space there, or start perf-agent-mcp with --captures naming another directory inside the profiles root",
		"cannot write in the capture directory %s inside the profiles root %s: %v", d.rel, d.profiles, pathless(err))
}

// cause is err as a capture failure: a capture.Error as it is, anything
// else as a write failure in the capture directory.
func (d *captureDir) cause(err error) *capture.Error {
	if e, ok := errors.AsType[*capture.Error](err); ok {
		return e
	}
	return d.writeError(err)
}

// validName reports whether s is 1 to max bytes of [A-Za-z0-9._-] and does
// not start with '.'.
func validName(s string, max int) bool {
	if s == "" || len(s) > max || s[0] == '.' {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func validSeries(s string) bool { return validName(s, maxSeriesName) }
func validLabel(s string) bool  { return validName(s, maxLabel) }

// formatID is a capture id as names and the wire show it: three digits,
// or more once the series passes 999.
func formatID(id int) string { return fmt.Sprintf("%03d", id) }

// parseID is the id formatID wrote as s, refusing any other spelling.
func parseID(s string) (int, bool) {
	id, err := strconv.Atoi(s)
	if err != nil || id < 1 || formatID(id) != s {
		return 0, false
	}
	return id, true
}

// nameKind is what a name in a series is.
type nameKind int

const (
	kindManifest nameKind = iota + 1
	kindIntent
	kindHeartbeat
	kindFile
)

// The suffixes that tell the kinds apart; anything else after "NNN-" is a
// capture file, "<label>-<output>".
const (
	manifestSuffix  = ".capture.json"
	intentSuffix    = ".intent.json"
	heartbeatSuffix = ".intent.hb"
)

// parseName splits a series entry "NNN-<rest>" into its id, the rest and
// its kind: for a manifest, an intent and a heartbeat the rest is the
// label; for a capture file it is "<label>-<output>".
func parseName(name string) (id int, rest string, kind nameKind, ok bool) {
	digits, rest, found := strings.Cut(name, "-")
	if !found {
		return 0, "", 0, false
	}
	if id, ok = parseID(digits); !ok {
		return 0, "", 0, false
	}
	kind = kindFile
	for _, k := range []struct {
		kind   nameKind
		suffix string
	}{{kindManifest, manifestSuffix}, {kindIntent, intentSuffix}, {kindHeartbeat, heartbeatSuffix}} {
		if label, cut := strings.CutSuffix(rest, k.suffix); cut {
			rest, kind = label, k.kind
			break
		}
	}
	if rest == "" {
		return 0, "", 0, false
	}
	return id, rest, kind, true
}

func finalName(id int, label, output string) string {
	return formatID(id) + "-" + label + "-" + output
}
func manifestName(id int, label string) string  { return formatID(id) + "-" + label + manifestSuffix }
func intentName(id int, label string) string    { return formatID(id) + "-" + label + intentSuffix }
func heartbeatName(id int, label string) string { return formatID(id) + "-" + label + heartbeatSuffix }

// seqRecord is .seq's content: the next id the series hands out.
type seqRecord struct {
	Format    int    `json:"format"`
	Next      string `json:"next"`
	WrittenBy string `json:"written_by"`
}

// seqTempPrefix starts the name of .seq's temporary.
const seqTempPrefix = seqFile + ".tmp-"

// nextID hands out the series' next capture id. It is the larger of what
// .seq says and one past the highest manifest or intent id, and .seq is
// rewritten to the id after it (a temporary, fsync, rename, then a
// directory fsync) before the id is returned, so a crash wastes an id and
// never hands one out twice. It must run under the captures lock, the only
// writer of .seq. A .seq that is not this server's is refused.
func (s *Server) nextID(ser *os.Root) (int, error) {
	next, err := readSeq(ser)
	if err != nil {
		return 0, err
	}
	high, err := highestID(ser)
	if err != nil {
		return 0, err
	}
	id := max(next, high+1)
	b, _ := json.Marshal(seqRecord{Format: 1, Next: strconv.Itoa(id + 1), WrittenBy: writtenBy})
	var r [8]byte
	_, _ = rand.Read(r[:])
	tmp := seqTempPrefix + hex.EncodeToString(r[:])
	tid, err := s.writeNewFile(ser, tmp, append(b, '\n'), "seq")
	if err != nil {
		return 0, err
	}
	if err = s.wErr("seq rename"); err == nil {
		err = ser.Rename(tmp, seqFile)
	}
	if err != nil {
		if !errors.Is(err, errCrash) {
			_, _, _ = removeByIdentity(ser, tmp, tid)
		}
		return 0, fmt.Errorf("cannot write %s in %s: %w", seqFile, ser.Name(), err)
	}
	if err := s.syncDir(ser, ".", "fsync seq"); err != nil {
		return 0, err
	}
	return id, nil
}

// readSeq is the id .seq says comes next, 0 when there is no .seq.
func readSeq(ser *os.Root) (int, error) {
	b, _, err := readSmall(ser, seqFile, maxSmallFile)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	var r seqRecord
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	next, perr := strconv.Atoi(r.Next)
	if err != nil || perr != nil || r.Format != 1 || r.WrittenBy != writtenBy || next < 1 {
		return 0, fmt.Errorf("%s in %s is not one this server wrote (%v); move it away to let the series continue", seqFile, ser.Name(), errors.Join(err, perr))
	}
	return next, nil
}

// highestID is the highest id among the series' manifests and intents.
func highestID(ser *os.Root) (int, error) {
	names, err := readDirNames(ser, ".")
	if err != nil {
		return 0, err
	}
	high := 0
	for _, n := range names {
		if id, _, kind, ok := parseName(n); ok && (kind == kindManifest || kind == kindIntent) {
			high = max(high, id)
		}
	}
	return high, nil
}

// readDirNames lists dir inside root.
func readDirNames(root *os.Root, dir string) ([]string, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// syncDir fsyncs dir inside root; op names it for the write seam.
func (s *Server) syncDir(root *os.Root, dir, op string) error {
	if err := s.fault(faultFsync); err != nil {
		return err
	}
	if err := s.wErr(op); err != nil {
		return err
	}
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) {
		// Some filesystems cannot fsync a directory; their renames and
		// links are as durable as they get.
		return nil
	}
	return err
}
