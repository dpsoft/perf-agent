package profilemodel

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"slices"
	"strings"
)

// Presence says on which side of a diff a union node exists.
type Presence string

const (
	PresenceBoth     Presence = "both"
	PresenceOnlyBase Presence = "only_base"
	PresenceOnlyNew  Presence = "only_new"
)

// Match says how a union node's base and new nodes were paired.
type Match string

const (
	// MatchExact pairs equal FunctionID.Key()s where both sides have a
	// build id.
	MatchExact Match = "exact"
	// MatchByPath pairs equal PathKey()s, real module files only, where at
	// least one side has no build id.
	MatchByPath Match = "by_path"
	// MatchByName pairs equal NameKey()s, one node on each side.
	MatchByName Match = "by_name"
	// MatchAmbiguous marks a node whose name key has several nodes on one
	// side or the other: it is not paired and lists its Candidates.
	MatchAmbiguous Match = "ambiguous"
)

// Interpretation says whether a union node's change can be read as a
// change in cost.
type Interpretation string

const (
	Comparable    Interpretation = "comparable"
	NotComparable Interpretation = "not_comparable"
)

// Sides of a diff, as a Candidate names them.
const (
	SideBase = "base"
	SideNew  = "new"
)

// Reasons a per-unit-of-work delta is unavailable.
const (
	ReasonNoWorkCount  = "no work count"                      // a count is 0, the zero value: not supplied
	ReasonBadWorkCount = "work count not positive and finite" // negative, NaN or infinite
	ReasonNonFinite    = "non-finite result"                  // the delta itself is not a finite float64
)

// Comparability thresholds: a share moving by more than this between the
// sides makes the nodes judged on it not comparable.
const (
	MaxUnresolvedShift = 0.10 // a module's unresolved share
	MaxJoinShift       = 0.10 // the gpu_join=exact share, GPU views only
	MaxSerializedShift = 0.10 // each gpu_serialized state's share, GPU views only
)

// DiffNode is one node of the union of two views.
//
// Weights are in the views' sample unit. Combined is BaseCum + NewCum and
// is additive: it equals the children's Combined plus BaseSelf + NewSelf,
// so the union can be drawn with it and a removed or added node keeps its
// width.
type DiffNode struct {
	Base   *Node  `json:"-"` // nil when only_new
	New    *Node  `json:"-"` // nil when only_base
	BaseID NodeID `json:"base_id,omitempty"`
	NewID  NodeID `json:"new_id,omitempty"`
	Name   string `json:"name"`
	// Presence is structural; a node on one side only stays in the union.
	Presence Presence `json:"presence"`
	// Match is how the node was paired. It is "" on the synthetic root,
	// which pairs by construction, and on a one-sided node with no node
	// of the same name key on the other side.
	Match Match `json:"match,omitempty"`
	// Candidates are the other side's nodes with the same name key, set
	// exactly when Match is "ambiguous".
	Candidates     []Candidate    `json:"candidates,omitzero"`
	Interpretation Interpretation `json:"interpretation"`
	// Why is the first cause when the node is not comparable. On a
	// comparable node it lists the comparisons that applied but could not
	// be made (a zero value on a side), "; "-separated, and is "" when
	// every one was made. Comparisons that do not apply to the view type,
	// the GPU-only rules on a CPU view, are reported once in
	// Diff.QualityWarnings, not per node.
	Why      string `json:"why,omitempty"`
	BaseCum  int64  `json:"base_cum"`
	NewCum   int64  `json:"new_cum"`
	BaseSelf int64  `json:"base_self"`
	NewSelf  int64  `json:"new_self"`
	Combined int64  `json:"combined"`
	DeltaAbs int64  `json:"delta_abs"` // NewCum - BaseCum
	// DeltaShare is the new share of the new total minus the base share of
	// the base total; unavailable "zero total" when either total is 0.
	DeltaShare Fact `json:"delta_share"`
	// DeltaPerUnit is NewCum/work.New - BaseCum/work.Base, in
	// "<sample unit>/work"; unavailable unless both work counts are
	// positive and finite and the result is finite.
	DeltaPerUnit Fact        `json:"delta_per_unit"`
	Children     []*DiffNode `json:"children,omitzero"` // by |DeltaAbs| desc, then Name
}

// Candidate names a node of one side of a diff. Side is "base" or "new":
// the same NodeID can occur in both views.
type Candidate struct {
	Side   string `json:"side"`
	Handle Handle `json:"handle"`
	Node   NodeID `json:"node"`
}

// WorkCounts are the units of work each side performed, supplied by the
// caller. The zero value of a count means it was not supplied; the diff
// never guesses one.
type WorkCounts struct{ Base, New float64 }

// Diff is the union of a base and a new view.
type Diff struct {
	BaseHandle  Handle  `json:"base_handle"`
	NewHandle   Handle  `json:"new_handle"`
	SampleType  string  `json:"sample_type"`
	Unit        string  `json:"unit"`
	BaseTotal   int64   `json:"base_total"`
	NewTotal    int64   `json:"new_total"`
	BaseQuality Quality `json:"base_quality"`
	NewQuality  Quality `json:"new_quality"`
	// QualityWarnings compares the two views as wholes: every module whose
	// unresolved share moved past the threshold, then the join coverage,
	// sample period and perturbation causes, then the comparisons that
	// could not be made. Empty, not nil, when there is nothing to say.
	QualityWarnings []string  `json:"quality_warnings"`
	Root            *DiffNode `json:"root"`
}

// ErrIncompatibleViews is returned when the views differ in sample type,
// unit, resolved sample index or CollapseVendorRuns.
var ErrIncompatibleViews = errors.New("profilemodel: views differ in sample type, unit or options")

// The two sides, as array indices.
const (
	baseSide = 0
	newSide  = 1
)

var sideNames = [2]string{SideBase, SideNew}

// Compare builds the union of base and new.
//
// Children are matched per matched parent, in order: exact, by_path,
// by_name, and what is left with several candidates is ambiguous (see
// Match). A node whose Cum is 0 on a side, reached only by zero-value
// samples, is absent from that side. Nothing below a one-sided node is
// matched.
//
// A node is judged on the quality of the nearest matched node at or above
// it, on each side; the root's is the view's. The synthetic root has no
// module, so the module rule is skipped for it: the view-level module
// comparison is in QualityWarnings. A node is not comparable when,
// in order: its module's unresolved share moves by more than
// MaxUnresolvedShift (the module is found on the other side by build id,
// else full path, else base name); in GPU views the gpu_join=exact share
// moves by more than MaxJoinShift; the sample periods differ; or, in GPU
// views, the perturbation policy fails: a gpu_serialized state's share
// moves by more than MaxSerializedShift, or perturbation is known on one
// side (true + false > 0) and entirely unknown or missing on the other. A
// share that is unavailable (a zero value, a CPU view) never takes part as
// 0: that comparison is recorded in Why as unavailable and does not by
// itself make the node not comparable. A one-sided node below a one-sided
// node that is not comparable is not comparable for the same reason: its
// absence follows from its parent's.
func Compare(baseView, newView *View, work WorkCounts) (*Diff, error) {
	if baseView.SampleType != newView.SampleType || baseView.Unit != newView.Unit ||
		baseView.Index != newView.Index || baseView.Options.CollapseVendorRuns != newView.Options.CollapseVendorRuns {
		return nil, fmt.Errorf("%w: base %s, new %s", ErrIncompatibleViews, describeView(baseView), describeView(newView))
	}
	d := &differ{views: [2]*View{baseView, newView}, work: [2]float64{work.Base, work.New}}
	roots := [2]*Node{baseView.Root, newView.Root}
	top, err := d.basisOf(roots)
	if err != nil {
		return nil, err
	}
	root, err := d.node(roots, "", nil, top, nil)
	if err != nil {
		return nil, err
	}
	return &Diff{
		BaseHandle:      baseView.Handle,
		NewHandle:       newView.Handle,
		SampleType:      baseView.SampleType,
		Unit:            baseView.Unit,
		BaseTotal:       baseView.Total,
		NewTotal:        newView.Total,
		BaseQuality:     top.q[baseSide],
		NewQuality:      top.q[newSide],
		QualityWarnings: d.viewWarnings(top),
		Root:            root,
	}, nil
}

func describeView(v *View) string {
	return fmt.Sprintf("%s/%s#%d collapse=%t", v.SampleType, v.Unit, v.Index, v.Options.CollapseVendorRuns)
}

type differ struct {
	views [2]*View
	work  [2]float64
}

func (d *differ) gpu() bool { return d.views[baseSide].SampleType == gpuSampleType }

// basis is a matched pair that nodes are judged on, with its quality and
// the findings that do not depend on a node's module. It is computed once
// per matched pair, the only NodeQuality call for it, and shared by every
// one-sided node below the pair.
type basis struct {
	q      [2]Quality
	value  [2]int64
	shared findings
}

func (d *differ) basisOf(pair [2]*Node) (*basis, error) {
	b := &basis{}
	for s, n := range pair {
		q, err := d.views[s].NodeQuality(n.ID)
		if err != nil {
			return nil, err
		}
		b.q[s], b.value[s] = q, n.Cum
	}
	d.sharedFindings(b)
	return b, nil
}

// findings are the causes that make a node not comparable, in rule order,
// and the comparisons that were unavailable.
type findings struct{ causes, notes []string }

func (f *findings) cause(format string, args ...any) {
	f.causes = append(f.causes, fmt.Sprintf(format, args...))
}

func (f *findings) note(format string, args ...any) {
	f.notes = append(f.notes, fmt.Sprintf(format, args...))
}

// node builds the union node for pair (one side may be nil) and its
// subtree. up is the basis of the nearest matched ancestor, parent the
// union parent (nil for the root).
func (d *differ) node(pair [2]*Node, match Match, cands []Candidate, up *basis, parent *DiffNode) (*DiffNode, error) {
	dn := &DiffNode{Base: pair[baseSide], New: pair[newSide], Match: match, Candidates: cands}
	var cum [2]int64
	for s, n := range pair {
		if n == nil {
			continue
		}
		cum[s] = n.Cum
		if dn.Name == "" {
			dn.Name = n.Function.Name
		}
	}
	if n := pair[baseSide]; n != nil {
		dn.BaseID, dn.BaseCum, dn.BaseSelf = n.ID, n.Cum, n.Self
	}
	if n := pair[newSide]; n != nil {
		dn.NewID, dn.NewCum, dn.NewSelf = n.ID, n.Cum, n.Self
	}
	var err error
	if dn.Combined, err = addChecked(cum[baseSide], cum[newSide], "combined weight"); err != nil {
		return nil, err
	}
	// Both are in [0, MaxInt64], so the difference cannot wrap.
	dn.DeltaAbs = cum[newSide] - cum[baseSide]
	dn.DeltaShare = d.shareDelta(cum)
	dn.DeltaPerUnit = d.perUnitDelta(cum)

	matched := pair[baseSide] != nil && pair[newSide] != nil
	here := up
	switch {
	case matched:
		dn.Presence = PresenceBoth
		if parent != nil {
			if here, err = d.basisOf(pair); err != nil {
				return nil, err
			}
		}
	case pair[baseSide] != nil:
		dn.Presence = PresenceOnlyBase
	default:
		dn.Presence = PresenceOnlyNew
	}

	if parent != nil && parent.Presence != PresenceBoth && parent.Interpretation == NotComparable {
		dn.Interpretation, dn.Why = NotComparable, parent.Why
	} else {
		dn.Interpretation, dn.Why = judge(here, pair, parent == nil)
	}

	for _, p := range matchChildren(pair, d.views) {
		c, err := d.node(p.nodes, p.match, p.cands, here, dn)
		if err != nil {
			return nil, err
		}
		dn.Children = append(dn.Children, c)
	}
	slices.SortFunc(dn.Children, func(a, b *DiffNode) int {
		return cmp.Or(
			cmp.Compare(absDelta(b.DeltaAbs), absDelta(a.DeltaAbs)),
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Presence, b.Presence),
			cmp.Compare(a.BaseID, b.BaseID),
			cmp.Compare(a.NewID, b.NewID),
		)
	})
	return dn, nil
}

func absDelta(x int64) int64 { return max(x, -x) }

func (d *differ) shareDelta(cum [2]int64) Fact {
	var share [2]float64
	for s, v := range d.views {
		if v.Total == 0 {
			f := unavailableFact(MetricShareDelta, UnitRatio, ReasonZeroTotal)
			f.Denominator = DenominatorViewTotal
			return f
		}
		share[s] = float64(cum[s]) / float64(v.Total)
	}
	delta := share[newSide] - share[baseSide]
	f := Fact{Ratio: new(delta), Unit: UnitRatio, Status: StatusOK, Denominator: DenominatorViewTotal, Rounding: RoundingFor(MetricShareDelta)}
	if delta == 0 {
		f.Status = StatusZero
	}
	return f
}

func (d *differ) perUnitDelta(cum [2]int64) Fact {
	unit := d.views[baseSide].Unit + "/work"
	for _, w := range d.work {
		if w == 0 {
			return unavailableFact(MetricPerWorkDelta, unit, ReasonNoWorkCount)
		}
	}
	for _, w := range d.work {
		if !(w > 0) || math.IsInf(w, 1) {
			return unavailableFact(MetricPerWorkDelta, unit, ReasonBadWorkCount)
		}
	}
	delta := float64(cum[newSide])/d.work[newSide] - float64(cum[baseSide])/d.work[baseSide]
	if math.IsInf(delta, 0) || math.IsNaN(delta) {
		return unavailableFact(MetricPerWorkDelta, unit, ReasonNonFinite)
	}
	f := Fact{Derived: new(delta), Unit: unit, Status: StatusOK, Rounding: RoundingFor(MetricPerWorkDelta)}
	if delta == 0 {
		f.Status = StatusZero
	}
	return f
}

// pairing is one union child before it is built.
type pairing struct {
	nodes [2]*Node
	match Match
	cands []Candidate
}

// matchChildren pairs the children of a matched pair, or lists the
// children of a one-sided node. Children with Cum 0 are absent.
func matchChildren(parents [2]*Node, views [2]*View) []pairing {
	var left [2][]*Node
	for s, p := range parents {
		if p == nil {
			continue
		}
		for _, c := range p.Children {
			if c.Cum > 0 {
				left[s] = append(left[s], c)
			}
		}
	}
	var out []pairing
	if parents[baseSide] == nil || parents[newSide] == nil {
		for s := range left {
			for _, n := range left[s] {
				var p pairing
				p.nodes[s] = n
				out = append(out, p)
			}
		}
		return out
	}

	used := map[*Node]bool{}
	pair := func(b, n *Node, m Match) {
		out = append(out, pairing{nodes: [2]*Node{b, n}, match: m})
		used[b], used[n] = true, true
	}
	unused := func() {
		for s := range left {
			left[s] = slices.DeleteFunc(left[s], func(n *Node) bool { return used[n] })
		}
	}

	// exact needs a build id on both sides: without one, Key() is the file
	// path, which two different builds can share. A parent's children have
	// distinct keys, so this is one to one.
	exact := map[string]*Node{}
	for _, n := range left[newSide] {
		if n.Function.Module.BuildID != "" {
			exact[n.Function.Key()] = n
		}
	}
	for _, b := range left[baseSide] {
		if b.Function.Module.BuildID == "" {
			continue
		}
		if n, ok := exact[b.Function.Key()]; ok {
			pair(b, n, MatchExact)
		}
	}
	unused()

	// by_path: one node per side at the path, not both with a build id
	// (two different build ids at one path are a rebuild: by_name). A path
	// with several nodes on a side is left to by_name, where it is
	// ambiguous.
	keys, groups := groupBy(left, FunctionID.PathKey)
	for _, k := range keys {
		g := groups[k]
		if len(g[baseSide]) == 1 && len(g[newSide]) == 1 &&
			(g[baseSide][0].Function.Module.BuildID == "" || g[newSide][0].Function.Module.BuildID == "") {
			pair(g[baseSide][0], g[newSide][0], MatchByPath)
		}
	}
	unused()

	keys, groups = groupBy(left, func(f FunctionID) (string, bool) { return f.NameKey(), true })
	for _, k := range keys {
		g := groups[k]
		switch {
		case len(g[baseSide]) == 1 && len(g[newSide]) == 1:
			pair(g[baseSide][0], g[newSide][0], MatchByName)
		case len(g[baseSide]) > 0 && len(g[newSide]) > 0:
			for s := range g {
				other := 1 - s
				cands := make([]Candidate, 0, len(g[other]))
				for _, c := range g[other] {
					cands = append(cands, Candidate{Side: sideNames[other], Handle: views[other].Handle, Node: c.ID})
				}
				for _, n := range g[s] {
					var p pairing
					p.nodes[s], p.match, p.cands = n, MatchAmbiguous, cands
					out = append(out, p)
				}
			}
		default:
			for s := range g {
				for _, n := range g[s] {
					var p pairing
					p.nodes[s] = n
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// groupBy groups each side's nodes by key, skipping nodes without one, and
// returns the keys sorted.
func groupBy(sides [2][]*Node, key func(FunctionID) (string, bool)) ([]string, map[string]*[2][]*Node) {
	groups := map[string]*[2][]*Node{}
	for s, nodes := range sides {
		for _, n := range nodes {
			k, ok := key(n.Function)
			if !ok {
				continue
			}
			g := groups[k]
			if g == nil {
				g = &[2][]*Node{}
				groups[k] = g
			}
			g[s] = append(g[s], n)
		}
	}
	return slices.Sorted(maps.Keys(groups)), groups
}

// judge decides a node's interpretation on basis b. The synthetic root is
// judged on the shared findings only: it has no module of its own, and
// its "no module" share would only repeat a view-level warning.
func judge(b *basis, pair [2]*Node, root bool) (Interpretation, string) {
	var f findings
	if !root {
		symbolFindings(&f, b, nodeModules(b, pair))
	}
	f.causes = append(f.causes, b.shared.causes...)
	f.notes = append(f.notes, b.shared.notes...)
	if len(f.causes) > 0 {
		return NotComparable, f.causes[0]
	}
	return Comparable, strings.Join(f.notes, "; ")
}

// moduleSides is a module named by label and, on each side, the
// UnresolvedByModule keys that stand for it.
type moduleSides struct {
	label string
	keys  [2][]string
}

// nodeModules finds a node's module on each side: its own key where the
// node exists, else the modules matching it on the other side.
func nodeModules(b *basis, pair [2]*Node) moduleSides {
	own := pair[baseSide]
	if own == nil {
		own = pair[newSide]
	}
	m := moduleSides{label: moduleLabel(own.Function.Module)}
	for s, n := range pair {
		if n != nil {
			m.keys[s] = []string{n.Function.Module.Key()}
		} else {
			m.keys[s] = matchingModules(b.q[s], own.Function.Module)
		}
	}
	return m
}

// matchingModules returns the UnresolvedModules keys of q that stand for
// m: those with m's build id, else at m's full path, else, for a real
// module, with m's base name. nil when none does.
func matchingModules(q Quality, m ModuleRef) []string {
	isReal := func(r ModuleRef) bool { return r.File != "" && r.Sentinel == "" }
	levels := []func(ModuleRef) bool{
		func(r ModuleRef) bool { return m.BuildID != "" && r.BuildID == m.BuildID },
		func(r ModuleRef) bool { return r.File == m.File },
		func(r ModuleRef) bool { return isReal(m) && isReal(r) && path.Base(r.File) == path.Base(m.File) },
	}
	keys := slices.Sorted(maps.Keys(q.UnresolvedModules))
	for _, matches := range levels {
		var out []string
		for _, k := range keys {
			if matches(q.UnresolvedModules[k]) {
				out = append(out, k)
			}
		}
		if out != nil {
			return out
		}
	}
	return nil
}

// moduleLabel names a module in a reason: its file's base name, its
// sentinel, or "no module".
func moduleLabel(m ModuleRef) string {
	switch {
	case m.File == "":
		return "no module"
	case m.Sentinel != "":
		return m.Sentinel
	}
	return path.Base(m.File)
}

// symbolFindings compares the module's unresolved share. A side with no
// key for it has a share of 0, which is a measurement only while that
// side's Unresolved is available. With several keys on a side (modules
// sharing a base name) the largest shift counts.
func symbolFindings(f *findings, b *basis, m moduleSides) {
	for s := range b.q {
		if u := b.q[s].Unresolved; u.Status == StatusUnavailable {
			f.note("symbols of %s: comparison unavailable in %s: %s", m.label, sideNames[s], u.Reason)
			return
		}
	}
	var shares [2][]float64
	for s, keys := range m.keys {
		if len(keys) == 0 {
			shares[s] = []float64{0}
		}
		for _, k := range keys {
			shares[s] = append(shares[s], ratioOf(b.q[s].UnresolvedByModule[k]))
		}
	}
	worst, at := -1.0, [2]float64{}
	for _, x := range shares[baseSide] {
		for _, y := range shares[newSide] {
			if shift := math.Abs(y - x); shift > worst {
				worst, at = shift, [2]float64{x, y}
			}
		}
	}
	if worst > MaxUnresolvedShift {
		f.cause("symbols of %s: unresolved share %.3f in base, %.3f in new", m.label, at[baseSide], at[newSide])
	}
}

// ratioOf is an available share's value; a missing entry is 0.
func ratioOf(f Fact) float64 {
	if f.Ratio == nil {
		return 0
	}
	return *f.Ratio
}

// unavailableSide returns the first side whose fact is unavailable.
func unavailableSide(facts [2]Fact) (int, bool) {
	for s, f := range facts {
		if f.Status == StatusUnavailable {
			return s, true
		}
	}
	return 0, false
}

// sharedFindings sets b.shared: the join coverage, sample period and
// perturbation comparisons, which do not depend on a node's module.
func (d *differ) sharedFindings(b *basis) {
	f := &b.shared
	if d.gpu() {
		join := [2]Fact{b.q[baseSide].JoinExact, b.q[newSide].JoinExact}
		if s, ok := unavailableSide(join); ok {
			f.note("join coverage: comparison unavailable in %s: %s", sideNames[s], join[s].Reason)
		} else if x, y := ratioOf(join[baseSide]), ratioOf(join[newSide]); math.Abs(y-x) > MaxJoinShift {
			f.cause("join coverage: exact share %.3f in base, %.3f in new", x, y)
		}
	}

	// A side with no value carries no weight to compare periods on, though
	// its zero-value samples may still name one.
	if s := slices.Index(b.value[:], 0); s >= 0 {
		f.note("sample period: comparison unavailable in %s: %s", sideNames[s], ReasonZeroTotal)
	} else if p, q := b.q[baseSide].SamplePeriods, b.q[newSide].SamplePeriods; !slices.Equal(p, q) {
		f.cause("sample period: %s in base, %s in new", periodList(p), periodList(q))
	}

	if d.gpu() {
		perturbationFindings(f, b)
	}
}

func periodList(p []string) string {
	if len(p) == 0 {
		return "none"
	}
	return strings.Join(p, ", ")
}

// perturbationFindings applies the perturbation policy: all four
// gpu_serialized states are compared.
func perturbationFindings(f *findings, b *basis) {
	states := [2][serializedStates]Fact{}
	for s := range b.q {
		ser := b.q[s].Serialized
		states[s] = [serializedStates]Fact{ser.True, ser.False, ser.Unknown, ser.Missing}
		for _, st := range states[s] {
			if st.Status == StatusUnavailable {
				f.note("perturbation: comparison unavailable in %s: %s", sideNames[s], st.Reason)
				return
			}
		}
	}
	names := [serializedStates]string{"true", "false", "unknown", "missing"}
	for i, name := range names {
		if x, y := ratioOf(states[baseSide][i]), ratioOf(states[newSide][i]); math.Abs(y-x) > MaxSerializedShift {
			f.cause("perturbation: serialized %s share %.3f in base, %.3f in new", name, x, y)
			return
		}
	}
	var known [2]bool
	for s := range states {
		known[s] = ratioOf(states[s][serializedTrue]) > 0 || ratioOf(states[s][serializedFalse]) > 0
	}
	if known[baseSide] != known[newSide] {
		k := baseSide
		if known[newSide] {
			k = newSide
		}
		f.cause("perturbation: known in %s, only unknown or missing in %s", sideNames[k], sideNames[1-k])
	}
}

// viewWarnings compares the views as wholes, on the root basis: every
// module with an unresolved frame on either side, then the shared causes,
// then every comparison that could not be made.
func (d *differ) viewWarnings(top *basis) []string {
	var f findings
	unresolved := [2]Fact{top.q[baseSide].Unresolved, top.q[newSide].Unresolved}
	if s, ok := unavailableSide(unresolved); ok {
		f.note("symbols: comparison unavailable in %s: %s", sideNames[s], unresolved[s].Reason)
	} else {
		reached := [2]map[string]bool{{}, {}}
		for s := range top.q {
			other := 1 - s
			for _, k := range slices.Sorted(maps.Keys(top.q[s].UnresolvedModules)) {
				if reached[s][k] {
					continue
				}
				mod := top.q[s].UnresolvedModules[k]
				m := moduleSides{label: moduleLabel(mod)}
				m.keys[s] = []string{k}
				m.keys[other] = matchingModules(top.q[other], mod)
				for _, ok := range m.keys[other] {
					reached[other][ok] = true
				}
				symbolFindings(&f, top, m)
			}
		}
	}
	f.causes = append(f.causes, top.shared.causes...)
	f.notes = append(f.notes, top.shared.notes...)
	if !d.gpu() {
		f.note("perturbation: comparison unavailable: %s", ReasonNotGPUView)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, w := range slices.Concat(f.causes, f.notes) {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}
