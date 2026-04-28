package dwarfagent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf/ringbuf"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// cfiMissEvent mirrors the BPF struct cfi_miss_event in
// bpf/unwind_common.h. Layout (with padding for u64 alignment):
//
//   offset 0:  u32 pid
//   offset 4:  4 bytes pad (BPF compiler aligns u64 to 8)
//   offset 8:  u64 table_id
//   offset 16: u64 rel_pc
//   offset 24: u64 ktime_ns
//
// Total 32 bytes.
type cfiMissEvent struct {
	PID     uint32
	TableID uint64
	RelPC   uint64
	KtimeNs uint64
}

const cfiMissEventSize = 32

// parseMissEvent decodes a single cfi_miss_event from a ringbuf raw
// sample. Returns an error if the buffer is shorter than expected.
func parseMissEvent(raw []byte) (*cfiMissEvent, error) {
	if len(raw) < cfiMissEventSize {
		return nil, fmt.Errorf("miss event too short: %d bytes (want %d)", len(raw), cfiMissEventSize)
	}
	return &cfiMissEvent{
		PID:     binary.LittleEndian.Uint32(raw[0:4]),
		// bytes 4-7 are alignment padding before the u64 table_id
		TableID: binary.LittleEndian.Uint64(raw[8:16]),
		RelPC:   binary.LittleEndian.Uint64(raw[16:24]),
		KtimeNs: binary.LittleEndian.Uint64(raw[24:32]),
	}, nil
}

// Sentinel errors for the drainer's resolve step. Both are benign drops.
var (
	ErrPIDGone        = errors.New("dwarfagent: pid /proc entry vanished")
	ErrTableNotMapped = errors.New("dwarfagent: table_id not in any executable mapping")
)

// MissStats summarises the lazy-CFI drainer's lifetime activity.
// Returned by (*Profiler).MissStats() for tests and the bench harness.
type MissStats struct {
	Received           uint64 // ringbuf reads succeeded
	Deduped            uint64 // (pid, table_id) already in flight
	Resolved           uint64 // tracker.AttachCompileOnly succeeded
	DroppedPIDGone     uint64 // /proc/<pid>/maps disappeared
	DroppedNotMapped   uint64 // table_id not in any mapping
	DroppedAttach      uint64 // AttachCompileOnly errored
	PoisonedKeys       uint64 // (pid, table_id) marked permanently failed
	LastLatencyNs      uint64 // BPF emit → userspace receipt
}

// missCounters is the writable shape used internally; MissStats is the
// snapshot returned to callers.
type missCounters struct {
	received         atomic.Uint64
	deduped          atomic.Uint64
	resolved         atomic.Uint64
	droppedPIDGone   atomic.Uint64
	droppedNotMapped atomic.Uint64
	droppedAttach    atomic.Uint64
	poisonedKeys     atomic.Uint64
	lastLatencyNs    atomic.Uint64
}

func (c *missCounters) snapshot() MissStats {
	return MissStats{
		Received:         c.received.Load(),
		Deduped:          c.deduped.Load(),
		Resolved:         c.resolved.Load(),
		DroppedPIDGone:   c.droppedPIDGone.Load(),
		DroppedNotMapped: c.droppedNotMapped.Load(),
		DroppedAttach:    c.droppedAttach.Load(),
		PoisonedKeys:     c.poisonedKeys.Load(),
		LastLatencyNs:    c.lastLatencyNs.Load(),
	}
}

// cfiMissKey is the userspace dedup key — same shape as the BPF rate-limit key.
type cfiMissKey struct {
	pid     uint32
	tableID uint64
}

// consumeCFIMisses reads cfi_miss_events ringbuf records, dedupes
// per-(pid, table_id), and drives tracker.AttachCompileOnly to compile
// CFI on demand. Spawned via s.drainerWG.Add(1) + go s.consumeCFIMisses().
//
// Terminates when s.stop closes OR s.missReader.Close() is called.
func (s *session) consumeCFIMisses() {
	inflight := map[cfiMissKey]struct{}{}
	failures := map[cfiMissKey]int{}
	var mu sync.Mutex

	for {
		select {
		case <-s.stop:
			return
		default:
		}
		s.missReader.SetDeadline(time.Now().Add(200 * time.Millisecond))
		rec, err := s.missReader.Read()
		if err != nil {
			switch {
			case errors.Is(err, os.ErrDeadlineExceeded):
				continue
			case errors.Is(err, ringbuf.ErrClosed):
				return
			default:
				log.Printf("dwarfagent: miss ringbuf read: %v", err)
				return
			}
		}
		ev, err := parseMissEvent(rec.RawSample)
		if err != nil {
			continue
		}
		s.missCounters.received.Add(1)
		s.missCounters.lastLatencyNs.Store(uint64(time.Now().UnixNano()) - ev.KtimeNs)

		key := cfiMissKey{pid: ev.PID, tableID: ev.TableID}
		mu.Lock()
		if _, alreadyInflight := inflight[key]; alreadyInflight {
			mu.Unlock()
			s.missCounters.deduped.Add(1)
			continue
		}
		if failures[key] >= 3 {
			mu.Unlock()
			// poisoned — skip
			continue
		}
		inflight[key] = struct{}{}
		mu.Unlock()

		ok := s.compileForMiss(ev)

		mu.Lock()
		delete(inflight, key)
		if !ok {
			failures[key]++
			if failures[key] >= 3 {
				s.missCounters.poisonedKeys.Add(1)
			}
		} else {
			delete(failures, key)
		}
		mu.Unlock()
	}
}

// compileForMiss is the per-event work: resolve the binary path and
// call AttachCompileOnly. Returns true on success (so the drainer can
// reset the failure counter), false on any drop.
func (s *session) compileForMiss(ev *cfiMissEvent) bool {
	binPath, err := resolveBinaryByTableID(ev.PID, ev.TableID)
	if err != nil {
		switch {
		case errors.Is(err, ErrPIDGone):
			s.missCounters.droppedPIDGone.Add(1)
		case errors.Is(err, ErrTableNotMapped):
			s.missCounters.droppedNotMapped.Add(1)
		default:
			log.Printf("dwarfagent: lazy resolve pid=%d table=%#x: %v", ev.PID, ev.TableID, err)
			s.missCounters.droppedAttach.Add(1)
		}
		return false
	}
	if err := s.tracker.AttachCompileOnly(ev.PID, binPath); err != nil {
		s.missCounters.droppedAttach.Add(1)
		log.Printf("dwarfagent: lazy attach pid=%d %s: %v", ev.PID, binPath, err)
		return false
	}
	s.missCounters.resolved.Add(1)
	return true
}

// resolveBinaryByTableID reads /proc/<pid>/maps and returns the
// executable mapping path whose build-id-derived tableID matches the
// requested value. Returns ErrPIDGone if /proc/<pid>/maps is missing,
// ErrTableNotMapped if no path matches.
func resolveBinaryByTableID(pid uint32, tableID uint64) (string, error) {
	mapsPath := fmt.Sprintf("/proc/%d/maps", pid)
	data, err := os.ReadFile(mapsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrPIDGone
		}
		return "", fmt.Errorf("read %s: %w", mapsPath, err)
	}
	seen := map[string]struct{}{}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if !strings.Contains(fields[1], "x") {
			continue
		}
		path := fields[5]
		if path == "" || strings.HasPrefix(path, "[") || strings.HasPrefix(path, "//anon") {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		buildID, err := ehmaps.ReadBuildID(path)
		if err != nil {
			slog.Debug("dwarfagent: build-id read failed", "path", path, "err", err)
			continue
		}
		if ehmaps.TableIDForBuildID(buildID) == tableID {
			return path, nil
		}
	}
	return "", ErrTableNotMapped
}
