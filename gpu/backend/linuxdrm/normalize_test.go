package linuxdrm

import "testing"

func TestNormalizeRecord(t *testing.T) {
	event, err := normalizeRecord(rawRecord{
		Kind:       recordKindIOCtl,
		PID:        123,
		TID:        124,
		FD:         9,
		Command:    0xc04064,
		ResultCode: -11,
		StartNs:    1000,
		EndNs:      1200,
		DeviceID:   77,
	})
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Backend != "linuxdrm" {
		t.Fatalf("backend=%q", event.Backend)
	}
	if event.Kind != "ioctl" {
		t.Fatalf("kind=%q", event.Kind)
	}
	if event.TimeNs != 1000 || event.DurationNs != 200 {
		t.Fatalf("timing=%#v", event)
	}
	if event.Source != "ebpf" || event.Confidence != "exact" {
		t.Fatalf("provenance=%#v", event)
	}
	if event.ContextID != "" || event.Queue != nil {
		t.Fatalf("expected unavailable queue/context fields: %#v", event)
	}
	if got := event.Attributes["device_id"]; got != "77" {
		t.Fatalf("device_id attr=%q", got)
	}
}

func TestNormalizeRecordRejectsUnknownKind(t *testing.T) {
	if _, err := normalizeRecord(rawRecord{}); err == nil {
		t.Fatal("expected error")
	}
}
