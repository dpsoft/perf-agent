package flamegraph

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// LiveOptions name what a live model of an explained view shows about its
// input, how large it may be and where its layout starts. The fields mean
// what ExplainedPageOptions' fields of the same names mean.
type LiveOptions struct {
	Title, ProfileName, Digest string
	Attempt                    string // the attempt the publication was made for
	MaxNodes, MaxBytes         int
	// Focus, when set, is the node the layout starts at: it is drawn at
	// depth 0 and full width with its descendants below it. Shares stay of
	// the view total.
	Focus profilemodel.NodeID
}

func (o LiveOptions) pageOptions() ExplainedPageOptions {
	return ExplainedPageOptions{Title: o.Title, ProfileName: o.ProfileName, Digest: o.Digest, Attempt: o.Attempt,
		MaxNodes: o.MaxNodes, MaxBytes: o.MaxBytes}
}

// liveModel is the live page's model of an explained view. Every string
// is the explainedPageData field the static template prints for it; the
// panel's model is the page's model JSON as it is.
type liveModel struct {
	Kind        string          `json:"kind"`
	Title       string          `json:"doc_title"` // the document title; the heading is TitleSegs
	TitleSegs   []exSegment     `json:"title_segs"`
	Attempt     string          `json:"attempt"`
	View        string          `json:"view"`
	Digest      string          `json:"digest"`
	ProfileName string          `json:"profile_name"`
	CheckedPill string          `json:"checked_pill"`
	Axis        string          `json:"axis"`
	Total       string          `json:"total"`
	TotalTitle  string          `json:"total_title"`
	Notes       []string        `json:"notes"`
	Quality     []exQualityRow  `json:"quality"`
	Legend      []exLegend      `json:"legend"`
	Height      string          `json:"height"`
	Drawn       []exDrawn       `json:"drawn"`
	NarrowNote  string          `json:"narrow_note"`
	LimitNote   string          `json:"limit_note"`
	SizeNote    string          `json:"size_note"`
	Rows        []exRow         `json:"rows"`
	Focus       string          `json:"focus"`
	Model       json.RawMessage `json:"model"`
}

// ExplainedLiveModel is the live page's model of v with the explanations r
// checked, as compact JSON. It is built by the static page's builder, so
// every string in it is the one the page prints for the same field, and
// encoding/json escapes "<", ">" and "&" in all of them. Integers are
// strings. It refuses a nil view and a check that did not pass.
//
// With opts.MaxBytes the model is fitted to the budget as the page is:
// frames are left out, and counted in size_note, until it fits; a model
// that does not fit with only the explained and referenced paths is an
// error. With opts.Focus the layout starts at that node, and the focus's
// callers and every explained and referenced path stay in the model,
// undrawn, for the path panel and the rows. An unknown focus is
// profilemodel.ErrUnknownNode.
func ExplainedLiveModel(v *profilemodel.View, r explain.Result, opts LiveOptions) ([]byte, error) {
	if err := checkLiveInput(v, r); err != nil {
		return nil, err
	}
	var root *profilemodel.Node
	if opts.Focus != "" {
		n, ok := v.Node(opts.Focus)
		if !ok {
			return nil, fmt.Errorf("%w: %s", profilemodel.ErrUnknownNode, opts.Focus)
		}
		root = n
	}
	render := func(frameBytes int64) ([]byte, error) {
		b := newExplainedBuilder(v, r, opts.pageOptions(), root, 0)
		b.modelOnly = true
		data, err := b.build(frameBytes)
		if err != nil {
			return nil, err
		}
		return json.Marshal(liveModelOf(data, opts.Focus))
	}
	if opts.MaxBytes <= 0 {
		return render(-1)
	}
	base, err := render(0)
	if err != nil {
		return nil, err
	}
	if len(base) > opts.MaxBytes {
		return nil, fmt.Errorf("flamegraph: the live model needs %d bytes with only the explained and referenced paths drawn, over the budget of %d bytes (MaxBytes)",
			len(base), opts.MaxBytes)
	}
	return fitFrames(opts.MaxBytes, base, render)
}

// LiveTitleSegments is the heading of a live publication titled title, of
// the profile profileName, as the JSON array a live model's title_segs
// holds: the title's numbers are marked unchecked, and an empty title is
// the profile's name. The page's list of publications shows it, so the
// list marks the title's numbers as the heading does.
func LiveTitleSegments(title, profileName string) []byte {
	out, err := json.Marshal(titleSegments(ExplainedPageOptions{Title: title, ProfileName: profileName}))
	if err != nil {
		// Segments are strings only, so they always encode.
		panic(err)
	}
	return out
}

func checkLiveInput(v *profilemodel.View, r explain.Result) error {
	if v == nil {
		return errors.New("flamegraph: nil view")
	}
	if !r.OK {
		return errors.New("flamegraph: the explanations did not pass the check; only a passing check is published")
	}
	return nil
}

// liveModelOf is data as the live model; lists are empty, never null.
func liveModelOf(data *explainedPageData, focus profilemodel.NodeID) liveModel {
	return liveModel{
		Kind:        "explained",
		Title:       data.Title,
		TitleSegs:   data.TitleSegs,
		Attempt:     data.Attempt,
		View:        data.View,
		Digest:      data.Digest,
		ProfileName: data.ProfileName,
		CheckedPill: data.CheckedPill,
		Axis:        data.Axis,
		Total:       data.Total,
		TotalTitle:  data.TotalTitle,
		Notes:       nonNil(data.Notes),
		Quality:     nonNil(data.Quality),
		Legend:      nonNil(data.Legend),
		Height:      data.Height,
		Drawn:       nonNil(data.Frames),
		NarrowNote:  data.NarrowNote,
		LimitNote:   data.LimitNote,
		SizeNote:    data.SizeNote,
		Rows:        nonNil(data.Rows),
		Focus:       string(focus),
		Model:       json.RawMessage(data.Model),
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// livePathItem is a path entry as the panel reads it: its id, and the
// same id as node, as the static page's path buttons name it.
type livePathItem struct {
	ID string `json:"id"`
	exPathItem
}

type liveFrame struct {
	Frame *exFrame       `json:"frame"`
	Path  []livePathItem `json:"path"`
}

// ExplainedLiveFrame is node id of v as the live page's panel shows it,
// drawn or not: its frame with its facts, the facts only an explanation
// has and its explanations, and its path from below the root. A ref names
// a frame's place in one model's frames, which a single frame does not
// know, so the frame, its path and the node chips of its explanations carry
// none; the page names them from its model. digest is the profile's,
// as the model's prompts name it. An unknown node is
// profilemodel.ErrUnknownNode.
func ExplainedLiveFrame(v *profilemodel.View, r explain.Result, id profilemodel.NodeID, digest string) ([]byte, error) {
	if err := checkLiveInput(v, r); err != nil {
		return nil, err
	}
	n, ok := v.Node(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", profilemodel.ErrUnknownNode, id)
	}
	b := newExplainedBuilder(v, r, ExplainedPageOptions{Digest: digest}, nil, 0)
	b.partial = true
	if err := b.addPath(n); err != nil {
		return nil, err
	}
	clear(b.refs)
	if err := b.attachExplanations(); err != nil {
		return nil, err
	}
	for _, f := range b.frames {
		f.Ref = ""
	}
	f := b.byID[n.ID]
	out := liveFrame{Frame: f, Path: []livePathItem{}}
	for _, p := range b.path(f) {
		out.Path = append(out.Path, livePathItem{ID: p.Node, exPathItem: p})
	}
	return json.Marshal(out)
}

// historyRowOverhead bounds the JSON of one rejected row besides its text:
// its keys, quotes and separators.
const historyRowOverhead = 160

type liveHistory struct {
	Publication  string       `json:"publication"`
	Version      string       `json:"version"`
	RejectedPill string       `json:"rejected_pill"`
	LimitPill    string       `json:"limit_pill"`
	HistoryPill  string       `json:"history_pill"`
	RejectedNote string       `json:"rejected_note"`
	Rows         []exRejected `json:"rows"`
}

// ExplainedLiveHistory is the rejected attempts of v's publication pub, at
// history version version, as the live page lists them: the rows and pills
// of the static page's rejected section, built by the same function. Over
// maxBytes the oldest rows are left out and counted in rejected_note, as
// on the page; the pills count every attempt. maxBytes <= 0 means no
// bound. Rows are estimated before
// any is encoded, so a long history costs no more than the budget. Without
// any row the history is returned even over maxBytes: its pills are small.
func ExplainedLiveHistory(v *profilemodel.View, pub, version string, rejected []explain.RejectedAttempt, dropped int, incomplete bool, maxBytes int) []byte {
	b := newExplainedBuilder(v, explain.Result{}, ExplainedPageOptions{Rejected: rejected, DroppedRejected: dropped, HistoryIncomplete: incomplete}, nil, -1)
	rows, rejectedPill, limitPill := b.rejected()
	h := liveHistory{Publication: pub, Version: version, RejectedPill: rejectedPill, LimitPill: limitPill,
		HistoryPill: historyPill(incomplete), Rows: []exRejected{}}
	encode := func(keep int) []byte {
		h.Rows, h.RejectedNote = nonNil(rows[len(rows)-keep:]), ""
		if n := len(rows) - keep; n > 0 {
			h.RejectedNote = rejectedNote(n)
		}
		out, err := json.Marshal(h)
		if err != nil {
			// Strings and string slices always encode.
			panic(err)
		}
		return out
	}
	if maxBytes <= 0 {
		return encode(len(rows))
	}
	bare := encode(0)
	keep, room := 0, int64(maxBytes-len(bare))
	for i := len(rows) - 1; i >= 0; i-- {
		if room -= historyRowEstimate(rows[i]); room < 0 {
			break
		}
		keep++
	}
	// The estimate bounds each row, so this encoding fits; halving is only
	// a safety net.
	for ; keep > 0; keep /= 2 {
		if out := encode(keep); len(out) <= maxBytes {
			return out
		}
	}
	return bare
}

// LiveHistoryRowBound bounds the bytes, its comma included, the row of
// failure f of attempt (made with evidence, "" for none) takes in
// ExplainedLiveHistory's JSON of view v. It builds nothing: a caller fits
// a history with it before copying or encoding any of it.
func LiveHistoryRowBound(v *profilemodel.View, attempt, evidence string, f explain.Failure) int64 {
	n := int64(historyRowOverhead) + jsonEscapedBound(attempt) + jsonEscapedBound(evidence) + int64(len("reference")) +
		// the label: "claim <claim>", "limit <ref>" or a reference label
		int64(len("reference {n:}")) + jsonEscapedBound(f.Claim) + jsonEscapedBound(f.Ref) +
		jsonEscapedBound(string(f.Code)) + jsonEscapedBound(f.Message) + jsonEscapedBound(f.Agent)
	if e := f.Engine; e != nil {
		n += jsonEscapedBound(e.Metric) + engineBound(*e) + nodeNameBound(v, e.Node)
	}
	return n
}

// engineBound bounds the JSON of displayEngine(e): an unavailable value's
// reason, or a value at most doubled by grouping and units.
func engineBound(e explain.EngineValue) int64 {
	unavailable := int64(len("unavailable: the engine has no value")) + jsonEscapedBound(e.Reason)
	shown := 2*jsonEscapedBound(e.Value) + jsonEscapedBound(e.Unit) + 24
	return max(unavailable, shown)
}

// nodeNameBound bounds the JSON of the name a row gives node id: the whole
// view, or the node's shown name and ID.
func nodeNameBound(v *profilemodel.View, id profilemodel.NodeID) int64 {
	n := int64(len("the whole view") + len("node  (node )") + len(id))
	if v != nil {
		if nd, ok := v.Node(id); ok {
			name := nd.Function.Name
			n += jsonEscapedBound(name[:min(len(name), maxShownName)]) + int64(len("…"))
		}
	}
	return n
}

// LiveHistoryAttemptBound bounds the bytes rows of an attempt's "more"
// rows take in ExplainedLiveHistory's JSON: an attempt has one for its
// omitted failures and one for those too large to keep.
func LiveHistoryAttemptBound(attempt, evidence string, rows int) int64 {
	return int64(rows) * (int64(historyRowOverhead) + jsonEscapedBound(attempt) + jsonEscapedBound(evidence) + int64(len("more")) + 64)
}

// LiveHistoryBaseBound bounds the bytes ExplainedLiveHistory's JSON of
// publication pub at version takes besides its rows: its keys, pills and
// note.
func LiveHistoryBaseBound(pub, version string) int64 {
	return 1<<10 + jsonEscapedBound(pub) + jsonEscapedBound(version)
}

// historyRowEstimate bounds the bytes one rejected row takes in the
// history JSON.
func historyRowEstimate(row exRejected) int64 {
	n := int64(historyRowOverhead)
	for _, s := range []string{row.Attempt, row.Evidence, row.Kind, row.Label, row.Metric, row.Node, row.Agent, row.Engine, row.Code, row.Reason} {
		n += jsonEscapedBound(s)
	}
	return n
}
