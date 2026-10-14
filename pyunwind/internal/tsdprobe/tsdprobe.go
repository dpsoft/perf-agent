//go:build linux && (amd64 || arm64) && cgo

// Package tsdprobe measures glibc's pthread TSD layout from inside a live
// thread.
//
// It exists as a package rather than a test file because Go does not allow
// cgo in _test.go, and the measurement has to be real C calling the real
// pthread API -- the whole point is to observe where glibc actually puts
// things, not to restate what a header says.
//
// Nothing in the agent imports this. It is internal/ so nothing outside
// pyunwind can, and it is used only by pyunwind's tests.
package tsdprobe

/*
#cgo LDFLAGS: -lpthread
#include <pthread.h>
#include <stdint.h>
#include <stddef.h>

// Measures glibc's pthread TSD layout from inside a live thread, the same
// way the amd64 numbers in glibcTSDOffsets were originally obtained:
// create real keys, store unique markers through the real API, then find
// the markers relative to `struct pthread`.
//
// Nothing here reads glibc's private headers. The layout is derived from
// where the markers actually land, so the result describes the libc this
// test is running against rather than the libc it was written against.
typedef struct {
	uint64_t ok;
	uint64_t pthread_size;      // thread_pointer - pthread_self()
	uint64_t specific_1stblock; // offsetof(struct pthread, specific_1stblock)
	uint64_t key_data_size;     // sizeof(struct pthread_key_data)
	uint64_t key_data_off;      // offsetof(struct pthread_key_data, data)
} tsd_probe_t;

// Scan window, in bytes, from the start of `struct pthread`.
//
// This MUST stay inside the struct. 4096 did not: on x86-64 `struct pthread`
// is roughly 2.3 KB and the static TLS block sits at NEGATIVE offsets from
// the thread pointer, so scanning forward past the struct walked into
// unmapped memory and took the test binary down with SIGSEGV. It survived a
// single-test run and only crashed under the full suite, which is the worst
// way for a bounds bug to behave.
//
// 1536 is inside the struct on both targets -- aarch64's is 1856 bytes
// (the measured PthreadSize) and x86-64's is larger -- and comfortably past
// the furthest slot the scan needs: specific_1stblock at 0x310 plus a
// double-digit key index times the 16-byte stride is still under 1200.
#define PROBE_SCAN_BYTES 1536

static tsd_probe_t pa_probe_tsd(void) {
	tsd_probe_t out = {0};

	// Three keys: one locates the block, the other two give the stride,
	// which is what separates sizeof(pthread_key_data) from a coincidence.
	pthread_key_t k[3];
	for (int i = 0; i < 3; i++) {
		if (pthread_key_create(&k[i], NULL) != 0) return out;
	}
	// Markers are distinctive values unlikely to appear in live TLS.
	const uintptr_t marker[3] = {
		(uintptr_t)0x5041544Dul << 16 | 0x0001,
		(uintptr_t)0x5041544Dul << 16 | 0x0002,
		(uintptr_t)0x5041544Dul << 16 | 0x0003,
	};
	for (int i = 0; i < 3; i++) {
		if (pthread_setspecific(k[i], (void *)marker[i]) != 0) return out;
	}

	// On glibc, pthread_self() IS the `struct pthread *`. The thread
	// pointer is the same address on x86-64 (TLS variant II) and a higher
	// one on aarch64 (variant I, struct before the TCB) -- their
	// difference is exactly the TLS_PRE_TCB_SIZE the walk has to undo.
	uintptr_t self = (uintptr_t)pthread_self();
	uintptr_t tp   = (uintptr_t)__builtin_thread_pointer();
	out.pthread_size = (uint64_t)(tp - self);

	// Find each marker's address relative to `struct pthread`.
	uintptr_t at[3] = {0, 0, 0};
	unsigned char *base = (unsigned char *)self;
	int found = 0;
	for (size_t off = 0; off + sizeof(uintptr_t) <= PROBE_SCAN_BYTES && found < 3; off += sizeof(uintptr_t)) {
		uintptr_t v;
		__builtin_memcpy(&v, base + off, sizeof(v));
		for (int i = 0; i < 3; i++) {
			if (v == marker[i] && at[i] == 0) { at[i] = off; found++; }
		}
	}
	for (int i = 0; i < 3; i++) {
		if (at[i] == 0) return out;
	}

	// Consecutive keys are consecutive pthread_key_data structs, so the
	// gap between their data members is the struct's size.
	uint64_t stride1 = (uint64_t)(at[1] - at[0]);
	uint64_t stride2 = (uint64_t)(at[2] - at[1]);
	if (stride1 == 0 || stride1 != stride2) return out;
	out.key_data_size = stride1;

	// data sits after seq (one uintptr_t) in
	// struct pthread_key_data { uintptr_t seq; void *data; }.
	out.key_data_off = sizeof(uintptr_t);

	// at[0] is where KEY k[0]'s data landed, and k[0] is not necessarily
	// key index 0 -- the runtime may have taken earlier keys. Step back by
	// the key's own index to reach the start of the block.
	uint64_t first = (uint64_t)at[0] - (uint64_t)k[0] * stride1;
	if (first < out.key_data_off) return out;
	out.specific_1stblock = first - out.key_data_off;

	out.ok = 1;
	return out;
}
*/
import "C"

// Offsets is a measured glibc pthread TSD layout. It mirrors
// pyunwind.tsdOffsets, deliberately as a separate type: this package
// reports what it measured, and pyunwind decides whether that agrees with
// the table it serves.
type Offsets struct {
	Specific1stBlock uint64
	KeyDataSize      uint64
	KeyDataOff       uint64
	PthreadSize      uint64
}

// Measure runs the probe on the CALLING OS thread.
//
// The caller must hold runtime.LockOSThread: the probe reads this thread's
// own pthread struct, and a goroutine that migrates mid-measurement would
// mix two threads' addresses.
//
// ok is false when the layout could not be determined -- a libc that is not
// glibc, or one whose layout the scan does not recognise. That is a skip
// condition, not a failure: the probe declines rather than reporting a
// number it did not establish.
func Measure() (off Offsets, ok bool) {
	got := C.pa_probe_tsd()
	if got.ok == 0 {
		return Offsets{}, false
	}
	return Offsets{
		Specific1stBlock: uint64(got.specific_1stblock),
		KeyDataSize:      uint64(got.key_data_size),
		KeyDataOff:       uint64(got.key_data_off),
		PthreadSize:      uint64(got.pthread_size),
	}, true
}
