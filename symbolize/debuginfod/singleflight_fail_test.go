package debuginfod

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
)

type countingStore struct{}

func (countingStore) WriteAtomic(string, cache.Kind, io.Reader) (string, error) {
	return "", errors.New("unused")
}
func (countingStore) Evict() error { return nil }

// Issue #109. singleflight collapses concurrent fetches and does nothing for
// sequential ones. Symbolization runs once per SAMPLE, so a build-id no server
// can serve was re-attempted on every sample — and when the server hangs
// instead of answering, each attempt costs the whole fetch timeout. Measured:
// one call in a real capture took 30.022s, exactly the default timeout.
func TestAFailedFetchIsNotRetriedWithinItsTTL(t *testing.T) {
	// The memo is driven directly rather than through fetchAndStore: upstream
	// is a concrete *fetcher with no interface to substitute, and the memo is
	// what this test is about. The wiring that calls noteFailure on a failed
	// fetch is one line in fetchAndStore.
	sf := &singleflightFetcher{
		cache:  countingStore{},
		failed: make(map[string]fetchFailure),
	}
	key := "debuginfo:deadbeef"
	if _, ok := sf.recentFailure(key); ok {
		t.Fatal("nothing should be remembered yet")
	}
	sf.noteFailure(key, ErrNotFound)

	err, ok := sf.recentFailure(key)
	if !ok {
		t.Fatal("the failure must be remembered")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("remembered error = %v, want ErrNotFound", err)
	}
}

// The memo expires, so a transient outage does not poison symbolization for
// the life of a long-running agent.
func TestARememberedFailureExpires(t *testing.T) {
	sf := &singleflightFetcher{failed: make(map[string]fetchFailure)}
	key := "debuginfo:cafe"
	sf.failedMu.Lock()
	sf.failed[key] = fetchFailure{err: ErrNotFound, retryAfter: time.Now().Add(-time.Second)}
	sf.failedMu.Unlock()

	if _, ok := sf.recentFailure(key); ok {
		t.Fatal("an expired failure must not suppress a retry")
	}
	// And it is dropped on the way past rather than left to accumulate.
	sf.failedMu.Lock()
	_, still := sf.failed[key]
	sf.failedMu.Unlock()
	if still {
		t.Fatal("expired entry should have been deleted")
	}
}

// The memo is bounded: a machine can map more distinct build-ids than are
// worth remembering, and an unbounded map in a long-lived agent is a leak.
func TestTheFailureMemoIsBounded(t *testing.T) {
	sf := &singleflightFetcher{failed: make(map[string]fetchFailure)}
	for i := range maxFetchFailEntries + 100 {
		sf.noteFailure(string(rune('a'+i%26))+itoa(i), ErrNotFound)
	}
	sf.failedMu.Lock()
	n := len(sf.failed)
	sf.failedMu.Unlock()
	if n > maxFetchFailEntries {
		t.Fatalf("memo grew to %d, want <= %d", n, maxFetchFailEntries)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
