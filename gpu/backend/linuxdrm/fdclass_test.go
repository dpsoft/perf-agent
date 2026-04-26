package linuxdrm

import "testing"

func TestClassifyFileIdentityRenderNode(t *testing.T) {
	device, attrs := classifyFileIdentity(rawRecord{
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       4096,
	})
	if device == nil {
		t.Fatal("expected device")
	}
	if device.DeviceID != "226:128:4096" {
		t.Fatalf("device id=%q", device.DeviceID)
	}
	if device.Name != "drm-render" {
		t.Fatalf("device name=%q", device.Name)
	}
	if attrs["node_class"] != "render" {
		t.Fatalf("node_class=%q", attrs["node_class"])
	}
}

func TestClassifyFileIdentityCardNode(t *testing.T) {
	device, attrs := classifyFileIdentity(rawRecord{
		DeviceMajor: 226,
		DeviceMinor: 1,
		Inode:       55,
	})
	if device == nil {
		t.Fatal("expected device")
	}
	if device.Name != "drm-card" {
		t.Fatalf("device name=%q", device.Name)
	}
	if attrs["node_class"] != "card" {
		t.Fatalf("node_class=%q", attrs["node_class"])
	}
}

func TestClassifyFileIdentityUnknownDevice(t *testing.T) {
	device, attrs := classifyFileIdentity(rawRecord{
		DeviceMajor: 1,
		DeviceMinor: 3,
		Inode:       77,
	})
	if device != nil {
		t.Fatalf("unexpected device: %#v", device)
	}
	if attrs["node_class"] != "unknown" {
		t.Fatalf("node_class=%q", attrs["node_class"])
	}
}
