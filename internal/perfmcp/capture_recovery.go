package perfmcp

import (
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
		// A temporary that is a second name of the intent itself (a crash
		// between linking the intent and removing that name) is proven by
		// the intent's own identity.
		for _, tn := range names {
			if strings.HasPrefix(tn, "."+n+".tmp-"+in.Token[:16]) {
				alias := self
				alias.Name = tn
				if removed, _, err := removeByIdentity(ser, tn, alias); err != nil {
					return notes, d.writeError(err)
				} else if removed {
					claimed[tn] = true
				}
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
