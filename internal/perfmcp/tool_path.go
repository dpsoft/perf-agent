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
    "node": {"type": "string", "description": "A node ID from top with rows=\"nodes\"."}
  },
  "required": ["handle", "view", "node"],
  "additionalProperties": false
}`

const pathDescription = "The call path to a node, root first with the synthetic root left out, with each frame's own facts " +
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
}

func (s *Server) handlePath(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a nodeArgs
	if err := decodeArgs("path", raw, &a); err != nil {
		return mcpstdio.Result{}, err
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
	out := pathResult{Handle: a.Handle, View: a.View, Notes: notes, Node: n.ID, Frames: []pathFrame{}}
	for _, q := range v.Path(n) {
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
