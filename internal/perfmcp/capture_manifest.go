package perfmcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// The manifest's fixed values: its format, who writes it, and the
// outcomes it records.
const (
	manifestFormat = 1
	writtenBy      = "perf-agent-mcp"

	captureOK          = "ok"
	captureFailed      = "failed"
	captureInterrupted = "interrupted"

	// maxManifestBytes bounds a manifest or an intent read back.
	maxManifestBytes = 1 << 20
)

// wireSettings are a capture's settings as the wire, the manifest and the
// settings digest show them.
type wireSettings struct {
	Modes        []string `json:"modes"`
	Duration     string   `json:"duration"`
	SampleRate   string   `json:"sample_rate"`
	Unwind       string   `json:"unwind"`
	KernelStacks bool     `json:"kernel_stacks"`
}

// toWire is st with its modes in canonical order.
func toWire(st capture.Settings) wireSettings {
	st = st.Canonical()
	w := wireSettings{
		Modes:        []string{},
		Duration:     st.Duration.String(),
		SampleRate:   strconv.Itoa(st.SampleRate),
		Unwind:       st.Unwind,
		KernelStacks: st.KernelStacks,
	}
	for _, m := range st.Modes {
		w.Modes = append(w.Modes, string(m))
	}
	return w
}

// settingsDigest is "sha256:" and the SHA-256 of w's canonical JSON: the
// fields in their fixed order and the modes as toWire orders them.
func settingsDigest(w wireSettings) string {
	if w.Modes == nil {
		w.Modes = []string{}
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fileIdentity pins a file the protocol made: its name, device and inode,
// and for a capture file its size and SHA-256. Staged is the file's name
// in the staging directory, in the intent only.
type fileIdentity struct {
	Name   string `json:"name"`
	Staged string `json:"staged,omitzero"`
	Dev    uint64 `json:"dev,string"`
	Ino    uint64 `json:"ino,string"`
	Size   int64  `json:"size,string"`
	SHA256 string `json:"sha256,omitzero"`
}

// statInfo is what the protocol reads of a file's stat. The fields are
// converted with uint64() so this builds where their types differ.
type statInfo struct {
	dev, ino, nlink, uid uint64
}

func statOf(fi fs.FileInfo) (statInfo, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return statInfo{}, false
	}
	return statInfo{dev: uint64(st.Dev), ino: uint64(st.Ino), nlink: uint64(st.Nlink), uid: uint64(st.Uid)}, true
}

// identityOf is name's identity from fi, without a digest.
func identityOf(name string, fi fs.FileInfo) fileIdentity {
	st, _ := statOf(fi)
	return fileIdentity{Name: name, Dev: st.dev, Ino: st.ino, Size: fi.Size()}
}

// identify is name's identity inside root as Lstat sees it.
func identify(root *os.Root, name string) (fileIdentity, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return fileIdentity{}, err
	}
	return identityOf(name, fi), nil
}

// manifestTarget is the profiled process: never its command line.
type manifestTarget struct {
	PID       string `json:"pid,omitzero"`
	Comm      string `json:"comm,omitzero"`
	Exe       string `json:"exe,omitzero"`
	BuildID   string `json:"build_id,omitzero"`
	StartTime string `json:"start_time,omitzero"`
	Launched  bool   `json:"launched,omitzero"`
	Exit      string `json:"exit,omitzero"`
}

// manifestCommand is a launched command: its program and how many
// arguments it had, never the arguments.
type manifestCommand struct {
	Path string `json:"path"`
	Args string `json:"args"`
}

// perfAgentInfo is the perf-agent binary that ran.
type perfAgentInfo struct {
	Path         string   `json:"path,omitzero"`
	Version      string   `json:"version,omitzero"`
	Revision     string   `json:"revision,omitzero"`
	Capabilities []string `json:"capabilities,omitzero"`
}

// manifest is NNN-label.capture.json, the commit record of a capture
// (§7.2): written last, without replacing anything, and the only record
// capture_list, compare_with and retention trust. A failed or interrupted
// capture's manifest has no files.
type manifest struct {
	Format           int              `json:"format"`
	ID               string           `json:"id"`
	Series           string           `json:"series"`
	Label            string           `json:"label"`
	Outcome          string           `json:"outcome"`
	Error            *wireError       `json:"error,omitzero"`
	Settings         wireSettings     `json:"settings"`
	SettingsDigest   string           `json:"settings_digest"`
	Target           manifestTarget   `json:"target,omitzero"`
	Command          *manifestCommand `json:"command,omitzero"`
	PerfAgent        perfAgentInfo    `json:"perf_agent,omitzero"`
	Start            time.Time        `json:"start,omitzero"`
	AttachLatency    string           `json:"attach_latency,omitzero"`
	ProfiledDuration string           `json:"profiled_duration,omitzero"`
	StoppedBy        string           `json:"stopped_by,omitzero"`
	CompareWith      string           `json:"compare_with,omitzero"`
	Files            []fileIdentity   `json:"files,omitzero"`
	Kept             bool             `json:"kept"`
	Notes            []string         `json:"notes,omitzero"`
	Survivors        []wireSurvivor   `json:"survivors,omitzero"`
	Cleanup          string           `json:"cleanup,omitzero"`
	Cancel           string           `json:"cancel,omitzero"`
	WrittenBy        string           `json:"written_by"`
}

// readSmall reads name inside root, refusing a symbolic link, anything but
// a regular file, and a file over limit bytes. The file is opened after
// Lstat and must be the same file, so a link swapped in between is seen.
func readSmall(root *os.Root, name string, limit int64) ([]byte, fs.FileInfo, error) {
	f, fi, err := openNoFollow(root, name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if fi.Size() > limit {
		return nil, nil, fmt.Errorf("%s is %d bytes, over %d", name, fi.Size(), limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s is over %d bytes", name, limit)
	}
	return b, fi, err
}

// errNotRegular is a name that is a symbolic link or not a regular file.
var errNotRegular = errors.New("not a regular file")

// openNoFollow opens the regular file name inside root without following
// a symbolic link there: os.Root follows links inside the root, so the
// name is Lstat'd first and the opened file must be the same one.
func openNoFollow(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	lfi, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is %s: %w", name, describeType(lfi.Mode()), errNotRegular)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err == nil && !os.SameFile(lfi, fi) {
		err = fmt.Errorf("%s changed while it was opened: %w", name, errNotRegular)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// readManifest reads and checks the manifest name inside a series: at most
// 1 MiB, format 1, written by perf-agent-mcp, and the id and label its
// name says.
func readManifest(ser *os.Root, name string) (manifest, error) {
	var m manifest
	b, _, err := readSmall(ser, name, maxManifestBytes)
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if err != nil {
		return manifest{}, fmt.Errorf("manifest %s: %w", name, err)
	}
	id, label, kind, ok := parseName(name)
	switch {
	case m.Format != manifestFormat:
		return manifest{}, fmt.Errorf("manifest %s has format %d, not %d", name, m.Format, manifestFormat)
	case m.WrittenBy != writtenBy:
		return manifest{}, fmt.Errorf("manifest %s was not written by %s", name, writtenBy)
	case !ok || kind != kindManifest || m.ID != formatID(id) || m.Label != label:
		return manifest{}, fmt.Errorf("manifest %s names capture %s-%s", name, echo(m.ID), echo(m.Label))
	case m.Outcome != captureOK && m.Outcome != captureFailed && m.Outcome != captureInterrupted:
		return manifest{}, fmt.Errorf("manifest %s has outcome %q", name, echo(m.Outcome))
	case (m.Outcome == captureOK) != (len(m.Files) > 0):
		return manifest{}, fmt.Errorf("manifest %s is %s with %d files", name, m.Outcome, len(m.Files))
	}
	for _, f := range m.Files {
		out, ok := strings.CutPrefix(f.Name, formatID(id)+"-"+label+"-")
		if !ok || !validName(out, maxFileName) || !isHex(f.SHA256, 64) {
			return manifest{}, fmt.Errorf("manifest %s lists file %s", name, echo(f.Name))
		}
	}
	return m, nil
}

// seriesManifests reads every manifest of a series, in id order. bad
// names the manifests that could not be read or are not this server's.
func seriesManifests(ser *os.Root) (ms []manifest, bad []string, err error) {
	names, err := readDirNames(ser, ".")
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(names)
	for _, n := range names {
		if _, _, kind, ok := parseName(n); !ok || kind != kindManifest {
			continue
		}
		m, err := readManifest(ser, n)
		if err != nil {
			bad = append(bad, n)
			continue
		}
		ms = append(ms, m)
	}
	slices.SortFunc(ms, func(a, b manifest) int {
		ia, _ := parseID(a.ID)
		ib, _ := parseID(b.ID)
		return ia - ib
	})
	return ms, bad, nil
}

// verifyFiles checks that each of m's files is still the one it records:
// the same regular file by device and inode, with its size and SHA-256.
func verifyFiles(ser *os.Root, m manifest) error {
	for _, want := range m.Files {
		if why, err := matches(ser, want.Name, want); err != nil || why != "" {
			return fmt.Errorf("%s: %s", want.Name, cmpOrErr(why, err))
		}
	}
	return nil
}

func cmpOrErr(why string, err error) string {
	if err != nil {
		return err.Error()
	}
	return why
}

// matches says why name is not the file want records, "" when it is: a
// regular file (not a symbolic link) with want's device and inode, one or
// two links (a capture file's staging and final names), and want's size
// and SHA-256. The contents are what prove the file: an inode number can
// be reused, and this server's own links, renames and heartbeats change
// the file's ctime. An identity with no digest is never matched.
func matches(root *os.Root, name string, want fileIdentity) (why string, err error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	st, _ := statOf(fi)
	switch {
	case !fi.Mode().IsRegular():
		return describeType(fi.Mode()) + ", not the regular file recorded", nil
	case st.dev != want.Dev || st.ino != want.Ino:
		return "a different file from the one recorded", nil
	case st.nlink != 1 && st.nlink != 2:
		return fmt.Sprintf("%d links, not the one or two recorded", st.nlink), nil
	case want.SHA256 == "":
		return "a file whose contents were never recorded", nil
	case fi.Size() != want.Size:
		return fmt.Sprintf("%d bytes, not the %d recorded", fi.Size(), want.Size), nil
	}
	f, ofi, err := openNoFollow(root, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if !os.SameFile(fi, ofi) {
		return "a different file from the one recorded", nil
	}
	sum, err := hashFile(f)
	if err != nil {
		return "", err
	}
	if sum != want.SHA256 {
		return "contents that differ from the ones recorded", nil
	}
	return "", nil
}

// hashFile is the hex SHA-256 of f from its start.
func hashFile(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// quarantinePrefix starts the private name a file is moved to while its
// removal is decided.
const quarantinePrefix = ".del-"

// quarantineHook, when set, runs between the identity check and the move
// aside, and quarantineAfterHook just after the move (an errCrash from it
// stops there, as a crash would); seams for tests.
var (
	quarantineHook      func(root *os.Root, name string)
	quarantineAfterHook func() error
)

// removeByIdentity removes name inside root only when it is the file want
// records (see matches). It reports whether it removed the name, and when
// it left it, why: a name that is not provably the recorded file is never
// removed. A missing name is neither removed nor left. The check and the
// unlink are not one step, so the name is first renamed to a private
// quarantine name beside it, the file there is checked again, and only
// then unlinked; a file that turns out not to be the recorded one is put
// back under its name without replacing anything.
func removeByIdentity(root *os.Root, name string, want fileIdentity) (removed bool, left string, err error) {
	return removeTagged(root, name, want, "")
}

// removeTagged is removeByIdentity with tag in the quarantine name: a
// capture's token, so that recovery can tell whose quarantined file a
// crash left.
func removeTagged(root *os.Root, name string, want fileIdentity, tag string) (removed bool, left string, err error) {
	why, err := matches(root, name, want)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, "", nil
	case err != nil:
		return false, "", err
	case why != "":
		return false, why, nil
	}
	if quarantineHook != nil {
		quarantineHook(root, name)
	}
	q := filepath.Join(filepath.Dir(name), quarantinePrefix+tag+randHex(8))
	if err := root.Rename(name, q); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, "", nil
		}
		return false, "", err
	}
	if quarantineAfterHook != nil {
		if err := quarantineAfterHook(); err != nil {
			return false, "", err
		}
	}
	why, err = matches(root, q, want)
	if err == nil && why == "" {
		if err := root.Remove(q); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, "", err
		}
		return true, "", nil
	}
	return false, cmpOrErr(why, err) + restore(root, q, name), nil
}

// restore puts q back under name without replacing anything there, and
// says where the file is when it cannot.
func restore(root *os.Root, q, name string) string {
	if err := root.Link(q, name); err == nil {
		_ = root.Remove(q)
		return ""
	}
	if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		if err := root.Rename(q, name); err == nil {
			return ""
		}
	}
	return "; it was moved aside to " + q + ", since its name is taken now"
}
