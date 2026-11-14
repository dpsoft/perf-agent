package perfmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const sandwichSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "One of the view names open returned."},
    "function": {"type": "string", "description": "A function reference from top with rows=\"functions\"."},
    "limit": {"type": "integer", "minimum": 1, "description": "Rows per list per page (default 20, at most the server's MaxRows)."},
    "cursor": {"type": "string", "description": "next_cursor of callers or callees of the previous page of this same query."}
  },
  "required": ["handle", "view", "function"],
  "additionalProperties": false
}`

const sandwichDescription = "A function with its immediate callers and callees, each list largest value first and paged with limit and its own next_cursor " +
	"(a cursor advances one list and keeps the other where it was). " + sandwichRule + " GPU views carry the sampling notes."

// sandwichRule is stated in every sandwich result.
const sandwichRule = "each caller and callee is counted once per sample; under recursion rows overlap and may sum to more than the function"

func (s *Server) sandwichTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name: "sandwich", Title: "Callers and callees of a function", Description: sandwichDescription,
		InputSchema: json.RawMessage(sandwichSchema), Annotations: readOnly, Handler: s.handleSandwich,
	}
}

type sandwichArgs struct {
	Handle   profilemodel.Handle      `json:"handle"`
	View     string                   `json:"view"`
	Function profilemodel.FunctionRef `json:"function"`
	Limit    *int                     `json:"limit"`
	Cursor   string                   `json:"cursor"`
}

type sandwichQuery struct {
	Handle   profilemodel.Handle      `json:"handle"`
	View     string                   `json:"view"`
	Function profilemodel.FunctionRef `json:"function"`
}

type edgeRow struct {
	Function        profilemodel.FunctionRef `json:"function"`
	Name            string                   `json:"name"`
	Module          string                   `json:"module"`
	Domain          string                   `json:"domain"`
	Value           Fact                     `json:"value"`
	ShareOfFunction Fact                     `json:"share_of_function"`
}

// edgeList is one truncated list of a sandwich.
type edgeList struct {
	Rows []edgeRow `json:"rows"`
	pageInfo
}

type sandwichResult struct {
	Handle    profilemodel.Handle `json:"handle"`
	View      string              `json:"view"`
	Notes     []string            `json:"notes,omitzero"`
	Function  functionRow         `json:"function"`
	Recursive bool                `json:"recursive"`
	RootValue string              `json:"root_value"`
	Rule      string              `json:"rule"`
	Callers   edgeList            `json:"callers"`
	Callees   edgeList            `json:"callees"`
}

// sandwichSlot is the one sandwich an entry keeps, so paging a function
// does not scan the view again for every page.
type sandwichSlot struct {
	key string
	sw  profilemodel.Sandwich
}

const sandwichSlotKey = "sandwich"

func (s *Server) handleSandwich(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a sandwichArgs
	if err := decodeArgs("sandwich", raw, &a); err != nil {
		return mcpstdio.Result{}, err
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
	key := a.View + "|" + string(a.Function)
	var sw profilemodel.Sandwich
	if c, ok := e.sorted[sandwichSlotKey].(sandwichSlot); ok && c.key == key {
		sw = c.sw
	} else {
		if sw, err = v.Sandwich(a.Function); err != nil {
			if errors.Is(err, profilemodel.ErrUnknownFunction) {
				return mcpstdio.Result{}, fmt.Errorf("function %s is not in view %s; call top with rows=\"functions\" to list function refs", echo(string(a.Function)), a.View)
			}
			return mcpstdio.Result{}, fmt.Errorf("cannot compute the sandwich of function %s: %v", echo(string(a.Function)), err)
		}
		if e.sorted == nil {
			e.sorted = map[string]any{}
		}
		e.sorted[sandwichSlotKey] = sandwichSlot{key: key, sw: sw}
	}

	bind := binding("sandwich", sandwichQuery{Handle: a.Handle, View: a.View, Function: a.Function})
	var offs [2]int
	if a.Cursor != "" {
		// One bound serves both lists; each offset is checked against its own list.
		got, err := decodeCursor(a.Cursor, bind, max(len(sw.Callers), len(sw.Callees)), 2)
		if err != nil {
			return mcpstdio.Result{}, err
		}
		if got[0] > len(sw.Callers) || got[1] > len(sw.Callees) {
			return mcpstdio.Result{}, fmt.Errorf("cursor %q is not valid here (an offset is beyond its list); repeat the query without cursor to start from the first page", echo(a.Cursor))
		}
		offs = [2]int{got[0], got[1]}
	}
	callers, err := pageEdges(sw.Callers, offs, 0, bind, limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	callees, err := pageEdges(sw.Callees, offs, 1, bind, limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}

	f := sw.Function
	out := sandwichResult{
		Handle: a.Handle, View: a.View, Notes: notes,
		Function: functionRow{
			Function: f.Ref, Name: f.Function.Name, Module: f.Function.Module.File,
			Domain:     domainOf(f.Function.Name, f.Function.Module.File),
			Cumulative: intWire(profilemodel.MetricCumulative, f.Cum, v.Unit),
			Self:       intWire(profilemodel.MetricSelf, f.Self, v.Unit), ShareOfTotal: wireFact(f.ShareOfTotal),
		},
		Recursive: sw.Recursive, RootValue: wireInt(sw.RootValue), Rule: sandwichRule,
		Callers: callers.list(v.Unit), Callees: callees.list(v.Unit),
	}
	return s.respond("sandwich", shrinkWithLimit, out)
}

// edgePage is one page of one list and the cursor of its next page.
type edgePage struct {
	rows []profilemodel.EdgeRow
	info pageInfo
}

// pageEdges pages the list at index which of a sandwich's two. Its next
// cursor advances that list and keeps the other at its current offset.
func pageEdges(rows []profilemodel.EdgeRow, offs [2]int, which int, bind string, limit int) (edgePage, error) {
	p, err := page(rows, func(r profilemodel.EdgeRow) int64 { return r.Value }, offs[which], limit)
	if err != nil {
		return edgePage{}, err
	}
	info := pageInfo{
		TotalRows: wireInt(int64(len(rows))), Skipped: wireInt(int64(offs[which])),
		OmittedCount: wireInt(int64(p.OmittedCount)), OmittedWeight: p.OmittedWeight, OmittedWeightBasis: "value",
	}
	if p.Next >= 0 {
		next := offs
		next[which] = p.Next
		info.NextCursor = encodeCursor(bind, next[0], next[1])
	}
	return edgePage{rows: p.Rows, info: info}, nil
}

func (p edgePage) list(unit string) edgeList {
	out := edgeList{Rows: make([]edgeRow, 0, len(p.rows)), pageInfo: p.info}
	for _, r := range p.rows {
		out.Rows = append(out.Rows, edgeRow{
			Function: r.Ref, Name: r.Function.Name, Module: r.Function.Module.File,
			Domain:          domainOf(r.Function.Name, r.Function.Module.File),
			Value:           intWire(profilemodel.MetricCumulative, r.Value, unit),
			ShareOfFunction: wireFact(r.ShareOfFunction),
		})
	}
	return out
}
