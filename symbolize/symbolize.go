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

	// MapStart, MapLimit and MapOff describe the mapping Address fell in,
	// when one is known: the mapping's start and (exclusive) end virtual
	// addresses, and the file offset the mapping begins at.
	//
	// They exist because blazesym reports NOTHING but a failure reason for
	// an address it cannot name - not even which file the address was in
	// (capi/src/symbolize.rs zeroes the whole blaze_sym and sets only
	// `reason`). For a stripped vendor library such as libcuda.so.1 the
	// symbol is genuinely unrecoverable, but the module is not, and it is
	// most of the diagnostic value: "seven frames deep inside libcuda"
	// is an answer, "0x7f2c945b2c2b" is not.
	//
	// A Symbolizer fills these in from /proc/<pid>/maps, via a ModuleIndex,
	// only for frames it failed to name; see attachModules. All three are
	// zero when no mapping is known, and that case must stay visibly
	// distinct downstream rather than being filled with a plausible guess.
	MapStart uint64
	MapLimit uint64
	MapOff   uint64
}

// ModuleOffset returns Address relative to the start of the file backing its
// mapping - the number that is stable across runs, unlike the ASLR'd
// Address, and that addr2line/objdump/nvdisasm accept. ok is false when no
// mapping is known for this frame.
func (f Frame) ModuleOffset() (off uint64, ok bool) {
	if f.Module == "" || f.MapLimit <= f.MapStart {
		return 0, false
	}
	if f.Address < f.MapStart || f.Address >= f.MapLimit {
		return 0, false
	}
	return f.Address - f.MapStart + f.MapOff, true
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
