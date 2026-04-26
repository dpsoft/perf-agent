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
)

type ioctlMetadata struct {
	Number   uint64
	Type     uint64
	Size     uint64
	Dir      uint64
	TypeChar string
	DirName  string
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
