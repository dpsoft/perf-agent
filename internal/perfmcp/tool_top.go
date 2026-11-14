package perfmcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// defaultLimit is the page size of the paged tools when limit is omitted.
const defaultLimit = 20

const topSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "One of the view names open returned."},
    "rows": {"type": "string", "enum": ["functions", "nodes"], "description": "functions aggregates by function (a recursive function counts once per sample); nodes lists call-tree nodes, which path and facts take."},
    "by": {"type": "string", "enum": ["cum", "self"], "description": "Order by cumulative (default) or self value."},
    "domain": {"type": "string", "description": "Keep only rows of this inferred domain (DOMAINS). Filtered rows are counted in filtered_out_count."},
    "limit": {"type": "integer", "minimum": 1, "description": "Rows per page (default 20, at most the server's MaxRows)."},
    "cursor": {"type": "string", "description": "next_cursor of the previous page of this same query."}
  },
  "required": ["handle", "view", "rows"],
  "additionalProperties": false
}`

const topDescription = "Rank the functions or the call-tree nodes of a view by cumulative or self value. " +
	"Integers are decimal strings; shares carry their denominator. A truncated list has next_cursor; omitted_weight is the sum of the omitted rows' own values " +
	"(basis in omitted_weight_basis); the rows overlap, so it is never a remaining share. Domains are inferred from names and modules. " +
	"GPU views carry the sampling notes."

func (s *Server) topTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name: "top", Title: "Top functions or nodes", Description: topDescription,
		InputSchema: json.RawMessage(strings.Replace(topSchema, "DOMAINS", strings.Join(rowDomains(), ", "), 1)), Annotations: readOnly, Handler: s.handleTop,
	}
}

type topArgs struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Rows   string              `json:"rows"`
	By     string              `json:"by"`
	Domain string              `json:"domain"`
	Limit  *int                `json:"limit"`
	Cursor string              `json:"cursor"`
}

// topQuery is every argument that identifies a top query but the page.
type topQuery struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Rows   string              `json:"rows"`
	By     string              `json:"by"`
	Domain string              `json:"domain"`
}

// pageInfo are the fields every truncated list carries.
type pageInfo struct {
	TotalRows          string `json:"total_rows"`
	Skipped            string `json:"skipped"`
	OmittedCount       string `json:"omitted_count"`
	OmittedWeight      string `json:"omitted_weight"`
	OmittedWeightBasis string `json:"omitted_weight_basis"`
	NextCursor         string `json:"next_cursor,omitempty"`
}

// pageRows pages rows as the cursor in cursor says, limit rows at a time.
// bind is the query's binding; weight is the value omitted_weight sums.
func pageRows[T any](rows []T, weight func(T) int64, basis, bind, cursor string, limit int) ([]T, pageInfo, error) {
	offset := 0
	if cursor != "" {
		offs, err := decodeCursor(cursor, bind, len(rows), 1)
		if err != nil {
			return nil, pageInfo{}, err
		}
		offset = offs[0]
	}
	p, err := page(rows, weight, offset, limit)
	if err != nil {
		return nil, pageInfo{}, err
	}
	info := pageInfo{
		TotalRows: wireInt(int64(len(rows))), Skipped: wireInt(int64(offset)),
		OmittedCount: wireInt(int64(p.OmittedCount)), OmittedWeight: p.OmittedWeight, OmittedWeightBasis: basis,
	}
	if p.Next >= 0 {
		info.NextCursor = encodeCursor(bind, p.Next)
	}
	return p.Rows, info, nil
}

// pageLimit is the validated page size of a limit argument.
func (s *Server) pageLimit(limit *int) (int, error) {
	if limit == nil {
		return min(defaultLimit, s.lim.MaxRows), nil
	}
	if *limit < 1 || *limit > s.lim.MaxRows {
		return 0, fmt.Errorf("limit must be between 1 and %d (this server's MaxRows), got %d", s.lim.MaxRows, *limit)
	}
	return *limit, nil
}

// rowDomains are the domains a row can have: every key but the synthetic
// root's, which no row has.
func rowDomains() []string {
	return slices.DeleteFunc(flamegraph.DomainKeys(), func(k string) bool { return k == "root" })
}

// domainOf is the inferred domain key of a frame.
func domainOf(name, module string) string {
	return flamegraph.Classify(name, module).Info().Key
}

// viewNotes are the view's sampling notes, non-nil on a GPU view and nil
// elsewhere, so a CPU response has no notes key and a GPU one always has.
func viewNotes(v *profilemodel.View) ([]string, error) {
	if !v.IsGPU() {
		return nil, nil
	}
	notes, err := v.Notes()
	if err != nil {
		return nil, fmt.Errorf("cannot compute the sampling notes of %s: %v", v.Handle, err)
	}
	return nonNil(notes), nil
}

// intWire is an integer fact in the view's unit as clients read it.
func intWire(metric string, v int64, unit string) Fact {
	f := Fact{Value: wireInt(v), Unit: unit, Status: string(profilemodel.StatusOK), Rounding: profilemodel.RoundingFor(metric)}
	if v == 0 {
		f.Status = string(profilemodel.StatusZero)
	}
	return f
}

// errNode is the error for a node the view does not have.
func errNode(node profilemodel.NodeID, view string, h profilemodel.Handle) error {
	return fmt.Errorf("node %s is not in view %s of handle %s; call top with rows=\"nodes\" or path to find node IDs", echo(string(node)), view, h)
}

// nodeOf returns the node id of v.
func nodeOf(v *profilemodel.View, id profilemodel.NodeID, view string) (*profilemodel.Node, error) {
	n, ok := v.Node(id)
	if !ok {
		return nil, errNode(id, view, v.Handle)
	}
	return n, nil
}

type nodeRow struct {
	Node         profilemodel.NodeID `json:"node"`
	Name         string              `json:"name"`
	Module       string              `json:"module"`
	Domain       string              `json:"domain"`
	Cumulative   Fact                `json:"cumulative"`
	Self         Fact                `json:"self"`
	ShareOfTotal Fact                `json:"share_of_total"`
}

type functionRow struct {
	Function     profilemodel.FunctionRef `json:"function"`
	Name         string                   `json:"name"`
	Module       string                   `json:"module"`
	Domain       string                   `json:"domain"`
	Cumulative   Fact                     `json:"cumulative"`
	Self         Fact                     `json:"self"`
	ShareOfTotal Fact                     `json:"share_of_total"`
}

type topResult[R any] struct {
	Handle           profilemodel.Handle `json:"handle"`
	View             string              `json:"view"`
	Notes            []string            `json:"notes,omitzero"`
	Rows             []R                 `json:"rows"`
	FilteredOutCount string              `json:"filtered_out_count"`
	pageInfo
}

// sortedNodes are v's nodes but the root, by the weight by and then by ID,
// cached on the entry.
func sortedNodes(e *entry, v *profilemodel.View, view, by string) []*profilemodel.Node {
	key := view + "|nodes|" + by
	if c, ok := e.sorted[key]; ok {
		return c.([]*profilemodel.Node)
	}
	var rows []*profilemodel.Node
	v.Walk(func(n *profilemodel.Node) bool {
		if n != v.Root {
			rows = append(rows, n)
		}
		return true
	})
	slices.SortFunc(rows, func(a, b *profilemodel.Node) int {
		return cmp.Or(cmp.Compare(nodeWeight(b, by), nodeWeight(a, by)), cmp.Compare(a.ID, b.ID))
	})
	if e.sorted == nil {
		e.sorted = map[string]any{}
	}
	e.sorted[key] = rows
	return rows
}

func nodeWeight(n *profilemodel.Node, by string) int64 {
	if by == "self" {
		return n.Self
	}
	return n.Cum
}

func functionWeight(r profilemodel.FunctionRow, by string) int64 {
	if by == "self" {
		return r.Self
	}
	return r.Cum
}

// sortedFunctions are v's functions by the weight by and then by reference,
// cached on the entry.
func sortedFunctions(e *entry, v *profilemodel.View, view, by string) ([]profilemodel.FunctionRow, error) {
	key := view + "|functions|" + by
	if c, ok := e.sorted[key]; ok {
		return c.([]profilemodel.FunctionRow), nil
	}
	rows, err := v.Functions()
	if err != nil {
		return nil, fmt.Errorf("cannot aggregate view %s by function: %v", view, err)
	}
	slices.SortFunc(rows, func(a, b profilemodel.FunctionRow) int {
		return cmp.Or(cmp.Compare(functionWeight(b, by), functionWeight(a, by)), cmp.Compare(a.Ref, b.Ref))
	})
	if e.sorted == nil {
		e.sorted = map[string]any{}
	}
	e.sorted[key] = rows
	return rows, nil
}

func (s *Server) handleTop(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a topArgs
	if err := decodeArgs("top", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	if a.Rows != "functions" && a.Rows != "nodes" {
		return mcpstdio.Result{}, fmt.Errorf("rows must be \"functions\" or \"nodes\", got %q", a.Rows)
	}
	by := cmp.Or(a.By, "cum")
	if by != "cum" && by != "self" {
		return mcpstdio.Result{}, fmt.Errorf("by must be \"cum\" or \"self\", got %q", a.By)
	}
	if a.Domain != "" && !slices.Contains(rowDomains(), a.Domain) {
		return mcpstdio.Result{}, fmt.Errorf("unknown domain %q; domain is one of: %s", a.Domain, strings.Join(rowDomains(), ", "))
	}
	limit, err := s.pageLimit(a.Limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	e, v, err := s.store.view(a.Handle, a.View)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	notes, err := viewNotes(v)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	bind := binding("top", topQuery{Handle: a.Handle, View: a.View, Rows: a.Rows, By: by, Domain: a.Domain})
	if a.Rows == "nodes" {
		all := sortedNodes(e, v, a.View, by)
		rows := all
		if a.Domain != "" {
			rows = slices.DeleteFunc(slices.Clone(all), func(n *profilemodel.Node) bool {
				return domainOf(n.Function.Name, n.Function.Module.File) != a.Domain
			})
		}
		pg, info, err := pageRows(rows, func(n *profilemodel.Node) int64 { return nodeWeight(n, by) }, by, bind, a.Cursor, limit)
		if err != nil {
			return mcpstdio.Result{}, err
		}
		out := topResult[nodeRow]{Handle: a.Handle, View: a.View, Notes: notes, Rows: make([]nodeRow, 0, len(pg)),
			FilteredOutCount: wireInt(int64(len(all) - len(rows))), pageInfo: info}
		for _, n := range pg {
			c, err := v.CoreFacts(n.ID)
			if err != nil {
				return mcpstdio.Result{}, err
			}
			out.Rows = append(out.Rows, nodeRow{
				Node: n.ID, Name: n.Function.Name, Module: n.Function.Module.File,
				Domain:     domainOf(n.Function.Name, n.Function.Module.File),
				Cumulative: wireFact(c.Cumulative), Self: wireFact(c.Self), ShareOfTotal: wireFact(c.ShareOfTotal),
			})
		}
		return s.respond("top", shrinkWithLimit, out)
	}

	all, err := sortedFunctions(e, v, a.View, by)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	rows := all
	if a.Domain != "" {
		rows = slices.DeleteFunc(slices.Clone(all), func(r profilemodel.FunctionRow) bool {
			return domainOf(r.Function.Name, r.Function.Module.File) != a.Domain
		})
	}
	pg, info, err := pageRows(rows, func(r profilemodel.FunctionRow) int64 { return functionWeight(r, by) }, by, bind, a.Cursor, limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	out := topResult[functionRow]{Handle: a.Handle, View: a.View, Notes: notes, Rows: make([]functionRow, 0, len(pg)),
		FilteredOutCount: wireInt(int64(len(all) - len(rows))), pageInfo: info}
	for _, r := range pg {
		out.Rows = append(out.Rows, functionRow{
			Function: r.Ref, Name: r.Function.Name, Module: r.Function.Module.File,
			Domain:       domainOf(r.Function.Name, r.Function.Module.File),
			Cumulative:   intWire(profilemodel.MetricCumulative, r.Cum, v.Unit),
			Self:         intWire(profilemodel.MetricSelf, r.Self, v.Unit),
			ShareOfTotal: wireFact(r.ShareOfTotal),
		})
	}
	return s.respond("top", shrinkWithLimit, out)
}
