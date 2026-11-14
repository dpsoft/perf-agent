package perfmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const kernelsSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "A GPU view name open returned."},
    "node": {"type": "string", "description": "A node ID from top with rows=\"nodes\"."},
    "limit": {"type": "integer", "minimum": 1, "description": "Rows per page (default 20, at most the server's MaxRows)."},
    "cursor": {"type": "string", "description": "next_cursor of the previous page of this same query."}
  },
  "required": ["handle", "view", "node"],
  "additionalProperties": false
}`

const kernelsDescription = "The GPU kernels at or below a node of a GPU view, largest value first, with the value, its share of the node and of the view, " +
	"and the executions the checked executions log counts for each (unavailable without a usable log; never inferred from samples). " +
	"Values are never scaled by the launch sampling period. The sampling notes are attached."

func (s *Server) kernelsTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name: "gpu_kernels_for", Title: "GPU kernels under a node", Description: kernelsDescription,
		InputSchema: json.RawMessage(kernelsSchema), Annotations: readOnly, Handler: s.handleKernels,
	}
}

type kernelsArgs struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Node   profilemodel.NodeID `json:"node"`
	Limit  *int                `json:"limit"`
	Cursor string              `json:"cursor"`
}

type kernelsQuery struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Node   profilemodel.NodeID `json:"node"`
}

type kernelRow struct {
	Kernel       string `json:"kernel"`
	Value        Fact   `json:"value"`
	ShareOfNode  Fact   `json:"share_of_node"`
	ShareOfTotal Fact   `json:"share_of_total"`
	Executions   Fact   `json:"executions"`
}

type kernelsResult struct {
	Handle profilemodel.Handle `json:"handle"`
	View   string              `json:"view"`
	Notes  []string            `json:"notes,omitzero"`
	Node   profilemodel.NodeID `json:"node"`
	Rows   []kernelRow         `json:"rows"`
	pageInfo
}

func (s *Server) handleKernels(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a kernelsArgs
	if err := decodeArgs("gpu_kernels_for", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	limit, err := s.pageLimit(a.Limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	_, v, err := s.store.view(a.Handle, a.View)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	if _, err := nodeOf(v, a.Node, a.View); err != nil {
		return mcpstdio.Result{}, err
	}
	rows, err := v.KernelsBelow(a.Node)
	if errors.Is(err, profilemodel.ErrNotGPUView) {
		return mcpstdio.Result{}, errors.New("node is in a cpu view; gpu_kernels_for needs a GPU view (open returned the views)")
	}
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("cannot list the kernels below node %s: %v", a.Node, err)
	}
	notes, err := viewNotes(v)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	bind := binding("gpu_kernels_for", kernelsQuery{Handle: a.Handle, View: a.View, Node: a.Node})
	pg, info, err := pageRows(rows, func(r profilemodel.KernelRow) int64 { return *r.Value.Int }, "value", bind, a.Cursor, limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	out := kernelsResult{Handle: a.Handle, View: a.View, Notes: notes, Node: a.Node, Rows: make([]kernelRow, 0, len(pg)), pageInfo: info}
	for _, r := range pg {
		out.Rows = append(out.Rows, kernelRow{
			Kernel: r.Name, Value: wireFact(r.Value), ShareOfNode: wireFact(r.ShareOfNode),
			ShareOfTotal: wireFact(r.ShareOfTotal), Executions: wireFact(r.Executions),
		})
	}
	return s.respond("gpu_kernels_for", shrinkWithLimit, out)
}
