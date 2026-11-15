package capture

// Identity pins a file's contents and attributes, so a change after
// verification is seen before the next exec. Mtime and Ctime are in
// nanoseconds; Xattr is the SHA-256 of the raw security.capability value,
// zero when the file has none.
type Identity struct {
	Dev, Ino     uint64
	Size         int64
	Mtime, Ctime int64
	Xattr        [32]byte
}

// Binary is a verified perf-agent: its resolved path, its identity at
// verification, its permitted capabilities, its Go module version and VCS
// revision, and warnings to log at start-up.
type Binary struct {
	Path     string
	ID       Identity
	Caps     CapSet
	Version  string
	Revision string
	Warnings []string
}

// Shim is a verified CUPTI adapter. perf-agent attaches to it by inode, so
// a target must map this (Dev, Ino).
type Shim struct {
	Path     string
	Dev, Ino uint64
}
