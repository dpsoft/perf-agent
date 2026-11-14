package explain

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// nodeFacts are the node-level metrics read from View.Facts.
var nodeFacts = map[string]func(*profilemodel.PathFacts) profilemodel.Fact{
	profilemodel.MetricCumulative:           func(p *profilemodel.PathFacts) profilemodel.Fact { return p.Cumulative },
	profilemodel.MetricSelf:                 func(p *profilemodel.PathFacts) profilemodel.Fact { return p.Self },
	profilemodel.MetricShareOfTotal:         func(p *profilemodel.PathFacts) profilemodel.Fact { return p.ShareOfTotal },
	profilemodel.MetricShareOfParent:        func(p *profilemodel.PathFacts) profilemodel.Fact { return p.ShareOfParent },
	profilemodel.MetricExecutionsBelow:      func(p *profilemodel.PathFacts) profilemodel.Fact { return p.ExecutionsBelow },
	profilemodel.MetricKernelNamesObserved:  func(p *profilemodel.PathFacts) profilemodel.Fact { return p.KernelNamesObserved },
	profilemodel.MetricInexactPortion:       func(p *profilemodel.PathFacts) profilemodel.Fact { return p.InexactPortion },
	profilemodel.MetricSerializationTrue:    func(p *profilemodel.PathFacts) profilemodel.Fact { return p.Serialization.True },
	profilemodel.MetricSerializationFalse:   func(p *profilemodel.PathFacts) profilemodel.Fact { return p.Serialization.False },
	profilemodel.MetricSerializationUnknown: func(p *profilemodel.PathFacts) profilemodel.Fact { return p.Serialization.Unknown },
	profilemodel.MetricSerializationMissing: func(p *profilemodel.PathFacts) profilemodel.Fact { return p.Serialization.Missing },
}

// qualityFacts are the quality metrics, read from NodeQuality at node scope
// and from Quality at view scope. quality_unresolved_by_module is not here:
// it also needs a module key (see moduleFact).
var qualityFacts = map[string]func(*profilemodel.Quality) profilemodel.Fact{
	profilemodel.MetricQualityJoinExact:         func(q *profilemodel.Quality) profilemodel.Fact { return q.JoinExact },
	profilemodel.MetricQualityJoinHeuristic:     func(q *profilemodel.Quality) profilemodel.Fact { return q.JoinHeuristic },
	profilemodel.MetricQualityJoinUnmatched:     func(q *profilemodel.Quality) profilemodel.Fact { return q.JoinUnmatched },
	profilemodel.MetricQualityAmbiguous:         func(q *profilemodel.Quality) profilemodel.Fact { return q.Ambiguous },
	profilemodel.MetricQualityGraphRefused:      func(q *profilemodel.Quality) profilemodel.Fact { return q.GraphRefused },
	profilemodel.MetricQualityUnsampled:         func(q *profilemodel.Quality) profilemodel.Fact { return q.UnsampledLaunches },
	profilemodel.MetricQualitySerializedTrue:    func(q *profilemodel.Quality) profilemodel.Fact { return q.Serialized.True },
	profilemodel.MetricQualitySerializedFalse:   func(q *profilemodel.Quality) profilemodel.Fact { return q.Serialized.False },
	profilemodel.MetricQualitySerializedUnknown: func(q *profilemodel.Quality) profilemodel.Fact { return q.Serialized.Unknown },
	profilemodel.MetricQualitySerializedMissing: func(q *profilemodel.Quality) profilemodel.Fact { return q.Serialized.Missing },
	profilemodel.MetricQualityUnresolved:        func(q *profilemodel.Quality) profilemodel.Fact { return q.Unresolved },
	profilemodel.MetricQualityCollision:         func(q *profilemodel.Quality) profilemodel.Fact { return q.CollisionPossible },
}

// isQuality reports whether metric is a quality metric, valid at both
// scopes.
func isQuality(metric string) bool {
	_, ok := qualityFacts[metric]
	return ok || metric == profilemodel.MetricQualityUnresolvedByModule
}

// isDiffMetric reports whether metric only exists between two profiles.
func isDiffMetric(metric string) bool {
	return metric == profilemodel.MetricShareDelta || metric == profilemodel.MetricPerWorkDelta
}

// claimable is every metric a claim can name, sorted, for messages.
var claimable = func() []string {
	ms := slices.Concat(slices.Collect(maps.Keys(nodeFacts)), slices.Collect(maps.Keys(qualityFacts)),
		[]string{profilemodel.MetricQualityUnresolvedByModule, profilemodel.MetricUnsampledPortion})
	slices.Sort(ms)
	return ms
}()

// Check validates explanations against v and, when nothing fails, returns
// them with the engine's values in place of the agent's. Per claim the rules
// run in this order and the first failing one is reported: a well-formed ID
// unique across the call; a known, claimable metric at a scope it has; a
// module exactly for quality_unresolved_by_module; a node in the view; an
// available fact; the fact's unit; the fact's denominator, exactly; a finite
// decimal value in the metric's range, an integer for exact metrics; and
// the value at the engine's rounding (profilemodel.PolicyFor) equal to the
// fact's canonical value, compared as decimals. Every explanation's node
// and every "{cN}" and "{n:…}" placeholder in its text must resolve. Any
// limit exceeded is reported instead of all of that. Every failure is
// reported, and nothing is accepted when there is one. v must not be nil.
func Check(v *profilemodel.View, exps []Explanation, lim Limits) Result {
	r := Result{}
	for _, e := range exps {
		r.Figures += len(e.Claims)
	}
	if r.Failures = limitFailures(exps, lim); len(r.Failures) > 0 {
		return r
	}
	c := &checker{
		v:       v,
		facts:   map[profilemodel.NodeID]factsOrErr{},
		quality: map[profilemodel.NodeID]qualityOrErr{},
		ids:     map[string]string{},
	}
	checked := make([]Checked, len(exps))
	for i, e := range exps {
		checked[i] = c.explanation(i, e)
	}
	refs := map[profilemodel.NodeID]struct{}{}
	for i, e := range exps {
		ch := &checked[i]
		ch.Prose = c.text(i, e.Prose, refs)
		for _, h := range e.Hypotheses {
			ch.Hypotheses = append(ch.Hypotheses, c.text(i, h, refs))
		}
		ch.NextMeasurement = c.text(i, e.NextMeasurement, refs)
	}
	r.References = len(refs)
	r.Failures = c.failures
	if len(r.Failures) == 0 {
		r.OK, r.Checked = true, checked
	}
	return r
}

// limitFailures reports every limit exceeded. The call-wide counts are
// checked first and alone, so a huge input yields one failure, not one per
// item.
func limitFailures(exps []Explanation, lim Limits) []Failure {
	over := func(i int, name, what string, got, limit int) Failure {
		return Failure{Explanation: i, Ref: name, Code: CodeLimitExceeded,
			Message: fmt.Sprintf("%s is %d, over the server's %s of %d: send less, or start the server with a higher %s", what, got, name, limit, name)}
	}
	if len(exps) > lim.MaxExplanations {
		return []Failure{over(-1, "MaxExplanations", "the number of explanations", len(exps), lim.MaxExplanations)}
	}
	total := 0
	for _, e := range exps {
		total += len(e.Claims)
	}
	if total > lim.MaxClaimsTotal {
		return []Failure{over(-1, "MaxClaimsTotal", "the number of claims in the call", total, lim.MaxClaimsTotal)}
	}
	var out []Failure
	for i, e := range exps {
		if len(e.Claims) > lim.MaxClaimsPerExplanation {
			out = append(out, over(i, "MaxClaimsPerExplanation", "the number of claims", len(e.Claims), lim.MaxClaimsPerExplanation))
		}
		if len(e.Prose) > lim.MaxProseBytes {
			out = append(out, over(i, "MaxProseBytes", "the prose's size in bytes", len(e.Prose), lim.MaxProseBytes))
		}
		if len(e.Hypotheses) > lim.MaxHypotheses {
			out = append(out, over(i, "MaxHypotheses", "the number of hypotheses", len(e.Hypotheses), lim.MaxHypotheses))
		} else {
			for j, h := range e.Hypotheses {
				if len(h) > lim.MaxHypothesisBytes {
					out = append(out, over(i, "MaxHypothesisBytes", fmt.Sprintf("hypothesis %d's size in bytes", j), len(h), lim.MaxHypothesisBytes))
				}
			}
		}
		if len(e.NextMeasurement) > lim.MaxNextBytes {
			out = append(out, over(i, "MaxNextBytes", "next_measurement's size in bytes", len(e.NextMeasurement), lim.MaxNextBytes))
		}
	}
	return out
}

type factsOrErr struct {
	pf  profilemodel.PathFacts
	err error
}

type qualityOrErr struct {
	q   profilemodel.Quality
	err error
}

// checker holds one Check's state. Facts and qualities are computed once
// per node, since many claims name the same one.
type checker struct {
	v           *profilemodel.View
	facts       map[profilemodel.NodeID]factsOrErr
	quality     map[profilemodel.NodeID]qualityOrErr
	viewQuality *profilemodel.Quality
	ids         map[string]string // claim ID -> where it was first defined
	failures    []Failure
}

func (c *checker) viewName() string {
	return fmt.Sprintf("%s/%s#%d", c.v.SampleType, c.v.Unit, c.v.Index)
}

func (c *checker) pathFacts(id profilemodel.NodeID) (profilemodel.PathFacts, error) {
	got, ok := c.facts[id]
	if !ok {
		got.pf, got.err = c.v.Facts(id, nil)
		c.facts[id] = got
	}
	return got.pf, got.err
}

func (c *checker) nodeQuality(id profilemodel.NodeID) (profilemodel.Quality, error) {
	got, ok := c.quality[id]
	if !ok {
		got.q, got.err = c.v.NodeQuality(id)
		c.quality[id] = got
	}
	return got.q, got.err
}

func (c *checker) wholeQuality() profilemodel.Quality {
	if c.viewQuality == nil {
		c.viewQuality = new(c.v.Quality())
	}
	return *c.viewQuality
}

func (c *checker) fail(f Failure) { c.failures = append(c.failures, f) }

// explanation checks e's node and claims.
func (c *checker) explanation(i int, e Explanation) Checked {
	ch := Checked{Node: profilemodel.NodeID(e.Node)}
	switch _, ok := c.v.Node(ch.Node); {
	case e.Node == "":
		c.fail(Failure{Explanation: i, Code: CodeUnknownNode,
			Message: fmt.Sprintf(`explanations[%d] has no "node": set it to the ID of the node it explains, from top, path or facts`, i)})
	case !ok:
		c.fail(Failure{Explanation: i, Ref: e.Node, Code: CodeUnknownNode,
			Message: fmt.Sprintf("explanations[%d]: node %q is not in view %s: take node IDs from top, path or facts on this view", i, e.Node, c.viewName())})
	default:
		pf, err := c.pathFacts(ch.Node)
		if err != nil {
			c.fail(Failure{Explanation: i, Ref: e.Node, Code: CodeUnavailableFact,
				Message: fmt.Sprintf("explanations[%d]: the engine could not compute the facts of node %s: %v", i, e.Node, err)})
		}
		ch.Facts = pf
	}
	for j, cl := range e.Claims {
		if cc, ok := c.claim(i, j, e, cl); ok {
			ch.Claims = append(ch.Claims, cc)
		}
	}
	return ch
}

// text tokenizes s, checks its placeholders resolve and adds the nodes it
// names to refs.
func (c *checker) text(i int, s string, refs map[profilemodel.NodeID]struct{}) []Segment {
	segs := Tokenize(s)
	for _, sg := range segs {
		switch sg.Kind {
		case SegmentClaim:
			if _, ok := c.ids[sg.Claim]; !ok {
				c.fail(Failure{Explanation: i, Ref: sg.Claim, Code: CodeUnresolvedClaimRef,
					Message: fmt.Sprintf("%s names no claim in this call: add a claim with id %q, or drop the placeholder", sg.Text, sg.Claim)})
			}
		case SegmentNode:
			refs[sg.Node] = struct{}{}
			if _, ok := c.v.Node(sg.Node); !ok {
				c.fail(Failure{Explanation: i, Ref: string(sg.Node), Code: CodeUnresolvedNodeRef,
					Message: fmt.Sprintf("%s is not a node of view %s: take node IDs from top, path or facts on this view", sg.Text, c.viewName())})
			}
		}
	}
	return segs
}

// claimError is a claim failure found while looking up its fact.
type claimError struct {
	code Code
	msg  string
}

func (e *claimError) Error() string { return e.msg }

// target is what a claim is about once its scope and node are settled.
type target struct {
	metric, scope string
	node          profilemodel.NodeID // "" at view scope
	module        *string
}

// claim checks one claim; see Check for the rules and their order.
func (c *checker) claim(i, j int, e Explanation, cl Claim) (CheckedClaim, bool) {
	path := fmt.Sprintf("explanations[%d].claims[%d]", i, j)
	var (
		engine *EngineValue
		ref    string
	)
	fail := func(code Code, format string, args ...any) (CheckedClaim, bool) {
		c.fail(Failure{Explanation: i, Claim: cl.ID, Ref: ref, Code: code, Message: fmt.Sprintf(format, args...), Agent: cl.Value.Text, Engine: engine})
		return CheckedClaim{}, false
	}

	if !validClaimID(cl.ID) {
		return fail(CodeBadClaimID, "%s: claim id %q does not match ^[A-Za-z][A-Za-z0-9_-]{0,31}$", path, cl.ID)
	}
	if first, dup := c.ids[cl.ID]; dup {
		return fail(CodeDuplicateClaimID, "%s: claim id %q is already used by %s: give each claim its own id", path, cl.ID, first)
	}
	c.ids[cl.ID] = path

	t := target{metric: cl.Metric, scope: cl.Scope, module: cl.Module}
	switch {
	case isDiffMetric(t.metric):
		return fail(CodeDiffMetric, "%s compares two profiles: diff reports it, and a claim about one view cannot state it", t.metric)
	case nodeFacts[t.metric] == nil && !isQuality(t.metric) && t.metric != profilemodel.MetricUnsampledPortion:
		return fail(CodeUnknownMetric, "metric %q is not a fact a claim can state; one of: %s", t.metric, strings.Join(claimable, ", "))
	}
	switch t.scope {
	case "", ScopeNode:
		t.scope = ScopeNode
		if t.metric == profilemodel.MetricUnsampledPortion {
			return fail(CodeWrongScope, `unsampled_portion is a view-level fact: claim it with "scope": "view" and no node`)
		}
	case ScopeView:
		if !isQuality(t.metric) && t.metric != profilemodel.MetricUnsampledPortion {
			return fail(CodeWrongScope, `%s is a node-level fact: claim it with "scope": "node" (the default) and a node`, t.metric)
		}
		if cl.Node != "" {
			return fail(CodeWrongScope, `a view-level claim takes no node: drop "node" or claim the node-level fact with "scope": "node"`)
		}
	default:
		return fail(CodeWrongScope, `scope must be "node" or "view", not %q`, t.scope)
	}
	if t.module != nil && t.metric != profilemodel.MetricQualityUnresolvedByModule {
		return fail(CodeModuleNotApplicable, `"module" applies only to %s: drop it from this %s claim`, profilemodel.MetricQualityUnresolvedByModule, t.metric)
	}
	if t.scope == ScopeNode {
		t.node = profilemodel.NodeID(cmp.Or(cl.Node, e.Node))
		if _, ok := c.v.Node(t.node); !ok {
			if t.node == "" {
				return fail(CodeUnknownNode, `%s has no "node" and neither has its explanation: name the node the claim is about`, path)
			}
			ref = string(t.node)
			return fail(CodeUnknownNode, "%s: node %q is not in view %s: take node IDs from top, path or facts on this view", path, t.node, c.viewName())
		}
	}

	fact, err := c.engineFact(t)
	if err != nil {
		if ce, ok := errors.AsType[*claimError](err); ok {
			return fail(ce.code, "%s", ce.msg)
		}
		return fail(CodeUnavailableFact, "the engine could not compute %s: %v", t.metric, err)
	}
	policy, _ := profilemodel.PolicyFor(t.metric)
	ev := EngineValue{Metric: t.metric, Scope: t.scope, Node: t.node, Denominator: fact.Denominator,
		Unit: fact.Unit, Rounding: policy.String(), Status: fact.Status, Reason: fact.Reason}
	if t.module != nil {
		ev.Module = new(*t.module) // a copy: the result never aliases the caller's claims
	}
	want, available := fact.Canonical()
	if available {
		ev.Value = want
	}
	engine = &ev

	if fact.Status == profilemodel.StatusUnavailable || !available {
		return fail(CodeUnavailableFact, "%s is unavailable here: %s", t.metric, cmp.Or(fact.Reason, "the engine has no value"))
	}
	switch {
	case cl.Unit == "":
		return fail(CodeWrongUnit, `"unit" is required: %s is in %s`, t.metric, fact.Unit)
	case cl.Unit != fact.Unit:
		return fail(CodeWrongUnit, "unit is %q, but %s is in %s", cl.Unit, t.metric, fact.Unit)
	}
	switch {
	case fact.Denominator == "" && cl.Denominator != "":
		return fail(CodeWrongDenominator, `%s has no denominator: drop "denominator"`, t.metric)
	case cl.Denominator == "" && fact.Denominator != "":
		return fail(CodeWrongDenominator, `"denominator" is required: %s here is a share of %s`, t.metric, fact.Denominator)
	case cl.Denominator != fact.Denominator:
		return fail(CodeWrongDenominator, "denominator is %q, but %s here is a share of %s", cl.Denominator, t.metric, fact.Denominator)
	}

	x, code, msg := parseValue(cl.Value)
	if code != "" {
		return fail(code, "%s", msg)
	}
	switch {
	case fact.Unit == profilemodel.UnitRatio && (x.Sign() < 0 || x.Cmp(big.NewRat(1, 1)) > 0):
		return fail(CodeOutOfRange, "%s is a share and lies in [0, 1]; %s does not", t.metric, cl.Value.Text)
	case x.Sign() < 0:
		return fail(CodeOutOfRange, "%s is never negative; %s is", t.metric, cl.Value.Text)
	case policy.Kind == profilemodel.RoundExact && !x.IsInt():
		return fail(CodeNotInteger, "%s is an exact integer in %s; %s is not an integer: state it exactly, digits only", t.metric, fact.Unit, cl.Value.Text)
	}
	got, ok := policy.Canonical(x)
	if !ok || got != want {
		return fail(CodeValueMismatch, "%s is %s at the engine's rounding (%s); %s is %s at that rounding", t.metric, want, policy, cl.Value.Text, cmp.Or(got, "not representable"))
	}
	return CheckedClaim{ID: cl.ID, EngineValue: ev}, true
}

// engineFact maps a claim's metric and scope to the engine's fact:
// unsampled_portion at view scope to View.UnsampledPortion, quality metrics
// to Quality (view) or NodeQuality (node), every other metric to its field
// of View.Facts.
func (c *checker) engineFact(t target) (profilemodel.Fact, error) {
	if t.scope == ScopeView {
		if t.metric == profilemodel.MetricUnsampledPortion {
			return c.v.UnsampledPortion()
		}
		return qualityFact(c.wholeQuality(), t)
	}
	if isQuality(t.metric) {
		q, err := c.nodeQuality(t.node)
		if err != nil {
			return profilemodel.Fact{}, err
		}
		return qualityFact(q, t)
	}
	pf, err := c.pathFacts(t.node)
	if err != nil {
		return profilemodel.Fact{}, err
	}
	return nodeFacts[t.metric](&pf), nil
}

// qualityFact reads t's metric from q. unresolved_by_module needs a module
// that is a key of q.UnresolvedByModule; "" is the "no module" key.
func qualityFact(q profilemodel.Quality, t target) (profilemodel.Fact, error) {
	if get, ok := qualityFacts[t.metric]; ok {
		return get(&q), nil
	}
	keys := "none: no frame here is unresolved"
	if len(q.UnresolvedByModule) > 0 {
		var quoted []string
		for _, k := range slices.Sorted(maps.Keys(q.UnresolvedByModule)) {
			quoted = append(quoted, fmt.Sprintf("%q", k))
		}
		keys = strings.Join(quoted, ", ")
	}
	if t.module == nil {
		return profilemodel.Fact{}, &claimError{CodeUnknownModule,
			fmt.Sprintf(`%s needs "module", a key of unresolved_by_module ("" is no module); the keys here are: %s`, t.metric, keys)}
	}
	f, ok := q.UnresolvedByModule[*t.module]
	if !ok {
		return profilemodel.Fact{}, &claimError{CodeUnknownModule,
			fmt.Sprintf("module %q has no unresolved frames here; the keys of unresolved_by_module are: %s", *t.module, keys)}
	}
	return f, nil
}

// maxIntDigits, maxFracDigits and maxExpDigits bound the decimal grammar a
// value must match before big.Rat parses it, so no value can make the
// check do unbounded arithmetic.
const (
	maxIntDigits  = 40
	maxFracDigits = 40
	maxExpDigits  = 3
)

// parseValue parses a claim's value literal: a JSON number or a string,
// either matching [+-]?D{1,40}(.D{1,40})?([eE][+-]?D{1,3})? with ASCII
// digits D. A NaN or infinity spelling, in any case and with any sign, is
// non_finite; anything else, including fractions like "1/3" and radix
// forms like "0x10" that big.Rat would accept, is bad_value.
func parseValue(l Literal) (*big.Rat, Code, string) {
	switch l.Kind {
	case "":
		return nil, CodeBadValue, `the claim has no value: send "value" as a JSON number or a decimal string`
	case LiteralOther:
		return nil, CodeBadValue, fmt.Sprintf("value %s is not a number: send a JSON number or a decimal string", l.Text)
	}
	if isNonFinite(l.Text) {
		return nil, CodeNonFinite, fmt.Sprintf("value %q is not finite: every fact is a finite number", l.Text)
	}
	if !isDecimal(l.Text) {
		return nil, CodeBadValue, fmt.Sprintf("value %q is not a decimal: write digits with an optional sign, fraction and exponent, like 0.411, \"9007199254740993\" or 4.11e-1 (at most %d digits before the point, %d after it and %d in the exponent)",
			l.Text, maxIntDigits, maxFracDigits, maxExpDigits)
	}
	x, ok := new(big.Rat).SetString(l.Text)
	if !ok {
		return nil, CodeBadValue, fmt.Sprintf("value %q is not a decimal", l.Text)
	}
	return x, "", ""
}

// isNonFinite reports whether s spells NaN or an infinity.
func isNonFinite(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	return strings.EqualFold(s, "nan") || strings.EqualFold(s, "inf") || strings.EqualFold(s, "infinity")
}

// isDecimal reports whether s matches the bounded decimal grammar of
// parseValue.
func isDecimal(s string) bool {
	i := 0
	sign := func() {
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
	}
	digits := func(most int) bool {
		start := i
		for i < len(s) && '0' <= s[i] && s[i] <= '9' {
			i++
		}
		return i > start && i-start <= most
	}
	sign()
	if !digits(maxIntDigits) {
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if !digits(maxFracDigits) {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		sign()
		if !digits(maxExpDigits) {
			return false
		}
	}
	return i == len(s)
}
