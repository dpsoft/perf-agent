package dwarfagent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

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
