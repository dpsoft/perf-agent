package perfmcp

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// fileKey is a file by its device and inode.
type fileKey struct{ Dev, Ino uint64 }

// protection is what one retention pass must keep (§7.3): files by device
// and inode, and captures by captureKey. err says protected could not read
// all it protects; retention then removes nothing.
type protection struct {
	files    map[fileKey]bool
	captures map[string]bool
	err      error
}

// captureKey names a capture as the wire does: "<series>/<NNN>".
func captureKey(series, id string) string { return series + "/" + id }

// protected is what retention must keep, beside every kept capture and
// anything it cannot prove is a capture's own: the files open in the
// store, the files the captureProtect hook names, the captures this
// server's stranded rollbacks and every intent name, baseline (the call's
// own, a captureKey, or ""), and each series' newest compare_with. It runs
// under the engine, which the store belongs to. It never reads the live
// page: a publication holds its profile in memory, so removing its files
// loses nothing it shows; serve protects them through captureProtect.
func (s *Server) protected(d *captureDir, baseline string) protection {
	p := protection{files: map[fileKey]bool{}, captures: map[string]bool{}}
	if baseline != "" {
		p.captures[baseline] = true
	}
	if s.store != nil {
		for _, e := range s.store.entries {
			for _, fi := range e.opened {
				if st, ok := statOf(fi); ok {
					p.files[fileKey{st.dev, st.ino}] = true
				}
			}
		}
	}
	if s.captureProtect != nil {
		for _, k := range s.captureProtect() {
			p.files[k] = true
		}
	}
	s.capMu.Lock()
	for _, t := range s.stranded {
		p.captures[captureKey(t.series, formatID(t.id))] = true
	}
	s.capMu.Unlock()
	series, err := seriesDirs(d)
	if err != nil {
		p.err = err
		return p
	}
	for _, name := range series {
		p.err = errors.Join(p.err, p.addSeries(d, name))
	}
	return p
}

// addSeries protects what series holds: the capture of each intent, each
// kept capture, and the baseline the newest capture comparing with one
// named.
func (p *protection) addSeries(d *captureDir, series string) error {
	ser, err := d.openSeries(series)
	if err != nil {
		return err
	}
	defer func() { _ = ser.Close() }()
	names, err := readDirNames(ser, ".")
	if err != nil {
		return err
	}
	for _, n := range names {
		if id, _, kind, ok := parseName(n); ok && kind == kindIntent {
			p.captures[captureKey(series, formatID(id))] = true
		}
	}
	ms, err := protectedManifests(ser, names)
	if err != nil {
		return err
	}
	newest := ""
	for _, m := range ms {
		if m.Kept {
			p.captures[captureKey(series, m.ID)] = true
		}
		newest = cmp.Or(m.CompareWith, newest)
	}
	if newest != "" {
		p.captures[captureKey(series, newest)] = true
	}
	return nil
}

// protectedManifests reads the manifests among names, in id order. A
// manifest this server did not write is passed over; one that cannot be
// read (an I/O error, a permission) is an error, since it may name a
// baseline or be kept.
func protectedManifests(ser *os.Root, names []string) ([]manifest, error) {
	var ms []manifest
	for _, n := range names {
		if _, _, kind, ok := parseName(n); !ok || kind != kindManifest {
			continue
		}
		b, _, err := readSmall(ser, n, maxManifestBytes)
		if _, isIO := errors.AsType[*fs.PathError](err); isIO && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("manifest %s: %w", n, err)
		}
		var m manifest
		if err != nil || json.Unmarshal(b, &m) != nil || checkManifest(m, n) != nil {
			continue
		}
		ms = append(ms, m)
	}
	slices.SortFunc(ms, func(a, b manifest) int {
		ia, _ := parseID(a.ID)
		ib, _ := parseID(b.ID)
		return ia - ib
	})
	return ms, nil
}

// seriesDirs lists the series directories of the capture directory.
func seriesDirs(d *captureDir) ([]string, error) {
	names, err := readDirNames(d.root, ".")
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, n := range names {
		if fi, err := d.root.Lstat(n); err == nil && fi.IsDir() && validSeries(n) {
			dirs = append(dirs, n)
		}
	}
	slices.Sort(dirs)
	return dirs, nil
}

// storeHasRoom reports whether a capture directory of used bytes has room,
// under Capture.MaxStoredBytes, for a capture that may write need bytes
// (CaptureLimits.captureNeed).
func (s *Server) storeHasRoom(used, need int64) bool {
	return used+need <= s.lim.Capture.MaxStoredBytes
}

// dirBytes is the size of the regular files under root, each file counted
// once however many names it has. Symbolic links are not followed.
func dirBytes(root *os.Root) (int64, error) {
	seen := map[fileKey]bool{}
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(_ string, de fs.DirEntry, err error) error {
		if err != nil || !de.Type().IsRegular() {
			// A name removed while it is walked holds nothing.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		fi, err := de.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if st, ok := statOf(fi); ok {
			k := fileKey{st.dev, st.ino}
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		total += fi.Size()
		return nil
	})
	return total, err
}

// held is a committed capture (its manifest, and its files when it is
// ok) as one retention pass sees it.
type held struct {
	series string
	ser    *os.Root
	m      manifest
	// self is the manifest file as it was read, its digest included:
	// under its own name, or under the eviction marker's when the
	// manifest's name is gone (viaMarker).
	self fileIdentity
	// written is the capture's start, or its manifest's time when the
	// manifest records none (a failure before the capture started).
	written time.Time
	// marked says the eviction marker is there, a second name of the
	// manifest: an earlier pass began to evict the capture, and this one
	// finishes it. viaMarker says the manifest's own name is gone already
	// and the marker is all of it that is left. blocked says why the
	// marker's name holds something else.
	marked, viaMarker bool
	blocked           string
	// gone says this pass evicted it; spared that it holds something
	// retention cannot prove is its own, so this pass leaves it.
	gone, spared bool
}

func (h *held) key() string      { return captureKey(h.series, h.m.ID) }
func (h *held) name() string     { return h.series + "/" + h.m.ID + "-" + h.m.Label }
func (h *held) failure() bool    { return h.m.Outcome != captureOK }
func (h *held) id() int          { id, _ := parseID(h.m.ID); return id }
func (h *held) marker() string   { return evictingName(h.id(), h.m.Label) }
func (h *held) manifest() string { return manifestName(h.id(), h.m.Label) }

// tag is the quarantine tag of retention's removals of h: the capture's
// token (so recovery's report names the capture) and "r-", which no
// removal of the capture's own transaction carries.
func (h *held) tag() string {
	if len(h.m.Token) >= 16 {
		return h.m.Token[:16] + "-r-"
	}
	return "r-"
}

// retention is one pass.
type retention struct {
	s *Server
	d *captureDir
	p protection
	// all are the captures in series and id order; names each series'
	// entries as the pass found them.
	all     []*held
	names   map[string][]string
	roots   []*os.Root
	evicted []string
}

// retain removes committed captures from the capture directory as §7.3
// orders, keeping everything p protects:
//  1. it puts back under their names the files an interrupted pass left
//     under quarantine names, then finishes every eviction an earlier
//     pass began, which the eviction marker shows;
//  2. when Capture.TTL is set, it removes every capture older than that
//     (by its start), unless the clock reads earlier than the newest
//     capture's start;
//  3. per series, it removes the unkept captures past the newest
//     Capture.KeepPerSeries, oldest first; failed and interrupted
//     captures have a budget of their own, of the same size;
//  4. while the directory lacks room for the largest capture under
//     Capture.MaxStoredBytes, it removes the oldest unkept capture, least
//     recently written series first; but only when removing every capture
//     it may remove would make that room.
//
// Before it removes anything of a capture with files, it links the
// eviction marker, NNN-label.evicting, to the manifest, so a pass that is
// cut short is finished by the next one, and a capture missing a file
// with no marker (a user moved or deleted it) is kept and noted. A
// capture's files are removed before its manifest, and the marker last;
// each only when it is still the file the manifest records (device, inode,
// one link, size and SHA-256), moved aside and checked first: if any is
// not, every file is put back, and the capture is kept and noted. What an
// interrupted pass left under quarantine names is put back first. So no id
// is ever handed out again: the manifest, or its marker, outlives the
// files. It runs under the captures lock and the engine, and returns the
// captures it evicted, "<series>/<NNN>-<label>", each logged.
func (s *Server) retain(d *captureDir, p protection, now time.Time) (evicted []string, err error) {
	if p.err != nil {
		return nil, capture.Errorf(capture.CodeDisk, "", "retention removed nothing in the capture directory %s inside the profiles root %s: it could not read what it must keep: %v",
			d.rel, d.profiles, pathless(p.err))
	}
	r := &retention{s: s, d: d, p: p, names: map[string][]string{}}
	defer r.close()
	if err := r.load(); err != nil {
		return nil, r.fail(err)
	}
	if err := r.run(now); err != nil {
		return r.evicted, r.fail(err)
	}
	return r.evicted, nil
}

// run is the pass's four steps, after it puts back what an interrupted
// pass left aside.
func (r *retention) run(now time.Time) error {
	for _, h := range r.all {
		if err := r.unquarantine(h); err != nil {
			return err
		}
	}
	for _, h := range r.all {
		if h.marked || h.viaMarker {
			if err := r.evict(h, "finishing an eviction that was cut short"); err != nil {
				return err
			}
		}
	}
	if err := r.byAge(now); err != nil {
		return err
	}
	if err := r.byCount(); err != nil {
		return err
	}
	return r.bySize()
}

// byAge is the TTL step.
func (r *retention) byAge(now time.Time) error {
	ttl := r.s.lim.Capture.TTL
	if ttl <= 0 {
		return nil
	}
	var newest time.Time
	for _, h := range r.all {
		if !h.gone && h.m.Start.After(newest) {
			newest = h.m.Start
		}
	}
	if now.Before(newest) {
		r.note(fmt.Sprintf("the clock reads %s, earlier than the newest capture's start %s; the TTL step is skipped this pass",
			now.UTC().Format(time.RFC3339), newest.UTC().Format(time.RFC3339)))
		return nil
	}
	old := func(h *held) bool { return !h.gone && now.Sub(h.written) > ttl && !r.isProtected(h) }
	counts := map[string]int{}
	for _, h := range r.all {
		if old(h) {
			counts[h.series]++
		}
	}
	for _, series := range r.seriesOrder(false) {
		if n := counts[series]; n > r.s.lim.Capture.KeepPerSeries {
			r.s.log.Warn("capture retention removes many captures by age at once",
				"series", series, "count", n, "ttl", ttl.String(), "keep_per_series", r.s.lim.Capture.KeepPerSeries)
		}
	}
	for _, h := range r.all {
		if old(h) {
			if err := r.evict(h, "older than Capture.TTL "+ttl.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

// byCount is the keep-count step: per series, the newest KeepPerSeries
// unkept captures stay, and so do the newest KeepPerSeries failure
// manifests, counted apart, so a run of failures never pushes out a
// capture that succeeded.
func (r *retention) byCount() error {
	keep := r.s.lim.Capture.KeepPerSeries
	for _, series := range r.seriesOrder(false) {
		for _, failures := range []bool{false, true} {
			var unkept []*held
			for _, h := range r.all {
				if h.series == series && !h.gone && !h.m.Kept && h.failure() == failures {
					unkept = append(unkept, h)
				}
			}
			if len(unkept) <= keep {
				continue
			}
			what := "unkept captures"
			if failures {
				what = "failed or interrupted captures"
			}
			for _, h := range unkept[:len(unkept)-keep] {
				if err := r.evict(h, fmt.Sprintf("past the newest %d %s of its series (Capture.KeepPerSeries)", keep, what)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// bySize is the stored-bytes step. It measures the directory once and
// counts down what each eviction frees. When removing every capture it
// may remove would still leave no room, or the directory cannot be
// measured, it removes nothing: the capture that needs the room then
// fails with disk, and no capture is removed for nothing.
func (r *retention) bySize() error {
	need := r.s.lim.Capture.largestCapture()
	used, err := dirBytes(r.d.root)
	if err != nil {
		r.note("the stored-bytes step removes nothing: it cannot measure the capture directory: " + pathless(err).Error())
		return nil
	}
	if r.s.storeHasRoom(used, need) {
		return nil
	}
	var cands []*held
	var freeable int64
	for _, series := range r.seriesOrder(true) {
		for _, h := range r.all {
			if h.series == series && r.evictable(h) {
				cands = append(cands, h)
				freeable += r.size(h)
			}
		}
	}
	if !r.s.storeHasRoom(used-freeable, need) {
		r.note(fmt.Sprintf("the capture directory holds %d bytes; a capture may need %d more, under Capture.MaxStoredBytes %d, and removing every capture retention may remove would free only %d, so it removes none by size",
			used, need, r.s.lim.Capture.MaxStoredBytes, freeable))
		return nil
	}
	for _, h := range cands {
		if r.s.storeHasRoom(used, need) {
			return nil
		}
		size := r.size(h)
		if err := r.evict(h, "making room under Capture.MaxStoredBytes"); err != nil {
			return err
		}
		if h.gone {
			used -= size
		}
	}
	return nil
}

// evictable reports whether evict would try to remove h.
func (r *retention) evictable(h *held) bool {
	return !h.gone && !h.spared && !h.m.Kept && !r.isProtected(h) && h.blocked == "" &&
		(h.marked || h.viaMarker || r.missing(h) == "")
}

// size is what removing h frees: its files still there, and its manifest.
func (r *retention) size(h *held) int64 {
	n := h.self.Size
	for _, f := range h.m.Files {
		if fi, err := h.ser.Lstat(f.Name); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// missing is the first file of h that is not there, "" when none is.
func (r *retention) missing(h *held) string {
	for _, f := range h.m.Files {
		if _, err := h.ser.Lstat(f.Name); errors.Is(err, fs.ErrNotExist) {
			return f.Name
		}
	}
	return ""
}

// seriesOrder lists the series in name order or, by recency, least
// recently written first (by the newest capture each holds still).
func (r *retention) seriesOrder(byRecency bool) []string {
	latest := map[string]time.Time{}
	var order []string
	for _, h := range r.all {
		if _, ok := latest[h.series]; !ok {
			order = append(order, h.series)
		}
		if !h.gone && h.written.After(latest[h.series]) {
			latest[h.series] = h.written
		}
	}
	if byRecency {
		slices.SortStableFunc(order, func(a, b string) int { return latest[a].Compare(latest[b]) })
	}
	return order
}

// load reads every series' manifests and eviction markers. A manifest
// this server did not write, or cannot read, is never a capture retention
// removes.
func (r *retention) load() error {
	series, err := seriesDirs(r.d)
	if err != nil {
		return err
	}
	for _, name := range series {
		ser, err := r.d.openSeries(name)
		if err != nil {
			continue
		}
		r.roots = append(r.roots, ser)
		names, err := readDirNames(ser, ".")
		if err != nil {
			return err
		}
		slices.Sort(names)
		r.names[name] = names
		byName := map[string]*held{}
		var hs []*held
		for _, n := range names {
			if _, _, kind, ok := parseName(n); !ok || kind != kindManifest {
				continue
			}
			if h, ok := readHeld(ser, name, n, n); ok {
				hs = append(hs, h)
				byName[n] = h
			}
		}
		for _, n := range names {
			id, label, kind, ok := parseName(n)
			if !ok || kind != kindEvicting {
				continue
			}
			if h := byName[manifestName(id, label)]; h != nil {
				r.markedBy(h, n)
				continue
			}
			// The manifest's name is gone: the marker is the manifest
			// itself, read as one, and the rest of the eviction is
			// finished from it.
			if h, ok := readHeld(ser, name, n, manifestName(id, label)); ok && !h.failure() {
				h.viaMarker = true
				hs = append(hs, h)
			}
		}
		slices.SortFunc(hs, func(a, b *held) int { return a.id() - b.id() })
		r.all = append(r.all, hs...)
	}
	return nil
}

// markedBy records what the marker name n beside h's manifest is: the
// manifest's second name (the eviction began), or something else.
func (r *retention) markedBy(h *held, n string) {
	mfi, merr := h.ser.Lstat(h.self.Name)
	fi, err := h.ser.Lstat(n)
	switch {
	case err != nil || merr != nil:
		h.blocked = "the eviction marker could not be read"
	case fi.Mode().IsRegular() && os.SameFile(fi, mfi):
		h.marked = true
	default:
		h.blocked = "the eviction marker's name holds a file retention did not link there"
	}
}

// readHeld reads name in series as the manifest as (its own name, or the
// manifest name an eviction marker stands for), as readManifest checks it,
// and takes its identity from the bytes read.
func readHeld(ser *os.Root, series, name, as string) (*held, bool) {
	b, fi, err := readSmall(ser, name, maxManifestBytes)
	if err != nil {
		return nil, false
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil || checkManifest(m, as) != nil || m.Series != series {
		return nil, false
	}
	self := identityOf(name, fi)
	sum := sha256.Sum256(b)
	self.SHA256 = hex.EncodeToString(sum[:])
	written := m.Start
	if written.IsZero() {
		written = fi.ModTime()
	}
	return &held{series: series, ser: ser, m: m, self: self, written: written}, true
}

func (r *retention) close() {
	for _, ser := range r.roots {
		_ = ser.Close()
	}
}

// fail is err as retention's failure: a simulated crash as it is.
func (r *retention) fail(err error) error {
	if errors.Is(err, errCrash) {
		return err
	}
	if _, ok := errors.AsType[*capture.Error](err); ok {
		return err
	}
	return capture.Errorf(capture.CodeDisk,
		"free space there, or start perf-agent-mcp with --captures naming another directory inside the profiles root",
		"retention stopped in the capture directory %s inside the profiles root %s: %v; the next pass goes on from there", r.d.rel, r.d.profiles, pathless(err))
}

// isProtected reports whether p keeps h (a kept capture among them).
func (r *retention) isProtected(h *held) bool {
	if r.p.captures[h.key()] || r.p.files[fileKey{h.self.Dev, h.self.Ino}] {
		return true
	}
	return slices.ContainsFunc(h.m.Files, func(f fileIdentity) bool { return r.p.files[fileKey{f.Dev, f.Ino}] })
}

// evict removes h: the marker first, then its files, its manifest and the
// marker last. It does nothing for a protected or kept capture, and keeps,
// noting why, a capture that is not wholly its own: a file replaced or
// linked elsewhere, or missing with no marker to show retention removed
// it. why says which rule evicts it.
func (r *retention) evict(h *held, why string) error {
	if h.gone || h.spared || h.m.Kept || r.isProtected(h) {
		return nil
	}
	if h.blocked != "" {
		r.spare(h, h.marker(), h.blocked)
		return nil
	}
	if !h.marked && !h.viaMarker {
		if f := r.missing(h); f != "" {
			r.spare(h, f, "incomplete: "+f+" is missing, and nothing shows retention began to remove the capture")
			return nil
		}
	}
	// By their stat first: a capture that is plainly not its own is kept
	// before anything is written.
	for _, f := range h.m.Files {
		fi, err := h.ser.Lstat(f.Name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return err
		}
		if why := cmp.Or(statWhy(fi, f), linksWhy(fi, 1)); why != "" {
			r.spare(h, f.Name, why)
			return nil
		}
	}
	if len(h.m.Files) > 0 && !h.marked && !h.viaMarker {
		if err := r.mark(h); err != nil || h.spared {
			return err
		}
	}
	if spared, err := r.removeFiles(h); err != nil {
		return err
	} else if spared {
		r.unmark(h)
		return nil
	}
	s := r.s
	if len(h.m.Files) > 0 {
		// The files are gone for good before the manifest goes.
		if err := s.syncDir(h.ser, ".", "fsync retention"); err != nil {
			return err
		}
	}
	if !h.viaMarker {
		links := uint64(1)
		if h.marked {
			links = 2
		}
		if err := s.wErr("retention remove"); err != nil {
			return err
		}
		if err := s.fault(faultRetentionHash); err != nil {
			return err
		}
		_, left, err := removeLinked(h.ser, h.self.Name, h.self, h.tag(), links)
		switch {
		case err != nil:
			return err
		case left != "":
			r.spare(h, h.self.Name, left)
			return nil
		}
		if err := s.syncDir(h.ser, ".", "fsync retention"); err != nil {
			return err
		}
	}
	h.gone = true
	r.evicted = append(r.evicted, h.name())
	s.log.Info("capture evicted", "capture", h.name(), "why", why)
	if !h.marked && !h.viaMarker {
		return nil
	}
	// The marker is the manifest's last name now.
	marker := h.self
	marker.Name = h.marker()
	if err := s.wErr("retention remove"); err != nil {
		return err
	}
	if err := s.fault(faultRetentionHash); err != nil {
		return err
	}
	if _, left, err := removeLinked(h.ser, marker.Name, marker, h.tag(), 1); err != nil {
		return err
	} else if left != "" {
		r.note(fmt.Sprintf("left %s/%s: %s; capture %s is evicted, and this file is kept", h.series, marker.Name, left, h.name()))
	}
	return s.syncDir(h.ser, ".", "fsync retention")
}

// mark links the eviction marker to h's manifest, without replacing
// anything, and makes it durable: from here on the eviction is finished
// even when the pass is cut short. A name taken by another file keeps the
// capture.
func (r *retention) mark(h *held) error {
	s := r.s
	if err := s.wErr("retention mark"); err != nil {
		return err
	}
	err := s.linkNoReplace(h.ser, rootCaptures, h.self.Name, h.marker())
	if err != nil && !errors.Is(err, errCrash) && landed(h.ser, h.marker(), h.self) == linkMade {
		err = nil
	}
	switch {
	case errors.Is(err, fs.ErrExist):
		r.spare(h, h.marker(), "the eviction marker's name holds a file retention did not link there")
		return nil
	case err != nil:
		return err
	}
	h.marked = true
	return s.syncDir(h.ser, ".", "fsync retention")
}

// unmark removes the marker of a capture whose files are all back under
// their names after it was spared: nothing of it was removed. When a file
// is missing, the marker stays, and the capture is still one whose
// eviction began.
func (r *retention) unmark(h *held) {
	if !h.marked || r.missing(h) != "" {
		return
	}
	marker := h.self
	marker.Name = h.marker()
	removed, left, err := removeLinked(h.ser, marker.Name, marker, h.tag(), 2)
	if err != nil || left != "" {
		r.note(fmt.Sprintf("left %s/%s: the eviction marker of capture %s, which retention kept, stays: %s", h.series, marker.Name, h.name(), cmpOrErr(left, err)))
		return
	}
	if removed {
		h.marked = false
		_ = r.s.syncDir(h.ser, ".", "fsync retention")
	}
}

// aside is a capture file removeFiles moved to its quarantine name.
type aside struct {
	want fileIdentity
	q    string
	f    *os.File
	fi   fs.FileInfo
}

// removeFiles removes h's files: it moves each aside, then checks each
// there (its stat and its SHA-256, once), then unlinks them. If any is not
// the file the manifest records, every one moved is put back under its
// name, and the capture is spared. A crash leaves the moved files under
// quarantine names, which the next pass puts back (unquarantine).
func (r *retention) removeFiles(h *held) (spared bool, err error) {
	s, tag := r.s, h.tag()
	var moved []*aside
	defer func() {
		for _, a := range moved {
			if a.f != nil {
				_ = a.f.Close()
			}
		}
	}()
	undo := func() string {
		var why []string
		for _, a := range moved {
			if a.q != "" {
				if w := restore(h.ser, a.q, a.want.Name); w != "" {
					why = append(why, a.want.Name+w)
				}
				a.q = ""
			}
		}
		return strings.Join(why, "")
	}
	fail := func(err error) (bool, error) {
		if errors.Is(err, errCrash) {
			return false, err
		}
		if w := undo(); w != "" {
			err = fmt.Errorf("%w; %s", err, w)
		}
		return false, err
	}
	spare := func(name, why string) (bool, error) {
		if w := undo(); w != "" {
			why += "; " + w
		}
		r.spare(h, name, why)
		return true, nil
	}
	for _, f := range h.m.Files {
		if err := s.wErr("retention remove"); err != nil {
			return fail(err)
		}
		q, left, err := moveAside(h.ser, f.Name, f, tag, 1)
		switch {
		case err != nil:
			return fail(err)
		case left != "":
			return spare(f.Name, left)
		case q != "":
			moved = append(moved, &aside{want: f, q: q})
		}
	}
	for _, a := range moved {
		if err := s.fault(faultRetentionHash); err != nil {
			return fail(err)
		}
		f, fi, left, err := checkAside(h.ser, a.q, a.want.Name, a.want, 1)
		if f == nil {
			// checkAside put this one back itself.
			a.q = ""
			if err != nil {
				return fail(err)
			}
			return spare(a.want.Name, left)
		}
		a.f, a.fi = f, fi
	}
	for _, a := range moved {
		_, left, err := unlinkAside(h.ser, a.q, a.want.Name, a.fi)
		q := a.q
		a.q = ""
		switch {
		case err != nil:
			return fail(err)
		case left != "":
			return spare(q, left)
		}
	}
	return false, nil
}

// unquarantine undoes what an interrupted pass of retention left under
// its quarantine names for h, before h is judged again: a name that is a
// second name of a file h still holds under its own name (a file put back
// before a crash removed its quarantine name) is dropped, which loses
// nothing; any other file with the device and inode of one of h's files
// (or of its manifest) goes back under that file's name when the name is
// free. Nothing is deleted on its contents' account here: the files back
// under their names are judged like any other.
func (r *retention) unquarantine(h *held) error {
	type cand struct {
		id    fileIdentity
		names []string
	}
	cands := []cand{{h.self, []string{h.manifest(), h.marker()}}}
	for _, f := range h.m.Files {
		cands = append(cands, cand{f, []string{f.Name}})
	}
	for _, n := range r.names[h.series] {
		if !strings.HasPrefix(n, quarantinePrefix+h.tag()) {
			continue
		}
		qfi, err := h.ser.Lstat(n)
		if err != nil || !qfi.Mode().IsRegular() {
			continue
		}
		st, _ := statOf(qfi)
		i := slices.IndexFunc(cands, func(c cand) bool { return st.dev == c.id.Dev && st.ino == c.id.Ino })
		if i < 0 {
			continue
		}
		c := cands[i]
		held := slices.ContainsFunc(c.names, func(name string) bool {
			fi, err := h.ser.Lstat(name)
			return err == nil && os.SameFile(fi, qfi)
		})
		if held {
			if err := h.ser.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if why := restore(h.ser, n, c.names[0]); why != "" {
			r.note(fmt.Sprintf("left %s/%s: retention moved it aside from %s before a crash%s", h.series, n, c.names[0], why))
		}
	}
	return nil
}

// spare leaves h for this pass, noting the name that is not provably its
// own.
func (r *retention) spare(h *held, name, why string) {
	h.spared = true
	r.note(fmt.Sprintf("left %s/%s: %s; capture %s keeps it and its manifest, since retention removes only files it can prove are the ones the manifest records",
		h.series, name, why, h.name()))
}

// note logs what the pass left, or did not do, and why.
func (r *retention) note(n string) {
	r.s.log.Warn("capture retention", "note", n)
}
