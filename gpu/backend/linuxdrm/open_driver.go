package linuxdrm

import (
	"fmt"
	"path/filepath"
)

const sysDevCharRoot = "/sys/dev/char"

type drmDeviceInfo struct {
	Driver string
	Node   string
}

func lookupDRMDeviceInfo(major, minor uint32) (drmDeviceInfo, bool) {
	return lookupDRMDeviceInfoFrom(sysDevCharRoot, major, minor)
}

func lookupDRMDeviceInfoFrom(root string, major, minor uint32) (drmDeviceInfo, bool) {
	linkPath := filepath.Join(root, fmt.Sprintf("%d:%d", major, minor))
	resolved, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		return drmDeviceInfo{}, false
	}

	info := drmDeviceInfo{
		Node: filepath.Base(resolved),
	}

	driverLink := filepath.Join(filepath.Dir(filepath.Dir(resolved)), "driver")
	driverPath, err := filepath.EvalSymlinks(driverLink)
	if err == nil {
		info.Driver = filepath.Base(driverPath)
	}

	if info.Driver == "" && info.Node == "" {
		return drmDeviceInfo{}, false
	}
	return info, true
}
