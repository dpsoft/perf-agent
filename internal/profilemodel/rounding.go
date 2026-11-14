package profilemodel

import (
	"math/big"
	"strconv"
	"strings"
)

// RoundingKind is how a metric's value is rounded for display and for the
// comparison of a claim against it. The zero value means "no policy".
type RoundingKind uint8

const (
	RoundExact RoundingKind = iota + 1
	RoundDecimalPlaces
	RoundSignificantFigures
)

// Rounding is a metric's rounding policy. Digits is the number of decimal
// places or significant figures; RoundExact ignores it.
type Rounding struct {
	Kind   RoundingKind
	Digits int
}

var (
	roundExact   = Rounding{Kind: RoundExact}
	roundRatio   = Rounding{Kind: RoundDecimalPlaces, Digits: 3}
	roundPerWork = Rounding{Kind: RoundSignificantFigures, Digits: 3}
)

// String is the wording facts carry in their "rounding" field: "exact",
// "0.001 ratio" for 3 decimal places, "3 significant figures"; "" for no
// policy.
func (r Rounding) String() string {
	switch r.Kind {
	case RoundExact:
		return "exact"
	case RoundDecimalPlaces:
		if r.Digits == 3 {
			return "0.001 ratio"
		}
		return "0." + strings.Repeat("0", max(r.Digits-1, 0)) + "1 ratio"
	case RoundSignificantFigures:
		return strconv.Itoa(r.Digits) + " significant figures"
	}
	return ""
}

// PolicyFor is the engine's rounding policy for metric, and false for a
// metric it does not know. See RoundingFor for the table.
func PolicyFor(metric string) (Rounding, bool) {
	switch metric {
	case MetricShareOfTotal, MetricShareOfParent, MetricShareDelta,
		MetricQualityJoinExact, MetricQualityJoinHeuristic, MetricQualityJoinUnmatched,
		MetricQualityAmbiguous, MetricQualityGraphRefused, MetricQualityUnsampled,
		MetricQualitySerializedTrue, MetricQualitySerializedFalse,
		MetricQualitySerializedUnknown, MetricQualitySerializedMissing,
		MetricQualityUnresolved, MetricQualityUnresolvedByModule, MetricQualityCollision:
		return roundRatio, true
	case MetricCumulative, MetricSelf, MetricInexactPortion, MetricUnsampledPortion,
		MetricSerializationTrue, MetricSerializationFalse, MetricSerializationUnknown, MetricSerializationMissing,
		MetricExecutionsBelow, MetricKernelNamesObserved:
		return roundExact, true
	case MetricPerWorkDelta:
		return roundPerWork, true
	}
	return Rounding{}, false
}

// Canonical is x written at the policy, as a decimal string. Exact gives the
// integer's digits and is false when x is not an integer. Decimal places
// round half away from zero to exactly Digits decimals, and a value that
// rounds to zero has no sign. Significant figures have no rational form and
// are false.
func (r Rounding) Canonical(x *big.Rat) (string, bool) {
	switch r.Kind {
	case RoundExact:
		if !x.IsInt() {
			return "", false
		}
		return x.Num().String(), true
	case RoundDecimalPlaces:
		if r.Digits < 0 {
			return "", false
		}
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(r.Digits)), nil)
		num := new(big.Int).Abs(x.Num())
		num.Mul(num, scale)
		q, rem := new(big.Int).QuoRem(num, x.Denom(), new(big.Int))
		if rem.Lsh(rem, 1).Cmp(x.Denom()) >= 0 {
			q.Add(q, big.NewInt(1))
		}
		neg := x.Sign() < 0 && q.Sign() != 0
		digits := q.String()
		if r.Digits > 0 {
			digits = strings.Repeat("0", max(r.Digits+1-len(digits), 0)) + digits
			cut := len(digits) - r.Digits
			digits = digits[:cut] + "." + digits[cut:]
		}
		if neg {
			digits = "-" + digits
		}
		return digits, true
	}
	return "", false
}

// Policy is the rounding policy of the fact's metric.
func (f Fact) Policy() Rounding { return f.policy }

// Exact is the fact's value as an exact rational: the integer for sample-unit
// values and counts, the part/whole fraction for a share. Derived values and
// unavailable facts have none.
func (f Fact) Exact() (*big.Rat, bool) {
	switch {
	case f.Int != nil:
		return new(big.Rat).SetInt64(*f.Int), true
	case f.Ratio != nil && f.exact != nil:
		return new(big.Rat).Set(f.exact), true
	}
	return nil, false
}

// Canonical is the fact's value at its policy as a decimal string, the form
// a claim is compared in. Derived values use the policy's significant
// figures; an unavailable fact has none.
func (f Fact) Canonical() (string, bool) {
	if f.Derived != nil {
		if f.policy.Kind != RoundSignificantFigures {
			return "", false
		}
		return strconv.FormatFloat(*f.Derived, 'g', f.policy.Digits, 64), true
	}
	x, ok := f.Exact()
	if !ok {
		return "", false
	}
	return f.policy.Canonical(x)
}
