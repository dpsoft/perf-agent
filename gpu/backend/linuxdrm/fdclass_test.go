package linuxdrm

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestLookupDRMDeviceInfoFromSysfs(t *testing.T) {
	root := t.TempDir()
	sysPath := filepath.Join(root, "devices/pci0000:00/0000:00:08.1/0000:c4:00.0/drm/renderD128")
	if err := os.MkdirAll(sysPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(sysPath): %v", err)
	}
	driverTarget := filepath.Join(root, "bus/pci/drivers/amdgpu")
	if err := os.MkdirAll(driverTarget, 0o755); err != nil {
		t.Fatalf("MkdirAll(driverTarget): %v", err)
	}
	driverLink := filepath.Join(root, "devices/pci0000:00/0000:00:08.1/0000:c4:00.0/driver")
	if err := os.Symlink(driverTarget, driverLink); err != nil {
		t.Fatalf("Symlink(driver): %v", err)
	}
	devCharLink := filepath.Join(root, "226:128")
	if err := os.Symlink(sysPath, devCharLink); err != nil {
		t.Fatalf("Symlink(devChar): %v", err)
	}

	info, ok := lookupDRMDeviceInfoFrom(root, 226, 128)
	if !ok {
		t.Fatal("expected device info")
	}
	if info.Driver != "amdgpu" {
		t.Fatalf("driver=%q", info.Driver)
	}
	if info.Node != "renderD128" {
		t.Fatalf("node=%q", info.Node)
	}
}

func TestClassifyFileIdentityAddsOpenDriverAttrs(t *testing.T) {
	device, attrs := classifyFileIdentityWithLookup(rawRecord{
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       4096,
	}, func(uint32, uint32) (drmDeviceInfo, bool) {
		return drmDeviceInfo{Driver: "xe", Node: "renderD128"}, true
	})
	if device == nil {
		t.Fatal("expected device")
	}
	if got := attrs["driver"]; got != "xe" {
		t.Fatalf("driver=%q", got)
	}
	if got := attrs["drm_node"]; got != "renderD128" {
		t.Fatalf("drm_node=%q", got)
	}
}
