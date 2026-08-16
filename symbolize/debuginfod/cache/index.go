package cache

import "time"

// Entry describes a cached artifact.
type Entry struct {
	BuildID    string
	Kind       Kind
	Size       int64
	LastAccess time.Time
}

// Index tracks cache entries for LRU eviction. Implementations must be
// safe for concurrent use by the cache.
type Index interface {
	// Touch records (or refreshes) an entry. Called on every cache write
	// and on every cache hit (so LastAccess reflects actual use).
	Touch(buildID string, kind Kind, size int64) error

	// TotalBytes returns the sum of recorded entry sizes.
	TotalBytes() (int64, error)

	// EvictTo deletes the LRU-oldest entries until TotalBytes ≤ maxBytes.
	// Returns the entries that were evicted (caller is responsible for
	// removing the corresponding files).
	EvictTo(maxBytes int64) ([]Entry, error)

	// Iter visits every entry. The callback returns false to stop early.
	// Used at startup to re-populate from disk.
	Iter(yield func(Entry) bool) error

	// Forget removes a single entry's row (used during file deletion).
	Forget(buildID string, kind Kind) error

	Close() error
}
