package sourceroot

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Prober reads a git checkout's HEAD. Head returns the commit HEAD names,
// the repository's object format and its top-level; Blob returns the
// object id of rel (a slash path relative to dir) in that commit's tree.
// Blob takes the commit Head returned, so both answers are about one commit.
type Prober interface {
	Head(dir string) (HeadInfo, error)
	Blob(dir, commit, rel string) (string, error)
}

// HeadInfo is what Head reads: the commit HEAD names, the object format
// ("sha1" or "sha256") and the repository's working tree top-level.
type HeadInfo struct {
	Commit, Format, TopLevel string
}

// Errors a Prober returns; each becomes a source_unverified reason.
var (
	ErrGitUnavailable = errors.New("git not available")
	ErrGitTooOld      = errors.New("git too old to probe offline")
	ErrNotCheckout    = errors.New("not a git checkout")
	ErrNoCommit       = errors.New("no commit at HEAD")
	ErrNotTracked     = errors.New("not tracked at HEAD")
	ErrObjectMissing  = errors.New("object not available locally")
)

// gitReason is the source_unverified reason for a probe error.
func gitReason(err error) string {
	for _, known := range []error{ErrGitUnavailable, ErrGitTooOld, ErrNotCheckout, ErrNoCommit, ErrNotTracked, ErrObjectMissing} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "git probe failed: " + err.Error()
}

// gitOptions come before the subcommand on every run. --no-lazy-fetch keeps
// a partial clone from fetching a missing object (git fails instead).
// protocol.allow=never is a second guard, but a repository's own
// protocol.<name>.allow can override it; the empty GIT_ALLOW_PROTOCOL in
// gitEnv is the guard repository configuration cannot lift.
// core.fsmonitor=false keeps a repository's configuration from starting a
// monitor.
var gitOptions = []string{"--no-lazy-fetch", "-c", "protocol.allow=never", "-c", "core.fsmonitor=false"}

// gitEnv is git's whole environment besides PATH: no transport allowed
// (an empty GIT_ALLOW_PROTOCOL lists none, and overrides any
// protocol.*.allow), no user or system configuration, no lazy fetch, no
// prompts, no optional lock files, no replace refs, and messages in the C
// locale. Nothing else from the server's environment (GIT_DIR, HOME, ...)
// reaches git.
var gitEnv = []string{
	"GIT_ALLOW_PROTOCOL=",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_NO_LAZY_FETCH=1",
	"GIT_NO_REPLACE_OBJECTS=1",
	"GIT_OPTIONAL_LOCKS=0",
	"GIT_PROTOCOL_FROM_USER=0",
	"GIT_TERMINAL_PROMPT=0",
	"LC_ALL=C",
}

// maxGitOutput bounds what is kept of one run's stdout and stderr.
const maxGitOutput = 4096

type gitProber struct {
	timeout time.Duration
	// lazyFetch drops --no-lazy-fetch and GIT_NO_LAZY_FETCH, so a test can
	// show the protocol settings alone keep git offline.
	lazyFetch bool
}

// GitProber runs only `git rev-parse`, offline and read-only, each run
// limited to timeout. It never runs status, diff or anything that applies
// filters: the file's blob id is computed here from the snapshot.
func GitProber(timeout time.Duration) Prober { return gitProber{timeout: timeout} }

// revParse runs `git <options> -C dir rev-parse args...`. A clean exit
// gives stdout without its surrounding space; exit 1 (what --verify --quiet gives for a
// name that does not resolve) gives found == false and no error.
func (g gitProber) revParse(dir string, args ...string) (out string, found bool, err error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return "", false, ErrGitUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	opts, env := gitOptions, gitEnv
	if g.lazyFetch {
		opts = slices.DeleteFunc(slices.Clone(opts), func(o string) bool { return o == "--no-lazy-fetch" })
		env = slices.DeleteFunc(slices.Clone(env), func(e string) bool { return e == "GIT_NO_LAZY_FETCH=1" })
	}
	argv := append(append(slices.Clone(opts), "-C", dir, "rev-parse"), args...)
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	cmd.Dir = dir
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return "", false, fmt.Errorf("git timed out after %s", g.timeout)
	}
	if runErr == nil {
		return strings.TrimSpace(stdout.String()), true, nil
	}
	exitErr, ok := errors.AsType[*exec.ExitError](runErr)
	if !ok {
		return "", false, fmt.Errorf("cannot run git: %w", runErr)
	}
	msg := stderr.String()
	switch code := exitErr.ExitCode(); {
	case code == 129 && strings.Contains(msg, "no-lazy-fetch"):
		return "", false, ErrGitTooOld
	case strings.Contains(msg, "not a git repository"):
		return "", false, ErrNotCheckout
	case strings.Contains(msg, "from promisor remote"), strings.Contains(msg, "transport '") && strings.Contains(msg, "' not allowed"):
		// A missing object git was refused leave to fetch.
		return "", false, ErrObjectMissing
	case strings.Contains(msg, "dubious ownership"):
		return "", false, errors.New("git refuses a repository owned by another user")
	case code == 1 && strings.TrimSpace(msg) == "":
		return "", false, nil
	default:
		first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
		return "", false, fmt.Errorf("git exited %d: %s", code, first)
	}
}

func (g gitProber) Head(dir string) (HeadInfo, error) {
	commit, found, err := g.revParse(dir, "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return HeadInfo{}, err
	}
	if !found {
		return HeadInfo{}, ErrNoCommit
	}
	out, found, err := g.revParse(dir, "--show-object-format", "--show-toplevel")
	if err != nil {
		return HeadInfo{}, err
	}
	format, top, _ := strings.Cut(out, "\n")
	if !found || oidLen(format) == 0 {
		return HeadInfo{}, fmt.Errorf("unknown object format %q", format)
	}
	if !validOID(commit) || len(commit) != oidLen(format) {
		return HeadInfo{}, fmt.Errorf("unexpected commit id %q", commit)
	}
	if !filepath.IsAbs(top) || strings.Contains(top, "\n") {
		return HeadInfo{}, fmt.Errorf("unexpected top-level %q", top)
	}
	return HeadInfo{Commit: commit, Format: format, TopLevel: top}, nil
}

// Blob looks rel up in commit's tree and checks the object is present
// locally. When the lookup fails it walks the path's directories from the
// top to tell an untracked file from a tree object missing in a partial
// clone.
func (g gitProber) Blob(dir, commit, rel string) (string, error) {
	if !validOID(commit) {
		return "", fmt.Errorf("unexpected commit id %q", commit)
	}
	oid, found, err := g.revParse(dir, "--verify", "--quiet", commit+":./"+rel)
	if err != nil {
		return "", err
	}
	if found {
		if err := g.present(dir, oid); err != nil {
			return "", err
		}
		return oid, nil
	}
	// The commit's root tree, then each directory down to rel's parent.
	tree, found, err := g.revParse(dir, "--verify", "--quiet", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrObjectMissing
	}
	if err := g.present(dir, tree); err != nil {
		return "", err
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		sub, found, err := g.revParse(dir, "--verify", "--quiet", commit+":./"+strings.Join(parts[:i], "/"))
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrNotTracked
		}
		if err := g.present(dir, sub); err != nil {
			return "", err
		}
	}
	return "", ErrNotTracked
}

// present returns ErrObjectMissing unless oid is in the local object store.
func (g gitProber) present(dir, oid string) error {
	if !validOID(oid) {
		return fmt.Errorf("unexpected object id %q", oid)
	}
	_, found, err := g.revParse(dir, "--verify", "--quiet", oid+"^{object}")
	if err != nil {
		return err
	}
	if !found {
		return ErrObjectMissing
	}
	return nil
}

func oidLen(format string) int {
	switch format {
	case "sha1":
		return 2 * sha1.Size
	case "sha256":
		return 2 * sha256.Size
	}
	return 0
}

// validOID reports whether s is a full lowercase hex object id.
func validOID(s string) bool {
	if len(s) != 2*sha1.Size && len(s) != 2*sha256.Size {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// blobID is git's object id for a blob holding data: the format's hash of
// "blob <len>\x00" and the bytes. It is "" for an unknown format.
func blobID(format string, data []byte) string {
	var h hash.Hash
	switch format {
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	default:
		return ""
	}
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// capped keeps the first maxGitOutput bytes written to it.
type capped struct{ buf bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxGitOutput - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }
