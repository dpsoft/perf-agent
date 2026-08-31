package pyunwind

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fakePerCPUArray answers Lookup out of a fixed slot -> per-CPU-values map,
// standing in for the BPF_MAP_TYPE_PERCPU_ARRAY. Nothing in this package can
// load a BPF program (no capabilities in unit tests), so the decoder's slot
// mapping and its summing are tested here rather than nowhere.
type fakePerCPUArray struct {
	vals map[uint32][]uint64
	err  error
}

func (f fakePerCPUArray) Lookup(key, valueOut any) error {
	if f.err != nil {
		return f.err
	}
	k, ok := key.(*uint32)
	if !ok {
		return fmt.Errorf("key is %T, want *uint32", key)
	}
	dst, ok := valueOut.(*[]uint64)
	if !ok {
		return fmt.Errorf("valueOut is %T, want *[]uint64", valueOut)
	}
	*dst = append([]uint64(nil), f.vals[*k]...)
	return nil
}

// A per-CPU counter is only meaningful summed: the CPU that took the sample
// is not the CPU that reads the map, so a reader that looked at one CPU
// would report zero on a busy machine and be believed.
func TestReadWalkCountersSumsAcrossCPUsIntoNamedFields(t *testing.T) {
	m := fakePerCPUArray{vals: map[uint32][]uint64{
		PyCntFramesPushed:     {10, 20, 12},
		PyCntTSSMiss:          {3, 4},
		PyCntNoProcInfo:       {1},
		PyCntTStateReadFail:   {0, 0},
		PyCntFrameReadFail:    {7},
		PyCntOwnerImplausible: {2, 2, 2},
		PyCntChainTruncated:   {5},
		PyCntPushRefused:      {9, 1},
		PyCntNoneExecutable:   {6},
		PyCntChainAbandoned:   {11, 4},
	}}

	got, err := ReadWalkCounters(m)
	if err != nil {
		t.Fatalf("ReadWalkCounters: %v", err)
	}
	want := WalkCounters{
		FramesPushed:     42,
		TSSMiss:          7,
		NoProcInfo:       1,
		TStateReadFail:   0,
		FrameReadFail:    7,
		OwnerImplausible: 6,
		ChainTruncated:   5,
		PushRefused:      10,
		NoneExecutable:   6,
		ChainAbandoned:   15,
	}
	if got != want {
		t.Fatalf("counters decoded to the wrong fields:\n got %+v\nwant %+v", got, want)
	}
}

// Every slot must land in a field of its own. A duplicated destination in
// ReadWalkCounters' table would make two different failures report as one
// and the other read zero forever -- silence, arrived at by typo.
func TestEveryWalkCounterSlotHasItsOwnField(t *testing.T) {
	if n := reflect.TypeOf(WalkCounters{}).NumField(); n != PyCntMax {
		t.Fatalf("WalkCounters has %d fields, py_walk_counters has %d slots", n, PyCntMax)
	}
	for slot := range uint32(PyCntMax) {
		m := fakePerCPUArray{vals: map[uint32][]uint64{slot: {1}}}
		got, err := ReadWalkCounters(m)
		if err != nil {
			t.Fatalf("slot %d: %v", slot, err)
		}
		v := reflect.ValueOf(got)
		nonZero := 0
		for i := range v.NumField() {
			if v.Field(i).Uint() != 0 {
				nonZero++
			}
		}
		if nonZero != 1 {
			t.Fatalf("slot %d set %d fields, want exactly 1: %+v", slot, nonZero, got)
		}
	}
}

func TestReadWalkCountersReportsLookupFailure(t *testing.T) {
	sentinel := errors.New("boom")
	if _, err := ReadWalkCounters(fakePerCPUArray{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("a map read failure must surface, got %v", err)
	}
}

// The Go indices and the C #defines are one contract split across two files.
// A drift makes every lookup miss (index past PY_CNT_MAX) or read the wrong
// slot, and neither end errors -- the counters simply report the wrong thing
// forever. Read out of the header text because this package cannot load the
// object.
func TestWalkCounterSlotsMirrorTheBPFHeader(t *testing.T) {
	src, err := os.ReadFile("../bpf/interp/python/python_walk.h")
	if err != nil {
		t.Fatalf("read python_walk.h: %v", err)
	}
	body := string(src)

	for _, tc := range []struct {
		macro string
		want  int
	}{
		{"PY_CNT_FRAMES_PUSHED", PyCntFramesPushed},
		{"PY_CNT_TSS_MISS", PyCntTSSMiss},
		{"PY_CNT_NO_PROC_INFO", PyCntNoProcInfo},
		{"PY_CNT_TSTATE_READ_FAIL", PyCntTStateReadFail},
		{"PY_CNT_FRAME_READ_FAIL", PyCntFrameReadFail},
		{"PY_CNT_OWNER_IMPLAUSIBLE", PyCntOwnerImplausible},
		{"PY_CNT_CHAIN_TRUNCATED", PyCntChainTruncated},
		{"PY_CNT_PUSH_REFUSED", PyCntPushRefused},
		{"PY_CNT_NONE_EXECUTABLE", PyCntNoneExecutable},
		{"PY_CNT_CHAIN_ABANDONED", PyCntChainAbandoned},
		{"PY_CNT_MAX", PyCntMax},
	} {
		want := fmt.Sprintf("#define %s ", tc.macro)
		idx := strings.Index(body, want)
		if idx < 0 {
			t.Errorf("%s is not defined in bpf/python_walk.h", tc.macro)
			continue
		}
		line := body[idx:]
		line = line[:strings.IndexByte(line, '\n')]
		if got := strings.Fields(line)[2]; got != fmt.Sprint(tc.want) {
			t.Errorf("%s is %s in the header, %d in Go", tc.macro, got, tc.want)
		}
	}
}
