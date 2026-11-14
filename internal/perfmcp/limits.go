package perfmcp

import (
	"fmt"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// Limits bound what a server accepts and answers. A server copies them at
// New and never changes them, so two servers with different limits can hold
// the same handle and still judge its evidence differently.
//
// New fills in defaults: a zero Limits is DefaultLimits, and a zero Model
// is profilemodel.DefaultLimits, so a Limits that sets only some of the
// server's own fields still loads profiles. MaxRows, MaxCachedDiffs and MaxDiffRows are filled in when zero. Any other
// non-positive server field is refused.
type Limits struct {
	// Model bounds every profile and executions log the server loads.
	Model profilemodel.Limits
	// MaxMessageBytes bounds one incoming JSON-RPC line.
	MaxMessageBytes int
	// MaxResponseBytes bounds the JSON text of one tool result.
	MaxResponseBytes int
	// MaxOpenHandles is how many handles the server keeps open; opening one
	// more closes the least recently used.
	MaxOpenHandles int
	// MaxRows bounds the limit argument of the tools that page rows.
	MaxRows int
	// MaxCachedDiffs is how many computed diffs the server keeps; one more
	// evicts the least recently used.
	MaxCachedDiffs int
	// MaxDiffRows bounds the limit argument of diff.
	MaxDiffRows int
}

// defaultMaxRows is the MaxRows a server runs with unless told otherwise.
const defaultMaxRows = 200

// Defaults of the diff limits.
const (
	defaultMaxCachedDiffs = 4
	defaultMaxDiffRows    = 500
)

// DefaultLimits are the limits a server runs with unless told otherwise.
func DefaultLimits() Limits {
	return Limits{
		Model:            profilemodel.DefaultLimits(),
		MaxMessageBytes:  4 << 20,
		MaxResponseBytes: 1 << 20,
		MaxOpenHandles:   8,
		MaxRows:          defaultMaxRows,
		MaxCachedDiffs:   defaultMaxCachedDiffs,
		MaxDiffRows:      defaultMaxDiffRows,
	}
}

// withDefaults is l with the zero parts New fills in.
func (l Limits) withDefaults() Limits {
	if l == (Limits{}) {
		return DefaultLimits()
	}
	if l.Model == (profilemodel.Limits{}) {
		l.Model = profilemodel.DefaultLimits()
	}
	if l.MaxRows == 0 {
		l.MaxRows = defaultMaxRows
	}
	if l.MaxCachedDiffs == 0 {
		l.MaxCachedDiffs = defaultMaxCachedDiffs
	}
	if l.MaxDiffRows == 0 {
		l.MaxDiffRows = defaultMaxDiffRows
	}
	return l
}

// validate refuses limits a server cannot run with.
func (l Limits) validate() error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{"MaxMessageBytes", l.MaxMessageBytes},
		{"MaxResponseBytes", l.MaxResponseBytes},
		{"MaxOpenHandles", l.MaxOpenHandles},
		{"MaxRows", l.MaxRows},
		{"MaxCachedDiffs", l.MaxCachedDiffs},
		{"MaxDiffRows", l.MaxDiffRows},
	} {
		if f.v <= 0 {
			return fmt.Errorf("perfmcp: limit %s must be positive, got %d", f.name, f.v)
		}
	}
	return nil
}
