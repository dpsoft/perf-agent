package symbolize

import (
	"sync"

	blazesym "github.com/libbpf/blazesym/go"
)

// startLineCache remembers the declaration line of a function, keyed by the
// module it lives in and the function's start address within that module.
//
// Keyed on (module, symbol start) rather than on the name: two binaries can
// define the same symbol at different lines, and the same binary can be
// mapped by many processes. The address is the module-relative symbol start,
// so the key survives ASLR.
type startLineCache struct {
	mu sync.RWMutex
	m  map[startLineKey]int
}

type startLineKey struct {
	module string
	symOff uint64
}

func newStartLineCache() *startLineCache {
	return &startLineCache{m: make(map[startLineKey]int)}
}

func (c *startLineCache) get(k startLineKey) (int, bool) {
	c.mu.RLock()
	v, ok := c.m[k]
	c.mu.RUnlock()
	return v, ok
}

func (c *startLineCache) put(k startLineKey, line int) {
	c.mu.Lock()
	c.m[k] = line
	c.mu.Unlock()
}

// fillStartLines populates Frame.StartLine for frames that were named.
//
// Go's PGO matches profile functions on (name, start_line) and refuses a
// profile that omits the field, so without this `go build -pgo=` rejects
// every profile this agent writes (#171).
//
// The value is obtained by symbolizing the FUNCTION'S OWN ENTRY ADDRESS --
// blazesym reports it as Sym.Addr -- and taking the line that comes back.
// That is not an approximation of the declaration line, it is the
// declaration line: a function's prologue is attributed to its `func` (or
// equivalent) source line. Measured against profiles written by Go's own
// runtime/pprof, which is the authority Go's PGO is built around:
//
//	main.hot    entry 0x4c4b20 -> main.go:10    runtime/pprof StartLine 10
//	main.main   entry 0x4c4be0 -> main.go:21    runtime/pprof StartLine 21
//
// Cost is one extra symbolization batch per collect, sized by DISTINCT
// functions rather than by samples, and cached across collects. A profile
// with 50,000 samples over 200 functions pays for 200 lookups once.
//
// Frames whose entry address is the sampled address already carry the right
// line, and frames that could not be named have no function to locate, so
// both are skipped rather than queried.
//
// INLINED frames are not covered. They live in Frame.Inlined, and blazesym
// reports an inlined entry's CALL SITE rather than the inlined function's
// own entry address, so the derivation above has nothing to symbolize. On a
// Go CPU profile that leaves the inlined math/runtime helpers without a
// start line while every out-of-line function has one -- measured 14 of 21
// on a math.Sin workload. Go's PGO keys its hot nodes on out-of-line
// functions, so it accepts such a profile and applies:
//
//	hot-node enabled increased budget=2000 for func=main.main.func1
//
// Closing the remaining gap needs the inlined function's entry, which is a
// blazesym-side capability, not something recoverable here.
func (s *LocalSymbolizer) fillStartLines(pid uint32, frames []Frame) {
	type want struct {
		idx  int
		key  startLineKey
		addr uint64
	}
	var todo []want
	seen := make(map[uint64]int) // entry addr -> first todo index wanting it

	for i := range frames {
		f := &frames[i]
		if f.Reason != FailureNone || f.Name == "" {
			continue
		}
		// Offset is the distance from the symbol's start to the sampled
		// address, so the entry is behind us by exactly that much.
		if f.Offset > f.Address {
			continue
		}
		entry := f.Address - f.Offset
		key := startLineKey{module: f.Module, symOff: entry - f.MapStart}
		if line, ok := s.startLines.get(key); ok {
			f.StartLine = line
			continue
		}
		if f.Offset == 0 {
			// The sampled address IS the entry, so the line already in hand
			// is the declaration line. No lookup needed.
			f.StartLine = f.Line
			s.startLines.put(key, f.Line)
			continue
		}
		if _, dup := seen[entry]; !dup {
			seen[entry] = len(todo)
		}
		todo = append(todo, want{idx: i, key: key, addr: entry})
	}
	if len(todo) == 0 {
		return
	}

	// One batch for every distinct entry address still unknown.
	addrs := make([]uint64, 0, len(todo))
	for _, w := range todo {
		addrs = append(addrs, w.addr)
	}
	syms, err := s.bz.SymbolizeProcessAbsAddrs(addrs, pid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	if err != nil {
		// Not an error for the caller: a profile without start lines is the
		// status quo, and everything else about it is still correct. Go's
		// PGO will refuse it, which is the condition #171 describes, and the
		// batch-failure counter already records why.
		s.noteBatchFailure(pid, err)
		return
	}
	for i, w := range todo {
		if i >= len(syms) {
			break
		}
		ci := syms[i].CodeInfo
		if ci == nil || ci.Line == 0 {
			continue
		}
		line := int(ci.Line)
		frames[w.idx].StartLine = line
		s.startLines.put(w.key, line)
	}
}
