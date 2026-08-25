package gpu

import (
	"container/list"
	"fmt"
	"sync"

	"github.com/dpsoft/perf-agent/internal/cubin"
)

// SrcStatus is the four-valued gpu_src_status from the PC-sampling design:
// why a PC sample does, or does not, have a source location.
//
// The four values are mutually exclusive and exhaustive, and ModuleStore is the
// only place any of them is decided. They exist because an ABSENT source label
// reads as "not sampled", while an explicit status reads as "sampled,
// unresolvable" - different facts, needing different actions from the reader:
//
//	resolved     the module's line table covers this PC
//	no-lineinfo  the module is in hand and was built WITHOUT -lineinfo
//	no-module    no usable module bytes exist for this CRC
//	unmapped     the module has a line table, but nothing covers this PC
//
// no-lineinfo is a property of the MODULE and unmapped is a property of the PC.
// Keeping them apart is the difference between "recompile with -lineinfo" and
// "the compiler emitted no line for this instruction", which are different
// actions. A line is never synthesized: no nearest-line search, no fall back to
// the function's first line.
//
// The zero value is deliberately NOT one of the four. It is invalid, it names
// itself as invalid in String, and it refuses to marshal, so a status that was
// never decided by this store cannot be mistaken for one that was. Resolve
// never returns it.
type SrcStatus uint8

const (
	// srcStatusInvalid is the zero value: not a status. It is unexported so
	// that the only SrcStatus values a caller can name are the four real
	// ones, and it is checked for rather than defaulted to.
	srcStatusInvalid SrcStatus = iota

	// SrcResolved means the module's line table covers this pcOffset. It is
	// the only status under which a Resolution carries a function, file and
	// line.
	SrcResolved

	// SrcNoLineInfo means the module is in hand, parsed, and has no
	// .debug_line at all - it was built without -lineinfo. The fix is a
	// build flag.
	SrcNoLineInfo

	// SrcNoModule means no usable module bytes exist for this CRC: none ever
	// arrived, they were evicted, they failed to parse, or the line table
	// they carry is damaged. In every one of those cases the actionable fact
	// for the reader is identical - there is nothing here to resolve against
	// - and reporting any of them as SrcNoLineInfo would send the reader
	// after a compiler flag that would not help. See ModuleStore.Resolve.
	SrcNoModule

	// SrcUnmapped means the module has a usable line table but nothing in it
	// covers this (functionIndex, pcOffset). A property of the PC, not of
	// the build.
	SrcUnmapped
)

// srcStatusToName is the wire spelling of each status. These strings are the
// gpu_src_status label values exactly as the design fixes them; they are not
// cosmetic and must not be reworded.
var srcStatusToName = map[SrcStatus]string{
	SrcResolved:   "resolved",
	SrcNoLineInfo: "no-lineinfo",
	SrcNoModule:   "no-module",
	SrcUnmapped:   "unmapped",
}

// srcStatuses lists the four in stable order. Ordered so a consumer rendering
// a breakdown gets the same order every time, and so an exhaustiveness test has
// something to enumerate.
var srcStatuses = []SrcStatus{SrcResolved, SrcNoLineInfo, SrcNoModule, SrcUnmapped}

// SrcStatuses returns the four gpu_src_status values in stable order.
//
// It exists so that a consumer switching on a status can be tested for
// exhaustiveness against the enum itself rather than against a hand-copied
// list that a fifth value would silently escape.
func SrcStatuses() []SrcStatus {
	out := make([]SrcStatus, len(srcStatuses))
	copy(out, srcStatuses)
	return out
}

func (s SrcStatus) String() string {
	if name, ok := srcStatusToName[s]; ok {
		return name
	}
	if s == srcStatusInvalid {
		// The zero value: a status nobody decided. Named apart from a
		// fabricated one because the causes differ - this one is a struct
		// that was never given an answer, which is the "silent default" the
		// enum is shaped to prevent.
		return "unset-src-status"
	}
	return fmt.Sprintf("invalid-src-status-%d", uint8(s))
}

// MarshalJSON refuses any value that is not one of the four, matching
// ClockDomain and GPUCapability in this package: a status nobody decided must
// fail loudly at the serialization boundary rather than ship as a string a
// consumer would read as meaningful.
func (s SrcStatus) MarshalJSON() ([]byte, error) {
	name, ok := srcStatusToName[s]
	if !ok {
		return nil, fmt.Errorf("invalid gpu_src_status %d", uint8(s))
	}
	return []byte(`"` + name + `"`), nil
}

// Resolution is ModuleStore.Resolve's answer: the four-valued status, and - only
// when that status is SrcResolved - the source location.
//
// Every field is unexported and there is no exported constructor, so a
// Resolution can only come from ModuleStore.Resolve. That is what makes "the
// store is the single place gpu_src_status is decided" structurally true rather
// than a comment: no caller can build one carrying a status the store did not
// choose, and no caller can attach a file, line or function to a status that
// does not have one.
//
// The source location is reachable only through Source, which returns ok
// together with the data. There are deliberately no separate File/Line/Function
// getters: a caller who wants the location has to take the ok in the same
// expression, so emitting gpu_src_func under a no-lineinfo status requires
// ignoring a returned bool rather than merely forgetting a check.
//
// The zero Resolution is not a valid answer - its Status is srcStatusInvalid -
// and Resolve never returns one.
type Resolution struct {
	status   SrcStatus
	function string
	file     string
	line     uint32
}

// Status returns the four-valued gpu_src_status. It is always one of the four
// for any Resolution that came from Resolve.
func (r Resolution) Status() SrcStatus { return r.status }

// Source returns the source location, and ok reporting whether there is one.
//
// ok is true exactly when Status is SrcResolved. When it is false the other
// three returns are zero: the store never hands out a partial location, because
// a location paired with a non-resolved status is precisely the mislabeling the
// four-valued status exists to prevent.
func (r Resolution) Source() (function, file string, line uint32, ok bool) {
	if r.status != SrcResolved {
		return "", "", 0, false
	}
	return r.function, r.file, r.line, true
}

// The four constructors below are the ONLY places a Resolution is built, and
// each one is the sole producer of its status. Three of the four cannot carry a
// location because they take no arguments; that is the type doing the work.

func resolvedAt(function, file string, line uint32) Resolution {
	return Resolution{status: SrcResolved, function: function, file: file, line: line}
}

func noModule() Resolution   { return Resolution{status: SrcNoModule} }
func noLineInfo() Resolution { return Resolution{status: SrcNoLineInfo} }
func unmapped() Resolution   { return Resolution{status: SrcUnmapped} }

// ModuleStoreStats reports what the store holds, how it classified what it was
// given, and what it lost. Every eviction path is counted, and the classifying
// counters partition ModulesStored exactly, so a caller can reconcile rather
// than trust.
//
// The Modules* classification counters are CUMULATIVE over everything ever
// stored, not gauges of what is live: an evicted module's classification is not
// un-counted. Live and LiveBytes are the gauges.
type ModuleStoreStats struct {
	// Live and LiveBytes are gauges of what the store currently holds.
	Live      int   `json:"live"`
	LiveBytes int64 `json:"live_bytes"`

	// ModulesStored counts every Put that inserted a new CRC. A repeated Put
	// for a CRC already held is a no-op that refreshes recency and is not
	// counted here, so ModulesStored is the number of distinct modules the
	// store has ever admitted.
	ModulesStored uint64 `json:"modules_stored,omitempty"`

	// ModulesEvicted counts modules dropped to stay inside the bounds. It is
	// the total; ModulesEvictedCapacity and ModulesEvictedBytes are its
	// mutually exclusive breakdown and always sum to it.
	//
	// An eviction is a real loss of resolution: every subsequent Resolve for
	// that CRC answers SrcNoModule. That is the honest answer and it is the
	// one the store gives - there is no memo of a previous resolution that
	// could outlive the bytes it was derived from.
	ModulesEvicted         uint64 `json:"modules_evicted,omitempty"`
	ModulesEvictedCapacity uint64 `json:"modules_evicted_capacity,omitempty"`
	ModulesEvictedBytes    uint64 `json:"modules_evicted_bytes,omitempty"`

	// The four classification counters below partition ModulesStored: every
	// stored module is classified exactly once, at Put, and
	//
	//	ModulesWithLineInfo + ModulesWithoutLineInfo +
	//	ModulesDamagedLineInfo + ModulesUnparseable == ModulesStored
	//
	// is an invariant the tests assert. The partition is deliberately the
	// same shape as the Resolve* partition below, which is what lets a
	// reader reconcile "what we hold" against "what we could answer".

	// ModulesWithLineInfo counts modules that parsed and carry a usable line
	// table. These are the only modules that can produce SrcResolved.
	ModulesWithLineInfo uint64 `json:"modules_with_line_info,omitempty"`

	// ModulesWithoutLineInfo counts modules that parsed but have no
	// .debug_line at all - built without -lineinfo. This is a BUILD-FLAG
	// fact about an otherwise healthy pipeline, and it is why this counter
	// is kept apart from ModulesUnparseable: one says "add -lineinfo", the
	// other says "the bytes are wrong".
	ModulesWithoutLineInfo uint64 `json:"modules_without_line_info,omitempty"`

	// ModulesDamagedLineInfo counts modules that parsed as ELF and DO carry
	// a .debug_line, but whose line table could not be read
	// (cubin.LineInfoErr). They resolve as SrcNoModule, alongside
	// ModulesUnparseable, because we hold bytes we cannot use for source.
	//
	// It is a separate counter from ModulesUnparseable on purpose. The two
	// point at different causes: unparseable means the bytes did not survive
	// transport or are not a cubin at all, while a damaged table means the
	// ELF is fine and OUR READER refused the DWARF in it - the shape a
	// future toolkit emitting a line table this reader does not understand
	// would take. Folded together, that case would read as a transport bug
	// and send the operator to inspect a channel that is working perfectly.
	// This project's recurring defect is a counter that points the wrong
	// way; one number covering both would be exactly that.
	ModulesDamagedLineInfo uint64 `json:"modules_damaged_line_info,omitempty"`

	// ModulesUnparseable counts modules whose bytes cubin.Parse rejected
	// outright: not an ELF, not a CUDA ELF, truncated, no symbol table. They
	// are STORED (so an offer for the same CRC is not re-parsed) and they
	// resolve as SrcNoModule, because holding bytes we cannot use is the
	// same actionable fact for the reader as holding nothing.
	ModulesUnparseable uint64 `json:"modules_unparseable,omitempty"`

	// The four counters below partition every call to Resolve exactly:
	//
	//	ResolveResolved + ResolveNoModule +
	//	ResolveNoLineInfo + ResolveUnmapped == calls to Resolve
	//
	// That identity is the reason Resolve has exactly four return points and
	// increments exactly one counter at each. It is asserted by the tests.
	ResolveResolved   uint64 `json:"resolve_resolved,omitempty"`
	ResolveNoModule   uint64 `json:"resolve_no_module,omitempty"`
	ResolveNoLineInfo uint64 `json:"resolve_no_line_info,omitempty"`
	ResolveUnmapped   uint64 `json:"resolve_unmapped,omitempty"`
}

// ResolveTotal returns the number of Resolve calls the four Resolve* counters
// account for. The identity it exists to make checkable is that this equals the
// number of times Resolve was actually called.
func (s ModuleStoreStats) ResolveTotal() uint64 {
	return s.ResolveResolved + s.ResolveNoModule + s.ResolveNoLineInfo + s.ResolveUnmapped
}

// ModulesClassified returns the number of stored modules the four
// classification counters account for, which must equal ModulesStored.
func (s ModuleStoreStats) ModulesClassified() uint64 {
	return s.ModulesWithLineInfo + s.ModulesWithoutLineInfo +
		s.ModulesDamagedLineInfo + s.ModulesUnparseable
}

// ModuleStoreConfig bounds the store two ways, because one way is not a bound.
//
// A count bound alone is not a memory bound: cubins run from a few KB to
// hundreds of KB, so a capacity that is safe for small modules is a hundredfold
// overshoot for large ones. A byte bound alone is not a bound either, since a
// flood of tiny modules costs map and list overhead the byte count does not
// see. Both are enforced, and evictions under each are counted separately.
type ModuleStoreConfig struct {
	// Capacity is the maximum number of modules held. Zero means
	// defaultModuleStoreCapacity.
	Capacity int

	// MaxBytes is the maximum total of stored cubin bytes. Zero means
	// defaultModuleStoreMaxBytes.
	//
	// The bound is absolute: a single module larger than MaxBytes is stored,
	// then immediately evicted along with everything else, leaving the store
	// empty rather than one entry over the limit. Its Resolves then answer
	// SrcNoModule, which is true - we are not holding it.
	MaxBytes int64
}

const (
	// defaultModuleStoreCapacity bounds distinct modules. A process loading
	// more than this many distinct cubins is the JIT/template-explosion case
	// the design names; the LRU plus ModulesEvicted is the answer to it.
	defaultModuleStoreCapacity = 512

	// defaultModuleStoreMaxBytes bounds total held cubin bytes. Sized for a
	// per-pod agent: large enough for a few hundred ordinary cubins, small
	// enough that a pathological producer cannot turn the store into the
	// agent's dominant allocation.
	defaultModuleStoreMaxBytes = 64 << 20 // 64 MiB
)

// moduleEntry is one stored module: the bytes as offered, what parsing them
// produced, and the functionIndex -> name table Resolve needs.
type moduleEntry struct {
	crc   uint64
	bytes []byte

	// parsed is nil exactly when parseErr is non-nil.
	parsed   *cubin.Cubin
	parseErr error

	// byIndex maps a PC record's functionIndex to a device function name.
	//
	// It is built from cubin.Function.SymIndex, which is the function's index
	// in the module's .symtab. CUPTI documents CUpti_PCSamplingPCData's
	// functionIndex as "the function's unique symbol index in the module";
	// whether that is the .symtab index cannot be determined without
	// hardware, so this mapping is the design's PREMISE, not a measured
	// fact. It is measured on the RTX 3090 by the task that enables PC
	// sampling, and the design pre-approves a wire-format fallback (a v2 PC
	// record carrying kernel_id) if it turns out to be something else. An
	// index that misses this table answers SrcUnmapped - never a guess at a
	// neighbouring function.
	byIndex map[uint32]string

	// elem is this entry's position in the LRU list, so a touch is O(1).
	elem *list.Element
}

// ModuleStore maps a cubin CRC to the module's parsed line table, bounded and
// LRU, and is the single place the four-valued gpu_src_status is decided.
//
// It sits beside Timeline rather than inside it. Timeline's EmitModule records
// THAT a module loaded (its CRC, size and load time, in a bounded ring); this
// store holds what a module IS. The two are deliberately separate: the ring is
// a timeline of events and evicting from it loses history, while this store is
// a cache of content and evicting from it loses resolution - different losses,
// different bounds, different counters.
//
// # Why the LRU is a real LRU
//
// Recency is refreshed by Resolve, not only by Put. A module being actively
// sampled must not be evicted by a burst of unrelated module loads, which is
// exactly what an insertion-ordered FIFO would do. That is also why this does
// not reuse orderedFIFO: orderedFIFO reclaims superseded positions only while
// something is evicting, so touching an entry on every Resolve - a per-PC-sample
// operation - would grow its order slice without bound whenever the store sits
// under its capacity. A container/list LRU has no such garbage.
//
// # No memoized resolutions
//
// There is no cache of past Resolve answers anywhere in this type. A resolution
// is derived from the bytes on every call, so when a module is evicted its
// resolutions become SrcNoModule immediately and completely. A memo would be a
// stale answer that outlived its evidence, which is worse than no answer.
//
// Safe for concurrent use. Resolve takes the write lock because it refreshes
// recency; PC-sample decoding is batched, so the contention is per batch rather
// than per sample.
type ModuleStore struct {
	mu    sync.Mutex
	cfg   ModuleStoreConfig
	byCRC map[uint64]*moduleEntry

	// lru holds *moduleEntry, most recently used at the front.
	lru       *list.List
	liveBytes int64
	stats     ModuleStoreStats
}

// NewModuleStore constructs a bounded, LRU module store.
func NewModuleStore(cfg ModuleStoreConfig) *ModuleStore {
	if cfg.Capacity <= 0 {
		cfg.Capacity = defaultModuleStoreCapacity
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultModuleStoreMaxBytes
	}
	return &ModuleStore{
		cfg:   cfg,
		byCRC: make(map[uint64]*moduleEntry),
		lru:   list.New(),
	}
}

// Put stores a module's bytes under its CRC, parses them once, and builds the
// functionIndex -> name table Resolve uses.
//
// A CRC already held is a no-op: the bytes are not re-parsed and ModulesStored
// does not move. It still refreshes recency, because a producer re-offering a
// module is evidence that module is in use. Since cubin_crc is content
// addressed, a repeated CRC is by definition the same bytes, so there is
// nothing to update.
//
// The returned error reports that the bytes could not be parsed. It is
// DIAGNOSTIC, not a rejection: the entry is stored anyway and counted in
// ModulesUnparseable, so that a re-offer of the same bad module is not
// re-parsed, and its Resolves answer SrcNoModule. A caller must not read a
// non-nil error as "try again"; the counters, not the error, are the record.
//
// b is NOT retained; the store keeps its own copy. That is not defensiveness
// about mutation, it is the transport's contract: cubin bytes arrive as a
// sealed memfd the agent mmaps, and the mapping is dropped once the offer is
// handled. cubin.Parse is written not to retain its input for exactly that
// reason, and a store that retained it would quietly reintroduce the
// use-after-unmap the transport was designed to avoid. The copy is what
// ModuleStoreConfig.MaxBytes bounds.
func (s *ModuleStore) Put(crc uint64, b []byte) error {
	// A CRC already held costs no parse and no copy. cubin_crc is content
	// addressed, so a repeated CRC is the same bytes and there is nothing to
	// update beyond recency.
	s.mu.Lock()
	if e, ok := s.byCRC[crc]; ok {
		s.lru.MoveToFront(e.elem)
		err := e.parseErr
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	// Parsing is pure and can be slow on a large cubin, so it happens outside
	// the lock rather than under it.
	parsed, parseErr := cubin.Parse(b)
	owned := make([]byte, len(b))
	copy(owned, b)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-check: a concurrent Put for the same CRC may have landed while this
	// one was parsing. First writer wins; the loser is a no-op, exactly as a
	// re-offer is.
	if e, ok := s.byCRC[crc]; ok {
		s.lru.MoveToFront(e.elem)
		return e.parseErr
	}

	e := &moduleEntry{crc: crc, bytes: owned, parsed: parsed, parseErr: parseErr}
	switch {
	case parseErr != nil:
		s.stats.ModulesUnparseable++
	case parsed.LineInfoErr() != nil:
		s.stats.ModulesDamagedLineInfo++
	case !parsed.HasLineInfo():
		s.stats.ModulesWithoutLineInfo++
	default:
		s.stats.ModulesWithLineInfo++
	}
	if parsed != nil {
		e.byIndex = indexFunctions(parsed)
	}

	e.elem = s.lru.PushFront(e)
	s.byCRC[crc] = e
	s.liveBytes += int64(len(owned))
	s.stats.ModulesStored++
	s.evictLocked()
	return parseErr
}

// indexFunctions builds the functionIndex -> name table from a parsed cubin.
// See moduleEntry.byIndex for why .symtab index is the key and why that is a
// premise rather than a measurement.
func indexFunctions(c *cubin.Cubin) map[uint32]string {
	fns := c.Functions()
	m := make(map[uint32]string, len(fns))
	for _, fn := range fns {
		// A negative or out-of-range symbol index cannot be a functionIndex
		// on the wire, which is a uint32. Skipping is right: the name is
		// simply not addressable, and Resolve answers SrcUnmapped rather
		// than picking a neighbour.
		if fn.SymIndex < 0 || int64(fn.SymIndex) > int64(^uint32(0)) {
			continue
		}
		m[uint32(fn.SymIndex)] = fn.Name //nolint:gosec // range-checked immediately above.
	}
	return m
}

// Resolve turns a PC sample's (cubin CRC, function index, PC offset) into a
// source location, or into the reason there is not one.
//
// It has exactly four return points, one per gpu_src_status, and each
// increments exactly one Resolve* counter, so the four counters sum to the
// number of calls. The decision order is:
//
//	CRC not held                  -> no-module   (never arrived, or evicted)
//	bytes did not parse           -> no-module   (we hold bytes we cannot use)
//	.debug_line present, damaged  -> no-module   (same: bytes we cannot use)
//	no .debug_line at all         -> no-lineinfo (a build-flag fact)
//	functionIndex unknown         -> unmapped
//	line table does not cover PC  -> unmapped
//	otherwise                     -> resolved
//
// The third case is the one worth spelling out. A present-but-damaged line
// table is NOT reported as no-lineinfo, because no-lineinfo tells the operator
// to add a compiler flag they already passed - an active lie about the build.
// It is no-module for the same reason an unparseable cubin is: we hold bytes we
// cannot use, and for the reader that is the same actionable fact as holding
// nothing. ModulesDamagedLineInfo is what keeps the two causes distinguishable
// even though the answer they produce is identical.
//
// A module whose CRC is held but whose particular function carries no line-table
// sequence, in a module that does have a table, answers unmapped rather than
// no-lineinfo - by the same rule: the module was built with -lineinfo, so
// telling the reader otherwise would be wrong.
func (s *ModuleStore) Resolve(crc uint64, functionIndex uint32, pcOffset uint64) Resolution {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.byCRC[crc]
	if !ok {
		s.stats.ResolveNoModule++
		return noModule()
	}
	s.lru.MoveToFront(e.elem)

	if e.parseErr != nil || e.parsed.LineInfoErr() != nil {
		s.stats.ResolveNoModule++
		return noModule()
	}
	if !e.parsed.HasLineInfo() {
		s.stats.ResolveNoLineInfo++
		return noLineInfo()
	}
	fn, ok := e.byIndex[functionIndex]
	if !ok {
		s.stats.ResolveUnmapped++
		return unmapped()
	}
	file, line, ok := e.parsed.Resolve(fn, pcOffset)
	if !ok {
		s.stats.ResolveUnmapped++
		return unmapped()
	}
	s.stats.ResolveResolved++
	return resolvedAt(fn, file, line)
}

// Len reports how many modules are currently held.
func (s *ModuleStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byCRC)
}

// Stats returns the counters and the two gauges.
func (s *ModuleStore) Stats() ModuleStoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stats
	out.Live = len(s.byCRC)
	out.LiveBytes = s.liveBytes
	return out
}

// evictLocked drops least-recently-used modules until both bounds hold. The
// caller must hold s.mu.
//
// Capacity is enforced first and byte pressure second, and each is counted
// under its own reason so that "the store is too small" and "the modules are
// too big" are separable. The byte loop can empty the store entirely, which is
// the correct behaviour for a single module larger than MaxBytes: the bound is
// absolute, and a Resolve that then answers no-module is telling the truth.
func (s *ModuleStore) evictLocked() {
	for len(s.byCRC) > s.cfg.Capacity {
		if !s.evictOldestLocked() {
			return
		}
		s.stats.ModulesEvictedCapacity++
	}
	for s.liveBytes > s.cfg.MaxBytes {
		if !s.evictOldestLocked() {
			return
		}
		s.stats.ModulesEvictedBytes++
	}
}

// evictOldestLocked removes the least recently used module, returning false if
// there is nothing left to remove. The caller must hold s.mu.
func (s *ModuleStore) evictOldestLocked() bool {
	back := s.lru.Back()
	if back == nil {
		return false
	}
	e, ok := back.Value.(*moduleEntry)
	if !ok {
		// Unreachable: the list holds nothing else. Refusing to proceed is
		// better than a panic on a profiling path.
		s.lru.Remove(back)
		return false
	}
	s.lru.Remove(back)
	delete(s.byCRC, e.crc)
	s.liveBytes -= int64(len(e.bytes))
	s.stats.ModulesEvicted++
	return true
}
