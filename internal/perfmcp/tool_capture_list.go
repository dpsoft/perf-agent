package perfmcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

func (s *Server) captureListTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name:  "capture_list",
		Title: "List captures",
		Description: s.captureDescription("List this server's capture jobs, the running one first, then the history read from the manifests on disk, " +
			"so a loop finds its captures after a restart. Without series, the series with their capture counts and bytes; with series, " +
			"its captures newest first: id, label, outcome (ok or the failure code), settings, target, time, paths, kept, compare_with, " +
			"and the handle when this server has the profile open. It pages like top. A history not read in time is \"loading\": call again."),
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "series": {"type": "string", "description": "A series to list the captures of; without it, the series are listed."},
    "limit": {"type": "integer", "minimum": 1, "description": "Rows per page."},
    "cursor": {"type": "string", "description": "next_cursor of the previous page, with the same series."}
  },
  "additionalProperties": false
}`),
		Annotations: readOnly,
		Handler:     s.handleList,
	}
}

// listArgs are capture_list's arguments.
type listArgs struct {
	Series *string `json:"series"`
	Limit  *int    `json:"limit"`
	Cursor string  `json:"cursor"`
}

// listJob is a job of this server as capture_list shows it.
type listJob struct {
	jobView
	Series string `json:"series"`
	Label  string `json:"label"`
}

// seriesRow is a series of the capture directory: its captures (marker-
// only ones left out), its bytes, what recovery would note as left over,
// and the captures in progress (an intent without a manifest yet).
type seriesRow struct {
	Series     string `json:"series"`
	Captures   string `json:"captures"`
	Bytes      string `json:"bytes"`
	Leftovers  string `json:"leftovers"`
	InProgress string `json:"in_progress"`
}

// listRow is a capture of a series as its manifest records it. It never
// carries the manifest's token.
type listRow struct {
	ID               string        `json:"id"`
	Capture          string        `json:"capture"`
	Label            string        `json:"label"`
	Outcome          string        `json:"outcome"`
	StoppedBy        string        `json:"stopped_by,omitzero"`
	Settings         wireSettings  `json:"settings"`
	SettingsDigest   string        `json:"settings_digest"`
	Target           listTarget    `json:"target,omitzero"`
	Start            time.Time     `json:"start,omitzero"`
	ProfiledDuration string        `json:"profiled_duration,omitzero"`
	Manifest         string        `json:"manifest"`
	Profiles         []listProfile `json:"profiles,omitzero"`
	Kept             bool          `json:"kept"`
	CompareWith      string        `json:"compare_with,omitzero"`
}

// listTarget is the profiled process by its name and build-id.
type listTarget struct {
	Comm    string `json:"comm,omitzero"`
	Exe     string `json:"exe,omitzero"`
	BuildID string `json:"build_id,omitzero"`
}

// listProfile is a file of a capture, with its handle when this server
// has it open.
type listProfile struct {
	Path   string              `json:"path"`
	Handle profilemodel.Handle `json:"handle,omitzero"`
}

// strandedRow is a capture of this server whose files are not settled
// yet: a rollback that could not write its failure manifest, or a commit
// whose manifest link had an outcome nobody could read.
type strandedRow struct {
	Capture string `json:"capture"`
	Label   string `json:"label"`
	State   string `json:"state"`
	Note    string `json:"note"`
}

// listAnswer is capture_list's answer.
type listAnswer struct {
	Active     string        `json:"active,omitzero"`
	Jobs       []listJob     `json:"jobs"`
	Stranded   []strandedRow `json:"stranded,omitzero"`
	Series     []seriesRow   `json:"series,omitzero"`
	Captures   []listRow     `json:"captures,omitzero"`
	Leftovers  string        `json:"leftovers,omitzero"`
	InProgress string        `json:"in_progress,omitzero"`
	Warnings   []string      `json:"warnings,omitzero"`
	Loading    bool          `json:"loading,omitzero"`
	// ReadAt is when the history answered was read, when it is an earlier
	// read's because this call's did not finish in time.
	ReadAt     string `json:"read_at,omitzero"`
	RetryAfter string `json:"retry_after,omitzero"`

	TotalRows    string `json:"total_rows,omitzero"`
	Skipped      string `json:"skipped,omitzero"`
	OmittedCount string `json:"omitted_count,omitzero"`
	NextCursor   string `json:"next_cursor,omitzero"`
}

// The states and notes of a capture whose files are not settled yet.
const (
	stateRollback  = "rollback stranded"
	strandedNote   = "its rollback has not written its failure manifest yet; this server retries it before each capture and every 30s"
	stateUndecided = "commit undecided"
	undecidedNote  = "its manifest link had an outcome nobody could read, so it may have committed; this server settles it from the disk before each capture and every 30s, and capture_wait then answers how it ended"
)

// handleList answers from memory at once (the jobs, the stranded
// rollbacks) and reads the history from the manifests through the lookup
// pool, waiting at most listWait for it.
func (s *Server) handleList(ctx context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.captureDisabled != "" {
		return toolError(s.disabledError(), nil), nil
	}
	var a listArgs
	if err := decodeArgs("capture_list", raw, &a); err != nil {
		return toolError(invalidArgument("", "%s", err.Error()), nil), nil
	}
	series := ""
	if a.Series != nil {
		if !validSeries(*a.Series) {
			return toolError(invalidArgument("pass a series as capture_list without series shows them", "series %q is not a series name", echo(*a.Series)), nil), nil
		}
		series = *a.Series
	}
	limit, err := s.pageLimit(a.Limit)
	if err != nil {
		return toolError(invalidArgument("", "%s", err.Error()), nil), nil
	}

	var out listAnswer
	if active := s.registry.active.Load(); active != nil {
		out.Active = active.id
	}
	out.Jobs = []listJob{}
	if a.Cursor == "" {
		for _, j := range s.registry.list() {
			js, label := cmp.Or(j.req.Series, defaultSeries), cmp.Or(j.req.Label, defaultLabel)
			if series == "" || js == series {
				out.Jobs = append(out.Jobs, listJob{jobView: j.view(), Series: js, Label: label})
			}
		}
	}
	out.Stranded = s.unsettledRows(series)

	h, asOf, err := s.history(ctx, series)
	switch {
	case errors.Is(err, errLoading):
		out.Loading, out.RetryAfter = true, loadingRetry.String()
		return answer(out)
	case err != nil:
		out.Warnings = append(out.Warnings, "the history cannot be read: "+causeText(err))
		return answer(out)
	}
	out.Leftovers, out.Warnings = strconv.Itoa(h.leftovers), append(out.Warnings, h.warnings...)
	if !asOf.IsZero() {
		out.ReadAt = asOf.UTC().Format(time.RFC3339)
	}
	bind := binding("capture_list", struct {
		Series string `json:"series"`
	}{series})
	var info pageInfo
	if series == "" {
		out.Series, info, err = pageRows(h.series, func(r seriesRow) int64 { n, _ := strconv.ParseInt(r.Captures, 10, 64); return n },
			"captures", bind, a.Cursor, limit)
	} else {
		out.InProgress = strconv.Itoa(h.inProgress)
		out.Captures, info, err = pageRows(h.rows, func(listRow) int64 { return 1 }, "captures", bind, a.Cursor, limit)
		out.Captures = s.withHandles(out.Captures)
	}
	if err != nil {
		return toolError(invalidArgument("", "%s", err.Error()), nil), nil
	}
	out.TotalRows, out.Skipped, out.OmittedCount, out.NextCursor = info.TotalRows, info.Skipped, info.OmittedCount, info.NextCursor
	b, err := marshal(out)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	if len(b) > s.lim.MaxResponseBytes {
		return toolError(invalidArgument(shrinkWithLimit, "the capture_list answer is %d bytes, over MaxResponseBytes=%d", len(b), s.lim.MaxResponseBytes), nil), nil
	}
	return mcpstdio.Result{Text: string(b)}, nil
}

// unsettledRows are this server's captures in series ("" for all) whose
// files are not settled: an undecided commit (its transaction is an
// undecided job's) or a stranded rollback. Both lists are read under
// capMu, which also orders a job's transaction against its removal.
func (s *Server) unsettledRows(series string) []strandedRow {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	var rows []strandedRow
	undecided := map[*captureTxn]bool{}
	for _, j := range s.undecided {
		if j.txn != nil {
			undecided[j.txn] = true
		}
	}
	add := func(t *captureTxn, state, note string) {
		if series == "" || t.series == series {
			rows = append(rows, strandedRow{Capture: captureKey(t.series, formatID(t.id)), Label: t.in.Label, State: state, Note: note})
		}
	}
	for _, t := range s.stranded {
		if undecided[t] {
			add(t, stateUndecided, undecidedNote)
			delete(undecided, t)
			continue
		}
		add(t, stateRollback, strandedNote)
	}
	for _, j := range s.undecided {
		if j.txn != nil && undecided[j.txn] {
			add(j.txn, stateUndecided, undecidedNote)
		}
	}
	return rows
}

// errLoading is a history not read by the call's deadline.
var errLoading = errors.New("the history is still being read")

// listKeep is how old a kept history may be to be answered.
const listKeep = 30 * time.Second

// maxListKept bounds the histories kept, one per series queried.
const maxListKept = 64

// keptHistory is the last history read for a query, at gen, the history
// generation then, and at, when it was read.
type keptHistory struct {
	h   *captureHistory
	gen uint64
	at  time.Time
}

// keepHistory keeps k as the last history read for key. Histories that
// can no longer be answered (older than listKeep, or of an earlier
// generation) are dropped, and beyond maxListKept the oldest go.
func (s *Server) keepHistory(key string, k keptHistory) {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	gen := s.historyGen.Load()
	maps.DeleteFunc(s.listKept, func(_ string, v keptHistory) bool {
		return v.gen != gen || time.Since(v.at) >= listKeep
	})
	s.listKept[key] = k
	for len(s.listKept) > maxListKept {
		oldest := ""
		for name, v := range s.listKept {
			if oldest == "" || v.at.Before(s.listKept[oldest].at) {
				oldest = name
			}
		}
		delete(s.listKept, oldest)
	}
}

// history is the capture directory's history for series, read through
// the lookup pool, waiting at most listWait. A read that does not finish
// by then goes on, and what the last finished read found is answered
// instead, with when it was read, while this server has written nothing
// in the capture directory since and it is under listKeep old: a read
// slower than the call's wait still reaches the agent. errLoading says
// there is nothing to answer yet.
func (s *Server) history(ctx context.Context, series string) (h *captureHistory, asOf time.Time, err error) {
	key := "list:" + series
	load := func() (any, error) {
		gen := s.historyGen.Load()
		h, err := s.loadHistory(series)
		if err == nil {
			s.keepHistory(key, keptHistory{h: h, gen: gen, at: time.Now()})
		}
		return h, err
	}
	if l, ok := s.lookups.get(key, load); ok {
		v, ready, err := l.wait(ctx, fromReceipt(ctx, cmp.Or(s.listWait, listWaitDefault)))
		switch {
		case ready && err != nil:
			return nil, asOf, err
		case ready:
			return v.(*captureHistory), asOf, nil
		}
	}
	s.listMu.Lock()
	kept, ok := s.listKept[key]
	s.listMu.Unlock()
	if ok && kept.gen == s.historyGen.Load() && time.Since(kept.at) < listKeep {
		return kept.h, kept.at, nil
	}
	return nil, asOf, errLoading
}

// historyChanged says the capture directory was written: a kept history
// is stale.
func (s *Server) historyChanged() { s.historyGen.Add(1) }

// withHandles is rows, copied, with the handle of each profile this
// server has open, read from the map the store publishes for any
// goroutine.
func (s *Server) withHandles(rows []listRow) []listRow {
	open := s.store.openNames()
	out := slices.Clone(rows)
	for i := range out {
		ps := slices.Clone(out[i].Profiles)
		for k := range ps {
			ps[k].Handle = open[ps[k].Path]
		}
		out[i].Profiles = ps
	}
	return out
}

// captureHistory is the capture directory as capture_list reads it: the
// series, or one series' captures newest first, what is left over, and
// warnings.
type captureHistory struct {
	series     []seriesRow
	rows       []listRow
	leftovers  int
	inProgress int
	warnings   []string
}

// loadHistory reads the capture directory, creating nothing, on a lookup
// worker: every series with series "", else that series' captures.
func (s *Server) loadHistory(series string) (*captureHistory, error) {
	if s.lookupHook != nil {
		s.lookupHook("list:" + series)
	}
	h := &captureHistory{series: []seriesRow{}, rows: []listRow{}}
	d, err := s.readCaptureDir()
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.close()
	if series != "" {
		if _, err := d.root.Lstat(series); errors.Is(err, fs.ErrNotExist) {
			return h, nil
		}
		ser, err := d.openSeries(series)
		if err != nil {
			return nil, err
		}
		defer func() { _ = ser.Close() }()
		sc, err := s.scanSeries(ser, series, true)
		if err != nil {
			return nil, d.writeError(err)
		}
		h.rows, h.leftovers, h.inProgress, h.warnings = sc.rows, sc.leftovers, sc.inProgress, sc.warnings
		return h, nil
	}
	names, err := readDirNames(d.root, ".")
	if err != nil {
		return nil, d.writeError(err)
	}
	slices.Sort(names)
	for _, n := range names {
		fi, err := d.root.Lstat(n)
		if err != nil {
			continue
		}
		if !validSeries(n) || !fi.IsDir() {
			// What recovery would note in the capture directory itself: a
			// temporary of its own writes.
			if strings.HasPrefix(n, ".") && n != ownerFile && n != lockFile {
				h.leftovers++
			}
			continue
		}
		ser, err := d.openSeries(n)
		if err != nil {
			h.warnings = append(h.warnings, fmt.Sprintf("series %s cannot be read: %s", n, causeText(err)))
			continue
		}
		sc, err := s.scanSeries(ser, n, false)
		var bytes int64
		if err == nil {
			bytes, err = dirBytes(ser)
		}
		_ = ser.Close()
		if err != nil {
			h.warnings = append(h.warnings, fmt.Sprintf("series %s cannot be read: %s", n, causeText(err)))
			continue
		}
		h.series = append(h.series, seriesRow{
			Series: n, Captures: strconv.Itoa(sc.captures), Bytes: strconv.FormatInt(bytes, 10),
			Leftovers: strconv.Itoa(sc.leftovers), InProgress: strconv.Itoa(sc.inProgress),
		})
		h.leftovers += sc.leftovers
		h.warnings = append(h.warnings, sc.warnings...)
	}
	return h, nil
}

// seriesScan is one series as capture_list reads it.
type seriesScan struct {
	rows                            []listRow
	captures, leftovers, inProgress int
	warnings                        []string
}

// scanSeries reads a series without changing it. A capture is a manifest;
// one whose eviction left only its marker is being removed and is not
// counted. An intent is a capture in progress, and claims its staging
// directory, heartbeat and temporaries. What recovery would note is
// counted as left over: a name starting with a dot that no intent claims
// (a staging directory, a temporary, a file moved aside), a heartbeat no
// intent names, an intent that cannot be read. A series with captures and
// no .seq is warned of: its next capture could reuse an evicted id.
func (s *Server) scanSeries(ser *os.Root, series string, rows bool) (seriesScan, error) {
	var sc seriesScan
	names, err := readDirNames(ser, ".")
	if err != nil {
		return sc, err
	}
	slices.Sort(names)
	claimed := map[string]bool{}
	hasSeq, entries := false, 0
	for _, n := range names {
		if n == seqFile {
			hasSeq = true
			continue
		}
		id, label, kind, ok := parseName(n)
		if !ok {
			continue
		}
		switch kind {
		case kindManifest:
			entries++
			sc.captures++
			if rows {
				m, err := readManifest(ser, n)
				if err != nil {
					sc.warnings = append(sc.warnings, fmt.Sprintf("the manifest of %s/%s cannot be read: %s", series, formatID(id), causeText(err)))
					continue
				}
				sc.rows = append(sc.rows, s.listRowOf(m))
			}
		case kindEvicting:
			entries++
		case kindIntent:
			entries++
			in, _, err := readIntent(ser, n)
			if err != nil {
				sc.leftovers++
				continue
			}
			sc.inProgress++
			claimed[in.Staging] = true
			claimed[heartbeatName(id, label)] = true
			if in.Heartbeat != nil {
				claimed[in.Heartbeat.Name] = true
			}
			if len(in.Token) >= 16 {
				for _, o := range names {
					if strings.Contains(o, ".tmp-"+in.Token[:16]) {
						claimed[o] = true
					}
				}
			}
		}
	}
	for _, n := range names {
		if claimed[n] || n == seqFile {
			continue
		}
		_, _, kind, ok := parseName(n)
		if strings.HasPrefix(n, ".") || ok && kind == kindHeartbeat {
			sc.leftovers++
		}
	}
	if !hasSeq && entries > 0 {
		sc.warnings = append(sc.warnings, fmt.Sprintf(
			"series %s has captures and no .seq, so its next capture could take the id of one retention removed; .seq must not be removed by hand", series))
	}
	slices.SortFunc(sc.rows, func(a, b listRow) int {
		ia, _ := parseID(a.ID)
		ib, _ := parseID(b.ID)
		return ib - ia
	})
	return sc, nil
}

// listRowOf is m as a capture_list row: never its token.
func (s *Server) listRowOf(m manifest) listRow {
	id, _ := parseID(m.ID)
	outcome := m.Outcome
	if m.Outcome == captureFailed && m.Error != nil {
		outcome = m.Error.Code
	}
	r := listRow{
		ID: m.ID, Capture: captureKey(m.Series, m.ID), Label: m.Label, Outcome: outcome, StoppedBy: m.StoppedBy,
		Settings: m.Settings, SettingsDigest: m.SettingsDigest,
		Target: listTarget{Comm: m.Target.Comm, Exe: m.Target.Exe, BuildID: m.Target.BuildID},
		Start:  m.Start, ProfiledDuration: m.ProfiledDuration,
		Manifest: s.capturePath(m.Series, manifestName(id, m.Label)), Kept: m.Kept, CompareWith: m.CompareWith,
	}
	for _, f := range m.Files {
		r.Profiles = append(r.Profiles, listProfile{Path: s.capturePath(m.Series, f.Name)})
	}
	return r
}
