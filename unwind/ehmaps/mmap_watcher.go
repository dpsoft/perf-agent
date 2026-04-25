package ehmaps

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// MmapEventKind distinguishes event records emitted by MmapWatcher.
type MmapEventKind int

const (
	MmapEvent MmapEventKind = iota + 1
	ExitEvent
	ForkEvent
)

// MmapEventRecord is a parsed PERF_RECORD_MMAP2 or PERF_RECORD_EXIT.
// Fields are populated based on Kind — Exit uses PID + TID (to distinguish
// group-leader exit from per-thread exit); Mmap uses all the mapping fields.
type MmapEventRecord struct {
	Kind     MmapEventKind
	PID      uint32 // TGID
	TID      uint32 // thread ID (equals PID when the group leader itself)
	Addr     uint64
	Len      uint64
	PgOff    uint64
	Prot     uint32
	Filename string
}

const (
	// Record types per include/uapi/linux/perf_event.h.
	perfRecordMmap2 = 10
	perfRecordExit  = 4
	perfRecordFork  = 7

	// Offsets into the perf_event_mmap_page; guaranteed by the kernel ABI.
	mwDataHeadOffset = 1024
	mwDataTailOffset = 1032

	// mwRingPages data pages (power of 2). 64 × 4K = 256KB buffer.
	mwRingPages = 64
)

// MmapWatcher owns one perf_event fd + ring buffer and delivers parsed
// MMAP2 and EXIT records via Events(). Construct with NewMmapWatcher;
// always Close() when done. Close is idempotent and synchronous — it
// waits for the reader goroutine to finish before unmapping the ring.
type MmapWatcher struct {
	fd       int
	mmap     []byte
	data     []byte
	pageSz   int
	dataHead *uint64
	dataTail *uint64
	events   chan MmapEventRecord
	done     chan struct{} // closed by Close to signal shutdown
	exited   chan struct{} // closed by loop() when it has returned
}

// newMmapWatcher opens the perf_event fd, mmaps the ring, and starts
// the reader goroutine. Shared by NewMmapWatcher (per-PID, cpu=-1) and
// NewSystemWideMmapWatcher (pid=-1, per-CPU).
//
// Mmap+Mmap2 together → kernel emits PERF_RECORD_MMAP2 (the richer variant
// with inode metadata). Setting Mmap2 alone works on many kernels but is
// inconsistent in practice — libbpf and perf(1) both set both, which we
// mirror. Task enables FORK/EXIT/COMM as a bundle; we consume EXIT and FORK.
// NOTE: we deliberately do NOT set attr.inherit. The kernel rejects mmap()
// on a perf_event fd when inherit=1 is combined with a non-CPU-wide target
// (EINVAL from perf_mmap). Per-CPU watchers (pid=-1) avoid this restriction
// and capture mmaps from any task that runs on that CPU.
func newMmapWatcher(pid, cpu int) (*MmapWatcher, error) {
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_DUMMY,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Bits:   unix.PerfBitMmap | unix.PerfBitMmap2 | unix.PerfBitTask | unix.PerfBitDisabled,
	}
	fd, err := unix.PerfEventOpen(attr, pid, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("perf_event_open (mmap2, pid=%d, cpu=%d): %w", pid, cpu, err)
	}
	pageSz := os.Getpagesize()
	mmapSz := pageSz * (1 + mwRingPages)
	mapped, err := unix.Mmap(fd, 0, mmapSz, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("mmap mmap-watcher ring: %w", err)
	}
	w := &MmapWatcher{
		fd:       fd,
		mmap:     mapped,
		data:     mapped[pageSz:],
		pageSz:   pageSz,
		dataHead: (*uint64)(unsafe.Pointer(&mapped[mwDataHeadOffset])),
		dataTail: (*uint64)(unsafe.Pointer(&mapped[mwDataTailOffset])),
		events:   make(chan MmapEventRecord, 128),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("perf_event enable: %w", err)
	}
	go w.loop()
	return w, nil
}

// NewMmapWatcher attaches a per-TID watcher to pid. Requires CAP_PERFMON
// (or CAP_BPF / root). The watcher delivers PERF_RECORD_MMAP2,
// PERF_RECORD_EXIT, and PERF_RECORD_FORK records to Events(). Note that
// only mmaps issued by the exact TID we opened against generate records —
// see newMmapWatcher for background on the inherit restriction.
func NewMmapWatcher(pid uint32) (*MmapWatcher, error) {
	return newMmapWatcher(int(pid), -1)
}

// NewSystemWideMmapWatcher attaches to CPU `cpu` with pid=-1 — sees
// MMAP2/EXIT/FORK from any task that runs on that CPU. Combine N
// instances (one per online CPU) via MultiCPUMmapWatcher for
// whole-system coverage.
func NewSystemWideMmapWatcher(cpu int) (*MmapWatcher, error) {
	return newMmapWatcher(-1, cpu)
}

// Events returns the channel of parsed records. Closed when the watcher
// shuts down (via Close or unrecoverable ring error).
func (w *MmapWatcher) Events() <-chan MmapEventRecord { return w.events }

// Close stops the reader goroutine and releases the fd + mapping.
// Idempotent. Waits for the reader goroutine to return before unmapping
// so an in-flight drain() can't fault on unmapped memory.
func (w *MmapWatcher) Close() error {
	select {
	case <-w.done:
		// already closed
	default:
		close(w.done)
	}
	// Wait for loop() to finish any in-flight drain before we free
	// w.data / w.mmap. Without this, an unlucky race between
	// drain()'s atomic.LoadUint64(w.dataHead) and Munmap segfaults.
	<-w.exited
	var firstErr error
	if w.fd > 0 {
		if err := unix.IoctlSetInt(w.fd, unix.PERF_EVENT_IOC_DISABLE, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if w.mmap != nil {
		if err := unix.Munmap(w.mmap); err != nil && firstErr == nil {
			firstErr = err
		}
		w.mmap = nil
	}
	if w.fd > 0 {
		if err := unix.Close(w.fd); err != nil && firstErr == nil {
			firstErr = err
		}
		w.fd = 0
	}
	return firstErr
}

// loop is the reader goroutine. Polls with a 100ms timeout so Close() is
// responsive, then drains any records. Closes the events channel on exit.
func (w *MmapWatcher) loop() {
	defer close(w.exited)
	defer close(w.events)
	pfd := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		select {
		case <-w.done:
			return
		default:
		}
		_, _ = unix.Poll(pfd, 100)
		if !w.drain() {
			return
		}
	}
}

// drain consumes all records currently between data_tail and data_head.
// Returns false if done was signaled mid-drain (caller should exit).
func (w *MmapWatcher) drain() bool {
	head := atomic.LoadUint64(w.dataHead)
	tail := atomic.LoadUint64(w.dataTail)
	size := uint64(len(w.data))
	for tail < head {
		// perf_event_header is 8 bytes: u32 type, u16 misc, u16 size.
		base := tail % size
		hdr := w.readBytes(base, 8)
		typ := binary.LittleEndian.Uint32(hdr[0:4])
		recSize := uint64(binary.LittleEndian.Uint16(hdr[6:8]))
		if recSize < 8 || recSize > size {
			// Corrupt record — bail and let the kernel catch up.
			atomic.StoreUint64(w.dataTail, head)
			return true
		}
		var ev MmapEventRecord
		var ok bool
		switch typ {
		case perfRecordMmap2:
			body := w.readBytes((base+8)%size, recSize-8)
			ev, ok = parseMmap2(body)
		case perfRecordExit:
			body := w.readBytes((base+8)%size, recSize-8)
			ev, ok = parseExit(body)
		case perfRecordFork:
			body := w.readBytes((base+8)%size, recSize-8)
			ev, ok = parseFork(body)
		}
		if ok {
			select {
			case w.events <- ev:
			case <-w.done:
				return false
			}
		}
		tail += recSize
	}
	atomic.StoreUint64(w.dataTail, tail)
	return true
}

// readBytes reads n bytes starting at offset `off` in the ring, handling
// wraparound. Returns a slice that may alias the ring (caller must not
// retain it past the next drain step).
func (w *MmapWatcher) readBytes(off, n uint64) []byte {
	size := uint64(len(w.data))
	if off+n <= size {
		return w.data[off : off+n]
	}
	buf := make([]byte, n)
	first := size - off
	copy(buf, w.data[off:])
	copy(buf[first:], w.data[:n-first])
	return buf
}

// parseMmap2 decodes a PERF_RECORD_MMAP2 body (kernel-header layout minus
// the 8-byte perf_event_header the caller already consumed):
//
//	u32 pid, u32 tid
//	u64 addr, u64 len, u64 pgoff
//	u32 maj, u32 min, u64 ino, u64 ino_generation
//	u32 prot, u32 flags
//	char filename[]   // NUL-terminated, padded to u64
func parseMmap2(body []byte) (MmapEventRecord, bool) {
	const minHdr = 4 + 4 + 8 + 8 + 8 + 4 + 4 + 8 + 8 + 4 + 4 // = 64
	if len(body) < minHdr {
		return MmapEventRecord{}, false
	}
	pid := binary.LittleEndian.Uint32(body[0:4])
	tid := binary.LittleEndian.Uint32(body[4:8])
	addr := binary.LittleEndian.Uint64(body[8:16])
	length := binary.LittleEndian.Uint64(body[16:24])
	pgoff := binary.LittleEndian.Uint64(body[24:32])
	prot := binary.LittleEndian.Uint32(body[56:60])
	name := body[64:]
	if i := indexOfZero(name); i >= 0 {
		name = name[:i]
	}
	return MmapEventRecord{
		Kind: MmapEvent, PID: pid, TID: tid,
		Addr: addr, Len: length, PgOff: pgoff, Prot: prot,
		Filename: string(name),
	}, true
}

// parseExit decodes PERF_RECORD_EXIT body:
//
//	u32 pid, u32 ppid, u32 tid, u32 ptid, u64 time
//
// PERF_RECORD_EXIT fires per-task. When a thread in the watched TGID
// exits, pid is the TGID and tid is the exiting thread's ID. Callers
// distinguish group-leader exit (tid == pid, whole process gone) from a
// worker-thread exit (tid != pid, process still alive).
func parseExit(body []byte) (MmapEventRecord, bool) {
	if len(body) < 16 {
		return MmapEventRecord{}, false
	}
	return MmapEventRecord{
		Kind: ExitEvent,
		PID:  binary.LittleEndian.Uint32(body[0:4]),
		TID:  binary.LittleEndian.Uint32(body[8:12]),
	}, true
}

// parseFork decodes PERF_RECORD_FORK body. Shares the first 16 bytes
// of its layout with PERF_RECORD_EXIT:
//
//	u32 pid, u32 ppid, u32 tid, u32 ptid, u64 time
//
// Callers only act on group-leader forks (pid == tid) — per-thread
// forks within a tracked process are no-ops.
func parseFork(body []byte) (MmapEventRecord, bool) {
	if len(body) < 16 {
		return MmapEventRecord{}, false
	}
	return MmapEventRecord{
		Kind: ForkEvent,
		PID:  binary.LittleEndian.Uint32(body[0:4]),
		TID:  binary.LittleEndian.Uint32(body[8:12]),
	}, true
}

func indexOfZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
