package linuxdrm

import (
	"fmt"
	"strconv"
)

const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14
	iocDirBits  = 2

	iocNRMask   = (1 << iocNRBits) - 1
	iocTypeMask = (1 << iocTypeBits) - 1
	iocSizeMask = (1 << iocSizeBits) - 1
	iocDirMask  = (1 << iocDirBits) - 1

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits

	drmIOCtlType   = 'd'
	drmCommandBase = 0x40
	drmCommandEnd  = 0xa0
	drmModeBase    = 0xa0
	drmModeEnd     = 0xbf
	drmSyncobjBase = 0xbf
	drmSyncobjEnd  = 0xce
)

type ioctlMetadata struct {
	Number   uint64
	Type     uint64
	Size     uint64
	Dir      uint64
	TypeChar string
	DirName  string
}

type ioctlClassification struct {
	Name       string
	Attributes map[string]string
}

func decodeIOCtl(command uint64) ioctlMetadata {
	meta := ioctlMetadata{
		Number: (command >> iocNRShift) & iocNRMask,
		Type:   (command >> iocTypeShift) & iocTypeMask,
		Size:   (command >> iocSizeShift) & iocSizeMask,
		Dir:    (command >> iocDirShift) & iocDirMask,
	}
	if meta.Type >= 32 && meta.Type <= 126 {
		meta.TypeChar = string(rune(meta.Type))
	}
	switch meta.Dir {
	case 0:
		meta.DirName = "none"
	case 1:
		meta.DirName = "write"
	case 2:
		meta.DirName = "read"
	case 3:
		meta.DirName = "readwrite"
	default:
		meta.DirName = "unknown"
	}
	return meta
}

func ioctlAttributes(command uint64) map[string]string {
	meta := decodeIOCtl(command)
	attrs := map[string]string{
		"command":      strconv.FormatUint(command, 10),
		"command_hex":  fmt.Sprintf("0x%x", command),
		"ioctl_nr":     strconv.FormatUint(meta.Number, 10),
		"ioctl_type":   strconv.FormatUint(meta.Type, 10),
		"ioctl_size":   strconv.FormatUint(meta.Size, 10),
		"ioctl_dir":    meta.DirName,
		"ioctl_dir_id": strconv.FormatUint(meta.Dir, 10),
	}
	if meta.TypeChar != "" {
		attrs["ioctl_type_char"] = meta.TypeChar
	}
	return attrs
}

func classifyIOCtl(command uint64) (ioctlClassification, bool) {
	meta := decodeIOCtl(command)
	if meta.Type != drmIOCtlType {
		return ioctlClassification{}, false
	}

	switch meta.Number {
	case 0x00:
		return classifiedIOCtl("drm-version", "drm-core", "version", "query"), true
	case 0x0c:
		return classifiedIOCtl("drm-get-cap", "drm-core", "get_cap", "query"), true
	case 0x0d:
		return classifiedIOCtl("drm-set-client-cap", "drm-core", "set_client_cap", "capability-set"), true
	case 0x09:
		return classifiedIOCtl("drm-gem-close", "drm-core", "gem_close", "memory-release"), true
	case 0x2d:
		return classifiedIOCtl("drm-prime-handle-to-fd", "drm-core", "prime_handle_to_fd", "prime-export"), true
	case 0x2e:
		return classifiedIOCtl("drm-prime-fd-to-handle", "drm-core", "prime_fd_to_handle", "prime-import"), true
	case 0x3a:
		return classifiedIOCtl("drm-wait-vblank", "drm-core", "wait_vblank", "display-wait"), true
	case 0xbc:
		return classifiedIOCtl("drm-mode-atomic", "drm-mode", "mode_atomic", "display-commit"), true
	case 0xc3:
		return classifiedIOCtl("drm-syncobj-wait", "drm-core", "syncobj_wait", "sync-wait"), true
	case 0xca:
		return classifiedIOCtl("drm-syncobj-timeline-wait", "drm-core", "syncobj_timeline_wait", "sync-wait"), true
	case 0xc5:
		return classifiedIOCtl("drm-syncobj-signal", "drm-core", "syncobj_signal", "sync-signal"), true
	case 0xcd:
		return classifiedIOCtl("drm-syncobj-timeline-signal", "drm-core", "syncobj_timeline_signal", "sync-signal"), true
	}

	if meta.Number >= drmCommandBase && meta.Number < drmCommandEnd {
		return ioctlClassification{
			Name: "drm-driver-ioctl",
			Attributes: map[string]string{
				"command_family":    "drm-driver",
				"semantic":          "driver-command",
				"drm_command_index": strconv.FormatUint(meta.Number-drmCommandBase, 10),
			},
		}, true
	}
	if meta.Number >= drmModeBase && meta.Number < drmModeEnd {
		return ioctlClassification{
			Name: "drm-mode-ioctl",
			Attributes: map[string]string{
				"command_family": "drm-mode",
				"semantic":       "display-mode",
			},
		}, true
	}
	if meta.Number >= drmSyncobjBase && meta.Number < drmSyncobjEnd {
		return ioctlClassification{
			Name: "drm-syncobj-ioctl",
			Attributes: map[string]string{
				"command_family": "drm-core",
				"semantic":       "syncobj",
			},
		}, true
	}

	return ioctlClassification{
		Name: "drm-ioctl",
		Attributes: map[string]string{
			"command_family": "drm-core",
			"semantic":       "core-ioctl",
		},
	}, true
}

func classifiedIOCtl(name, family, commandName, semantic string) ioctlClassification {
	return ioctlClassification{
		Name: name,
		Attributes: map[string]string{
			"command_family": family,
			"command_name":   commandName,
			"semantic":       semantic,
		},
	}
}
