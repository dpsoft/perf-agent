package perfdata

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnOutsideReaderCanParseWhatWeWrite is the test whose absence let #161
// ship: this package's own round-trip test reads the file back with this
// package's understanding of the format, so a writer that is wrong in a
// self-consistent way round-trips perfectly.
//
// The concrete bug was that the attr advertised sample_id_all while no
// record carried the sample_id trailer it promises. Nothing in-tree noticed.
// create_llvm_prof -- the actual consumer the PGO examples are built around
// -- rejected every file this package produced:
//
//	[ERROR] sample_info_reader.cc:286 Couldn't read PERF_SAMPLE_TID
//	[ERROR] perf_reader.cc:1053 Couldn't read event PERF_RECORD_COMM
//
// So the assertion here is specifically that an OUTSIDE parser gets through
// the record stream. Whether it then finds samples worth converting is a
// different question and deliberately not asserted: an empty profile is a
// valid outcome for synthetic input, a parse failure is not.
func TestAnOutsideReaderCanParseWhatWeWrite(t *testing.T) {
	tool, err := exec.LookPath("create_llvm_prof")
	if err != nil {
		t.Skip("create_llvm_prof not on PATH; get the prebuilt binary from " +
			"https://github.com/google/autofdo/releases (no LLVM build needed)")
	}

	// Map a real ELF so the reader has something coherent to resolve against.
	target, err := exec.LookPath("ls")
	if err != nil {
		t.Skip("no /bin/ls to use as the mapped binary")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	w, err := Open(path, EventSpec{
		Type:         perfTypeSoftware,
		Config:       perfCountSWCPUClock,
		SamplePeriod: 99,
		Frequency:    true,
	}, MetaInfo{Hostname: "test-host", OSRelease: "6.0.0-test", NumCPUs: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const base = 0x400000
	w.AddComm(CommRecord{Pid: 1234, Tid: 1234, Comm: "ls", Time: 1000, Cpu: 0})
	w.AddMmap2(Mmap2Record{
		Pid: 1234, Tid: 1234,
		Addr: base, Len: 0x100000, Pgoff: 0,
		Filename: target,
		Time:     1000, Cpu: 0,
	})
	for i := range 32 {
		ip := uint64(base + 0x1000 + i*0x10)
		w.AddSample(SampleRecord{
			IP: ip, Pid: 1234, Tid: 1234,
			Time: uint64(2000 + i), Cpu: 0, Period: 1,
			UserIPs: []uint64{ip},
		})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := filepath.Join(dir, "out.prof")
	cmd := exec.Command(tool,
		"--binary="+target,
		"--profile="+path,
		"--out="+out,
		"--use_lbr=false",
	)
	combined, runErr := cmd.CombinedOutput()
	text := string(combined)
	t.Logf("create_llvm_prof output:\n%s", strings.TrimSpace(text))

	// These are parse failures, not "nothing to convert".
	for _, bad := range []string{
		"Couldn't read",
		"Error reading profile",
		"Cannot read",
		"not a perf data file",
	} {
		if strings.Contains(text, bad) {
			t.Fatalf("an outside reader could not parse our perf.data (%q).\n"+
				"This is issue #161's shape: the attr advertises sample_id_all, so every\n"+
				"kernel-generated non-SAMPLE record owes a %d-byte sample_id trailer.\n"+
				"full output:\n%s", bad, sampleIDSize, text)
		}
	}
	if runErr != nil {
		// A non-zero exit with no parse error is acceptable: synthetic
		// samples may resolve to nothing the converter considers a profile.
		t.Logf("create_llvm_prof exited non-zero without a parse error (%v); "+
			"the record stream was read, which is what this test asserts", runErr)
	}
	if fi, err := os.Stat(out); err == nil {
		t.Logf("converter wrote %s (%d bytes)", out, fi.Size())
	}
}
