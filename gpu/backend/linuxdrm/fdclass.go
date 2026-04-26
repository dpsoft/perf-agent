package linuxdrm

import (
	"fmt"
	"strconv"

	"github.com/dpsoft/perf-agent/gpu"
)

const drmMajor = 226

func classifyFileIdentity(record rawRecord) (*gpu.GPUDeviceRef, map[string]string) {
	attrs := map[string]string{
		"device_major": strconv.FormatUint(uint64(record.DeviceMajor), 10),
		"device_minor": strconv.FormatUint(uint64(record.DeviceMinor), 10),
		"inode":        strconv.FormatUint(record.Inode, 10),
	}

	if record.DeviceMajor != drmMajor {
		attrs["node_class"] = "unknown"
		return nil, attrs
	}

	nodeClass := "card"
	name := "drm-card"
	if record.DeviceMinor >= 128 {
		nodeClass = "render"
		name = "drm-render"
	}

	deviceID := fmt.Sprintf("%d:%d:%d", record.DeviceMajor, record.DeviceMinor, record.Inode)
	attrs["device_id"] = deviceID
	attrs["node_class"] = nodeClass

	return &gpu.GPUDeviceRef{
		Backend:  "linuxdrm",
		DeviceID: deviceID,
		Name:     name,
	}, attrs
}
