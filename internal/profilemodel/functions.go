package profilemodel

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// FunctionRef names a function within a view: the first 16 lowercase hex
// characters of the SHA-256 of FunctionID.Key().
type FunctionRef string

// RefOf returns f's reference.
func RefOf(f FunctionID) FunctionRef {
	sum := sha256.Sum256([]byte(f.Key()))
	return FunctionRef(hex.EncodeToString(sum[:])[:16])
}

// ErrUnknownFunction is returned when no sample with a non-zero value holds this function.
var ErrUnknownFunction = errors.New("profilemodel: no such function in the view")

// denominatorFunction prefixes a function reference in a share's denominator.
const denominatorFunction = "function:"

// FunctionRow is one function's totals over the view. Cum counts a sample
// once however often the function appears in its stack.
type FunctionRow struct {
	Ref          FunctionRef
	Function     FunctionID
	Cum, Self    int64
	ShareOfTotal Fact
}

// EdgeRow is one neighbour of a function in the call graph. Value is the
// value of samples in which the function has this neighbour directly next
// to it, once per sample.
type EdgeRow struct {
	Ref             FunctionRef
	Function        FunctionID
	Value           int64
	ShareOfFunction Fact
}

// Sandwich is a function with its immediate callers and callees.
type Sandwich struct {
	Function FunctionRow
	Callers  []EdgeRow
	Callees  []EdgeRow
	// RootValue is the value of samples in which an occurrence of the
	// function is the outermost frame.
	RootValue int64
	// Recursive is set when any sample holds the function more than once.
	Recursive bool
}

// funcTotals accumulates one function's cum and self.
type funcTotals struct {
	fn        FunctionID
	cum, self int64
}

// scan makes one pass over the samples with a non-zero value. When target is
// non-empty it also collects that function's edges.
func (v *View) scan(target FunctionRef) (map[string]*funcTotals, *Sandwich, map[string]*EdgeRow, map[string]*EdgeRow, error) {
	b := newBuilder()
	totals := map[string]*funcTotals{}
	refs := map[string]FunctionRef{}
	refOf := func(fr frameRef) FunctionRef {
		r, ok := refs[fr.key]
		if !ok {
			r = RefOf(fr.fn)
			refs[fr.key] = r
		}
		return r
	}
	var sw *Sandwich
	var callers, callees map[string]*EdgeRow
	if target != "" {
		sw = &Sandwich{}
		callers, callees = map[string]*EdgeRow{}, map[string]*EdgeRow{}
	}
	addEdge := func(m map[string]*EdgeRow, seen map[string]struct{}, fr frameRef, val int64) error {
		if _, dup := seen[fr.key]; dup {
			return nil
		}
		seen[fr.key] = struct{}{}
		row := m[fr.key]
		if row == nil {
			row = &EdgeRow{Ref: refOf(fr), Function: fr.fn}
			m[fr.key] = row
		}
		var err error
		row.Value, err = addChecked(row.Value, val, "edge value")
		return err
	}

	for _, s := range v.profile.Source.Sample {
		val := s.Value[v.Index]
		if val == 0 {
			continue
		}
		frames := b.framesOf(s)
		seen := map[string]struct{}{}
		var seenCallers, seenCallees map[string]struct{}
		inTarget, rootHit, occurrences := false, false, 0
		if sw != nil {
			seenCallers, seenCallees = map[string]struct{}{}, map[string]struct{}{}
		}
		for i, fr := range frames {
			t := totals[fr.key]
			if t == nil {
				t = &funcTotals{fn: fr.fn}
				totals[fr.key] = t
			}
			if _, dup := seen[fr.key]; !dup {
				seen[fr.key] = struct{}{}
				var err error
				if t.cum, err = addChecked(t.cum, val, "cumulative value"); err != nil {
					return nil, nil, nil, nil, err
				}
			}
			if i == len(frames)-1 {
				var err error
				if t.self, err = addChecked(t.self, val, "self value"); err != nil {
					return nil, nil, nil, nil, err
				}
			}
			if sw == nil || refOf(fr) != target {
				continue
			}
			inTarget = true
			occurrences++
			if i == 0 && !rootHit {
				rootHit = true
				var err error
				if sw.RootValue, err = addChecked(sw.RootValue, val, "root value"); err != nil {
					return nil, nil, nil, nil, err
				}
			}
			if i > 0 {
				if err := addEdge(callers, seenCallers, frames[i-1], val); err != nil {
					return nil, nil, nil, nil, err
				}
			}
			if i+1 < len(frames) {
				if err := addEdge(callees, seenCallees, frames[i+1], val); err != nil {
					return nil, nil, nil, nil, err
				}
			}
		}
		if inTarget && occurrences > 1 {
			sw.Recursive = true
		}
	}
	return totals, sw, callers, callees, nil
}

func (v *View) functionRow(t *funcTotals) FunctionRow {
	ref := RefOf(t.fn)
	return FunctionRow{Ref: ref, Function: t.fn, Cum: t.cum, Self: t.self,
		ShareOfTotal: shareFact(MetricShareOfTotal, t.cum, v.Total, DenominatorViewTotal)}
}

// Functions aggregates the view by function, ordered by Cum descending then
// Ref. Cum is the value of samples whose stack holds the function at least
// once; Self is the value of samples whose leaf frame it is.
func (v *View) Functions() ([]FunctionRow, error) {
	totals, _, _, _, err := v.scan("")
	if err != nil {
		return nil, err
	}
	rows := make([]FunctionRow, 0, len(totals))
	for _, t := range totals {
		rows = append(rows, v.functionRow(t))
	}
	slices.SortFunc(rows, func(a, b FunctionRow) int {
		return cmp.Or(cmp.Compare(b.Cum, a.Cum), cmp.Compare(a.Ref, b.Ref))
	})
	return rows, nil
}

// Sandwich returns the function's callers and callees. A row X among the
// callers is the value of samples in which some occurrence of the function
// has X as its immediate parent frame, once per sample per X; callees are
// the same for the immediate child.
func (v *View) Sandwich(ref FunctionRef) (Sandwich, error) {
	totals, sw, callers, callees, err := v.scan(ref)
	if err != nil {
		return Sandwich{}, err
	}
	var self *funcTotals
	for _, t := range totals {
		if RefOf(t.fn) == ref {
			self = t
			break
		}
	}
	if self == nil {
		return Sandwich{}, fmt.Errorf("%w: %q", ErrUnknownFunction, ref)
	}
	sw.Function = v.functionRow(self)
	den := denominatorFunction + string(ref)
	finish := func(m map[string]*EdgeRow) []EdgeRow {
		rows := make([]EdgeRow, 0, len(m))
		for _, r := range m {
			r.ShareOfFunction = shareFact(MetricShareOfParent, r.Value, self.cum, den)
			rows = append(rows, *r)
		}
		slices.SortFunc(rows, func(a, b EdgeRow) int {
			return cmp.Or(cmp.Compare(b.Value, a.Value), cmp.Compare(a.Ref, b.Ref))
		})
		return rows
	}
	sw.Callers, sw.Callees = finish(callers), finish(callees)
	return *sw, nil
}
