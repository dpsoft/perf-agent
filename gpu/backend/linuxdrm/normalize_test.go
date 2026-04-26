package linuxdrm

import "testing"

func TestNormalizeRecord(t *testing.T) {
	event, err := normalizeRecord(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     0xc04064,
		ResultCode:  -11,
		StartNs:     1000,
		EndNs:       1200,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
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
	if event.Name != "drm-render-ioctl" {
		t.Fatalf("name=%q", event.Name)
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
	if event.Device == nil {
		t.Fatalf("expected classified device: %#v", event)
	}
	if got := event.Attributes["device_id"]; got != "226:128:77" {
		t.Fatalf("device_id attr=%q", got)
	}
	if got := event.Attributes["node_class"]; got != "render" {
		t.Fatalf("node_class=%q", got)
	}
	if got := event.Attributes["command_hex"]; got != "0xc04064" {
		t.Fatalf("command_hex=%q", got)
	}
	if got := event.Attributes["ioctl_type_char"]; got != "@" {
		t.Fatalf("ioctl_type_char=%q", got)
	}
}

func TestNormalizeRecordRejectsUnknownKind(t *testing.T) {
	if _, err := normalizeRecord(rawRecord{}); err == nil {
		t.Fatal("expected error")
	}
}
