package perfmcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const diffSchema = `{
  "type": "object",
  "properties": {
    "base_handle": {"type": "string", "description": "Handle of the base profile."},
    "base_view": {"type": "string", "description": "A view name of the base handle."},
    "new_handle": {"type": "string", "description": "Handle of the new profile."},
    "new_view": {"type": "string", "description": "A view name of the new handle; it must have the base view's sample type, unit and index."},
    "work_base": {"type": "number", "exclusiveMinimum": 0, "description": "Units of work the base run performed; with work_new it enables delta_per_unit."},
    "work_new": {"type": "number", "exclusiveMinimum": 0, "description": "Units of work the new run performed."},
    "limit": {"type": "integer", "minimum": 1, "description": "Rows per page (default 50, at most the server's MaxDiffRows)."},
    "cursor": {"type": "string", "description": "next_cursor of the previous page of this same query."}
  },
  "required": ["base_handle", "base_view", "new_handle", "new_view"],
  "additionalProperties": false
}`

const diffDescription = "Compare two views of the same sample type as a flat list of union nodes (id, parent id, depth), " +
	"the largest absolute change first, paged with limit and cursor. A node on one side only stays in the list. " +
	"The header carries both totals, both qualities, the quality warnings, the GPU sampling notes and whether per-unit deltas are possible. " +
	"omitted_weight sums the omitted rows' absolute deltas; rows overlap, so it is never a remaining share."

// defaultDiffLimit is the page size of diff when limit is omitted.
const defaultDiffLimit = 50

func (s *Server) diffTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name: "diff", Title: "Diff two profiles", Description: diffDescription,
		InputSchema: json.RawMessage(diffSchema), Annotations: readOnly, Handler: s.handleDiff,
	}
}

type diffArgs struct {
	BaseHandle profilemodel.Handle `json:"base_handle"`
	BaseView   string              `json:"base_view"`
	NewHandle  profilemodel.Handle `json:"new_handle"`
	NewView    string              `json:"new_view"`
	WorkBase   *float64            `json:"work_base"`
	WorkNew    *float64            `json:"work_new"`
	Limit      *int                `json:"limit"`
	Cursor     string              `json:"cursor"`
}

// diffQuery is every argument that identifies a diff but the page.
type diffQuery struct {
	BaseHandle profilemodel.Handle `json:"base_handle"`
	BaseView   string              `json:"base_view"`
	NewHandle  profilemodel.Handle `json:"new_handle"`
	NewView    string              `json:"new_view"`
	WorkBase   *float64            `json:"work_base"`
	WorkNew    *float64            `json:"work_new"`
}

// diffNodeRow is a flat diff node as clients read it.
type diffNodeRow struct {
	ID             profilemodel.UnionID        `json:"id"`
	Parent         profilemodel.UnionID        `json:"parent,omitempty"`
	Depth          string                      `json:"depth"`
	BaseID         profilemodel.NodeID         `json:"base_id,omitempty"`
	NewID          profilemodel.NodeID         `json:"new_id,omitempty"`
	Name           string                      `json:"name"`
	Presence       profilemodel.Presence       `json:"presence"`
	Match          profilemodel.Match          `json:"match,omitempty"`
	Candidates     []profilemodel.Candidate    `json:"candidates,omitzero"`
	Interpretation profilemodel.Interpretation `json:"interpretation"`
	Why            string                      `json:"why,omitempty"`
	BaseCum        string                      `json:"base_cum"`
	NewCum         string                      `json:"new_cum"`
	BaseSelf       string                      `json:"base_self"`
	NewSelf        string                      `json:"new_self"`
	Combined       string                      `json:"combined"`
	DeltaAbs       string                      `json:"delta_abs"`
	DeltaShare     Fact                        `json:"delta_share"`
	DeltaPerUnit   *Fact                       `json:"delta_per_unit,omitempty"`
}

type diffResult struct {
	BaseHandle      profilemodel.Handle       `json:"base_handle"`
	NewHandle       profilemodel.Handle       `json:"new_handle"`
	View            string                    `json:"view"`
	SampleType      string                    `json:"sample_type"`
	Unit            string                    `json:"unit"`
	BaseTotal       string                    `json:"base_total"`
	NewTotal        string                    `json:"new_total"`
	BaseQuality     Quality                   `json:"base_quality"`
	NewQuality      Quality                   `json:"new_quality"`
	QualityWarnings []string                  `json:"quality_warnings"`
	BaseNotes       []string                  `json:"base_notes"`
	NewNotes        []string                  `json:"new_notes"`
	PerUnit         profilemodel.Availability `json:"per_unit"`
	Rows            []diffNodeRow             `json:"rows"`
	pageInfo
}

// diffHeader is what a cached diff keeps besides its sorted nodes.
type diffHeader struct {
	profilemodel.FlatDiff
}

// diffCache is a per-server LRU of flat diffs, rows sorted for paging.
type diffCache struct {
	max   int
	m     map[string]*diffHeader
	order []string // least recently used first
}

func newDiffCache(max int) *diffCache { return &diffCache{max: max, m: map[string]*diffHeader{}} }

func (c *diffCache) get(key string) (*diffHeader, bool) {
	d, ok := c.m[key]
	if ok {
		if i := slices.Index(c.order, key); i >= 0 {
			c.order = append(slices.Delete(c.order, i, i+1), key)
		}
	}
	return d, ok
}

func (c *diffCache) put(key string, d *diffHeader) {
	c.m[key] = d
	c.order = append(c.order, key)
	for len(c.order) > c.max {
		delete(c.m, c.order[0])
		c.order = slices.Delete(c.order, 0, 1)
	}
}

// diffKey identifies a diff by its evidence, views and work counts.
func diffKey(q diffQuery) string {
	b, err := json.Marshal(q)
	if err != nil {
		panic(fmt.Sprintf("perfmcp: diff key: %v", err))
	}
	return string(b)
}

// sortFlat orders nodes by absolute delta, then combined weight, both
// descending, then ID.
func sortFlat(nodes []profilemodel.FlatDiffNode) {
	slices.SortStableFunc(nodes, func(a, b profilemodel.FlatDiffNode) int {
		return cmp.Or(
			cmp.Compare(absInt(b.DeltaAbs), absInt(a.DeltaAbs)),
			cmp.Compare(b.Combined, a.Combined),
			cmp.Compare(a.ID, b.ID),
		)
	})
}

func absInt(x int64) int64 { return max(x, -x) }

// workArg validates one work count.
func workArg(name string, w *float64) error {
	if w != nil && (*w <= 0 || math.IsInf(*w, 0) || math.IsNaN(*w)) {
		return fmt.Errorf("%s must be a positive finite number of work units, got %v; omit both work counts for no per-unit deltas", name, *w)
	}
	return nil
}

func (s *Server) handleDiff(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a diffArgs
	if err := decodeArgs("diff", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	limit := min(defaultDiffLimit, s.lim.MaxDiffRows)
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > s.lim.MaxDiffRows {
			return mcpstdio.Result{}, fmt.Errorf("limit must be between 1 and %d (this server's MaxDiffRows), got %d", s.lim.MaxDiffRows, *a.Limit)
		}
		limit = *a.Limit
	}
	if err := errors.Join(workArg("work_base", a.WorkBase), workArg("work_new", a.WorkNew)); err != nil {
		return mcpstdio.Result{}, err
	}
	_, bv, err := s.store.view(a.BaseHandle, a.BaseView)
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("base: %w", err)
	}
	_, nv, err := s.store.view(a.NewHandle, a.NewView)
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("new: %w", err)
	}
	q := diffQuery{BaseHandle: a.BaseHandle, BaseView: a.BaseView, NewHandle: a.NewHandle, NewView: a.NewView, WorkBase: a.WorkBase, WorkNew: a.WorkNew}
	key := diffKey(q)
	d, ok := s.diffs.get(key)
	if !ok {
		var work profilemodel.WorkCounts
		if a.WorkBase != nil {
			work.Base = *a.WorkBase
		}
		if a.WorkNew != nil {
			work.New = *a.WorkNew
		}
		full, err := profilemodel.Compare(bv, nv, work)
		if errors.Is(err, profilemodel.ErrIncompatibleViews) {
			return mcpstdio.Result{}, fmt.Errorf("cannot diff view %s of %s against view %s of %s: %v; pick views with the same sample type, unit and index",
				a.BaseView, a.BaseHandle, a.NewView, a.NewHandle, err)
		}
		if err != nil {
			return mcpstdio.Result{}, fmt.Errorf("cannot diff the views: %v", err)
		}
		d = &diffHeader{FlatDiff: full.Flatten()}
		sortFlat(d.Nodes)
		s.diffs.put(key, d)
	}

	pg, info, err := pageRows(d.Nodes, func(n profilemodel.FlatDiffNode) int64 { return absInt(n.DeltaAbs) }, "abs_delta", binding("diff", q), a.Cursor, limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	out := diffResult{
		BaseHandle: d.BaseHandle, NewHandle: d.NewHandle, View: d.View, SampleType: d.SampleType, Unit: d.Unit,
		BaseTotal: wireInt(d.BaseTotal), NewTotal: wireInt(d.NewTotal),
		BaseQuality: wireQuality(d.BaseQuality), NewQuality: wireQuality(d.NewQuality),
		QualityWarnings: nonNil(d.QualityWarnings), BaseNotes: nonNil(d.BaseNotes), NewNotes: nonNil(d.NewNotes),
		PerUnit: d.PerUnit, Rows: make([]diffNodeRow, 0, len(pg)), pageInfo: info,
	}
	for _, n := range pg {
		r := diffNodeRow{
			ID: n.ID, Parent: n.Parent, Depth: wireInt(int64(n.Depth)), BaseID: n.BaseID, NewID: n.NewID, Name: n.Name,
			Presence: n.Presence, Match: n.Match, Candidates: n.Candidates, Interpretation: n.Interpretation, Why: n.Why,
			BaseCum: wireInt(n.BaseCum), NewCum: wireInt(n.NewCum), BaseSelf: wireInt(n.BaseSelf), NewSelf: wireInt(n.NewSelf),
			Combined: wireInt(n.Combined), DeltaAbs: wireInt(n.DeltaAbs), DeltaShare: wireFact(n.DeltaShare),
		}
		if n.DeltaPerUnit != nil {
			f := wireFact(*n.DeltaPerUnit)
			r.DeltaPerUnit = &f
		}
		out.Rows = append(out.Rows, r)
	}
	return s.respond("diff", shrinkWithLimit, out)
}
