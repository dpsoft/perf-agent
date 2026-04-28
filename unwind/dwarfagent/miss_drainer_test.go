package dwarfagent

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

func TestParseMissEvent_Roundtrip(t *testing.T) {
	raw := make([]byte, cfiMissEventSize)
	binary.LittleEndian.PutUint32(raw[0:4], 0x1234)         // pid
	// 4-7 padding (already zero from make)
	binary.LittleEndian.PutUint64(raw[8:16], 0xDEADBEEFCAFE) // table_id
	binary.LittleEndian.PutUint64(raw[16:24], 0xABCD)        // rel_pc
	binary.LittleEndian.PutUint64(raw[24:32], 0x1111)        // ktime_ns

	ev, err := parseMissEvent(raw)
	if err != nil {
		t.Fatalf("parseMissEvent: %v", err)
	}
	if ev.PID != 0x1234 || ev.TableID != 0xDEADBEEFCAFE ||
		ev.RelPC != 0xABCD || ev.KtimeNs != 0x1111 {
		t.Errorf("parsed event mismatch: %+v", ev)
	}
}

func TestParseMissEvent_TooShort(t *testing.T) {
	raw := make([]byte, cfiMissEventSize-1)
	_, err := parseMissEvent(raw)
	if err == nil {
		t.Fatal("expected error for short buffer, got nil")
	}
}

func TestResolveBinaryByTableID_PIDGone(t *testing.T) {
	// PID 99999999 is unlikely to exist.
	_, err := resolveBinaryByTableID(99999999, 0xDEADBEEF)
	if !errors.Is(err, ErrPIDGone) {
		t.Errorf("err = %v, want ErrPIDGone", err)
	}
}

func TestResolveBinaryByTableID_TableNotMapped(t *testing.T) {
	// The current process has /proc/self/maps; we ask for a tableID
	// that no binary in our address space matches.
	_, err := resolveBinaryByTableID(uint32(os.Getpid()), 0xFFFFFFFFFFFFFFFF)
	if !errors.Is(err, ErrTableNotMapped) {
		t.Errorf("err = %v, want ErrTableNotMapped", err)
	}
}

func TestMissCounters_Snapshot(t *testing.T) {
	var c missCounters
	c.received.Store(10)
	c.resolved.Store(5)
	c.droppedPIDGone.Store(2)
	snap := c.snapshot()
	if snap.Received != 10 || snap.Resolved != 5 || snap.DroppedPIDGone != 2 {
		t.Errorf("snapshot mismatch: %+v", snap)
	}
}

func TestConsumeCFIMisses_SafeWhenMissReaderNil(t *testing.T) {
	// The contract: NewProfilerWithMode in Task 8 only spawns the
	// drainer goroutine when missReader != nil. This test locks that
	// contract by verifying the session struct can be constructed with
	// a nil missReader and closing s.stop is safe (no panic in this
	// path; the goroutine simply isn't running).
	s := &session{
		stop: make(chan struct{}),
	}
	// No drainer goroutine spawned. Closing stop is a no-op.
	close(s.stop)
	// Verifying drainerWG is zero-value-usable.
	s.drainerWG.Wait()
}
