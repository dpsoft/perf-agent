package perfmcp

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// Fact is a profilemodel.Fact as clients read it. Value is the fact's
// canonical decimal (Fact.Canonical): exact integers for sample-unit values
// and counts, the ratio at its rounding policy for shares. An unavailable
// fact has no value and always a reason, so it never reads as zero.
type Fact struct {
	Value       string `json:"value,omitempty"`
	Unit        string `json:"unit"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	Denominator string `json:"denominator,omitempty"`
	Rounding    string `json:"rounding"`
}

// reasonNoValue marks a fact the engine gave a status but no exact value; it
// is shown as unavailable rather than as a number nobody can check.
const reasonNoValue = "no exact value"

// wireFact converts f.
func wireFact(f profilemodel.Fact) Fact {
	w := Fact{Unit: f.Unit, Status: string(f.Status), Denominator: f.Denominator, Rounding: f.Rounding}
	if f.Status != profilemodel.StatusUnavailable {
		if v, ok := f.Canonical(); ok {
			w.Value = v
			return w
		}
		w.Status, w.Reason = string(profilemodel.StatusUnavailable), reasonNoValue
		return w
	}
	w.Reason = f.Reason
	if w.Reason == "" {
		w.Reason = "the engine gave no reason"
	}
	return w
}

// wireInt is an integer as clients read it: a JSON string of decimal
// digits, so values beyond 2^53 survive JavaScript clients.
func wireInt(v int64) string { return strconv.FormatInt(v, 10) }

// Serialization mirrors profilemodel.Serialization with wire facts.
type Serialization struct {
	True    Fact `json:"true"`
	False   Fact `json:"false"`
	Unknown Fact `json:"unknown"`
	Missing Fact `json:"missing"`
}

func wireSerialization(s profilemodel.Serialization) Serialization {
	return Serialization{True: wireFact(s.True), False: wireFact(s.False), Unknown: wireFact(s.Unknown), Missing: wireFact(s.Missing)}
}

// Quality mirrors profilemodel.Quality with wire facts; see it for what
// each field means. Lists and maps are empty, never null.
type Quality struct {
	JoinExact          Fact                              `json:"join_exact"`
	JoinHeuristic      Fact                              `json:"join_heuristic"`
	JoinUnmatched      Fact                              `json:"join_unmatched"`
	Ambiguous          Fact                              `json:"ambiguous"`
	GraphRefused       Fact                              `json:"graph_refused"`
	UnsampledLaunches  Fact                              `json:"unsampled_launches"`
	SamplePeriods      []string                          `json:"sample_periods"`
	Serialized         Serialization                     `json:"serialized"`
	Unresolved         Fact                              `json:"unresolved"`
	UnresolvedByModule map[string]Fact                   `json:"unresolved_by_module"`
	UnresolvedModules  map[string]profilemodel.ModuleRef `json:"unresolved_modules"`
	CollisionPossible  Fact                              `json:"collision_possible"`
	Executions         profilemodel.Availability         `json:"executions"`
}

func wireQuality(q profilemodel.Quality) Quality {
	w := Quality{
		JoinExact:          wireFact(q.JoinExact),
		JoinHeuristic:      wireFact(q.JoinHeuristic),
		JoinUnmatched:      wireFact(q.JoinUnmatched),
		Ambiguous:          wireFact(q.Ambiguous),
		GraphRefused:       wireFact(q.GraphRefused),
		UnsampledLaunches:  wireFact(q.UnsampledLaunches),
		SamplePeriods:      nonNil(q.SamplePeriods),
		Serialized:         wireSerialization(q.Serialized),
		Unresolved:         wireFact(q.Unresolved),
		UnresolvedByModule: make(map[string]Fact, len(q.UnresolvedByModule)),
		UnresolvedModules:  make(map[string]profilemodel.ModuleRef, len(q.UnresolvedModules)),
		CollisionPossible:  wireFact(q.CollisionPossible),
		Executions:         q.Executions,
	}
	for k, f := range q.UnresolvedByModule {
		w.UnresolvedByModule[k] = wireFact(f)
	}
	for k, m := range q.UnresolvedModules {
		w.UnresolvedModules[k] = m
	}
	return w
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// marshal encodes v as compact JSON without HTML escaping, so names such
// as "operator<" reach the client as written.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
