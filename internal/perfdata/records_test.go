package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeComm(t *testing.T) {
	var buf bytes.Buffer
	// pid=42, tid=42, comm="ls", no sample_id_all suffix
	encodeComm(&buf, commRecord{pid: 42, tid: 42, comm: "ls"})

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
