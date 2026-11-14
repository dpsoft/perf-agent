package perfmcp

import (
	"fmt"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// Limits bound what a server accepts and answers. A server copies them at
// New and never changes them, so two servers with different limits can hold
// the same handle and still judge its evidence differently.
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
}

// DefaultLimits are the limits a server runs with unless told otherwise.
func DefaultLimits() Limits {
	return Limits{
		Model:            profilemodel.DefaultLimits(),
		MaxMessageBytes:  4 << 20,
		MaxResponseBytes: 1 << 20,
		MaxOpenHandles:   8,
	}
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
	} {
		if f.v <= 0 {
			return fmt.Errorf("perfmcp: limit %s must be positive, got %d", f.name, f.v)
		}
	}
	return nil
}
