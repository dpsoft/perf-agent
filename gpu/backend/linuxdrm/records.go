package linuxdrm

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

type recordKind uint8

const (
	recordKindUnknown recordKind = iota
	recordKindIOCtl
)

type rawRecord struct {
	Kind        recordKind
	_           [3]byte
	PID         uint32
	TID         uint32
	FD          int32
	DeviceMajor uint32
	Command     uint64
	ResultCode  int64
	StartNs     uint64
	EndNs       uint64
	DeviceMinor uint32
	_           uint32
	Inode       uint64
}

func decodeRecord(data []byte) (rawRecord, error) {
	var out rawRecord
	if len(data) != binary.Size(out) {
		return rawRecord{}, fmt.Errorf("unexpected record size: got %d want %d", len(data), binary.Size(out))
	}
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &out); err != nil {
		return rawRecord{}, fmt.Errorf("decode record: %w", err)
	}
	return out, nil
}
