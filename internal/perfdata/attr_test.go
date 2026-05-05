package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeEventAttr_Software(t *testing.T) {
	a := eventAttr{
		typ:          perfTypeSoftware,
		config:       perfCountSWCPUClock,
		samplePeriod: 99, // Hz; freq mode set via flagsFreq
		sampleType:   sampleTypeIP | sampleTypeTID | sampleTypeTime | sampleTypeCPU | sampleTypeCallchain | sampleTypePeriod,
		flags:        flagDisabled | flagFreq | flagSampleIDAll | flagInherit | flagMmap | flagComm | flagMmap2,
		wakeupEvents: 1,
	}
	var buf bytes.Buffer
	encodeEventAttr(&buf, a)

	if buf.Len() != attrV8Size {
		t.Fatalf("attr size = %d, want %d", buf.Len(), attrV8Size)
	}

	got := buf.Bytes()
	// type at offset 0, u32 LE
	if got[0] != byte(perfTypeSoftware) || got[1] != 0 || got[2] != 0 || got[3] != 0 {
		t.Errorf("type bytes wrong: % x", got[0:4])
	}
	// size at offset 4, u32 LE = 136
	if got[4] != 0x88 || got[5] != 0 || got[6] != 0 || got[7] != 0 {
		t.Errorf("size bytes wrong: % x", got[4:8])
	}
	// config at offset 8, u64 LE = 0 (PERF_COUNT_SW_CPU_CLOCK)
	for i := 8; i < 16; i++ {
		if got[i] != 0 {
			t.Errorf("config byte %d = %02x, want 00", i, got[i])
		}
	}
}

func TestEncodeEventAttr_Hardware(t *testing.T) {
	a := eventAttr{
		typ:          perfTypeHardware,
		config:       perfCountHWCPUCycles, // = 0
		samplePeriod: 1000,
		sampleType:   sampleTypeIP | sampleTypeTID,
		flags:        flagDisabled,
	}
	var buf bytes.Buffer
	encodeEventAttr(&buf, a)
	if buf.Len() != attrV8Size {
		t.Fatalf("attr size = %d, want %d", buf.Len(), attrV8Size)
	}
	// type at offset 0 = perfTypeHardware = 0
	got := buf.Bytes()
	for i := range 4 {
		if got[i] != 0 {
			t.Errorf("type byte %d = %02x, want 00 (PERF_TYPE_HARDWARE)", i, got[i])
		}
	}
}
