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
	ms, _, err := seriesManifests(ser)
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

// storeHasRoom reports whether a capture directory of used bytes still
// has room for one more capture of the largest size under MaxStoredBytes.
func (s *Server) storeHasRoom(used int64) bool {
	c := s.lim.Capture
	return used+c.MaxProfileBytes+c.MaxExeclogBytes <= c.MaxStoredBytes
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
	// self is the manifest file as it was read, its digest included.
	self fileIdentity
	// written is the capture's start, or its manifest's time when the
	// manifest records none (a failure before the capture started).
	written time.Time
	// gone says this pass evicted it; spared that it holds something
	// retention cannot prove is its own, so this pass leaves it.
	gone, spared bool
}

func (h *held) key() string  { return captureKey(h.series, h.m.ID) }
func (h *held) name() string { return h.series + "/" + h.m.ID + "-" + h.m.Label }

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
// orders, keeping everything p protects: first it finishes any eviction a
// crash or a failure interrupted; then it removes every capture older than
// Capture.TTL (when set); then, per series, the unkept captures past the
// newest Capture.KeepPerSeries, oldest first; then, while the directory
// lacks room for one more capture of the largest size under
// Capture.MaxStoredBytes, the oldest unkept capture, least recently written
// series first. Failed and interrupted captures count like any other. It
// runs under the captures lock and the engine.
//
// A capture's files are removed before its manifest, each only when it is
// still the file the manifest records (device, inode, one link, size and
// SHA-256), and through the quarantine move removeByIdentity makes. A
// capture holding anything else under one of its names keeps it, and its
// manifest, and is noted. The manifest is removed last, by its own
// identity, so an eviction cut short leaves a manifest the next pass
// finishes, and no id is ever handed out again. It returns the captures it
// evicted, "<series>/<NNN>-<label>", each logged.
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

// run is the pass's four steps.
func (r *retention) run(now time.Time) error {
	lim := r.s.lim.Capture
	for _, h := range r.all {
		if r.partial(h) {
			if err := r.evict(h, "finishing an eviction that was cut short"); err != nil {
				return err
			}
		}
	}
	if lim.TTL > 0 {
		for _, h := range r.all {
			if now.Sub(h.written) > lim.TTL {
				if err := r.evict(h, "older than Capture.TTL "+lim.TTL.String()); err != nil {
					return err
				}
			}
		}
	}
	for _, series := range r.seriesOrder(false) {
		var unkept []*held
		for _, h := range r.all {
			if h.series == series && !h.gone && !h.m.Kept {
				unkept = append(unkept, h)
			}
		}
		if len(unkept) <= lim.KeepPerSeries {
			continue
		}
		for _, h := range unkept[:len(unkept)-lim.KeepPerSeries] {
			if err := r.evict(h, fmt.Sprintf("past the newest %d unkept captures of its series (Capture.KeepPerSeries)", lim.KeepPerSeries)); err != nil {
				return err
			}
		}
	}
	used, err := dirBytes(r.d.root)
	if err != nil {
		return err
	}
	for _, series := range r.seriesOrder(true) {
		for _, h := range r.all {
			if r.s.storeHasRoom(used) {
				return nil
			}
			if h.series != series {
				continue
			}
			if err := r.evict(h, "making room under Capture.MaxStoredBytes"); err != nil {
				return err
			}
			if h.gone {
				if used, err = dirBytes(r.d.root); err != nil {
					return err
				}
			}
		}
	}
	return nil
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

// load reads every series' manifests. A manifest this server did not
// write, or cannot read, is never a capture retention removes.
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
		var hs []*held
		for _, n := range names {
			if _, _, kind, ok := parseName(n); !ok || kind != kindManifest {
				continue
			}
			if h, ok := readHeld(ser, name, n); ok {
				hs = append(hs, h)
			}
		}
		slices.SortFunc(hs, func(a, b *held) int {
			ia, _ := parseID(a.m.ID)
			ib, _ := parseID(b.m.ID)
			return ia - ib
		})
		r.all = append(r.all, hs...)
	}
	return nil
}

// readHeld reads the manifest name of series as readManifest does, and
// takes its identity from the bytes read.
func readHeld(ser *os.Root, series, name string) (*held, bool) {
	b, fi, err := readSmall(ser, name, maxManifestBytes)
	if err != nil {
		return nil, false
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil || checkManifest(m, name) != nil || m.Series != series {
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

// partial reports whether an earlier pass began to evict h: a file of it
// is gone (removed, or left under retention's quarantine name).
func (r *retention) partial(h *held) bool {
	return slices.ContainsFunc(h.m.Files, func(f fileIdentity) bool {
		_, err := h.ser.Lstat(f.Name)
		return errors.Is(err, fs.ErrNotExist)
	})
}

// evict removes h, files first and its manifest last, unless it is
// protected or something of it is not provably its own; why says which
// rule evicts it.
func (r *retention) evict(h *held, why string) error {
	if h.gone || h.spared || r.isProtected(h) {
		return nil
	}
	s := r.s
	for _, f := range h.m.Files {
		if err := s.fault(faultRetentionHash); err != nil {
			return err
		}
		if left, err := checkEvictable(h.ser, f); err != nil {
			return err
		} else if left != "" {
			r.spare(h, f.Name, left)
			return nil
		}
	}
	if err := r.finishQuarantined(h); err != nil {
		return err
	}
	tag := h.tag()
	for _, f := range h.m.Files {
		if err := s.wErr("retention remove"); err != nil {
			return err
		}
		if err := s.fault(faultRetentionHash); err != nil {
			return err
		}
		if _, left, err := removeTagged(h.ser, f.Name, f, tag); err != nil {
			return err
		} else if left != "" {
			r.spare(h, f.Name, left)
			return nil
		}
	}
	if len(h.m.Files) > 0 {
		// The files are gone for good before the manifest goes.
		if err := s.syncDir(h.ser, ".", "fsync retention"); err != nil {
			return err
		}
	}
	if err := s.wErr("retention remove"); err != nil {
		return err
	}
	if err := s.fault(faultRetentionHash); err != nil {
		return err
	}
	removed, left, err := removeTagged(h.ser, h.self.Name, h.self, tag)
	switch {
	case err != nil:
		return err
	case left != "":
		r.spare(h, h.self.Name, left)
		return nil
	}
	h.gone = true
	if removed {
		r.evicted = append(r.evicted, h.name())
		s.log.Info("capture evicted", "capture", h.name(), "why", why)
	}
	return s.syncDir(h.ser, ".", "fsync retention")
}

// checkEvictable says why the file f records may not be removed, "" when
// it may or is gone already: it must still be the recorded file, with no
// name but the capture's.
func checkEvictable(ser *os.Root, f fileIdentity) (string, error) {
	fi, err := ser.Lstat(f.Name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	}
	if st, _ := statOf(fi); fi.Mode().IsRegular() && st.nlink != 1 {
		return fmt.Sprintf("%d links: another name shares the file, so it is not the capture's alone", st.nlink), nil
	}
	why, err := matches(ser, f.Name, f)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return why, err
}

// finishQuarantined removes the names of h's files an interrupted pass
// left under retention's quarantine name, each only when it is a file h's
// manifest records. Anything else there is left, for recovery to report.
func (r *retention) finishQuarantined(h *held) error {
	if len(h.m.Files) == 0 {
		return nil
	}
	tag := h.tag()
	for _, n := range r.names[h.series] {
		if !strings.HasPrefix(n, quarantinePrefix+tag) {
			continue
		}
		for _, f := range h.m.Files {
			id := f
			id.Name = n
			if err := r.s.fault(faultRetentionHash); err != nil {
				return err
			}
			if why, err := matches(h.ser, n, id); err != nil || why != "" {
				continue
			}
			if err := r.s.wErr("retention remove"); err != nil {
				return err
			}
			if _, _, err := removeTagged(h.ser, n, id, tag); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// spare leaves h for this pass, noting the name that is not provably its
// own.
func (r *retention) spare(h *held, name, why string) {
	h.spared = true
	note := fmt.Sprintf("left %s/%s: %s; capture %s keeps it and its manifest, since retention removes only files it can prove are the ones the manifest records",
		h.series, name, why, h.name())
	r.s.log.Warn("capture retention left a file in place", "note", note)
}
