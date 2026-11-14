package sourceroot

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// Resolution is where a profile's file name led and what ties the file
// found to the profiled binary.
type Resolution struct {
	Status     Status   `json:"status"`
	Reason     string   `json:"reason,omitempty"`
	Root       string   `json:"root,omitempty"` // the source root holding the file
	Path       string   `json:"path,omitempty"` // slash path relative to Root
	Candidates []string `json:"candidates,omitzero"`
	// Commit is the commit the evidence names: the HEAD a user_selected
	// file matched, or the manifest's commit.
	Commit     string `json:"commit,omitempty"`
	VerifiedBy string `json:"verified_by,omitempty"` // "manifest" or "git"
	// SHA256 is the hex digest of the snapshot, absent when the file was
	// over the size limit.
	SHA256 string `json:"sha256,omitempty"`

	snap *snapshot // the bytes the evidence is about; nil unless one file matched
}

// Line is one source line as Lines returns it: at most MaxLineBytes of
// valid UTF-8.
type Line struct {
	N         int64  `json:"n"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

func unresolved(reason string) Resolution {
	return Resolution{Status: StatusUnresolved, Reason: reason}
}

// hit is one matching file: a root index and the slash path inside it.
type hit struct {
	root int
	rel  string
	info os.FileInfo
}

// Resolve finds filename, a path as the profile records it. The path is
// cleaned, its leading "/" and any ".." dropped, and its suffixes tried
// longest first at the top of every root: the first suffix length that
// matches any regular file decides. One match is verified; several are
// ambiguous.
func (rs *Roots) Resolve(filename, buildID, captureID string) Resolution {
	comps := components(filename)
	if len(comps) == 0 {
		return unresolved("no file name to look up")
	}
	for k := range comps {
		suffix := comps[k:]
		if slices.Contains(suffix, ".git") {
			continue
		}
		rel := strings.Join(suffix, "/")
		var hits []hit
		for i, r := range rs.roots {
			fi, err := r.r.Stat(rel)
			if err == nil && fi.Mode().IsRegular() {
				hits = addHit(hits, hit{i, rel, fi})
			}
		}
		if len(hits) > 0 {
			return rs.decide(hits, buildID, captureID)
		}
	}
	return unresolved("no file under the source roots matches " + strings.Join(comps, "/"))
}

// components is the slash path's cleaned components without "/", "." or
// "..".
func components(name string) []string {
	var out []string
	for c := range strings.SplitSeq(path.Clean("/"+name), "/") {
		if c != "" && c != "." && c != ".." {
			out = append(out, c)
		}
	}
	return out
}

// addHit appends h unless it is a file already in hits, as when one root
// is nested in another.
func addHit(hits []hit, h hit) []hit {
	if slices.ContainsFunc(hits, func(o hit) bool { return os.SameFile(o.info, h.info) }) {
		return hits
	}
	return append(hits, h)
}

// ResolveBasename finds a bare file name, as PC sampling records it, by
// indexing each root's regular files (not following symlinks, skipping
// .git). Two matches anywhere are ambiguous: the lookup never guesses.
func (rs *Roots) ResolveBasename(base, buildID, captureID string) Resolution {
	if strings.Contains(base, "/") {
		base = path.Base(path.Clean(base))
	}
	if base == "" || base == "." || base == ".." || base == "/" || base == ".git" {
		return unresolved("no file name to look up")
	}
	var hits []hit
	limited := false
	for i, r := range rs.roots {
		idx := rs.indexOf(r)
		limited = limited || idx.limited
		for _, rel := range idx.files[base] {
			if fi, err := r.r.Lstat(rel); err == nil && fi.Mode().IsRegular() {
				hits = addHit(hits, hit{i, rel, fi})
			}
		}
	}
	switch {
	case len(hits) > 1:
		return rs.decide(hits, buildID, captureID)
	case limited:
		return unresolved("source walk limit reached")
	case len(hits) == 0:
		return unresolved("no file named " + base + " under the source roots")
	}
	return rs.decide(hits, buildID, captureID)
}

// basenameIndex maps a file name to the slash paths holding it in one root.
type basenameIndex struct {
	files   map[string][]string
	limited bool // the walk stopped at MaxWalkEntries; files is partial
}

// indexOf returns r's index, walking the root the first time.
func (rs *Roots) indexOf(r *root) *basenameIndex {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r.index != nil {
		return r.index
	}
	idx := &basenameIndex{files: map[string][]string{}}
	entries := 0
	_ = fs.WalkDir(r.r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is left out; the rest is still indexed.
			if d != nil && d.IsDir() && p != "." {
				return fs.SkipDir
			}
			return nil
		}
		if entries++; entries > rs.lim.MaxWalkEntries {
			idx.limited = true
			return fs.SkipAll
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if d.Type().IsRegular() {
			idx.files[d.Name()] = append(idx.files[d.Name()], p)
		}
		return nil
	})
	r.index = idx
	return idx
}

// decide turns the matches of one lookup into a resolution.
func (rs *Roots) decide(hits []hit, buildID, captureID string) Resolution {
	if len(hits) > 1 {
		res := Resolution{Status: StatusAmbiguous, Reason: fmt.Sprintf("%d files match", len(hits))}
		for _, h := range hits {
			res.Candidates = append(res.Candidates, filepath.Join(rs.roots[h.root].dir, filepath.FromSlash(h.rel)))
		}
		slices.Sort(res.Candidates)
		return res
	}
	h := hits[0]
	snap, err := rs.snapshot(h.root, h.rel)
	if err != nil {
		return unresolved("cannot read " + h.rel + ": " + pathless(err).Error())
	}
	rs.noteOpened(snap.info)
	res := Resolution{Root: rs.roots[h.root].dir, Path: h.rel, snap: snap}
	if snap.truncated {
		res.Status = StatusSourceUnverified
		res.Reason = fmt.Sprintf("file larger than the %d-byte source limit; only its start is shown", rs.lim.MaxFileBytes)
		return res
	}
	res.SHA256 = hex.EncodeToString(snap.digest[:])
	rs.evidence(&res, buildID, captureID)
	return res
}

// evidence sets the status from what ties the snapshot to the binary: a
// manifest for the build or capture listing the file, else the HEAD of the
// root's git checkout.
func (rs *Roots) evidence(res *Resolution, buildID, captureID string) {
	differs := false
	for _, m := range rs.manifests {
		if !m.names(buildID, captureID) {
			continue
		}
		want, listed := m.Files[res.Path]
		if !listed {
			continue
		}
		if want == res.SHA256 {
			res.Status, res.VerifiedBy, res.Commit = StatusBuildVerified, "manifest", m.Commit
			return
		}
		differs = true
	}
	res.Status = StatusSourceUnverified
	if differs {
		res.Reason = "file differs from the source manifest"
		return
	}
	if rs.probe == nil {
		res.Reason = "git probe disabled"
		return
	}
	dir := res.Root
	commit, format, err := rs.probe.Head(dir)
	if err != nil {
		res.Reason = gitReason(err)
		return
	}
	// The blob is looked up at the commit Head returned, never at HEAD
	// again, so a HEAD that moves in between cannot pair two commits.
	blob, err := rs.probe.Blob(dir, commit, res.Path)
	if err != nil {
		res.Reason = gitReason(err)
		return
	}
	if want := blobID(format, res.snap.data); want == "" || want != strings.ToLower(blob) {
		res.Reason = "file differs from HEAD " + commit
		return
	}
	res.Status, res.VerifiedBy, res.Commit = StatusUserSelected, "git", commit
}

// pathless is err without the path a PathError carries.
func pathless(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// Lines returns lines [line-context, line+context] of the resolution's
// snapshot, clamped to the file; context is capped at MaxContext. It never
// reads the file again.
func (rs *Roots) Lines(res Resolution, line int64, context int) ([]Line, error) {
	if res.snap == nil {
		return nil, fmt.Errorf("sourceroot: no file to read: the resolution is %s", res.Status)
	}
	if line < 1 {
		return nil, fmt.Errorf("sourceroot: line %d: lines start at 1", line)
	}
	context = min(max(context, 0), rs.lim.MaxContext)
	from, to := max(1, line-int64(context)), line+int64(context)
	var out []Line
	data := res.snap.data
	n := int64(0)
	for len(data) > 0 && n < to {
		n++
		raw, rest, found := bytes.Cut(data, []byte{'\n'})
		data = rest
		if n < from {
			continue
		}
		// A last line cut by the size limit is incomplete.
		cut := !found && res.snap.truncated
		raw = bytes.TrimSuffix(raw, []byte{'\r'})
		text, clipped := clip(raw, rs.lim.MaxLineBytes)
		out = append(out, Line{N: n, Text: text, Truncated: clipped || cut})
	}
	if line > n {
		return nil, fmt.Errorf("sourceroot: line %d is past the end of %s (%d lines)", line, res.Path, n)
	}
	return out, nil
}

// clip is raw as valid UTF-8 (each invalid byte becomes U+FFFD) cut on a
// character boundary to at most limit bytes, and whether anything was cut.
func clip(raw []byte, limit int) (string, bool) {
	if len(raw) <= limit && utf8.Valid(raw) {
		return string(raw), false
	}
	var b strings.Builder
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if b.Len()+utf8.RuneLen(r) > limit {
			return b.String(), true
		}
		b.WriteRune(r)
		i += size
	}
	return b.String(), false
}
