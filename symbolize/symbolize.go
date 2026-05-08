// Package symbolize provides perf-agent's address-to-frame resolution
// abstraction. Implementations live in this package (LocalSymbolizer)
// and in symbolize/debuginfod (off-box-fetch).
package symbolize

// Symbolizer resolves abs addresses in a process's address space to
// symbolic frames. Implementations are safe for concurrent use.
type Symbolizer interface {
	SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error)
	Close() error
}

// Frame is a single symbolized stack frame. Name is "" when resolution
// failed; Reason explains why. Inlined holds the inline-expansion chain
// in caller-most-to-callee order when the resolver supports it.
type Frame struct {
	Address uint64
	Name    string
	Module  string
	BuildID string
	File    string
	Line    int
	Column  int
	Offset  uint64
	Inlined []Frame
	Reason  FailureReason
}

// FailureReason describes why a Frame's Name is empty.
type FailureReason uint8

const (
	FailureNone FailureReason = iota
	FailureUnmapped
	FailureInvalidFileOffset
	FailureMissingComponent
	FailureMissingSymbols
	FailureUnknownAddress
	FailureFetchError
	FailureNoBuildID
)

func (r FailureReason) String() string {
	switch r {
	case FailureNone:
		return "none"
	case FailureUnmapped:
		return "unmapped"
	case FailureInvalidFileOffset:
		return "invalid_file_offset"
	case FailureMissingComponent:
		return "missing_component"
	case FailureMissingSymbols:
		return "missing_symbols"
	case FailureUnknownAddress:
		return "unknown_address"
	case FailureFetchError:
		return "fetch_error"
	case FailureNoBuildID:
		return "no_build_id"
	}
	return "unknown"
}
