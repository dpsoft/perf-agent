package perfmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const factsDescription = "Every fact about one node: values, shares, executions below it, kernel names observed, serialization, sample periods, " +
	"domain (inferred), the node's quality (shares of the node's own value) and the number of call sites. " +
	"view_scope.unsampled_portion and the notes describe the whole view, never this node. A zero fact has status \"zero\"; an unavailable one has a reason and no value."

func (s *Server) factsTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name: "facts", Title: "Facts about a node", Description: factsDescription,
		InputSchema: json.RawMessage(factsSchema), Annotations: readOnly, Handler: s.handleFacts,
	}
}

// viewScope holds values that describe the whole view, never the node.
type viewScope struct {
	UnsampledPortion Fact `json:"unsampled_portion"`
}

// shrinkFacts is the advice for a facts response over MaxResponseBytes: the
// node's own path names are what make it large, and path pages them.
const shrinkFacts = "restart perf-agent-mcp with a larger --limit MaxResponseBytes=N; " +
	"the path names and unresolved_by_module entries make it large, and path returns the frames page by page"

type factsResult struct {
	Handle              profilemodel.Handle `json:"handle"`
	View                string              `json:"view"`
	Node                profilemodel.NodeID `json:"node"`
	ParentNode          profilemodel.NodeID `json:"parent_node,omitempty"`
	Path                []string            `json:"path"`
	Cumulative          Fact                `json:"cumulative"`
	Self                Fact                `json:"self"`
	ShareOfTotal        Fact                `json:"share_of_total"`
	ShareOfParent       Fact                `json:"share_of_parent"`
	ExecutionsBelow     Fact                `json:"executions_below"`
	KernelNamesObserved Fact                `json:"kernel_names_observed"`
	InexactPortion      Fact                `json:"inexact_portion"`
	Serialization       Serialization       `json:"serialization"`
	SamplePeriods       []string            `json:"sample_periods"`
	Domain              string              `json:"domain"`
	DomainInferred      bool                `json:"domain_inferred"`
	CollisionPossible   bool                `json:"collision_possible"`
	ViewScope           viewScope           `json:"view_scope"`
	Notes               []string            `json:"notes,omitzero"`
	Quality             Quality             `json:"quality"`
	CallSites           string              `json:"call_sites"`
}

func (s *Server) handleFacts(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a nodeArgs
	if err := decodeArgs("facts", raw, &a); err != nil {
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
	pf, err := v.Facts(n.ID, domainOf)
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("cannot compute the facts of node %s: %v", n.ID, err)
	}
	q, err := v.NodeQuality(n.ID)
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("cannot compute the quality of node %s: %v", n.ID, err)
	}
	notes, err := viewNotes(v)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	out := factsResult{
		Handle: a.Handle, View: a.View, Node: pf.Node, ParentNode: pf.ParentNode, Path: nonNil(pf.Path),
		Cumulative: wireFact(pf.Cumulative), Self: wireFact(pf.Self),
		ShareOfTotal: wireFact(pf.ShareOfTotal), ShareOfParent: wireFact(pf.ShareOfParent),
		ExecutionsBelow: wireFact(pf.ExecutionsBelow), KernelNamesObserved: wireFact(pf.KernelNamesObserved),
		InexactPortion: wireFact(pf.InexactPortion), ViewScope: viewScope{UnsampledPortion: wireFact(pf.UnsampledPortion)},
		Serialization: wireSerialization(pf.Serialization), SamplePeriods: nonNil(pf.SamplePeriods),
		Domain: pf.Domain, DomainInferred: pf.DomainInferred, CollisionPossible: pf.CollisionPossible,
		Notes: notes, Quality: wireQuality(q), CallSites: wireInt(int64(len(n.CallSites))),
	}
	return s.respond("facts", shrinkFacts, out)
}
