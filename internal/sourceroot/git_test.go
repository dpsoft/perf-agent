package sourceroot

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// requireGit returns the path of the real git, skipping without one.
func requireGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return p
}

// testGit runs git for test setup, isolated from the user's configuration.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main", "-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitTree is sourceTree committed to a new repository at its top.
func gitTree(t *testing.T) (dir, commit string) {
	t.Helper()
	requireGit(t)
	dir = sourceTree(t)
	testGit(t, dir, "init", "-q")
	testGit(t, dir, "add", ".")
	testGit(t, dir, "commit", "-q", "-m", "init")
	return dir, testGit(t, dir, "rev-parse", "HEAD")
}

func TestGitCleanFileIsUserSelected(t *testing.T) {
	dir, commit := gitTree(t)
	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	res := r.Resolve("/build/app/src/app.c", "b1", "")
	if res.Status != StatusUserSelected || res.Commit != commit || res.VerifiedBy != "git" || res.Reason != "" {
		t.Fatalf("Resolve = %+v, want user_selected at %s", res, commit)
	}
}

func TestGitRepositoryAboveTheRootIsUnverified(t *testing.T) {
	dir, _ := gitTree(t)
	r := openRoots(t, []string{filepath.Join(dir, "src")}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	res := r.Resolve("/build/app/src/util.c", "", "")
	if res.Status != StatusSourceUnverified || res.Reason != "git repository is outside the source root" || res.GitTopLevel != dir || res.Commit != "" {
		t.Fatalf("Resolve = %+v, want source_unverified %q with top-level %s", res, "git repository is outside the source root", dir)
	}
}

func TestGitTopLevelIsReported(t *testing.T) {
	dir, commit := gitTree(t)
	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := r.Resolve("src/app.c", "", ""); res.Status != StatusUserSelected || res.GitTopLevel != dir || res.Commit != commit {
		t.Fatalf("Resolve = %+v, want user_selected with top-level %s", res, dir)
	}
}

func TestGitWorktreeRootIsUserSelected(t *testing.T) {
	dir, commit := gitTree(t)
	wt := filepath.Join(t.TempDir(), "wt")
	testGit(t, dir, "worktree", "add", "-q", "--detach", wt)
	r := openRoots(t, []string{wt}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := r.Resolve("/build/app/src/app.c", "", ""); res.Status != StatusUserSelected || res.Commit != commit || res.GitTopLevel != wt {
		t.Fatalf("Resolve = %+v, want user_selected at %s with top-level %s", res, commit, wt)
	}
}

func TestGitSubmoduleFileHasItsOwnReason(t *testing.T) {
	dir, _ := gitTree(t)
	// A repository embedded in the superproject (a gitlink, .git directory)
	// and a submodule proper (a .git file pointing into the superproject).
	nested := filepath.Join(dir, "vendor", "lib")
	writeFile(t, nested, "lib.c", "lib\n")
	testGit(t, nested, "init", "-q")
	testGit(t, nested, "add", ".")
	testGit(t, nested, "commit", "-q", "-m", "lib")
	testGit(t, dir, "add", "vendor/lib")
	sub := t.TempDir()
	writeFile(t, sub, "s/sub.c", "sub\n")
	testGit(t, sub, "init", "-q")
	testGit(t, sub, "add", ".")
	testGit(t, sub, "commit", "-q", "-m", "sub")
	testGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "third")
	testGit(t, dir, "commit", "-q", "-m", "modules")
	if fi, err := os.Lstat(filepath.Join(dir, "third", ".git")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("submodule .git is not a file (%v)", err)
	}

	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	for _, name := range []string{"vendor/lib/lib.c", "third/s/sub.c"} {
		if res := r.Resolve(name, "", ""); res.Status != StatusSourceUnverified || res.Reason != "inside a submodule or nested repository" {
			t.Errorf("Resolve(%s) = %+v, want source_unverified %q", name, res, "inside a submodule or nested repository")
		}
	}
	if res := r.Resolve("src/app.c", "", ""); res.Status != StatusUserSelected {
		t.Fatalf("superproject file: %+v, want user_selected", res)
	}
}

// countingProber answers like a clean checkout at one commit and counts
// the Head calls.
type countingProber struct {
	mu    sync.Mutex
	heads int
}

func (p *countingProber) Head(dir string) (HeadInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.heads++
	return HeadInfo{Commit: strings.Repeat("c", 40), Format: "sha1", TopLevel: dir}, nil
}

func (p *countingProber) Blob(string, string, string) (string, error) { return gitBlobSHA1(appC), nil }

func TestBatchProbesHeadOncePerRoot(t *testing.T) {
	p := &countingProber{}
	a, b := sourceTree(t), t.TempDir()
	writeFile(t, b, "b.c", appC)
	r := openRoots(t, []string{a, b}, nil, p, DefaultLimits())
	batch := r.Batch()
	for range 3 {
		if res := batch.Resolve("/build/src/app.c", "", ""); res.Status != StatusUserSelected || res.Root != a {
			t.Fatalf("Resolve = %+v, want user_selected in %s", res, a)
		}
	}
	if res := batch.ResolveBasename("b.c", "", ""); res.Status != StatusUserSelected || res.Root != b {
		t.Fatalf("ResolveBasename = %+v, want user_selected in %s", res, b)
	}
	if p.heads != 2 {
		t.Fatalf("a batch over two roots ran Head %d times, want 2", p.heads)
	}
	// Outside a batch every resolution reads HEAD afresh.
	r.Resolve("src/app.c", "", "")
	r.ResolveBasename("b.c", "", "")
	if p.heads != 4 {
		t.Fatalf("Head ran %d times, want 4", p.heads)
	}
}

func TestGitModifiedOrUntrackedIsUnverified(t *testing.T) {
	dir, commit := gitTree(t)
	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())

	writeFile(t, dir, "src/app.c", appC+"// edited\n")
	res := r.Resolve("/build/app/src/app.c", "", "")
	if want := "file differs from HEAD " + commit; res.Status != StatusSourceUnverified || res.Reason != want || res.Commit != "" {
		t.Fatalf("edited: %+v, want source_unverified %q", res, want)
	}

	writeFile(t, dir, "src/new.c", "new\n")
	writeFile(t, dir, "fresh/dir/new2.c", "new\n")
	for _, name := range []string{"src/new.c", "fresh/dir/new2.c"} {
		if res := r.Resolve(name, "", ""); res.Status != StatusSourceUnverified || res.Reason != "not tracked at HEAD" {
			t.Fatalf("untracked %s: %+v, want source_unverified %q", name, res, "not tracked at HEAD")
		}
	}

	plain := openRoots(t, []string{sourceTree(t)}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := plain.Resolve("src/app.c", "", ""); res.Status != StatusSourceUnverified || res.Reason != "not a git checkout" {
		t.Fatalf("plain dir: %+v, want source_unverified %q", res, "not a git checkout")
	}

	empty := sourceTree(t)
	testGit(t, empty, "init", "-q")
	er := openRoots(t, []string{empty}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := er.Resolve("src/app.c", "", ""); res.Status != StatusSourceUnverified || res.Reason != "no commit at HEAD" {
		t.Fatalf("repository without commits: %+v, want source_unverified %q", res, "no commit at HEAD")
	}
}

func TestGitNotAvailable(t *testing.T) {
	dir, _ := gitTree(t)
	t.Setenv("PATH", t.TempDir())
	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := r.Resolve("src/app.c", "", ""); res.Status != StatusSourceUnverified || res.Reason != "git not available" {
		t.Fatalf("Resolve = %+v, want source_unverified %q", res, "git not available")
	}
}

// fakeGitBin is a directory holding a "git" that runs script.
func fakeGitBin(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestGitTooOldFailsClosed(t *testing.T) {
	dir, _ := gitTree(t)
	bin := fakeGitBin(t, "echo 'unknown option: --no-lazy-fetch' >&2\nexit 129\n")
	t.Setenv("PATH", bin)
	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := r.Resolve("src/app.c", "", ""); res.Status != StatusSourceUnverified || res.Reason != "git too old to probe offline" {
		t.Fatalf("Resolve = %+v, want source_unverified %q", res, "git too old to probe offline")
	}
}

// invocation is one logged run of the wrapping fake git.
type invocation struct {
	args []string
	env  []string
}

func readInvocations(t *testing.T, log string) []invocation {
	t.Helper()
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []invocation
	var cur *invocation
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "BEGIN":
			out = append(out, invocation{})
			cur = &out[len(out)-1]
		case strings.HasPrefix(line, "ARG "):
			cur.args = append(cur.args, strings.TrimPrefix(line, "ARG "))
		case strings.HasPrefix(line, "ENV "):
			cur.env = append(cur.env, strings.TrimPrefix(line, "ENV "))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGitProberNeverRunsStatusOrFilters(t *testing.T) {
	realGit := requireGit(t)
	dir, _ := gitTree(t)
	// A clean filter that would leave a mark if anything ran it. It is
	// configured after the commit, so setting it up does not run it.
	mark := filepath.Join(t.TempDir(), "filter-ran")
	writeFile(t, dir, ".gitattributes", "*.c filter=mark\n")
	testGit(t, dir, "add", ".gitattributes")
	testGit(t, dir, "commit", "-q", "-m", "attributes")
	commit := testGit(t, dir, "rev-parse", "HEAD")
	testGit(t, dir, "config", "filter.mark.clean", "touch "+mark+"; cat")

	log := filepath.Join(t.TempDir(), "git.log")
	bin := fakeGitBin(t, fmt.Sprintf(`{
  echo BEGIN
  for a in "$@"; do echo "ARG $a"; done
  env | sort | sed 's/^/ENV /'
} >> %s
exec %s "$@"
`, log, realGit))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := r.Resolve("src/app.c", "", ""); res.Status != StatusUserSelected || res.Commit != commit {
		t.Fatalf("clean file: %+v, want user_selected", res)
	}
	writeFile(t, dir, "src/util.c", "edited\n")
	r.Resolve("src/util.c", "", "")
	writeFile(t, dir, "src/new/x.c", "new\n")
	r.Resolve("src/new/x.c", "", "")

	calls := readInvocations(t, log)
	if len(calls) < 6 {
		t.Fatalf("logged %d git runs, want the probes of three resolutions", len(calls))
	}
	prefix := []string{"--no-lazy-fetch", "-c", "protocol.allow=never", "-c", "core.fsmonitor=false", "-C", dir, "rev-parse"}
	wantEnv := []string{
		"GIT_ALLOW_PROTOCOL=", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0", "GIT_PROTOCOL_FROM_USER=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
	}
	for i, c := range calls {
		if len(c.args) <= len(prefix) || !slices.Equal(c.args[:len(prefix)], prefix) {
			t.Errorf("run %d: argv %q, want it to start %q", i, c.args, prefix)
		}
		for _, a := range c.args[min(len(prefix), len(c.args)):] {
			if !strings.HasPrefix(a, "--verify") && a != "--quiet" && a != "--show-object-format" && a != "--show-toplevel" && !isRevision(a) {
				t.Errorf("run %d: unexpected argument %q in %q", i, a, c.args)
			}
		}
		var env []string
		for _, e := range c.env {
			if strings.HasPrefix(e, "GIT_") || strings.HasPrefix(e, "LC_") || strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "XDG_") {
				env = append(env, e)
			}
		}
		if !slices.Equal(env, wantEnv) {
			t.Errorf("run %d: environment %q, want exactly %q", i, env, wantEnv)
		}
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clean filter ran (stat err %v)", err)
	}

	// The filter is live: hashing the file the way git status would runs it.
	testGit(t, dir, "hash-object", "src/app.c")
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("control: the clean filter did not run, so the check above proves nothing: %v", err)
	}
}

// isRevision reports whether a is an argument the prober builds from an
// object id: "<oid>^{...}" or "<oid>:./<path>".
func isRevision(a string) bool {
	if a == "HEAD^{commit}" {
		return true
	}
	oid, _, ok := strings.Cut(a, ":./")
	if !ok {
		oid, _, ok = strings.Cut(a, "^{")
	}
	return ok && validOID(oid)
}

// partialClone makes a promisor clone of a one-commit repository with
// filter, without a checkout, writes the committed d/f.c into its working
// tree, and points the promisor remote at a helper that leaves marker when
// git invokes it. It returns the clone, the helper's bin directory and the
// file's blob id.
func partialClone(t *testing.T, filter, marker string) (clone, bin, blob string) {
	t.Helper()
	requireGit(t)
	src := t.TempDir()
	writeFile(t, src, "d/f.c", "hello\n")
	testGit(t, src, "init", "-q")
	testGit(t, src, "add", ".")
	testGit(t, src, "commit", "-q", "-m", "x")
	blob = testGit(t, src, "rev-parse", "HEAD:d/f.c")

	base := t.TempDir()
	bare := filepath.Join(base, "bare.git")
	testGit(t, base, "clone", "-q", "--bare", src, bare)
	testGit(t, bare, "config", "uploadpack.allowFilter", "true")
	clone = filepath.Join(base, "clone")
	testGit(t, base, "clone", "-q", "--no-checkout", "--filter="+filter, "file://"+bare, clone)
	testGit(t, clone, "config", "remote.origin.url", "fake::"+bare)
	// Repository configuration that allows every transport: the prober
	// must not let it reopen the protocol layer.
	testGit(t, clone, "config", "protocol.allow", "always")
	testGit(t, clone, "config", "protocol.fake.allow", "always")
	writeFile(t, clone, "d/f.c", "hello\n")

	bin = t.TempDir()
	helper := fmt.Sprintf("#!/bin/sh\ntouch %s\nexit 1\n", marker)
	if err := os.WriteFile(filepath.Join(bin, "git-remote-fake"), []byte(helper), 0o755); err != nil {
		t.Fatal(err)
	}
	return clone, bin, blob
}

func TestGitPartialCloneNeverFetches(t *testing.T) {
	for _, filter := range []string{"blob:none", "tree:0"} {
		t.Run(filter, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "helper-ran")
			clone, bin, blob := partialClone(t, filter, marker)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			// Control: without the offline options git does reach for the
			// promisor remote, so the marker test below can fail.
			cmd := exec.Command("git", "-C", clone, "rev-parse", "--verify", "--quiet", blob+"^{blob}")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			_ = cmd.Run()
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("control: git did not invoke the promisor helper (%v), so this test proves nothing", err)
			}
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}

			r := openRoots(t, []string{clone}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
			res := r.Resolve("/build/d/f.c", "", "")
			if res.Status != StatusSourceUnverified || res.Reason != "object not available locally" {
				t.Fatalf("Resolve = %+v, want source_unverified %q", res, "object not available locally")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the prober invoked the promisor helper (stat err %v)", err)
			}
		})
	}
}

// movingProber answers Head with the next of heads and records the commit
// each Blob call asks for. Blob gives the file's real id only at good.
type movingProber struct {
	mu     sync.Mutex
	heads  []string
	asked  []string
	good   string
	blobID string
}

func (p *movingProber) Head(dir string) (HeadInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.heads[0]
	p.heads = p.heads[1:]
	return HeadInfo{Commit: h, Format: "sha1", TopLevel: dir}, nil
}

func (p *movingProber) Blob(_, commit, _ string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, commit)
	if commit == p.good {
		return p.blobID, nil
	}
	return strings.Repeat("0", 40), nil
}

func gitBlobSHA1(content string) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

func TestHeadMovingNeverPairsACommitWithAnotherBlob(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	// HEAD reads A, then moves to B, where the file matches. The first
	// resolution must compare against A's blob and say it differs, never
	// borrow B's blob for commit A.
	p := &movingProber{heads: []string{a, b}, good: b, blobID: gitBlobSHA1(appC)}
	r := openRoots(t, []string{sourceTree(t)}, nil, p, DefaultLimits())

	first := r.Resolve("src/app.c", "", "")
	if first.Status != StatusSourceUnverified || first.Reason != "file differs from HEAD "+a {
		t.Fatalf("first = %+v, want it to differ from HEAD %s", first, a)
	}
	second := r.Resolve("src/app.c", "", "")
	if second.Status != StatusUserSelected || second.Commit != b {
		t.Fatalf("second = %+v, want user_selected at %s", second, b)
	}
	if !slices.Equal(p.asked, []string{a, b}) {
		t.Fatalf("Blob asked for %v, want each resolution's own HEAD [%s %s]", p.asked, a, b)
	}
}

func TestBlobIDPerObjectFormat(t *testing.T) {
	if got := blobID("sha1", []byte(appC)); got != gitBlobSHA1(appC) {
		t.Fatalf("sha1 blob id = %s, want %s", got, gitBlobSHA1(appC))
	}
	// git hash-object --object-format=sha256 of "hello\n".
	const want256 = "2cf8d83d9ee29543b34a87727421fdecb7e3f3a183d337639025de576db9ebb4"
	if got := blobID("sha256", []byte("hello\n")); got != want256 {
		t.Fatalf("sha256 blob id = %s, want %s", got, want256)
	}
	if got := blobID("md5", []byte("x")); got != "" {
		t.Fatalf("unknown format: %q, want empty", got)
	}
}

func TestGitSHA256Repository(t *testing.T) {
	requireGit(t)
	dir := sourceTree(t)
	testGit(t, dir, "init", "-q", "--object-format=sha256")
	testGit(t, dir, "add", ".")
	testGit(t, dir, "commit", "-q", "-m", "init")
	commit := testGit(t, dir, "rev-parse", "HEAD")
	r := openRoots(t, []string{dir}, nil, GitProber(DefaultLimits().GitTimeout), DefaultLimits())
	if res := r.Resolve("src/app.c", "", ""); res.Status != StatusUserSelected || res.Commit != commit || len(commit) != 64 {
		t.Fatalf("Resolve = %+v, want user_selected at the 64-hex %s", res, commit)
	}
}

func TestGitProtocolLayerAloneBlocksTheHelper(t *testing.T) {
	for _, filter := range []string{"blob:none", "tree:0"} {
		t.Run(filter, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "helper-ran")
			clone, bin, _ := partialClone(t, filter, marker)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			// With lazy fetching left on, git does try the promisor remote;
			// only the protocol settings stand between it and the helper.
			p := gitProber{timeout: DefaultLimits().GitTimeout, lazyFetch: true}
			r := openRoots(t, []string{clone}, nil, p, DefaultLimits())
			res := r.Resolve("/build/d/f.c", "", "")
			if res.Status != StatusSourceUnverified || res.Reason != "object not available locally" {
				t.Fatalf("Resolve = %+v, want source_unverified %q", res, "object not available locally")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the protocol layer let git invoke the helper (stat err %v)", err)
			}
		})
	}
}
