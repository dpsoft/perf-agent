// Package explain checks the structured explanations an agent attaches to a
// profile view. Only structured claims are checked: each names a metric, a
// scope, a unit, a denominator and a value, and the value must equal the
// engine's fact at the engine's own rounding for that metric
// (profilemodel.PolicyFor), compared as exact decimals. Prose, hypotheses and
// next measurements are not checked; Tokenize splits them into placeholders,
// which resolve to claims and nodes, and free numbers, which a page marks as
// unchecked.
package explain

import (
	"encoding/json"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// Limits bound one call. Each is the maximum allowed; Check reports any
// excess as limit_exceeded naming the field.
type Limits struct {
	MaxExplanations         int // explanations in the call
	MaxClaimsPerExplanation int // claims in one explanation
	MaxClaimsTotal          int // claims in the call
	MaxProseBytes           int // bytes of one explanation's prose
	MaxHypotheses           int // hypotheses in one explanation
	MaxHypothesisBytes      int // bytes of one hypothesis
	MaxNextBytes            int // bytes of one next_measurement
	MaxTitleBytes           int // bytes of the page title
}

// DefaultLimits are the limits a server uses unless configured otherwise.
func DefaultLimits() Limits {
	return Limits{
		MaxExplanations:         64,
		MaxClaimsPerExplanation: 32,
		MaxClaimsTotal:          512,
		MaxProseBytes:           8192,
		MaxHypotheses:           8,
		MaxHypothesisBytes:      2048,
		MaxNextBytes:            2048,
		MaxTitleBytes:           200,
	}
}

// Explanation is what an agent says about one node.
type Explanation struct {
	Node            string   `json:"node"`
	Claims          []Claim  `json:"claims,omitzero"`
	Prose           string   `json:"prose,omitzero"`
	Hypotheses      []string `json:"hypotheses,omitzero"`
	NextMeasurement string   `json:"next_measurement,omitzero"`
}

// Scopes a claim can have. An empty Scope is ScopeNode.
const (
	ScopeNode = "node"
	ScopeView = "view"
)

// Claim is one structured statement of a fact's value. Node defaults to the
// explanation's node at node scope and must be empty at view scope. Module
// is nil when the agent did not send one; a non-nil "" is the "no module"
// key of unresolved_by_module.
type Claim struct {
	ID          string  `json:"id"`
	Metric      string  `json:"metric"`
	Scope       string  `json:"scope,omitzero"`
	Node        string  `json:"node,omitzero"`
	Module      *string `json:"module,omitzero"`
	Denominator string  `json:"denominator,omitzero"`
	Unit        string  `json:"unit,omitzero"`
	Value       Literal `json:"value,omitzero"`
}

// LiteralKind is the JSON type of a claim's value as sent. The zero value
// means no value was sent.
type LiteralKind string

const (
	LiteralNumber LiteralKind = "number" // Text is the number's JSON text, unchanged
	LiteralString LiteralKind = "string" // Text is the decoded string
	LiteralOther  LiteralKind = "other"  // Text is the JSON text of a bool, null, object or array
)

// Literal is a claim's value exactly as the agent sent it. It is never
// parsed into a float64, so large integers and long decimals survive.
type Literal struct {
	Text string
	Kind LiteralKind
}

// MarshalJSON writes the literal back as it was sent: a number or other
// value as its JSON text, a string as a JSON string, no value as null. A
// Text that is not valid JSON for its kind is written as a string.
func (l Literal) MarshalJSON() ([]byte, error) {
	switch l.Kind {
	case "":
		return []byte("null"), nil
	case LiteralNumber, LiteralOther:
		if json.Valid([]byte(l.Text)) {
			return []byte(l.Text), nil
		}
	}
	return json.Marshal(l.Text)
}

// maxEcho is the most characters of any one piece of agent text a failure,
// notice or error repeats back; longer text is cut and ends in "…".
const maxEcho = 64

// Notice is something Decode ignored, at a path like
// "explanations[0].claims[1].tolerance", or advice about text that passed.
// Code, when set, names the advice. A notice never fails a check.
type Notice struct {
	Path    string `json:"path"`
	Code    string `json:"code,omitzero"`
	Message string `json:"message"`
}

// NoticeUnitRepeated is the code of a notice about a unit written right
// after a claim placeholder: the page shows the claim with its own unit,
// so the unit would show twice.
const NoticeUnitRepeated = "unit_repeated"

// Code names why a claim or reference failed.
type Code string

const (
	CodeDuplicateClaimID    Code = "duplicate_claim_id"
	CodeBadClaimID          Code = "bad_claim_id"
	CodeUnresolvedClaimRef  Code = "unresolved_claim_ref"
	CodeUnresolvedNodeRef   Code = "unresolved_node_ref"
	CodeGluedToClaim        Code = "glued_to_claim"
	CodeUnknownNode         Code = "unknown_node"
	CodeUnknownMetric       Code = "unknown_metric"
	CodeDiffMetric          Code = "diff_metric"
	CodeWrongScope          Code = "wrong_scope"
	CodeUnknownModule       Code = "unknown_module"
	CodeModuleNotApplicable Code = "module_not_applicable"
	CodeUnavailableFact     Code = "unavailable_fact"
	CodeWrongUnit           Code = "wrong_unit"
	CodeWrongDenominator    Code = "wrong_denominator"
	CodeBadValue            Code = "bad_value"
	CodeNonFinite           Code = "non_finite"
	CodeOutOfRange          Code = "out_of_range"
	CodeNotInteger          Code = "not_integer"
	CodeValueMismatch       Code = "value_mismatch"
	CodeLimitExceeded       Code = "limit_exceeded"
	CodeUnplacedClaim       Code = "unplaced_claim"
)

// EngineValue is the engine's fact for a claim's metric and scope. Value is
// Fact.Canonical(), the form claims are compared in, and is empty when the
// fact is unavailable; Reason then says why. Node is empty at view scope;
// Module is set only for unresolved_by_module.
type EngineValue struct {
	Metric      string              `json:"metric"`
	Scope       string              `json:"scope"`
	Node        profilemodel.NodeID `json:"node,omitzero"`
	Module      *string             `json:"module,omitzero"`
	Denominator string              `json:"denominator,omitzero"`
	Unit        string              `json:"unit"`
	Value       string              `json:"value,omitzero"`
	Rounding    string              `json:"rounding"`
	Status      profilemodel.Status `json:"status"`
	Reason      string              `json:"reason,omitzero"`
}

// Failure is one failing claim or reference. Explanation is the index of the
// explanation it is in, or -1 for a limit on the whole call; like every
// integer on the wire it is a JSON string of decimal digits. Claim is the
// claim's ID as sent; Ref is the claim ID, node ID or limit name a reference
// or limit failure is about. Agent is the claim's value literal as sent, and
// Engine the engine's fact whenever the claim got as far as finding it.
type Failure struct {
	Explanation int          `json:"explanation,string"`
	Claim       string       `json:"claim,omitzero"`
	Ref         string       `json:"ref,omitzero"`
	Code        Code         `json:"code"`
	Message     string       `json:"message"`
	Agent       string       `json:"agent,omitzero"`
	Engine      *EngineValue `json:"engine,omitzero"`
}

// SegmentKind is what a piece of agent text is.
type SegmentKind string

const (
	SegmentText      SegmentKind = "text"             // plain text, no number in it
	SegmentClaim     SegmentKind = "claim"            // "{<claim id>}"; Claim is the ID
	SegmentNode      SegmentKind = "node"             // "{n:<node id>}"; Node is the ID
	SegmentUnchecked SegmentKind = "unchecked_number" // a number no claim checked
)

// Segment is one piece of tokenized agent text. Text is always the exact
// source text, so the segments of a string join back to it.
type Segment struct {
	Kind  SegmentKind         `json:"kind"`
	Text  string              `json:"text"`
	Claim string              `json:"claim,omitzero"`
	Node  profilemodel.NodeID `json:"node,omitzero"`
}

// CheckedClaim is a claim that passed, with the engine's value a page shows
// in its place.
type CheckedClaim struct {
	ID string `json:"id"`
	EngineValue
}

// Checked is one explanation that passed: its node's facts, its claims and
// its text split into segments.
type Checked struct {
	Node            profilemodel.NodeID    `json:"node"`
	Facts           profilemodel.PathFacts `json:"facts"`
	Claims          []CheckedClaim         `json:"claims"`
	Prose           []Segment              `json:"prose"`
	Hypotheses      [][]Segment            `json:"hypotheses"`
	NextMeasurement []Segment              `json:"next_measurement"`
}

// Result is the outcome of a check. OK is true only with no failure, and
// only then is Checked set. Figures is the number of claims in the call and
// References the number of distinct nodes named by "{n:…}" placeholders;
// both are JSON strings of decimal digits.
type Result struct {
	OK         bool      `json:"ok"`
	Checked    []Checked `json:"checked,omitzero"`
	Failures   []Failure `json:"failures,omitzero"`
	Figures    int       `json:"figures,string"`
	References int       `json:"references,string"`
}

// RejectedAttempt is a render call whose check failed, as a server records
// it for the page's "rejected at render" section. A server may keep only
// some of the failures; Omitted counts the ones it left out to keep fewer,
// TooLarge the ones it left out for the size of their text. Of those,
// OmittedClaims counts the claims and OmittedReferences the references
// that no kept failure names, so a page can count every claim and
// reference the attempt failed on while listing only the kept failures.
// Evidence, when set, says how the evidence the attempt was checked
// against differs from the page's: another handle of the same profile,
// opened with a different executions log or none.
type RejectedAttempt struct {
	Attempt           string    `json:"attempt"`
	RetryOf           string    `json:"retry_of,omitzero"`
	Failures          []Failure `json:"failures"`
	Omitted           int       `json:"omitted,omitzero"`
	TooLarge          int       `json:"too_large,omitzero"`
	OmittedClaims     int       `json:"omitted_claims,omitzero"`
	OmittedReferences int       `json:"omitted_references,omitzero"`
	Evidence          string    `json:"evidence,omitzero"`
}
