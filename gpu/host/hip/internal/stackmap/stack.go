package stackmap

import "encoding/binary"

const MaxFrames = 127

func ExtractIPs(stack []byte) []uint64 {
	slots := len(stack) / 8
	if slots > MaxFrames {
		slots = MaxFrames
	}
	ips := make([]uint64, 0, slots)
	for i := 0; i < slots; i++ {
		ip := binary.LittleEndian.Uint64(stack[i*8 : i*8+8])
		if ip == 0 {
			break
		}
		ips = append(ips, ip)
	}
	return ips
}
