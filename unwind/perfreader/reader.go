// Package perfreader captures PERF_RECORD_SAMPLE events via perf_event_open
// with REGS_USER + STACK_USER so userspace can DWARF-unwind the raw stack.
// It is the input stage of the planned --unwind {fp,dwarf,auto} pipeline;
// DWARF unwinding and symbolization happen in adjacent packages.
package perfreader

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Config parameterizes a Reader. StackBytes must be a multiple of 8 and no
// larger than 65528 (kernel limit). 8192 is a reasonable default covering
// typical stack depths without blowing up ring-buffer bandwidth.
type Config struct {
	PID        int
	CPU        int    // -1 for any CPU (only valid with PID != -1)
	SampleFreq uint64 // samples per second (Hz)
	StackBytes uint32 // bytes of user stack to copy per sample
	RingPages  int    // power-of-two data pages in the ring buffer (excl. metadata page)
}

// DefaultConfig returns a sensible Config scaffold. Caller must set PID / CPU.
func DefaultConfig() Config {
	return Config{
		SampleFreq: 99,
		StackBytes: 8192,
		RingPages:  64, // 256KB ring at 4K pages
	}
}

// Reader owns one perf_event fd and its mmap'd ring buffer. A profiler
// creates one Reader per CPU (or one per PID+CPU combination) and pumps
// events from Events() until Close().
type Reader struct {
	cfg    Config
	fd     int
	mmap   []byte          // full mapping: metadata page + data pages
	meta   *perfMmapPage   // first page
	data   []byte          // the ring portion of mmap
	pageSz int
}

// NewReader opens a perf_event sampling CPU_CLOCK at cfg.SampleFreq Hz,
// attached to cfg.PID (if >= 0). Requires CAP_PERFMON and CAP_BPF at
// minimum; caller is expected to have those or equivalent.
func NewReader(cfg Config) (*Reader, error) {
	if cfg.StackBytes%8 != 0 {
		return nil, fmt.Errorf("StackBytes must be multiple of 8, got %d", cfg.StackBytes)
	}
	if cfg.RingPages == 0 || (cfg.RingPages&(cfg.RingPages-1)) != 0 {
		return nil, fmt.Errorf("RingPages must be a power of two, got %d", cfg.RingPages)
	}

	pageSz := os.Getpagesize()

	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: cfg.SampleFreq,
		Sample_type: unix.PERF_SAMPLE_IP |
			unix.PERF_SAMPLE_TID |
			unix.PERF_SAMPLE_TIME |
			unix.PERF_SAMPLE_CALLCHAIN |
			unix.PERF_SAMPLE_REGS_USER |
			unix.PERF_SAMPLE_STACK_USER,
		Sample_regs_user:  SampleRegsUser,
		Sample_stack_user: cfg.StackBytes,
		Bits:              unix.PerfBitFreq | unix.PerfBitDisabled | unix.PerfBitExcludeKernel,
		Wakeup:            1, // wake on every sample; tune later
	}

	fd, err := unix.PerfEventOpen(attr, cfg.PID, cfg.CPU, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("perf_event_open(pid=%d cpu=%d): %w", cfg.PID, cfg.CPU, err)
	}

	mmapSz := pageSz * (1 + cfg.RingPages)
	mmap, err := unix.Mmap(fd, 0, mmapSz, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("mmap perf ring: %w", err)
	}

	r := &Reader{
		cfg:    cfg,
		fd:     fd,
		mmap:   mmap,
		meta:   (*perfMmapPage)(unsafe.Pointer(&mmap[0])),
		data:   mmap[pageSz:],
		pageSz: pageSz,
	}

	if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_RESET, 0); err != nil {
		r.Close()
		return nil, fmt.Errorf("perf_event reset: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		r.Close()
		return nil, fmt.Errorf("perf_event enable: %w", err)
	}

	return r, nil
}

// Close releases the perf_event fd and its mapping.
func (r *Reader) Close() error {
	var firstErr error
	if r.fd > 0 {
		if err := unix.IoctlSetInt(r.fd, unix.PERF_EVENT_IOC_DISABLE, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r.mmap != nil {
		if err := unix.Munmap(r.mmap); err != nil && firstErr == nil {
			firstErr = err
		}
		r.mmap = nil
	}
	if r.fd > 0 {
		if err := unix.Close(r.fd); err != nil && firstErr == nil {
			firstErr = err
		}
		r.fd = 0
	}
	return firstErr
}

// FD exposes the underlying perf_event file descriptor so callers can poll
// it with epoll/select when integrating into a larger event loop.
func (r *Reader) FD() int { return r.fd }

// ReadNext drains pending records from the ring buffer, invoking cb for
// each PERF_RECORD_SAMPLE it parses. Records of types other than SAMPLE
// (mmap/comm/lost/etc.) are handled internally or ignored.
//
// Returns the number of records consumed (all types, including ignored).
// Returns 0, nil if the ring is empty — caller should poll the FD and retry.
func (r *Reader) ReadNext(cb func(Sample)) (int, error) {
	head := atomic.LoadUint64(&r.meta.DataHead)
	tail := r.meta.DataTail
	if head == tail {
		return 0, nil
	}

	ringSize := uint64(len(r.data))
	consumed := 0
	pos := tail

	for pos < head {
		// Header is u32 type + u16 misc + u16 size.
		hdrOff := pos % ringSize
		if hdrOff+uint64(perfEventHeaderSize) > ringSize {
			// Header straddles the wrap; copy it into a stack buffer.
			var hdrBuf [perfEventHeaderSize]byte
			n := ringSize - hdrOff
			copy(hdrBuf[:n], r.data[hdrOff:])
			copy(hdrBuf[n:], r.data[:perfEventHeaderSize-int(n)])
			typ := binary.LittleEndian.Uint32(hdrBuf[0:4])
			size := binary.LittleEndian.Uint16(hdrBuf[6:8])
			if err := r.handleRecord(typ, uint64(size), pos, ringSize, cb); err != nil {
				return consumed, err
			}
			pos += uint64(size)
			consumed++
			continue
		}

		typ := binary.LittleEndian.Uint32(r.data[hdrOff:])
		size := binary.LittleEndian.Uint16(r.data[hdrOff+6:])
		if err := r.handleRecord(typ, uint64(size), pos, ringSize, cb); err != nil {
			return consumed, err
		}
		pos += uint64(size)
		consumed++
	}

	atomic.StoreUint64(&r.meta.DataTail, head)
	return consumed, nil
}

// handleRecord dispatches a single record starting at absolute ring
// position `pos` of length `size` (including header). For SAMPLE records
// it parses fields in the fixed order our Sample_type mask requests.
func (r *Reader) handleRecord(typ uint32, size uint64, pos, ringSize uint64, cb func(Sample)) error {
	bodyStart := pos + uint64(perfEventHeaderSize)
	bodyLen := size - uint64(perfEventHeaderSize)

	if typ != unix.PERF_RECORD_SAMPLE {
		// Lost, mmap, comm, exit, fork, throttle — ignore for the spike.
		// The ring advance still happens in the caller.
		return nil
	}

	body := readRing(r.data, bodyStart, bodyLen, ringSize)

	sample, err := parseSample(body)
	if err != nil {
		return fmt.Errorf("parse sample at pos %d: %w", pos, err)
	}
	cb(sample)
	return nil
}

// readRing returns a byte slice view of [off, off+n) in the ring, handling
// wrap by copying into a fresh buffer. The returned slice must not be kept
// past the next ring advance.
func readRing(ring []byte, off, n, ringSize uint64) []byte {
	start := off % ringSize
	if start+n <= ringSize {
		return ring[start : start+n]
	}
	out := make([]byte, n)
	firstPart := ringSize - start
	copy(out[:firstPart], ring[start:])
	copy(out[firstPart:], ring[:n-firstPart])
	return out
}

// parseSample consumes the body of a PERF_RECORD_SAMPLE, extracting fields
// in the same order the kernel serialized them (matching our Sample_type).
func parseSample(body []byte) (Sample, error) {
	var s Sample
	r := &byteCursor{b: body}

	// PERF_SAMPLE_IP
	ip, ok := r.u64()
	if !ok {
		return s, errors.New("short sample: IP")
	}
	s.IP = ip

	// PERF_SAMPLE_TID: pid (u32) tid (u32)
	pid, ok := r.u32()
	if !ok {
		return s, errors.New("short sample: PID")
	}
	tid, ok := r.u32()
	if !ok {
		return s, errors.New("short sample: TID")
	}
	s.PID = pid
	s.TID = tid

	// PERF_SAMPLE_TIME
	t, ok := r.u64()
	if !ok {
		return s, errors.New("short sample: TIME")
	}
	s.Time = t

	// PERF_SAMPLE_CALLCHAIN: nr (u64) then nr * u64
	nr, ok := r.u64()
	if !ok {
		return s, errors.New("short sample: CALLCHAIN nr")
	}
	s.Callchain = make([]uint64, nr)
	for i := range s.Callchain {
		v, ok := r.u64()
		if !ok {
			return s, errors.New("short sample: CALLCHAIN entry")
		}
		s.Callchain[i] = v
	}

	// PERF_SAMPLE_REGS_USER: abi (u64) then popcount(mask) * u64 if abi != NONE
	abi, ok := r.u64()
	if !ok {
		return s, errors.New("short sample: REGS_USER abi")
	}
	s.ABI = abi
	if abi != unix.PERF_SAMPLE_REGS_ABI_NONE {
		nRegs := popcount(SampleRegsUser)
		s.Regs = make([]uint64, nRegs)
		for i := range s.Regs {
			v, ok := r.u64()
			if !ok {
				return s, errors.New("short sample: REGS_USER value")
			}
			s.Regs[i] = v
		}
	}

	// PERF_SAMPLE_STACK_USER: size (u64) then size bytes then dyn_size (u64)
	// size is what we REQUESTED; dyn_size is what was actually copied.
	reqSize, ok := r.u64()
	if !ok {
		return s, errors.New("short sample: STACK_USER size")
	}
	if reqSize > 0 {
		if r.remaining() < int(reqSize)+8 {
			return s, errors.New("short sample: STACK_USER bytes")
		}
		stackBytes := r.bytes(int(reqSize))
		dynSize, ok := r.u64()
		if !ok {
			return s, errors.New("short sample: STACK_USER dyn_size")
		}
		if dynSize > reqSize {
			dynSize = reqSize
		}
		// Clone the stack bytes so the caller can hold onto them past the
		// next ring advance.
		s.Stack = append([]byte(nil), stackBytes[:dynSize]...)
		if len(s.Regs) > 0 {
			s.StackAddr = RegSP(s.Regs)
		}
	}

	return s, nil
}

// popcount counts the set bits in a uint64.
func popcount(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}

// byteCursor is a tiny helper for sequential little-endian reads.
type byteCursor struct {
	b   []byte
	off int
}

func (c *byteCursor) remaining() int { return len(c.b) - c.off }

func (c *byteCursor) u32() (uint32, bool) {
	if c.off+4 > len(c.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(c.b[c.off:])
	c.off += 4
	return v, true
}

func (c *byteCursor) u64() (uint64, bool) {
	if c.off+8 > len(c.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(c.b[c.off:])
	c.off += 8
	return v, true
}

func (c *byteCursor) bytes(n int) []byte {
	out := c.b[c.off : c.off+n]
	c.off += n
	return out
}

// perfMmapPage mirrors `struct perf_event_mmap_page` up to the fields we
// use (data_head, data_tail). Kernel definition in include/uapi/linux/perf_event.h.
//
// Only the head/tail cursors are needed — the ring buffer is [page_sz, mmap_end)
// and we compute positions modulo (page_sz * RingPages).
type perfMmapPage struct {
	Version        uint32
	CompatVersion  uint32
	Lock           uint32
	Index          uint32
	Offset         int64
	TimeEnabled    uint64
	TimeRunning    uint64
	CapabilitiesEtc uint64
	PmcWidth       uint16
	TimeShift      uint16
	TimeMult       uint32
	TimeOffset     uint64
	TimeZero       uint64
	Size           uint32
	_              uint32
	_              [948]byte
	DataHead       uint64
	DataTail       uint64
	DataOffset     uint64
	DataSize       uint64
}

const perfEventHeaderSize = 8 // u32 type + u16 misc + u16 size

// ensure we actually compile for linux only
var _ = runtime.GOOS
