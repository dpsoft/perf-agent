package gpuprobe

import (
	"slices"
	"time"

	"github.com/dpsoft/perf-agent/symbolize"
)

// Defaults for the symbolized-stack memo; see stackMemo.
const (
	defaultStackMemoEntries = 4096
	defaultStackMemoTTL     = 2 * time.Second
)

// stackMemo remembers what the symbolizer returned for a (pid, native IPs)
// pair, so a sampled launch from a call path already seen skips the
// symbolizer.
//
// Symbolization runs on the goroutine that drains the event ring, and it is
// what that goroutine spends most of its time on: about 55 µs per sampled
// launch through blazesym. A GPU workload launches from a handful of call
// paths millions of times, so nearly every capture repeats one already
// symbolized. While the drain is slow, gpu_stacks entries wait longer to be
// freed, the map fills, and the BPF side starts refusing captures
// (StackMapFull).
//
// Correctness of a hit rests on two things. The key is compared exactly (pid
// and every IP), never by hash alone. And an entry expires after ttl, because
// the same address in the same process can come to mean something else: a
// library unloaded and another mapped in its place, or a JIT rewriting a
// perf map. Within ttl a stale name is possible after such a remap; past it,
// never. Failed symbolizations are not remembered, and neither is one that
// resolved no frame: the symbolizer reports a failed batch that way, with no
// error. A partly resolved stack is remembered for ttl.
//
// The memo is bounded by entries. When full it is cleared rather than
// evicting one entry at a time: a working set larger than the bound gains
// nothing from a memo anyway, and clearing keeps an insert O(1) with no
// per-entry bookkeeping.
//
// Not safe for concurrent use; the consumer calls it under mu.
type stackMemo struct {
	max int
	ttl time.Duration
	m   map[uint64][]memoEntry
	n   int

	hits, misses, expired, clears uint64
}

type memoEntry struct {
	pid     uint32
	ips     []uint64
	frames  []symbolize.Frame
	expires int64 // UnixNano
}

// newStackMemo returns nil (a disabled memo) for max < 0.
func newStackMemo(max int, ttl time.Duration) *stackMemo {
	if max < 0 {
		return nil
	}
	if max == 0 {
		max = defaultStackMemoEntries
	}
	if ttl <= 0 {
		ttl = defaultStackMemoTTL
	}
	return &stackMemo{max: max, ttl: ttl, m: make(map[uint64][]memoEntry)}
}

func memoKey(pid uint32, ips []uint64) uint64 {
	// FNV-1a over the pid and each IP. Collisions only cost a miss: lookup
	// compares the full key.
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	h = (h ^ uint64(pid)) * prime
	for _, ip := range ips {
		h = (h ^ ip) * prime
	}
	return h
}

// get returns the remembered frames for (pid, ips). The returned slice is
// shared: callers must not modify it.
func (s *stackMemo) get(pid uint32, ips []uint64, now int64) ([]symbolize.Frame, bool) {
	if s == nil {
		return nil, false
	}
	k := memoKey(pid, ips)
	for i, e := range s.m[k] {
		if e.pid != pid || !slices.Equal(e.ips, ips) {
			continue
		}
		if now > e.expires {
			s.expired++
			s.drop(k, i)
			break
		}
		s.hits++
		return e.frames, true
	}
	s.misses++
	return nil, false
}

// put remembers frames for (pid, ips). It keeps its own copy of ips; frames
// is retained as is and must not be modified afterwards.
func (s *stackMemo) put(pid uint32, ips []uint64, frames []symbolize.Frame, now int64) {
	if s == nil {
		return
	}
	if s.n >= s.max {
		clear(s.m)
		s.n = 0
		s.clears++
	}
	k := memoKey(pid, ips)
	s.m[k] = append(s.m[k], memoEntry{
		pid:     pid,
		ips:     slices.Clone(ips),
		frames:  frames,
		expires: now + s.ttl.Nanoseconds(),
	})
	s.n++
}

func (s *stackMemo) drop(k uint64, i int) {
	b := slices.Delete(s.m[k], i, i+1)
	if len(b) == 0 {
		delete(s.m, k)
	} else {
		s.m[k] = b
	}
	s.n--
}

// frameResolved reports whether the symbolizer named f.
func frameResolved(f symbolize.Frame) bool { return f.Reason == symbolize.FailureNone }
