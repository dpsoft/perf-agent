package perfmcp

import (
	"context"
	"encoding/json"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const pathSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "One of the view names open returned."},
    "node": {"type": "string", "description": "A node ID from top with rows=\"nodes\"."},
    "limit": {"type": "integer", "minimum": 1, "description": "Frames per page (default 200, at most the server's MaxRows)."},
    "cursor": {"type": "string", "description": "next_cursor of the previous page of this same query."}
  },
  "required": ["handle", "view", "node"],
  "additionalProperties": false
}`

// factsSchema is the schema of the tools that take only a node.
const factsSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "One of the view names open returned."},
    "node": {"type": "string", "description": "A node ID from top with rows=\"nodes\"."}
  },
  "required": ["handle", "view", "node"],
  "additionalProperties": false
}`

// defaultPathLimit is the page size of path when limit is omitted.
const defaultPathLimit = 200

const pathDescription = "The call path to a node, root first with the synthetic root left out, paged with limit and cursor, with each frame's own facts " +
	"(cumulative, self, shares of the total and of its parent). Domains are inferred. GPU views carry the sampling notes."

func (s *Server) pathTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name: "path", Title: "Call path of a node", Description: pathDescription,
		InputSchema: json.RawMessage(pathSchema), Annotations: readOnly, Handler: s.handlePath,
	}
}

type nodeArgs struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Node   profilemodel.NodeID `json:"node"`
}

type pathArgs struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Node   profilemodel.NodeID `json:"node"`
	Limit  *int                `json:"limit"`
	Cursor string              `json:"cursor"`
}

type pathQuery struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Node   profilemodel.NodeID `json:"node"`
}

type pathFrame struct {
	Node              profilemodel.NodeID `json:"node"`
	Name              string              `json:"name"`
	Module            string              `json:"module"`
	BuildID           string              `json:"build_id"`
	Depth             string              `json:"depth"`
	Domain            string              `json:"domain"`
	DomainInferred    bool                `json:"domain_inferred"`
	CollisionPossible bool                `json:"collision_possible"`
	Cumulative        Fact                `json:"cumulative"`
	Self              Fact                `json:"self"`
	ShareOfTotal      Fact                `json:"share_of_total"`
	ShareOfParent     Fact                `json:"share_of_parent"`
}

type pathResult struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Notes  []string            `json:"notes,omitzero"`
	Node   profilemodel.NodeID `json:"node"`
	Frames []pathFrame         `json:"frames"`
	pageInfo
}

func (s *Server) handlePath(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a pathArgs
	if err := decodeArgs("path", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	limit := min(defaultPathLimit, s.lim.MaxRows)
	if a.Limit != nil {
		var err error
		if limit, err = s.pageLimit(a.Limit); err != nil {
			return mcpstdio.Result{}, err
		}
	}
	_, v, err := s.store.view(a.Handle, a.View)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	n, err := nodeOf(v, a.Node, a.View)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	notes, err := viewNotes(v)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	bind := binding("path", pathQuery{Handle: a.Handle, View: a.View, Node: a.Node})
	pg, info, err := pageRows(v.Path(n), func(q *profilemodel.Node) int64 { return q.Cum }, "cum", bind, a.Cursor, limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	out := pathResult{Handle: a.Handle, View: a.View, Notes: notes, Node: n.ID, Frames: make([]pathFrame, 0, len(pg)), pageInfo: info}
	for _, q := range pg {
		c, err := v.CoreFacts(q.ID)
		if err != nil {
			return mcpstdio.Result{}, err
		}
		out.Frames = append(out.Frames, pathFrame{
			Node: q.ID, Name: q.Function.Name, Module: q.Function.Module.File, BuildID: q.Function.Module.BuildID,
			Depth:  wireInt(int64(q.Depth)),
			Domain: domainOf(q.Function.Name, q.Function.Module.File), DomainInferred: true,
			CollisionPossible: q.CollisionPossible,
			Cumulative:        wireFact(c.Cumulative), Self: wireFact(c.Self),
			ShareOfTotal: wireFact(c.ShareOfTotal), ShareOfParent: wireFact(c.ShareOfParent),
		})
	}
	return s.respond("path", shrinkWithLimit, out)
}
