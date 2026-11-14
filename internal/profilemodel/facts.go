package profilemodel

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

// Status says whether a fact has a value. Zero and unavailable are
// different: "zero" is a measured 0, "unavailable" means the source needed
// for the fact is absent or unusable, and Fact.Reason says why.
type Status string

const (
	StatusOK          Status = "ok"
	StatusZero        Status = "zero"
	StatusUnavailable Status = "unavailable"
)

// Reasons a fact is unavailable, besides the executions log reasons
// (ReasonNoSidecar and the rest), which ExecutionsBelow carries verbatim.
const (
	ReasonZeroTotal  = "zero total"     // a share whose denominator is 0
	ReasonNoParent   = "no parent"      // share of parent on the synthetic root
	ReasonNotGPUView = "not a GPU view" // a GPU-only fact on another view
)

// Denominators a share names.
const (
	DenominatorViewTotal = "view_total"
	denominatorParent    = "parent:" // followed by the parent's NodeID
)

// Units that are not the view's sample unit.
const (
	UnitRatio = "ratio"
	UnitCount = "count"
)

// Metric names, as structured claims name them. RoundingFor takes these.
const (
	MetricCumulative           = "cumulative"
	MetricSelf                 = "self"
	MetricShareOfTotal         = "share_of_total"
	MetricShareOfParent        = "share_of_parent"
	MetricExecutionsBelow      = "executions_below"
	MetricKernelNamesObserved  = "kernel_names_observed"
	MetricInexactPortion       = "inexact_portion"
	MetricUnsampledPortion     = "unsampled_portion"
	MetricSerializationTrue    = "serialization_true"
	MetricSerializationFalse   = "serialization_false"
	MetricSerializationUnknown = "serialization_unknown"
	MetricSerializationMissing = "serialization_missing"
	MetricPerWorkDelta         = "per_work_delta"
)

// RoundingFor is the engine's display rounding for metric, and the only
// tolerance a claim about that metric is checked against:
//
//	share_of_total, share_of_parent     "0.001 ratio" (0.1 percentage point)
//	cumulative, self, inexact_portion,
//	unsampled_portion,
//	serialization_true, _false,
//	_unknown, _missing                  "exact" (integers in the sample unit)
//	executions_below,
//	kernel_names_observed               "exact" (counts)
//	per_work_delta                      "3 significant figures"
//
// Values in the sample unit and counts are exact integers, so a claim about
// one must state it exactly. It returns "" for a metric it does not know.
func RoundingFor(metric string) string {
	switch metric {
	case MetricShareOfTotal, MetricShareOfParent:
		return "0.001 ratio"
	case MetricCumulative, MetricSelf, MetricInexactPortion, MetricUnsampledPortion,
		MetricSerializationTrue, MetricSerializationFalse, MetricSerializationUnknown, MetricSerializationMissing,
		MetricExecutionsBelow, MetricKernelNamesObserved:
		return "exact"
	case MetricPerWorkDelta:
		return "3 significant figures"
	}
	return ""
}

// Fact is one value with its unit, status and the engine's rounding.
// Exactly one of Int, Ratio and Derived is set unless Status is
// "unavailable", when none is and Reason says why.
type Fact struct {
	Int         *int64   `json:"int,omitempty"`     // exact value for sample units and counts
	Ratio       *float64 `json:"ratio,omitempty"`   // set only when Unit == "ratio"
	Derived     *float64 `json:"derived,omitempty"` // finite derived metric with its own unit, e.g. "<sample unit>/work"
	Unit        string   `json:"unit"`              // sample unit, "ratio", "count" or "<sample unit>/work"
	Status      Status   `json:"status"`
	Reason      string   `json:"reason,omitempty"`      // set exactly when unavailable
	Denominator string   `json:"denominator,omitempty"` // "view_total" or "parent:<node id>", for shares
	Rounding    string   `json:"rounding"`              // RoundingFor the fact's metric
}

func intFact(metric string, v int64, unit string) Fact {
	f := Fact{Int: new(v), Unit: unit, Status: StatusOK, Rounding: RoundingFor(metric)}
	if v == 0 {
		f.Status = StatusZero
	}
	return f
}

func unavailableFact(metric, unit, reason string) Fact {
	return Fact{Unit: unit, Status: StatusUnavailable, Reason: reason, Rounding: RoundingFor(metric)}
}

// shareFact is part/whole, unavailable with ReasonZeroTotal when whole is 0
// so it is never NaN. Callers pass 0 <= part <= whole.
func shareFact(metric string, part, whole int64, denominator string) Fact {
	if whole == 0 {
		f := unavailableFact(metric, UnitRatio, ReasonZeroTotal)
		f.Denominator = denominator
		return f
	}
	f := Fact{Ratio: new(float64(part) / float64(whole)), Unit: UnitRatio, Status: StatusOK, Denominator: denominator, Rounding: RoundingFor(metric)}
	if part == 0 {
		f.Status = StatusZero
	}
	return f
}

// Classifier returns the domain key of a frame from its display name and
// module file ("" when it has none). A nil Classifier gives "unknown".
type Classifier func(name, module string) string

// Serialization is the value under a node by its samples' gpu_serialized
// label. Unknown also holds samples whose label is not exactly one of
// "true", "false" and "unknown"; Missing holds samples without the label.
type Serialization struct {
	True    Fact `json:"true"`
	False   Fact `json:"false"`
	Unknown Fact `json:"unknown"`
	Missing Fact `json:"missing"`
}

// PathFacts are the facts about one node of a view.
type PathFacts struct {
	Handle     Handle   `json:"handle"`
	View       string   `json:"view"` // "<type>/<unit>#<index>"
	Node       NodeID   `json:"node"`
	ParentNode NodeID   `json:"parent_node,omitempty"` // "" for the synthetic root
	Path       []string `json:"path"`                  // display names root-first, the synthetic root excluded
	// Cumulative and Self are in the view's sample unit.
	Cumulative    Fact `json:"cumulative"`
	Self          Fact `json:"self"`
	ShareOfTotal  Fact `json:"share_of_total"`  // Cumulative / view total
	ShareOfParent Fact `json:"share_of_parent"` // Cumulative / parent's cumulative; unavailable on the root
	// ExecutionsBelow counts distinct execution ordinals in the checked
	// executions log over every sample through the node, zero-value ones
	// included; unavailable with the log's reason when it is not usable.
	ExecutionsBelow Fact `json:"executions_below"`
	// KernelNamesObserved counts distinct "[gpu:kernel:<name>]" names at or
	// below the node in any sample through it, zero-value samples included:
	// names observed, not binary-identical kernels. GPU views only; else
	// unavailable "not a GPU view", since no other profile has kernel frames.
	KernelNamesObserved Fact `json:"kernel_names_observed"`
	// InexactPortion is the part of Cumulative from samples matching
	// foldedstacks.DefaultInexactLabels.
	InexactPortion Fact `json:"inexact_portion"`
	// UnsampledPortion is the view-level value under
	// foldedstacks.UnsampledLaunchFrame, never redistributed to any node.
	// GPU views only; else unavailable "not a GPU view".
	UnsampledPortion Fact `json:"unsampled_portion"`
	// Serialization is GPU views only; else all four are unavailable. On a
	// GPU view the four states sum to Cumulative.
	Serialization Serialization `json:"serialization"`
	// SamplePeriods are the distinct gpu_sample_period values on samples
	// through the node, zero-value ones included: integers first, in
	// numeric order, then any other values in string order.
	SamplePeriods     []string `json:"sample_periods"`
	Domain            string   `json:"domain"`             // the Classifier's key, or "unknown"
	DomainInferred    bool     `json:"domain_inferred"`    // always true: domains are a heuristic
	CollisionPossible bool     `json:"collision_possible"` // as Node.CollisionPossible
	// Notes is foldedstacks.SamplingNotes for the whole view, the same text
	// Fold warns with, on GPU views with a non-zero total; nil otherwise.
	Notes []string `json:"notes"`
}

// ErrUnknownNode is returned for a NodeID the view does not have.
var ErrUnknownNode = errors.New("profilemodel: no such node in the view")

// gpuSampleType is the sample type of a GPU view.
const gpuSampleType = "gpu"

// serializedLabel is the perturbation label gpu/projection.go sets on every
// execution.
const serializedLabel = "gpu_serialized"

// kernelFramePrefix starts the frame gpu/projection.go emits for a kernel,
// "[gpu:kernel:<name>]".
const kernelFramePrefix = "[gpu:kernel:"

// Facts returns the facts about node id. Nothing is scaled: values stay in
// the view's sample unit, and launch sampling is stated in Notes rather
// than corrected for.
func (v *View) Facts(id NodeID, classify Classifier) (PathFacts, error) {
	n, ok := v.nodes[id]
	if !ok {
		return PathFacts{}, fmt.Errorf("%w: %s", ErrUnknownNode, id)
	}
	path := v.Path(n)
	names := make([]string, 0, len(path))
	for _, q := range path {
		names = append(names, q.Function.Name)
	}
	pf := PathFacts{
		Handle:            v.Handle,
		View:              fmt.Sprintf("%s/%s#%d", v.SampleType, v.Unit, v.Index),
		Node:              n.ID,
		Path:              names,
		Cumulative:        intFact(MetricCumulative, n.Cum, v.Unit),
		Self:              intFact(MetricSelf, n.Self, v.Unit),
		ShareOfTotal:      shareFact(MetricShareOfTotal, n.Cum, v.Total, DenominatorViewTotal),
		ShareOfParent:     unavailableFact(MetricShareOfParent, UnitRatio, ReasonNoParent),
		InexactPortion:    intFact(MetricInexactPortion, n.Inexact, v.Unit),
		Domain:            "unknown",
		DomainInferred:    true,
		CollisionPossible: n.CollisionPossible,
	}
	if n.Parent != nil {
		pf.ParentNode = n.Parent.ID
		pf.ShareOfParent = shareFact(MetricShareOfParent, n.Cum, n.Parent.Cum, denominatorParent+string(n.Parent.ID))
	}
	if classify != nil {
		pf.Domain = classify(n.Function.Name, n.Function.Module.File)
	}

	observed := slices.Concat(n.samples, n.zeroSamples)
	pf.ExecutionsBelow = v.executionsBelow(observed)
	pf.SamplePeriods = v.samplePeriods(observed)

	if v.SampleType != gpuSampleType {
		pf.KernelNamesObserved = unavailableFact(MetricKernelNamesObserved, UnitCount, ReasonNotGPUView)
		pf.UnsampledPortion = unavailableFact(MetricUnsampledPortion, v.Unit, ReasonNotGPUView)
		pf.Serialization = Serialization{
			True:    unavailableFact(MetricSerializationTrue, v.Unit, ReasonNotGPUView),
			False:   unavailableFact(MetricSerializationFalse, v.Unit, ReasonNotGPUView),
			Unknown: unavailableFact(MetricSerializationUnknown, v.Unit, ReasonNotGPUView),
			Missing: unavailableFact(MetricSerializationMissing, v.Unit, ReasonNotGPUView),
		}
		return pf, nil
	}
	pf.KernelNamesObserved = intFact(MetricKernelNamesObserved, int64(v.kernelNamesBelow(n, observed)), UnitCount)
	unsampled, err := v.unsampledTotal()
	if err != nil {
		return PathFacts{}, err
	}
	pf.UnsampledPortion = intFact(MetricUnsampledPortion, unsampled, v.Unit)
	if pf.Serialization, err = v.serialization(n); err != nil {
		return PathFacts{}, err
	}
	if pf.Notes, err = v.samplingNotes(unsampled); err != nil {
		return PathFacts{}, err
	}
	return pf, nil
}

// executionsBelow counts the distinct ordinals the checked log names for
// samples. Ordinals are never inferred from sample counts.
func (v *View) executionsBelow(samples []int) Fact {
	ex := v.profile.Executions
	if !ex.OK {
		return unavailableFact(MetricExecutionsBelow, UnitCount, ex.Reason)
	}
	seen := map[uint64]struct{}{}
	for _, s := range samples {
		for _, e := range v.profile.execOf[s] {
			seen[e] = struct{}{}
		}
	}
	return intFact(MetricExecutionsBelow, int64(len(seen)), UnitCount)
}

// kernelNamesBelow counts the distinct kernel names at or below n over
// samples. The node at depth d is frame d-1 of every sample through it, so
// frames from there on are the ones below it.
func (v *View) kernelNamesBelow(n *Node, samples []int) int {
	b := newBuilder()
	from := max(0, n.Depth-1)
	names := map[string]struct{}{}
	for _, s := range samples {
		frames := b.framesOf(v.profile.Source.Sample[s])
		for _, fr := range frames[min(from, len(frames)):] {
			if name, ok := strings.CutPrefix(fr.fn.Name, kernelFramePrefix); ok {
				if name, ok = strings.CutSuffix(name, "]"); ok {
					names[name] = struct{}{}
				}
			}
		}
	}
	return len(names)
}

// samplePeriods returns the distinct gpu_sample_period values on samples:
// unsigned integers first, in numeric order (ties, such as "8" and "08",
// by string), then every other value in string order. That is a total
// order, so the result is the same for any map iteration.
func (v *View) samplePeriods(samples []int) []string {
	set := map[string]struct{}{}
	for _, s := range samples {
		for _, p := range v.profile.Source.Sample[s].Label[foldedstacks.SamplePeriodLabel] {
			set[p] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return slices.SortedFunc(maps.Keys(set), func(a, b string) int {
		x, errA := strconv.ParseUint(a, 10, 64)
		y, errB := strconv.ParseUint(b, 10, 64)
		switch {
		case errA == nil && errB == nil:
			return cmp.Or(cmp.Compare(x, y), cmp.Compare(a, b))
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		}
		return cmp.Compare(a, b)
	})
}

// unsampledTotal is the value under foldedstacks.UnsampledLaunchFrame as the
// stack's first frame, the population Fold reports as unsampled.
func (v *View) unsampledTotal() (int64, error) {
	var sum int64
	for _, c := range v.Root.Children {
		if c.Function.Name != foldedstacks.UnsampledLaunchFrame {
			continue
		}
		var err error
		if sum, err = addChecked(sum, c.Cum, "unsampled value"); err != nil {
			return 0, err
		}
	}
	return sum, nil
}

// serialization splits n's value by the samples' gpu_serialized label.
func (v *View) serialization(n *Node) (Serialization, error) {
	var t, f, u, m int64
	for _, s := range n.samples {
		smp := v.profile.Source.Sample[s]
		val := smp.Value[v.Index]
		vals := smp.Label[serializedLabel]
		acc := &u
		switch {
		case len(vals) == 0:
			acc = &m
		case len(vals) == 1 && vals[0] == "true":
			acc = &t
		case len(vals) == 1 && vals[0] == "false":
			acc = &f
		}
		var err error
		if *acc, err = addChecked(*acc, val, "serialization value"); err != nil {
			return Serialization{}, err
		}
	}
	return Serialization{
		True:    intFact(MetricSerializationTrue, t, v.Unit),
		False:   intFact(MetricSerializationFalse, f, v.Unit),
		Unknown: intFact(MetricSerializationUnknown, u, v.Unit),
		Missing: intFact(MetricSerializationMissing, m, v.Unit),
	}, nil
}

// samplingNotes states launch sampling for the view exactly as Fold does:
// the period is the gpu_sample_period value with the largest total (ties
// to the smaller string), counted over every sample carrying the label,
// and nothing is said when the view's total is 0.
func (v *View) samplingNotes(unsampled int64) ([]string, error) {
	if v.Total == 0 {
		return nil, nil
	}
	totals := map[string]int64{}
	carrying := 0
	for _, s := range v.profile.Source.Sample {
		vals, ok := s.Label[foldedstacks.SamplePeriodLabel]
		if !ok {
			continue
		}
		carrying++
		for _, p := range vals {
			sum, err := addChecked(totals[p], s.Value[v.Index], "sample period value")
			if err != nil {
				return nil, err
			}
			totals[p] = sum
		}
	}
	period, found := "", false
	for p, total := range totals {
		if !found || total > totals[period] || (total == totals[period] && p < period) {
			period, found = p, true
		}
	}
	return foldedstacks.SamplingNotes(unsampled, v.Total, period, carrying), nil
}
