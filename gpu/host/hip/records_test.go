package hip

import (
	"bytes"
	"encoding/binary"
	"testing"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func TestDecodeLaunchRecord(t *testing.T) {
	record := rawRecord{
		PID:          123,
		TID:          456,
		TimeNs:       789,
		FunctionAddr: 0x1234,
		UserStackID:  9,
		Stream:       0xbeef,
		CgroupID:     9876,
	}

	decoder := recordDecoder{
		resolveKernel: func(pid uint32, addr uint64) (string, bool) {
			if pid != 123 || addr != 0x1234 {
				t.Fatalf("resolveKernel(%d, %#x)", pid, addr)
			}
			return "hip_kernel", true
		},
		resolveStack: func(pid uint32, stackID int32) []pp.Frame {
			if pid != 123 || stackID != 9 {
				t.Fatalf("resolveStack(%d, %d)", pid, stackID)
			}
			return []pp.Frame{
				pp.FrameFromName("train_step"),
				pp.FrameFromName("hipLaunchKernel"),
			}
		},
	}

	launch, err := decoder.decode(record)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := (launchRecord{
		Backend:       "hip",
		PID:           123,
		TID:           456,
		TimeNs:        789,
		KernelName:    "hip_kernel",
		QueueID:       "stream:beef",
		CorrelationID: "hip:123:456:789",
		Source:        "ebpf",
	}); launch.Backend != want.Backend || launch.PID != want.PID || launch.TID != want.TID || launch.TimeNs != want.TimeNs || launch.KernelName != want.KernelName || launch.QueueID != want.QueueID || launch.CorrelationID != want.CorrelationID || launch.Source != want.Source {
		t.Fatalf("launch=%+v", launch)
	}
	if got := len(launch.CPUStack); got != 2 {
		t.Fatalf("cpu stack len=%d", got)
	}
	if got := launch.Tags["cgroup_id"]; got != "9876" {
		t.Fatalf("cgroup_id=%q", got)
	}
}

func TestDecodeLaunchRecordRejectsUnknownKernel(t *testing.T) {
	decoder := recordDecoder{
		resolveKernel: func(uint32, uint64) (string, bool) { return "", false },
		resolveStack:  func(uint32, int32) []pp.Frame { return nil },
	}
	if _, err := decoder.decode(rawRecord{PID: 123, TID: 456, TimeNs: 789, FunctionAddr: 0x1234}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRawRecordBinarySizeMatchesBPFLayout(t *testing.T) {
	if got, want := binary.Size(rawRecord{}), 48; got != want {
		t.Fatalf("size=%d want=%d", got, want)
	}
}

func TestDecodeRawRecord(t *testing.T) {
	record := rawRecord{
		PID:          123,
		TID:          456,
		TimeNs:       789,
		FunctionAddr: 0x1234,
		UserStackID:  9,
		Stream:       0xbeef,
		CgroupID:     9876,
	}

	var payload bytes.Buffer
	if err := binary.Write(&payload, binary.LittleEndian, record); err != nil {
		t.Fatalf("write raw record: %v", err)
	}

	got, err := decodeRecord(payload.Bytes())
	if err != nil {
		t.Fatalf("decodeRecord: %v", err)
	}
	if got != record {
		t.Fatalf("record=%+v", got)
	}
}
