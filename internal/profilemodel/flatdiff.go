package profilemodel

// UnionID identifies a union node: "<base node id>/<new node id>", with
// "-" for a side the node is absent from.
type UnionID string

func unionID(n *DiffNode) UnionID {
	b, w := string(n.BaseID), string(n.NewID)
	if b == "" {
		b = "-"
	}
	if w == "" {
		w = "-"
	}
	return UnionID(b + "/" + w)
}

// FlatDiffNode is a DiffNode without its tree links: it names its parent
// instead of holding its children. Copied fields keep DiffNode's JSON names.
type FlatDiffNode struct {
	ID     UnionID `json:"id"`
	Parent UnionID `json:"parent,omitempty"` // "" on the root
	Depth  int     `json:"depth"`

	BaseID         NodeID         `json:"base_id,omitempty"`
	NewID          NodeID         `json:"new_id,omitempty"`
	Name           string         `json:"name"`
	Presence       Presence       `json:"presence"`
	Match          Match          `json:"match,omitempty"`
	Candidates     []Candidate    `json:"candidates,omitzero"`
	Interpretation Interpretation `json:"interpretation"`
	Why            string         `json:"why,omitempty"`
	BaseCum        int64          `json:"base_cum"`
	NewCum         int64          `json:"new_cum"`
	BaseSelf       int64          `json:"base_self"`
	NewSelf        int64          `json:"new_self"`
	Combined       int64          `json:"combined"`
	DeltaAbs       int64          `json:"delta_abs"`
	DeltaShare     Fact           `json:"delta_share"`
	DeltaPerUnit   *Fact          `json:"delta_per_unit,omitempty"`
}

// FlatDiff is a Diff whose tree is a list of nodes in pre-order, children
// in Diff order, so every node follows its parent. Its JSON is a constant
// depth however deep the stacks are.
type FlatDiff struct {
	BaseHandle      Handle         `json:"base_handle"`
	NewHandle       Handle         `json:"new_handle"`
	View            string         `json:"view"`
	SampleType      string         `json:"sample_type"`
	Unit            string         `json:"unit"`
	BaseTotal       int64          `json:"base_total"`
	NewTotal        int64          `json:"new_total"`
	BaseQuality     Quality        `json:"base_quality"`
	NewQuality      Quality        `json:"new_quality"`
	QualityWarnings []string       `json:"quality_warnings"`
	BaseNotes       []string       `json:"base_notes"`
	NewNotes        []string       `json:"new_notes"`
	PerUnit         Availability   `json:"per_unit"`
	Nodes           []FlatDiffNode `json:"nodes"`
}

// Flatten lists the union in pre-order. The walk is iterative: a stack of
// recursion depth would be the stack depth of the profile.
func (d *Diff) Flatten() FlatDiff {
	f := FlatDiff{
		BaseHandle:      d.BaseHandle,
		NewHandle:       d.NewHandle,
		View:            d.View,
		SampleType:      d.SampleType,
		Unit:            d.Unit,
		BaseTotal:       d.BaseTotal,
		NewTotal:        d.NewTotal,
		BaseQuality:     d.BaseQuality,
		NewQuality:      d.NewQuality,
		QualityWarnings: d.QualityWarnings,
		BaseNotes:       d.BaseNotes,
		NewNotes:        d.NewNotes,
		PerUnit:         d.PerUnit,
	}
	if d.Root == nil {
		return f
	}
	type item struct {
		n      *DiffNode
		parent UnionID
		depth  int
	}
	stack := []item{{n: d.Root}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := it.n
		id := unionID(n)
		f.Nodes = append(f.Nodes, FlatDiffNode{
			ID: id, Parent: it.parent, Depth: it.depth,
			BaseID: n.BaseID, NewID: n.NewID, Name: n.Name,
			Presence: n.Presence, Match: n.Match, Candidates: n.Candidates,
			Interpretation: n.Interpretation, Why: n.Why,
			BaseCum: n.BaseCum, NewCum: n.NewCum, BaseSelf: n.BaseSelf, NewSelf: n.NewSelf,
			Combined: n.Combined, DeltaAbs: n.DeltaAbs,
			DeltaShare: n.DeltaShare, DeltaPerUnit: n.DeltaPerUnit,
		})
		// Pushed in reverse so the first child pops first.
		for i := len(n.Children) - 1; i >= 0; i-- {
			stack = append(stack, item{n: n.Children[i], parent: id, depth: it.depth + 1})
		}
	}
	return f
}
