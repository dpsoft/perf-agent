package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeComm(t *testing.T) {
	var buf bytes.Buffer
	// pid=42, tid=42, comm="ls", no sample_id_all suffix
	encodeComm(&buf, CommRecord{Pid: 42, Tid: 42, Comm: "ls"})

	got := buf.Bytes()
	// Header: type=PERF_RECORD_COMM=3 (u32), misc=0 (u16), size = 8 + 4 + 4 + 8 = 24 (u16)
	want := []byte{
		3, 0, 0, 0, // type = 3
		0, 0,        // misc = 0
		24, 0,       // size = 24
		42, 0, 0, 0, // pid
		42, 0, 0, 0, // tid
		'l', 's', 0, 0, 0, 0, 0, 0, // comm "ls" + NUL + padding to 8
	}
	if !bytes.Equal(got, want) {
		t.Errorf("COMM bytes mismatch:\n got: % x\nwant: % x", got, want)
	}
}

func TestEncodeFinishedRound(t *testing.T) {
	var buf bytes.Buffer
	encodeFinishedRound(&buf)

	want := []byte{
		12, 0, 0, 0, // type = PERF_RECORD_FINISHED_ROUND = 12
		0, 0,        // misc
		8, 0,        // size = 8 (header only, no payload)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("FINISHED_ROUND bytes mismatch:\n got: % x\nwant: % x", buf.Bytes(), want)
	}
}

func TestEncodeMmap2_NoBuildID(t *testing.T) {
	var buf bytes.Buffer
	encodeMmap2(&buf, Mmap2Record{
		Pid:      1234,
		Tid:      1234,
		Addr:     0x400000,
		Len:      0x1000,
		Pgoff:    0,
		Filename: "/usr/bin/ls",
		// no build-id → use the maj/min/ino union
	})

	got := buf.Bytes()
	// Expected total size:
	//   header(8) + pid(4) + tid(4) + addr(8) + len(8) + pgoff(8) +
	//   union(24: maj+min+ino+ino_gen) + prot(4) + flags(4) +
	//   filename "/usr/bin/ls" (12 chars+NUL=13, padded to 16) = 88 bytes
	if len(got) != 88 {
		t.Fatalf("MMAP2 size = %d, want 88; bytes: % x", len(got), got)
	}
	// header.type at offset 0 = PERF_RECORD_MMAP2 = 10
	if got[0] != 10 || got[1] != 0 {
		t.Errorf("type = % x, want 0a 00", got[0:2])
	}
	// header.size at offset 6 = 88 (u16 LE)
	if got[6] != 88 || got[7] != 0 {
		t.Errorf("size = % x, want 58 00", got[6:8])
	}
}

func TestEncodeMmap2_WithBuildID(t *testing.T) {
	bid := [20]byte{0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0xba, 0xbe, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	var buf bytes.Buffer
	encodeMmap2(&buf, Mmap2Record{
		Pid:         1234,
		Tid:         1234,
		Addr:        0x7f0000400000,
		Len:         0x1000,
		Pgoff:       0,
		HasBuildID:  true,
		BuildIDSize: 20,
		BuildID:     bid,
		Filename:    "/lib/x86_64-linux-gnu/libc.so.6",
	})

	got := buf.Bytes()
	// header.misc must have PERF_RECORD_MISC_MMAP_BUILD_ID = 1<<14 = 0x4000
	if got[4] != 0x00 || got[5] != 0x40 {
		t.Errorf("misc = % x, want 00 40 (MISC_MMAP_BUILD_ID)", got[4:6])
	}
	// build-id starts at offset 8(hdr) + 4(pid) + 4(tid) + 8(addr) + 8(len) + 8(pgoff) = 40
	// At offset 40: u8 build_id_size, u8[3] reserved, u8[20] build_id
	if got[40] != 20 {
		t.Errorf("build_id_size = %d, want 20", got[40])
	}
	if got[44] != 0xde || got[45] != 0xad {
		t.Errorf("build_id[0..2] = % x, want de ad", got[44:46])
	}
}

func TestEncodeSample(t *testing.T) {
	var buf bytes.Buffer
	encodeSample(&buf, SampleRecord{
		IP:        0x401000,
		Pid:       1234,
		Tid:       1234,
		Time:      1000000000, // 1 second in ns
		Cpu:       3,
		Period:    1,
		Callchain: []uint64{0x401000, 0x402000, 0x403000},
	})

	got := buf.Bytes()
	// sample_type = IP | TID | TIME | CPU | PERIOD | CALLCHAIN
	// Layout:
	//   header(8) + ip(8) + pid+tid(8) + time(8) + cpu+res(8) + period(8) +
	//   nr(8) + ips(3*8) = 80
	if len(got) != 80 {
		t.Fatalf("SAMPLE size = %d, want 80; bytes: % x", len(got), got)
	}
	// header.type at offset 0 = PERF_RECORD_SAMPLE = 9
	if got[0] != 9 {
		t.Errorf("type = %d, want 9", got[0])
	}
	// header.size at offset 6 (u16 LE) = 80
	if got[6] != 80 || got[7] != 0 {
		t.Errorf("size = % x, want 50 00", got[6:8])
	}
	// ip at offset 8 (u64 LE) = 0x401000
	wantIP := []byte{0x00, 0x10, 0x40, 0, 0, 0, 0, 0}
	if !bytes.Equal(got[8:16], wantIP) {
		t.Errorf("ip bytes = % x, want % x", got[8:16], wantIP)
	}
	// nr at offset 48 (u64 LE) = 3
	if got[48] != 3 || got[49] != 0 {
		t.Errorf("nr = % x, want 03 00", got[48:50])
	}
}
