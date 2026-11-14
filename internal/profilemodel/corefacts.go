package profilemodel

import "fmt"

// CoreFacts are the facts every node has on every view: the first five of
// PathFacts.
type CoreFacts struct {
	Node           NodeID `json:"node"`
	ParentNode     NodeID `json:"parent_node,omitempty"` // "" for the synthetic root
	Cumulative     Fact   `json:"cumulative"`
	Self           Fact   `json:"self"`
	ShareOfTotal   Fact   `json:"share_of_total"`
	ShareOfParent  Fact   `json:"share_of_parent"`
	InexactPortion Fact   `json:"inexact_portion"`
}

// CoreFacts returns the node-level facts of id; Facts is built on it.
func (v *View) CoreFacts(id NodeID) (CoreFacts, error) {
	n, ok := v.nodes[id]
	if !ok {
		return CoreFacts{}, fmt.Errorf("%w: %s", ErrUnknownNode, id)
	}
	c := CoreFacts{
		Node:           n.ID,
		Cumulative:     intFact(MetricCumulative, n.Cum, v.Unit),
		Self:           intFact(MetricSelf, n.Self, v.Unit),
		ShareOfTotal:   shareFact(MetricShareOfTotal, n.Cum, v.Total, DenominatorViewTotal),
		ShareOfParent:  unavailableFact(MetricShareOfParent, UnitRatio, ReasonNoParent),
		InexactPortion: intFact(MetricInexactPortion, n.Inexact, v.Unit),
	}
	if n.Parent != nil {
		c.ParentNode = n.Parent.ID
		c.ShareOfParent = shareFact(MetricShareOfParent, n.Cum, n.Parent.Cum, denominatorParent+string(n.Parent.ID))
	}
	return c, nil
}
