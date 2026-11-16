package perfmcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// recoverCaptures finishes or undoes every capture a crash left in the
// capture directory (§7.4), under the captures lock: for each intent,
// a committed one's leftovers are removed by identity; one whose owner is
// alive is left to it; an abandoned one is undone by identity and gets an
// interrupted manifest. What no intent accounts for (a staging directory,
// a temporary) is never removed: it is noted, and logged. notes says what
// recovery did and left, one line each, starting with its kind
// (finished, interrupted, left, orphan_staging, orphan_temp). An error
// (ENOSPC, say) stops recovery where it is; every step is idempotent and
// the next pass goes on from there.
func (s *Server) recoverCaptures(d *captureDir) (notes []string, err error) {
	names, err := readDirNames(d.root, ".")
	if err != nil {
		return nil, d.writeError(err)
	}
	slices.Sort(names)
	for _, n := range names {
		if !validSeries(n) {
			// A temporary the capture directory's own writes left (.owner's)
			// is reported like a series' and stays.
			if strings.HasPrefix(n, ".") && n != ownerFile && n != lockFile {
				note := fmt.Sprintf("orphan_temp %s: a file this server cannot prove it made, so it is never removed automatically; remove it by hand", n)
				s.log.Warn("capture recovery", "note", note)
				notes = append(notes, note)
			}
			continue
		}
		if fi, err := d.root.Lstat(n); err != nil || !fi.IsDir() {
			continue
		}
		ns, err := s.recoverSeries(d, n)
		notes = append(notes, ns...)
		if err != nil {
			return notes, err
		}
	}
	return notes, nil
}

// ownerState is what recovery makes of an intent's owner.
type ownerState int

const (
	ownerGone ownerState = iota
	ownerLive
	ownerUnclear
)

// ownerState judges t's owner: alive when its heartbeat is fresh and its
// server or reaper still runs, gone when neither holds, and unclear (left
// for a later pass) when the two disagree.
func (t *captureTxn) ownerState() ownerState {
	fresh := false
	if hb := t.in.Heartbeat; hb != nil {
		if fi, err := t.ser.Lstat(hb.Name); err == nil {
			st, _ := statOf(fi)
			// A heartbeat dated more than the bound ahead (a clock that
			// jumped back) is as stale as one that old.
			age := t.s.clock().Sub(fi.ModTime())
			fresh = st.dev == hb.Dev && st.ino == hb.Ino && age < staleAfter && age > -staleAfter
		}
	}
	alive := t.s.ownerIsAlive(t.in)
	switch {
	case fresh && alive:
		return ownerLive
	case !fresh && !alive:
		return ownerGone
	}
	return ownerUnclear
}

// recoverSeries recovers one series.
func (s *Server) recoverSeries(d *captureDir, series string) (notes []string, err error) {
	ser, err := d.openSeries(series)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ser.Close() }()
	add := func(kind, name, why string) {
		n := fmt.Sprintf("%s %s/%s: %s", kind, series, name, why)
		s.log.Warn("capture recovery", "note", n)
		notes = append(notes, n)
	}
	names, err := readDirNames(ser, ".")
	if err != nil {
		return nil, d.writeError(err)
	}
	slices.Sort(names)
	// claimed are the names a live intent or an unreadable one accounts
	// for; they are not reported as orphans.
	claimed := map[string]bool{}
	claim := func(in intent, name string) {
		claimed[name] = true
		claimed[in.Staging] = true
		if in.Heartbeat != nil {
			claimed[in.Heartbeat.Name] = true
		}
		if in.Token == "" {
			return
		}
		for _, n := range names {
			if strings.Contains(n, ".tmp-"+in.Token[:16]) {
				claimed[n] = true
			}
		}
	}
	for _, n := range names {
		if _, _, kind, ok := parseName(n); !ok || kind != kindIntent {
			continue
		}
		in, self, err := readIntent(ser, n)
		if err == nil && in.Series != series {
			err = fmt.Errorf("intent %s names series %s", n, echo(in.Series))
		}
		if err != nil {
			claimed[n] = true
			add("left", n, "an intent this server cannot act on ("+err.Error()+")")
			continue
		}
		t := s.adopt(d, ser, series, in, self)
		// An intent temporary is proven by the intent's own identity (a
		// second name of it, after a crash between linking the intent and
		// removing that name), or by holding an intent with this token: a
		// crash before a rewrite recorded it. Either holds only bytes this
		// server wrote.
		for _, tn := range names {
			if !in.isTemp(t.id, tn) || !strings.HasPrefix(tn, "."+n) {
				continue
			}
			alias := self
			alias.Name = tn
			if why, _ := matches(ser, tn, alias); why != "" {
				var ok bool
				if alias, ok = proveByToken(ser, tn, in, maxManifestBytes, in.holdsToken); !ok {
					continue
				}
			}
			if removed, _, err := removeByIdentity(ser, tn, alias); err != nil {
				return notes, d.writeError(err)
			} else if removed {
				claimed[tn] = true
			}
		}
		// A manifest temporary no rewrite recorded yet carries the token in
		// its name and holds this capture's manifest.
		for _, tn := range names {
			if !in.isTemp(t.id, tn) || !strings.HasPrefix(tn, "."+manifestName(t.id, in.Label)) || slices.ContainsFunc(in.Temps, func(f fileIdentity) bool { return f.Name == tn }) {
				continue
			}
			if id, ok := proveByToken(ser, tn, in, maxManifestBytes, in.holdsManifest); ok {
				if removed, _, err := t.remove(ser, tn, id); err != nil {
					return notes, d.writeError(err)
				} else if removed {
					claimed[tn] = true
				}
			}
		}
		// A heartbeat temporary holds the token: a second name of the
		// heartbeat, or one a crash left before it was linked.
		for _, tn := range names {
			if !in.isTemp(t.id, tn) || !strings.HasPrefix(tn, "."+heartbeatName(t.id, in.Label)) {
				continue
			}
			if id, ok := proveByToken(ser, tn, in, int64(len(in.Token)+1), func(b []byte) bool { return string(b) == in.Token+"\n" }); ok {
				if removed, _, err := t.remove(ser, tn, id); err != nil {
					return notes, d.writeError(err)
				} else if removed {
					claimed[tn] = true
				}
			}
		}
		// A heartbeat the intent does not record yet (a crash before the
		// rewrite that records it) is proven by holding the intent's token.
		hbName := heartbeatName(t.id, in.Label)
		if hb := in.Heartbeat; hb == nil || !t.isRecorded(*hb) {
			if id, ok := proveByToken(ser, hbName, in, int64(len(in.Token)+1), func(b []byte) bool {
				return string(b) == in.Token+"\n"
			}); ok {
				t.in.Heartbeat = &id
			}
		}
		// A name this capture's removal moved aside, when a crash came
		// before its unlink: proven against what the intent records.
		for _, tn := range names {
			if !strings.HasPrefix(tn, quarantinePrefix+t.tag()) {
				continue
			}
			fi, err := ser.Lstat(tn)
			if err != nil {
				continue
			}
			if fi.IsDir() {
				if st, _ := statOf(fi); in.StagingDev != 0 && st.dev == in.StagingDev && st.ino == in.StagingIno {
					if err := t.removeStagingDir(tn); err != nil {
						return notes, d.writeError(err)
					}
				}
			} else if id, ok := t.proveQuarantined(ser, tn); ok {
				if _, _, err := t.remove(ser, tn, id); err != nil {
					return notes, d.writeError(err)
				}
			}
			if _, err := ser.Lstat(tn); errors.Is(err, fs.ErrNotExist) {
				claimed[tn] = true
			}
		}
		which := formatID(t.id) + "-" + in.Label
		mname := manifestName(t.id, in.Label)
		if _, err := ser.Lstat(mname); err == nil {
			m, err := readManifest(ser, mname)
			if err != nil {
				claim(in, n)
				add("left", n, "its manifest is not one this server wrote ("+err.Error()+")")
				continue
			}
			err = t.finishCommitted(m.Outcome == captureOK)
			notes = append(notes, t.notes...)
			if err != nil {
				return notes, err
			}
			add("finished", which, "its manifest ("+m.Outcome+") was written; recovery removed what it left")
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return notes, d.writeError(err)
		}
		switch t.ownerState() {
		case ownerLive:
			claim(in, n)
			add("left", n, fmt.Sprintf("its owner (server pid %d) is alive and finishes it", in.ServerPID))
			continue
		case ownerUnclear:
			claim(in, n)
			add("left", n, "its heartbeat and its owner's process disagree; the next recovery judges it again")
			continue
		}
		err = t.unwind(t.manifestOf(manifest{Notes: []string{interruptedNote}}, captureInterrupted, nil), faultInRecovery)
		notes = append(notes, t.notes...)
		if err != nil {
			return notes, err
		}
		add("interrupted", which, "its server ended before it finished; its files were removed and an interrupted manifest written")
	}

	names, err = readDirNames(ser, ".")
	if err != nil {
		return notes, d.writeError(err)
	}
	slices.Sort(names)
	for _, n := range names {
		_, _, kind, ok := parseName(n)
		switch {
		case claimed[n], n == seqFile:
		case strings.HasPrefix(n, stagingPrefix):
			add("orphan_staging", n, "no intent identifies this staging directory, so it is never removed automatically; remove it by hand")
		case strings.HasPrefix(n, ".") && s.removeStray(ser, series, n):
		case strings.HasPrefix(n, "."):
			add("orphan_temp", n, "a file this server cannot prove it made, so it is never removed automatically; remove it by hand")
		case ok && kind == kindHeartbeat:
			add("left", n, "a heartbeat no intent names")
		}
	}
	return notes, nil
}

// lockFile is the captures lock's file, which the lock owns.
const lockFile = ".lock"

// interruptedNote is the note of a manifest recovery writes.
const interruptedNote = "the server running this capture ended before it finished; recovery removed what it had written"

// proveByToken is name's identity when it is a regular file with one or
// two links (the second a name of the same bytes), at most limit bytes,
// whose contents pass ours: bytes carrying in's owner token, which only
// this server wrote. ok is false otherwise.
func proveByToken(ser *os.Root, name string, in intent, limit int64, ours func([]byte) bool) (fileIdentity, bool) {
	b, fi, err := readSmall(ser, name, limit)
	if err != nil || in.Token == "" {
		return fileIdentity{}, false
	}
	if st, _ := statOf(fi); st.nlink < 1 || st.nlink > 2 || !ours(b) {
		return fileIdentity{}, false
	}
	id := identityOf(name, fi)
	sum := sha256.Sum256(b)
	id.SHA256 = hex.EncodeToString(sum[:])
	return id, true
}

// isRecorded reports whether the file id records is there.
func (t *captureTxn) isRecorded(id fileIdentity) bool {
	why, err := matches(t.ser, id.Name, id)
	return err == nil && why == ""
}

// holdsToken reports whether b is bytes only this capture wrote: its
// token (the heartbeat, the probe), or a version of its intent.
func (in intent) holdsToken(b []byte) bool {
	if string(b) == in.Token+"\n" {
		return true
	}
	var other intent
	return json.Unmarshal(b, &other) == nil && other.Token == in.Token && other.ID == in.ID && other.Label == in.Label && other.Series == in.Series
}

// holdsManifest reports whether b is a manifest of this capture.
func (in intent) holdsManifest(b []byte) bool {
	var m manifest
	return json.Unmarshal(b, &m) == nil && m.WrittenBy == writtenBy && m.ID == in.ID && m.Label == in.Label && m.Series == in.Series
}

// proveQuarantined is the identity that proves a quarantined name of this
// capture, when one does: a file the intent records (a capture file, a
// temporary, the heartbeat, the intent itself), the committed manifest
// under a second name, or bytes carrying the token.
func (t *captureTxn) proveQuarantined(root *os.Root, name string) (fileIdentity, bool) {
	cands := slices.Clone(t.in.Files)
	cands = append(cands, t.in.Temps...)
	if t.in.Heartbeat != nil {
		cands = append(cands, *t.in.Heartbeat)
	}
	cands = append(cands, t.self)
	if b, fi, err := readSmall(t.ser, manifestName(t.id, t.in.Label), maxManifestBytes); err == nil {
		id := identityOf(name, fi)
		sum := sha256.Sum256(b)
		id.SHA256 = hex.EncodeToString(sum[:])
		cands = append(cands, id)
	}
	for _, c := range cands {
		c.Name = name
		if why, err := matches(root, name, c); err == nil && why == "" {
			return c, true
		}
	}
	return proveByToken(root, name, t.in, maxManifestBytes, t.in.holdsToken)
}

// removeStray removes a dot-name of the series that no intent accounts
// for when what it holds proves it: a capture's intent a crash left
// quarantined after its manifest was written, or an intent temporary
// whose intent was never linked. It reports whether the name is gone.
func (s *Server) removeStray(ser *os.Root, series, name string) bool {
	var tok, final string
	switch {
	case strings.HasPrefix(name, quarantinePrefix):
		tok, _, _ = strings.Cut(strings.TrimPrefix(name, quarantinePrefix), "-")
	default:
		before, after, ok := strings.Cut(strings.TrimPrefix(name, "."), ".tmp-")
		if !ok || !strings.HasSuffix(before, intentSuffix) {
			return false
		}
		final, tok = before, after[:min(16, len(after))]
	}
	if !isHex(tok, 16) {
		return false
	}
	var in intent
	b, _, err := readSmall(ser, name, maxManifestBytes)
	if err != nil || json.Unmarshal(b, &in) != nil || len(in.Token) != 32 || in.Token[:16] != tok || in.Series != series {
		return false
	}
	id, ok := parseID(in.ID)
	if !ok || in.check(intentName(id, in.Label)) != nil || !in.isTemp(id, name) && final != "" {
		return false
	}
	if final == "" {
		// Quarantined: only ever the intent's last removal, after its
		// manifest.
		if _, err := readManifest(ser, manifestName(id, in.Label)); err != nil {
			return false
		}
	} else if _, err := ser.Lstat(final); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	proof, ok := proveByToken(ser, name, in, maxManifestBytes, in.holdsToken)
	if !ok {
		return false
	}
	removed, _, err := removeByIdentity(ser, name, proof)
	if removed {
		s.log.Info("capture recovery removed a leftover its contents prove", "series", series, "name", name)
	}
	return err == nil && removed
}

// adopt is the transaction of an intent another (gone) owner wrote, for
// recovery to finish or undo. ser stays recovery's.
func (s *Server) adopt(d *captureDir, ser *os.Root, series string, in intent, self fileIdentity) *captureTxn {
	id, _ := parseID(in.ID)
	return &captureTxn{s: s, d: d, ser: ser, series: series, id: id, in: in, self: self, borrowed: true}
}

// finishCommitted removes what a capture whose manifest exists left
// behind: staging, by identity; for a failure manifest, any final name
// still recorded; then the temporaries, the intent and the heartbeat.
func (t *captureTxn) finishCommitted(ok bool) error {
	if err := t.removeStaging(); err != nil {
		return err
	}
	if !ok {
		if err := t.removeFinals(); err != nil {
			return err
		}
	}
	return t.cleanup()
}
