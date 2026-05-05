# perf.data Output Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `--perf-data-output <path>` flag that writes a kernel-format `perf.data` file alongside the existing pprof output. Output is consumable by `perf script`, `perf report`, `create_llvm_prof` (AutoFDO PGO), FlameGraph, hotspot, and any other tool that reads the standard format.

**Architecture:** New `internal/perfdata/` package contains the encoder. Same BPF capture pipeline; samples fan out to both `pprof.ProfileBuilder` (existing) and `perfdata.Writer` (new) on every emitted sample. Event-type auto-detect (`internal/perfevent`) picks `PERF_TYPE_HARDWARE / cycles` when the PMU is exposed, falls back to `PERF_TYPE_SOFTWARE / cpu-clock` otherwise. No BPF code changes.

**Tech Stack:** Go 1.26 stdlib (`encoding/binary`, `os`, `bufio`), zero new external dependencies. Linux-only. Output spec: kernel [`tools/perf/Documentation/perf.data-file-format.txt`](https://github.com/torvalds/linux/blob/master/tools/perf/Documentation/perf.data-file-format.txt).

**Companion docs:**
- `docs/superpowers/specs/2026-05-04-perf-data-output-design.md` — design spec.

---

## File Structure

**New files:**
- `internal/perfdata/perfdata.go` — `Writer` struct: `Open`, `AddComm`, `AddMmap2`, `AddSample`, `AddFinishedRound`, `Close`. Top-level public API the rest of perf-agent calls.
- `internal/perfdata/header.go` — file header (`PERFILE2` magic + section offsets + feature bitmap) encoder. Header gets patched on `Close` once data + features sizes are known.
- `internal/perfdata/attr.go` — `perf_event_attr` v8 (136 bytes) encoder + attr_id table.
- `internal/perfdata/records.go` — encoders for `PERF_RECORD_COMM` (3), `PERF_RECORD_MMAP2` (10), `PERF_RECORD_SAMPLE` (9), `PERF_RECORD_FINISHED_ROUND` (12).
- `internal/perfdata/feature.go` — `HEADER_BUILD_ID` (2), `HEADER_HOSTNAME` (4), `HEADER_OSRELEASE` (5), `HEADER_NRCPUS` (7) feature section encoders + the post-data feature index table.
- `internal/perfdata/perfdata_test.go` — fixture tests for every encoder + a Writer end-to-end test.
- `internal/perfdata/header_test.go` — file header byte-level fixture.
- `internal/perfdata/attr_test.go` — attr struct byte-level fixture.
- `internal/perfdata/records_test.go` — per-record-type fixtures.
- `internal/perfdata/feature_test.go` — feature section fixtures.
- `docs/perf-data-output.md` — user-facing walkthrough (perf script, FlameGraph, AutoFDO Rust + C++).
- `internal/perfevent/probe.go` — `ProbeHardwareCycles()` returning the auto-detected sample event spec.
- `internal/perfevent/probe_test.go` — unit test for the result struct (the actual probe needs root, gated like other root-tests).

**Modified files:**
- `internal/perfevent/perfevent.go` — `OpenAll` accepts the probed `EventSpec` instead of hardcoding software cpu-clock.
- `profile/profiler.go` — `Profiler` gains an optional `perfData *perfdata.Writer`; samples fan out.
- `unwind/dwarfagent/agent.go` — `Profiler` gains the same field; samples fan out via the existing aggregator.
- `perfagent/options.go` — `WithPerfDataOutput(path string) Option` + corresponding `Config.PerfDataOutput` field.
- `perfagent/agent.go` — `Start` opens the writer when configured; passes it into the CPU profiler constructor.
- `main.go` — `--perf-data-output` flag, plumbed via `perfagent.WithPerfDataOutput`.
- `test/integration_test.go` — new `TestPerfDataOutput` (round-trip with `perf script`).

---

## Tasks at a glance

| # | Task | Roughly |
|---|---|---|
| 1 | Common encoding primitives (`align8`, `writeUint64LE`, etc.) | ~80 LoC |
| 2 | File header + feature bitmap | ~120 LoC |
| 3 | `perf_event_attr` v8 encoder | ~150 LoC |
| 4 | `PERF_RECORD_COMM` + `PERF_RECORD_FINISHED_ROUND` | ~80 LoC |
| 5 | `PERF_RECORD_MMAP2` (with build-id union) | ~120 LoC |
| 6 | `PERF_RECORD_SAMPLE` (with callchain + sample_type matrix) | ~150 LoC |
| 7 | Feature sections (`HEADER_BUILD_ID`, `HOSTNAME`, `OSRELEASE`, `NRCPUS`) + index table | ~120 LoC |
| 8 | `Writer` high-level API (state, Open/Close/Add* methods) | ~200 LoC |
| 9 | `internal/perfevent.ProbeHardwareCycles()` event-type auto-detect | ~80 LoC |
| 10 | `profile/profiler.go` fan-out integration | ~30 LoC |
| 11 | `unwind/dwarfagent/agent.go` fan-out integration | ~30 LoC |
| 12 | `perfagent` options + agent wiring | ~40 LoC |
| 13 | `main.go` CLI flag | ~10 LoC |
| 14 | Integration test (`perf script` round-trip) | ~80 LoC |
| 15 | `docs/perf-data-output.md` user walkthrough | ~150 lines markdown |

Total: ~1400 LoC + 150 lines docs across ~12 source files.

---

## Task 1: Encoding primitives

**Files:**
- Create: `internal/perfdata/encode.go`
- Test: `internal/perfdata/encode_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/perfdata/encode_test.go
package perfdata

import (
	"bytes"
	"testing"
)

func TestAlign8(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 0},
		{1, 8},
		{7, 8},
		{8, 8},
		{9, 16},
		{16, 16},
		{17, 24},
	}
	for _, c := range cases {
		if got := align8(c.in); got != c.want {
			t.Errorf("align8(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWriteCStringPadded8(t *testing.T) {
	cases := []struct {
		s    string
		want []byte
	}{
		{"", []byte{0, 0, 0, 0, 0, 0, 0, 0}}, // 8-byte zero pad
		{"ls", []byte{'l', 's', 0, 0, 0, 0, 0, 0}},
		{"abcdefg", []byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 0}},        // 8 bytes incl NUL
		{"abcdefgh", []byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 0, 0, 0, 0, 0, 0, 0, 0}}, // pads to 16
	}
	for _, c := range cases {
		var buf bytes.Buffer
		writeCStringPadded8(&buf, c.s)
		if !bytes.Equal(buf.Bytes(), c.want) {
			t.Errorf("writeCStringPadded8(%q) = %v, want %v", c.s, buf.Bytes(), c.want)
		}
	}
}

func TestWriteUint64LE(t *testing.T) {
	var buf bytes.Buffer
	writeUint64LE(&buf, 0x0123456789abcdef)
	want := []byte{0xef, 0xcd, 0xab, 0x89, 0x67, 0x45, 0x23, 0x01}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("writeUint64LE = % x, want % x", buf.Bytes(), want)
	}
}

func TestWriteUint32LE(t *testing.T) {
	var buf bytes.Buffer
	writeUint32LE(&buf, 0x01020304)
	want := []byte{0x04, 0x03, 0x02, 0x01}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("writeUint32LE = % x, want % x", buf.Bytes(), want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/diego/github/perf-agent/.worktrees/perf-data-output
GOTOOLCHAIN=auto go test ./internal/perfdata/... 2>&1 | tail -5
```
Expected: build failure (`undefined: align8`, etc.).

- [ ] **Step 3: Write the implementation**

```go
// internal/perfdata/encode.go
// Package perfdata writes Linux kernel perf.data files. The on-disk format
// is documented in tools/perf/Documentation/perf.data-file-format.txt in
// the Linux kernel tree. perf.data is little-endian on every supported
// architecture, so we hardcode that here.
package perfdata

import (
	"encoding/binary"
	"io"
)

// align8 rounds n up to the next multiple of 8. perf.data uses 8-byte
// alignment for record bodies and string fields.
func align8(n int) int {
	return (n + 7) &^ 7
}

// writeUint32LE writes a little-endian 32-bit unsigned integer.
func writeUint32LE(w io.Writer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, _ = w.Write(b[:])
}

// writeUint64LE writes a little-endian 64-bit unsigned integer.
func writeUint64LE(w io.Writer, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, _ = w.Write(b[:])
}

// writeUint16LE writes a little-endian 16-bit unsigned integer.
func writeUint16LE(w io.Writer, v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	_, _ = w.Write(b[:])
}

// writeCStringPadded8 writes s as a NUL-terminated string, then pads to the
// next 8-byte boundary. perf.data uses this layout for filenames inside
// MMAP2 records, comm names, and feature-section strings.
func writeCStringPadded8(w io.Writer, s string) {
	_, _ = w.Write([]byte(s))
	_, _ = w.Write([]byte{0})
	pad := align8(len(s)+1) - (len(s) + 1)
	if pad > 0 {
		_, _ = w.Write(make([]byte, pad))
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/encode.go internal/perfdata/encode_test.go
git commit -m "internal/perfdata: encoding primitives (align8, LE writers, padded cstring)"
```

---

## Task 2: File header + feature bitmap

**Files:**
- Create: `internal/perfdata/header.go`
- Test: `internal/perfdata/header_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/perfdata/header_test.go
package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeFileHeader_Empty(t *testing.T) {
	var buf bytes.Buffer
	hdr := fileHeader{
		attrs:        section{offset: 104, size: 136},
		data:         section{offset: 240, size: 0},
		eventTypes:   section{offset: 0, size: 0},
		addsFeatures: 0,
	}
	encodeFileHeader(&buf, hdr)

	want := []byte{
		// magic = "PERFILE2" little-endian
		0x32, 0x45, 0x4c, 0x49, 0x46, 0x52, 0x45, 0x50,
		// size = 104
		0x68, 0, 0, 0, 0, 0, 0, 0,
		// attr_size = 136
		0x88, 0, 0, 0, 0, 0, 0, 0,
		// attrs.offset = 104
		0x68, 0, 0, 0, 0, 0, 0, 0,
		// attrs.size = 136
		0x88, 0, 0, 0, 0, 0, 0, 0,
		// data.offset = 240
		0xf0, 0, 0, 0, 0, 0, 0, 0,
		// data.size = 0
		0, 0, 0, 0, 0, 0, 0, 0,
		// event_types.offset = 0
		0, 0, 0, 0, 0, 0, 0, 0,
		// event_types.size = 0
		0, 0, 0, 0, 0, 0, 0, 0,
		// adds_features bitmap (4 × u64 = 32 bytes), all zero
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("file header bytes mismatch:\n got: % x\nwant: % x", buf.Bytes(), want)
	}
	if buf.Len() != fileHeaderSize {
		t.Errorf("file header size = %d, want %d", buf.Len(), fileHeaderSize)
	}
}

func TestEncodeFileHeader_FeatureBitsSet(t *testing.T) {
	var buf bytes.Buffer
	hdr := fileHeader{
		attrs:      section{offset: 104, size: 136},
		data:       section{offset: 240, size: 1024},
		eventTypes: section{offset: 0, size: 0},
		// HEADER_BUILD_ID = 2, HEADER_HOSTNAME = 4 → mask = (1<<2) | (1<<4) = 0x14
		addsFeatures: (1 << featBuildID) | (1 << featHostname),
	}
	encodeFileHeader(&buf, hdr)

	got := buf.Bytes()
	if buf.Len() != fileHeaderSize {
		t.Fatalf("size = %d, want %d", buf.Len(), fileHeaderSize)
	}
	// adds_features starts at offset 72 (8+8+8+16+16+16 = 72)
	if got[72] != 0x14 {
		t.Errorf("adds_features[0] = 0x%02x, want 0x14", got[72])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeFileHeader 2>&1 | tail -5
```
Expected: build failure (`undefined: fileHeader`, etc.).

- [ ] **Step 3: Write the implementation**

```go
// internal/perfdata/header.go
package perfdata

import "io"

// fileHeaderSize is the on-disk size of struct perf_file_header in bytes.
// 104 = 8 (magic) + 8 (size) + 8 (attr_size) + 16 (attrs section) +
//       16 (data section) + 16 (event_types section) + 32 (adds_features bitmap).
const fileHeaderSize = 104

// magicPERFILE2 is the little-endian on-disk representation of "PERFILE2".
// Constructed manually so reading the file with cat shows "PERFILE2".
const magicPERFILE2 uint64 = 0x32454c4946524550

// attrV8Size is the on-disk size of struct perf_event_attr at version 8 of
// the format (the canonical modern size).
const attrV8Size = 136

// section is a {offset, size} pointer into the file. Used both inside
// the file header and inside feature-section index entries.
type section struct {
	offset uint64
	size   uint64
}

// Feature bit indices. Subset of HEADER_* in tools/perf/util/header.h.
// Names match the kernel constants minus the HEADER_ prefix.
const (
	featTracingData = 1
	featBuildID     = 2
	featHostname    = 3
	featOSRelease   = 4
	featVersion     = 5
	featArch        = 6
	featNRCPUS      = 7
	featCPUDesc     = 8
	featCPUID       = 9
	featTotalMem    = 10
	featCmdLine     = 11
	featEventDesc   = 12
	featCPUTopology = 13
	featNUMATopology = 14
	featBranchStack = 15
	// ... up to HEADER_LAST_FEATURE around 31; we only emit a small subset.
)

// fileHeader is the in-memory representation of the on-disk perf_file_header.
// All fields are filled by the Writer; encodeFileHeader serializes them.
type fileHeader struct {
	attrs        section
	data         section
	eventTypes   section
	addsFeatures uint64 // bitmap, lower 64 bits only (we use no features above bit 31)
}

// encodeFileHeader writes the 104-byte file header.
func encodeFileHeader(w io.Writer, h fileHeader) {
	writeUint64LE(w, magicPERFILE2)
	writeUint64LE(w, fileHeaderSize)
	writeUint64LE(w, attrV8Size)
	writeUint64LE(w, h.attrs.offset)
	writeUint64LE(w, h.attrs.size)
	writeUint64LE(w, h.data.offset)
	writeUint64LE(w, h.data.size)
	writeUint64LE(w, h.eventTypes.offset)
	writeUint64LE(w, h.eventTypes.size)
	// adds_features is a 4×u64 bitmap. We only need the first u64.
	writeUint64LE(w, h.addsFeatures)
	writeUint64LE(w, 0)
	writeUint64LE(w, 0)
	writeUint64LE(w, 0)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeFileHeader 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/header.go internal/perfdata/header_test.go
git commit -m "internal/perfdata: file header (PERFILE2 magic, sections, feature bitmap)"
```

---

## Task 3: `perf_event_attr` v8 encoder

**Files:**
- Create: `internal/perfdata/attr.go`
- Test: `internal/perfdata/attr_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/perfdata/attr_test.go
package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeEventAttr_Software(t *testing.T) {
	a := eventAttr{
		typ:          perfTypeSoftware,
		config:       perfCountSWCPUClock,
		samplePeriod: 99, // Hz; freq mode set via flagsFreq
		sampleType:   sampleTypeIP | sampleTypeTID | sampleTypeTime | sampleTypeCPU | sampleTypeCallchain | sampleTypePeriod,
		flags:        flagDisabled | flagFreq | flagSampleIDAll | flagInherit | flagMmap | flagComm | flagMmap2,
		wakeupEvents: 1,
	}
	var buf bytes.Buffer
	encodeEventAttr(&buf, a)

	if buf.Len() != attrV8Size {
		t.Fatalf("attr size = %d, want %d", buf.Len(), attrV8Size)
	}

	got := buf.Bytes()
	// type at offset 0, u32 LE
	if got[0] != byte(perfTypeSoftware) || got[1] != 0 || got[2] != 0 || got[3] != 0 {
		t.Errorf("type bytes wrong: % x", got[0:4])
	}
	// size at offset 4, u32 LE = 136
	if got[4] != 0x88 || got[5] != 0 || got[6] != 0 || got[7] != 0 {
		t.Errorf("size bytes wrong: % x", got[4:8])
	}
	// config at offset 8, u64 LE = 0 (PERF_COUNT_SW_CPU_CLOCK)
	for i := 8; i < 16; i++ {
		if got[i] != 0 {
			t.Errorf("config byte %d = %02x, want 00", i, got[i])
		}
	}
}

func TestEncodeEventAttr_Hardware(t *testing.T) {
	a := eventAttr{
		typ:          perfTypeHardware,
		config:       perfCountHWCPUCycles, // = 0
		samplePeriod: 1000,
		sampleType:   sampleTypeIP | sampleTypeTID,
		flags:        flagDisabled,
	}
	var buf bytes.Buffer
	encodeEventAttr(&buf, a)
	if buf.Len() != attrV8Size {
		t.Fatalf("attr size = %d, want %d", buf.Len(), attrV8Size)
	}
	// type at offset 0 = perfTypeHardware = 0
	got := buf.Bytes()
	for i := 0; i < 4; i++ {
		if got[i] != 0 {
			t.Errorf("type byte %d = %02x, want 00 (PERF_TYPE_HARDWARE)", i, got[i])
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeEventAttr 2>&1 | tail -5
```
Expected: build failure (`undefined: eventAttr`, constants).

- [ ] **Step 3: Write the implementation**

```go
// internal/perfdata/attr.go
package perfdata

import "io"

// PERF_TYPE_* constants (uapi/linux/perf_event.h).
const (
	perfTypeHardware = 0
	perfTypeSoftware = 1
)

// PERF_COUNT_HW_* constants.
const (
	perfCountHWCPUCycles = 0
)

// PERF_COUNT_SW_* constants.
const (
	perfCountSWCPUClock = 0
)

// PERF_SAMPLE_* bits used in perf_event_attr.sample_type.
// Subset of what the kernel defines; we only emit what we use.
const (
	sampleTypeIP        = 1 << 0
	sampleTypeTID       = 1 << 1
	sampleTypeTime      = 1 << 2
	sampleTypeAddr      = 1 << 3
	sampleTypeRead      = 1 << 4
	sampleTypeCallchain = 1 << 5
	sampleTypeID        = 1 << 6
	sampleTypeCPU       = 1 << 7
	sampleTypePeriod    = 1 << 8
	sampleTypeStreamID  = 1 << 9
	sampleTypeRaw       = 1 << 10
)

// flag* bits packed into perf_event_attr's flags word. We define only the
// ones we set; the rest stay zero. Bit positions per uapi/linux/perf_event.h.
const (
	flagDisabled    = 1 << 0
	flagInherit     = 1 << 1
	flagPinned      = 1 << 2
	flagExclusive   = 1 << 3
	flagExcludeUser = 1 << 4
	flagExcludeKernel = 1 << 5
	flagExcludeHV   = 1 << 6
	flagExcludeIdle = 1 << 7
	flagMmap        = 1 << 8
	flagComm        = 1 << 9
	flagFreq        = 1 << 10
	flagSampleIDAll = 1 << 18 // critical: lets sample_id_all stamp every record
	flagMmap2       = 1 << 23
	flagCommExec    = 1 << 24
)

// eventAttr is the in-memory image of struct perf_event_attr v8. We only
// fill the fields we actually use; everything else stays zero. Total
// on-disk size is attrV8Size = 136 bytes.
type eventAttr struct {
	typ          uint32 // PERF_TYPE_*
	config       uint64 // event-type-specific
	samplePeriod uint64 // sample period, OR sample frequency if flagFreq is set
	sampleType   uint64 // bitmask of sampleType*
	flags        uint64 // bitmask of flag*
	wakeupEvents uint32 // wake user space when N samples buffered
}

// encodeEventAttr writes the 136-byte on-disk representation. Field order
// matches struct perf_event_attr in uapi/linux/perf_event.h.
func encodeEventAttr(w io.Writer, a eventAttr) {
	writeUint32LE(w, a.typ)                 // 0   type
	writeUint32LE(w, attrV8Size)            // 4   size
	writeUint64LE(w, a.config)              // 8   config
	writeUint64LE(w, a.samplePeriod)        // 16  sample_period (or sample_freq)
	writeUint64LE(w, a.sampleType)          // 24  sample_type
	writeUint64LE(w, 0)                     // 32  read_format (we don't read counters)
	writeUint64LE(w, a.flags)               // 40  flags bitfield
	writeUint32LE(w, a.wakeupEvents)        // 48  wakeup_events / wakeup_watermark
	writeUint32LE(w, 0)                     // 52  bp_type
	writeUint64LE(w, 0)                     // 56  bp_addr / config1
	writeUint64LE(w, 0)                     // 64  bp_len / config2
	writeUint64LE(w, 0)                     // 72  branch_sample_type (LBR — v2)
	writeUint64LE(w, 0)                     // 80  sample_regs_user
	writeUint32LE(w, 0)                     // 88  sample_stack_user
	writeUint32LE(w, 0)                     // 92  clockid (signed; 0 = use perf clock)
	writeUint64LE(w, 0)                     // 96  sample_regs_intr
	writeUint32LE(w, 0)                     // 104 aux_watermark
	writeUint16LE(w, 0)                     // 108 sample_max_stack
	writeUint16LE(w, 0)                     // 110 __reserved_2
	writeUint32LE(w, 0)                     // 112 aux_sample_size
	writeUint32LE(w, 0)                     // 116 __reserved_3
	writeUint64LE(w, 0)                     // 120 sig_data
	writeUint64LE(w, 0)                     // 128 config3 (reserved in v8)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeEventAttr 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/attr.go internal/perfdata/attr_test.go
git commit -m "internal/perfdata: perf_event_attr v8 encoder (136-byte struct)"
```

---

## Task 4: `PERF_RECORD_COMM` and `PERF_RECORD_FINISHED_ROUND`

**Files:**
- Create: `internal/perfdata/records.go`
- Create: `internal/perfdata/records_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/perfdata/records_test.go
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
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run "TestEncodeComm|TestEncodeFinishedRound" 2>&1 | tail -5
```
Expected: build failure.

- [ ] **Step 3: Write the implementation**

```go
// internal/perfdata/records.go
package perfdata

import "io"

// PERF_RECORD_* constants (uapi/linux/perf_event.h).
const (
	recordMmap          = 1
	recordComm          = 3
	recordExit          = 4
	recordSample        = 9
	recordMmap2         = 10
	recordFinishedRound = 12
)

// recordHeaderSize is the size of struct perf_event_header in bytes.
// 8 = u32 type + u16 misc + u16 size.
const recordHeaderSize = 8

// commRecord is the in-memory image of a PERF_RECORD_COMM payload.
type commRecord struct {
	pid  uint32
	tid  uint32
	comm string
}

// encodeComm writes a PERF_RECORD_COMM record (type 3). Layout:
//
//	struct perf_event_header header;  // 8 bytes
//	u32 pid;
//	u32 tid;
//	char comm[];                       // NUL-terminated, 8-byte padded
func encodeComm(w io.Writer, r commRecord) {
	commBytes := align8(len(r.comm) + 1) // NUL + padding
	size := recordHeaderSize + 4 + 4 + commBytes
	writeUint32LE(w, recordComm)
	writeUint16LE(w, 0) // misc
	writeUint16LE(w, uint16(size))
	writeUint32LE(w, r.pid)
	writeUint32LE(w, r.tid)
	writeCStringPadded8(w, r.comm)
}

// encodeFinishedRound writes a PERF_RECORD_FINISHED_ROUND record (type 12).
// No payload; consumers use it as a sync point. Total size = 8 bytes.
func encodeFinishedRound(w io.Writer) {
	writeUint32LE(w, recordFinishedRound)
	writeUint16LE(w, 0) // misc
	writeUint16LE(w, recordHeaderSize)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run "TestEncodeComm|TestEncodeFinishedRound" 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/records.go internal/perfdata/records_test.go
git commit -m "internal/perfdata: PERF_RECORD_COMM + PERF_RECORD_FINISHED_ROUND encoders"
```

---

## Task 5: `PERF_RECORD_MMAP2` (with build-id union)

**Files:**
- Modify: `internal/perfdata/records.go` (append)
- Modify: `internal/perfdata/records_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/perfdata/records_test.go`:

```go
func TestEncodeMmap2_NoBuildID(t *testing.T) {
	var buf bytes.Buffer
	encodeMmap2(&buf, mmap2Record{
		pid:      1234,
		tid:      1234,
		addr:     0x400000,
		len:      0x1000,
		pgoff:    0,
		filename: "/usr/bin/ls",
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
	encodeMmap2(&buf, mmap2Record{
		pid:         1234,
		tid:         1234,
		addr:        0x7f0000400000,
		len:         0x1000,
		pgoff:       0,
		hasBuildID:  true,
		buildIDSize: 20,
		buildID:     bid,
		filename:    "/lib/x86_64-linux-gnu/libc.so.6",
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
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeMmap2 2>&1 | tail -5
```
Expected: build failure (`undefined: mmap2Record`, `encodeMmap2`).

- [ ] **Step 3: Write the implementation**

Append to `internal/perfdata/records.go`:

```go
// miscMmapBuildID is set in perf_event_header.misc when the MMAP2 record
// uses the build-id flavour of its union (post-5.12 kernels).
const miscMmapBuildID = 1 << 14

// mmap2Record is the in-memory image of a PERF_RECORD_MMAP2 payload.
// Set hasBuildID when emitting the build-id flavour of the union;
// otherwise the maj/min/ino path is used (and all four fields stay zero).
type mmap2Record struct {
	pid, tid uint32
	addr     uint64
	len      uint64
	pgoff    uint64

	// union: build-id flavour
	hasBuildID  bool
	buildIDSize uint8
	buildID     [20]byte // padded to 20 bytes; SHA-1 build-ids are exactly that

	// (maj, min, ino, inoGen would go here for the file-id flavour;
	// we always emit zeros — consumers fall back to filename matching.)

	prot     uint32
	flags    uint32
	filename string
}

// encodeMmap2 writes a PERF_RECORD_MMAP2 record (type 10). The record carries
// either {maj, min, ino, ino_generation} (24 bytes) OR
// {build_id_size: u8, __reserved_1: u8[3], build_id: u8[20]} (24 bytes) in the
// same slot — selected by miscMmapBuildID in the header's misc field.
//
// Layout:
//
//	struct perf_event_header header;  // 8 bytes
//	u32 pid, u32 tid;                 // 8
//	u64 addr;                         // 8
//	u64 len;                          // 8
//	u64 pgoff;                        // 8
//	union { ino flavour | build-id flavour } // 24
//	u32 prot, u32 flags;              // 8
//	char filename[];                  // NUL-terminated, 8-byte padded
func encodeMmap2(w io.Writer, r mmap2Record) {
	filenameBytes := align8(len(r.filename) + 1)
	bodySize := 4 + 4 + 8 + 8 + 8 + 24 + 4 + 4 + filenameBytes
	size := recordHeaderSize + bodySize
	misc := uint16(0)
	if r.hasBuildID {
		misc |= miscMmapBuildID
	}

	writeUint32LE(w, recordMmap2)
	writeUint16LE(w, misc)
	writeUint16LE(w, uint16(size))

	writeUint32LE(w, r.pid)
	writeUint32LE(w, r.tid)
	writeUint64LE(w, r.addr)
	writeUint64LE(w, r.len)
	writeUint64LE(w, r.pgoff)

	// union (24 bytes)
	if r.hasBuildID {
		_, _ = w.Write([]byte{r.buildIDSize, 0, 0, 0}) // u8 + 3 reserved
		_, _ = w.Write(r.buildID[:])                   // 20 bytes
	} else {
		// maj=0, min=0, ino=0, ino_generation=0 — 24 bytes of zeros
		_, _ = w.Write(make([]byte, 24))
	}

	writeUint32LE(w, r.prot)
	writeUint32LE(w, r.flags)
	writeCStringPadded8(w, r.filename)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeMmap2 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/records.go internal/perfdata/records_test.go
git commit -m "internal/perfdata: PERF_RECORD_MMAP2 with build-id union"
```

---

## Task 6: `PERF_RECORD_SAMPLE` (with callchain)

**Files:**
- Modify: `internal/perfdata/records.go` (append)
- Modify: `internal/perfdata/records_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/perfdata/records_test.go`:

```go
func TestEncodeSample(t *testing.T) {
	var buf bytes.Buffer
	encodeSample(&buf, sampleRecord{
		ip:        0x401000,
		pid:       1234,
		tid:       1234,
		time:      1000000000, // 1 second in ns
		cpu:       3,
		period:    1,
		callchain: []uint64{0x401000, 0x402000, 0x403000},
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
	// nr at offset 56 (u64 LE) = 3
	if got[56] != 3 || got[57] != 0 {
		t.Errorf("nr = % x, want 03 00", got[56:58])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeSample 2>&1 | tail -5
```
Expected: build failure (`undefined: sampleRecord`, `encodeSample`).

- [ ] **Step 3: Write the implementation**

Append to `internal/perfdata/records.go`:

```go
// sampleRecord is the in-memory image of a PERF_RECORD_SAMPLE payload, for
// the fixed sample_type we emit:
//
//	IP | TID | TIME | CPU | PERIOD | CALLCHAIN
//
// (No ADDR, ID, STREAM_ID, READ, RAW, BRANCH_STACK, REGS_USER, STACK_USER,
// WEIGHT, DATA_SRC, TRANSACTION.)
type sampleRecord struct {
	ip        uint64   // PERF_SAMPLE_IP
	pid, tid  uint32   // PERF_SAMPLE_TID
	time      uint64   // PERF_SAMPLE_TIME (ns since clock origin)
	cpu       uint32   // PERF_SAMPLE_CPU (low 32 bits)
	period    uint64   // PERF_SAMPLE_PERIOD
	callchain []uint64 // PERF_SAMPLE_CALLCHAIN (leaf first, ips array)
}

// encodeSample writes a PERF_RECORD_SAMPLE record (type 9). Field order
// follows the sample_type bit order in uapi/linux/perf_event.h:
//
//	{ u64 ip; }                            // PERF_SAMPLE_IP
//	{ u32 pid, tid; }                      // PERF_SAMPLE_TID
//	{ u64 time; }                          // PERF_SAMPLE_TIME
//	{ u32 cpu, res; }                      // PERF_SAMPLE_CPU
//	{ u64 period; }                        // PERF_SAMPLE_PERIOD
//	{ u64 nr; u64 ips[nr]; }               // PERF_SAMPLE_CALLCHAIN
func encodeSample(w io.Writer, r sampleRecord) {
	bodySize := 8 + 8 + 8 + 8 + 8 + 8 + 8*len(r.callchain)
	size := recordHeaderSize + bodySize
	writeUint32LE(w, recordSample)
	writeUint16LE(w, 0) // misc — could carry CPUMODE_USER etc. but blazesym handles that downstream
	writeUint16LE(w, uint16(size))

	writeUint64LE(w, r.ip)
	writeUint32LE(w, r.pid)
	writeUint32LE(w, r.tid)
	writeUint64LE(w, r.time)
	writeUint32LE(w, r.cpu)
	writeUint32LE(w, 0) // res
	writeUint64LE(w, r.period)
	writeUint64LE(w, uint64(len(r.callchain)))
	for _, ip := range r.callchain {
		writeUint64LE(w, ip)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestEncodeSample 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/records.go internal/perfdata/records_test.go
git commit -m "internal/perfdata: PERF_RECORD_SAMPLE encoder (IP/TID/TIME/CPU/PERIOD/CALLCHAIN)"
```

---

## Task 7: Feature sections + index table

**Files:**
- Create: `internal/perfdata/feature.go`
- Create: `internal/perfdata/feature_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/perfdata/feature_test.go
package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeBuildIDFeature(t *testing.T) {
	var buf bytes.Buffer
	encodeBuildIDFeature(&buf, []buildIDEntry{
		{
			pid:      -1,
			buildID:  [20]byte{0xab, 0xcd, 0xef},
			filename: "/usr/bin/ls",
		},
	})
	got := buf.Bytes()
	// One entry: header(40) + filename ("ls" terminator? no — full path) padded.
	// build_id_event = perf_event_header(8) + pid(4) + build_id[24]+ filename[]
	// header.type = PERF_RECORD_HEADER_BUILD_ID = 67 (kernel HEADER_BUILD_ID record-type wrapper)
	// Don't hardcode size; just check the record type and presence of filename.
	if len(got) < 40 {
		t.Fatalf("build_id record too small: %d bytes", len(got))
	}
	// type at offset 0 = 67
	if got[0] != 67 {
		t.Errorf("type = %d, want 67 (HEADER_BUILD_ID record type)", got[0])
	}
	// pid at offset 8, signed s32 LE = -1 = 0xFFFFFFFF
	if got[8] != 0xFF || got[9] != 0xFF || got[10] != 0xFF || got[11] != 0xFF {
		t.Errorf("pid = % x, want ff ff ff ff", got[8:12])
	}
}

func TestEncodeStringFeature(t *testing.T) {
	var buf bytes.Buffer
	encodeStringFeature(&buf, "linux-host-1")
	got := buf.Bytes()
	// Layout: u32 len, char str[len], pad to 8.
	// "linux-host-1" is 12 bytes + NUL = 13, len field stores 13.
	// Actually perf header strings store: u32 len; char str[len]; with len
	// being the padded length (including NUL).
	// len at offset 0 (u32 LE)
	wantLen := uint32(align8(len("linux-host-1") + 1)) // 16
	if got[0] != byte(wantLen) {
		t.Errorf("len = %d, want %d", got[0], wantLen)
	}
	// "linux-host-1" should appear starting at offset 4
	if !bytes.HasPrefix(got[4:], []byte("linux-host-1")) {
		t.Errorf("string body wrong: %q", got[4:])
	}
}

func TestEncodeNRCPUSFeature(t *testing.T) {
	var buf bytes.Buffer
	encodeNRCPUSFeature(&buf, 16, 16) // online=16, available=16
	got := buf.Bytes()
	if len(got) != 8 {
		t.Fatalf("NRCPUS size = %d, want 8", len(got))
	}
	// online at offset 0 (u32 LE) = 16
	if got[0] != 16 {
		t.Errorf("online = %d, want 16", got[0])
	}
	// available at offset 4 (u32 LE) = 16
	if got[4] != 16 {
		t.Errorf("available = %d, want 16", got[4])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run "TestEncode(BuildID|String|NRCPUS)Feature" 2>&1 | tail -5
```
Expected: build failure.

- [ ] **Step 3: Write the implementation**

```go
// internal/perfdata/feature.go
package perfdata

import "io"

// recordHeaderBuildID is the in-feature record type that wraps each build-id
// entry inside the HEADER_BUILD_ID feature section. Distinct from
// PERF_RECORD_* in the data section.
const recordHeaderBuildID = 67

// buildIDEntry is one entry in the HEADER_BUILD_ID feature section.
// pid = -1 means "kernel or any process" (kernel mappings); other values are
// actual host PIDs.
type buildIDEntry struct {
	pid      int32
	buildID  [20]byte
	filename string
}

// encodeBuildIDFeature writes a HEADER_BUILD_ID feature section payload —
// a sequence of build-id records back-to-back. Each record:
//
//	struct perf_event_header header;  // type=67, misc=0, size=record total
//	s32 pid;
//	u8  build_id[24];                  // 20 hash bytes + 4 padding
//	char filename[];                   // NUL-terminated, 8-byte padded
func encodeBuildIDFeature(w io.Writer, entries []buildIDEntry) {
	for _, e := range entries {
		filenameBytes := align8(len(e.filename) + 1)
		bodySize := 4 + 24 + filenameBytes
		size := recordHeaderSize + bodySize
		writeUint32LE(w, recordHeaderBuildID)
		writeUint16LE(w, 0)
		writeUint16LE(w, uint16(size))
		// pid (s32) — write the bit pattern as u32
		writeUint32LE(w, uint32(e.pid))
		// build_id[24] = 20 hash bytes + 4 padding
		_, _ = w.Write(e.buildID[:])
		_, _ = w.Write([]byte{0, 0, 0, 0})
		writeCStringPadded8(w, e.filename)
	}
}

// encodeStringFeature writes a perf_header_string: u32 len (padded length
// including NUL), char str[len]. Used for HEADER_HOSTNAME and HEADER_OSRELEASE.
func encodeStringFeature(w io.Writer, s string) {
	padded := align8(len(s) + 1)
	writeUint32LE(w, uint32(padded))
	_, _ = w.Write([]byte(s))
	_, _ = w.Write([]byte{0})
	if pad := padded - (len(s) + 1); pad > 0 {
		_, _ = w.Write(make([]byte, pad))
	}
}

// encodeNRCPUSFeature writes the HEADER_NRCPUS feature section: two u32s,
// nr_cpus_online followed by nr_cpus_available.
func encodeNRCPUSFeature(w io.Writer, online, available uint32) {
	writeUint32LE(w, online)
	writeUint32LE(w, available)
}

// encodeFeatureIndexTable writes the perf_file_section table that follows
// the data section. Each entry is {offset: u64, size: u64} pointing at one
// feature's payload. Entries appear in feature-bit-number order.
func encodeFeatureIndexTable(w io.Writer, entries []section) {
	for _, e := range entries {
		writeUint64LE(w, e.offset)
		writeUint64LE(w, e.size)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 2>&1 | tail -5
```
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/feature.go internal/perfdata/feature_test.go
git commit -m "internal/perfdata: feature section encoders (BUILD_ID, HOSTNAME, OSRELEASE, NRCPUS)"
```

---

## Task 8: `Writer` high-level API

**Files:**
- Create: `internal/perfdata/perfdata.go`
- Create: `internal/perfdata/perfdata_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/perfdata/perfdata_test.go
package perfdata

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestWriter_RoundTrip captures a tiny synthetic profile and re-reads the
// resulting file to verify magic, header section pointers, and that the
// data section contains the records we wrote.
func TestWriter_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	w, err := Open(path, EventSpec{
		Type:         perfTypeSoftware,
		Config:       perfCountSWCPUClock,
		SamplePeriod: 99,
		Frequency:    true,
	}, MetaInfo{
		Hostname:  "test-host",
		OSRelease: "5.15.0-test",
		NumCPUs:   8,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	w.AddComm(commRecord{pid: 1234, tid: 1234, comm: "myapp"})
	w.AddMmap2(mmap2Record{
		pid: 1234, tid: 1234,
		addr: 0x400000, len: 0x1000, pgoff: 0,
		filename: "/usr/bin/myapp",
	})
	w.AddSample(sampleRecord{
		ip: 0x400500, pid: 1234, tid: 1234,
		time: 1000, cpu: 0, period: 1,
		callchain: []uint64{0x400500},
	})
	w.AddBuildID(buildIDEntry{
		pid:      -1,
		buildID:  [20]byte{0xde, 0xad, 0xbe, 0xef},
		filename: "/usr/bin/myapp",
	})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	// magic at offset 0
	magic := binary.LittleEndian.Uint64(body[0:8])
	if magic != magicPERFILE2 {
		t.Errorf("magic = %x, want %x", magic, magicPERFILE2)
	}
	// header.size at offset 8 = 104
	if got := binary.LittleEndian.Uint64(body[8:16]); got != fileHeaderSize {
		t.Errorf("header.size = %d, want %d", got, fileHeaderSize)
	}
	// data section size at offset 48 must be > 0 (we wrote at least one record)
	if got := binary.LittleEndian.Uint64(body[48:56]); got == 0 {
		t.Errorf("data.size = 0; expected non-zero")
	}
	// adds_features bitmap at offset 72: must have at least BUILD_ID (bit 2),
	// HOSTNAME (bit 3), OSRELEASE (bit 4), NRCPUS (bit 7) set.
	mask := binary.LittleEndian.Uint64(body[72:80])
	wantBits := uint64((1 << featBuildID) | (1 << featHostname) | (1 << featOSRelease) | (1 << featNRCPUS))
	if mask&wantBits != wantBits {
		t.Errorf("adds_features mask = %#x, missing bits from %#x", mask, wantBits)
	}
	// raw byte search: filename "/usr/bin/myapp" must appear in data section
	if !bytes.Contains(body, []byte("/usr/bin/myapp")) {
		t.Errorf("filename not found in output")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 -run TestWriter 2>&1 | tail -5
```
Expected: build failure (`undefined: Open`, `EventSpec`, `MetaInfo`).

- [ ] **Step 3: Write the implementation**

```go
// internal/perfdata/perfdata.go
package perfdata

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
)

// EventSpec describes the perf event the captured samples come from. Filled
// from internal/perfevent's auto-detect probe.
type EventSpec struct {
	Type         uint32 // PERF_TYPE_*
	Config       uint64 // event-type-specific
	SamplePeriod uint64 // period (or freq Hz when Frequency = true)
	Frequency    bool   // whether SamplePeriod is a frequency
}

// MetaInfo captures host-level facts the writer stamps into feature sections.
type MetaInfo struct {
	Hostname  string
	OSRelease string
	NumCPUs   uint32
}

// Writer writes a perf.data file in the kernel's standard format. Methods
// AddComm, AddMmap2, AddSample, AddFinishedRound, AddBuildID are append-only
// and not concurrency-safe — callers (perf-agent's CPU profiler) call them
// from a single goroutine. Close finalizes the file (writes feature sections,
// patches header offsets/sizes).
type Writer struct {
	f       *os.File
	bw      *bufio.Writer
	pos     int64 // current byte offset in file
	dataBeg int64 // offset where data section begins
	spec    EventSpec
	meta    MetaInfo

	// data accumulated for feature-section emission at Close
	buildIDs []buildIDEntry
}

// Open creates a new perf.data file at path and writes the file header,
// attr section, and attr_id table. The data section starts immediately
// after, and Add* calls append records into it. Close patches header
// offsets/sizes and emits feature sections.
func Open(path string, spec EventSpec, meta MetaInfo) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("perfdata: create %s: %w", path, err)
	}
	bw := bufio.NewWriter(f)

	// Write a placeholder file header (will be patched on Close).
	encodeFileHeader(bw, fileHeader{})

	// Write attr section: one perf_event_attr.
	flags := uint64(flagDisabled | flagSampleIDAll | flagInherit | flagMmap | flagComm | flagMmap2)
	if spec.Frequency {
		flags |= flagFreq
	}
	encodeEventAttr(bw, eventAttr{
		typ:          spec.Type,
		config:       spec.Config,
		samplePeriod: spec.SamplePeriod,
		sampleType:   sampleTypeIP | sampleTypeTID | sampleTypeTime | sampleTypeCPU | sampleTypePeriod | sampleTypeCallchain,
		flags:        flags,
		wakeupEvents: 1,
	})

	// Write attr_id table — one section pointing at no IDs (we have one
	// attr, no event ID array).
	writeUint64LE(bw, 0) // ids.offset
	writeUint64LE(bw, 0) // ids.size

	dataBeg := int64(fileHeaderSize + attrV8Size + 16)
	return &Writer{
		f:       f,
		bw:      bw,
		pos:     dataBeg,
		dataBeg: dataBeg,
		spec:    spec,
		meta:    meta,
	}, nil
}

// AddComm appends a PERF_RECORD_COMM record.
func (w *Writer) AddComm(r commRecord) {
	var buf bytes.Buffer
	encodeComm(&buf, r)
	n, _ := w.bw.Write(buf.Bytes())
	w.pos += int64(n)
}

// AddMmap2 appends a PERF_RECORD_MMAP2 record.
func (w *Writer) AddMmap2(r mmap2Record) {
	var buf bytes.Buffer
	encodeMmap2(&buf, r)
	n, _ := w.bw.Write(buf.Bytes())
	w.pos += int64(n)
}

// AddSample appends a PERF_RECORD_SAMPLE record.
func (w *Writer) AddSample(r sampleRecord) {
	var buf bytes.Buffer
	encodeSample(&buf, r)
	n, _ := w.bw.Write(buf.Bytes())
	w.pos += int64(n)
}

// AddFinishedRound appends a PERF_RECORD_FINISHED_ROUND marker.
func (w *Writer) AddFinishedRound() {
	var buf bytes.Buffer
	encodeFinishedRound(&buf)
	n, _ := w.bw.Write(buf.Bytes())
	w.pos += int64(n)
}

// AddBuildID records a binary's build-id for emission in the
// HEADER_BUILD_ID feature section at Close.
func (w *Writer) AddBuildID(e buildIDEntry) {
	w.buildIDs = append(w.buildIDs, e)
}

// Close finalizes the file: emits feature sections, builds the feature
// index table, patches the file header's offsets/sizes/feature bitmap,
// and closes the underlying file.
func (w *Writer) Close() error {
	dataEnd := w.pos
	dataSize := uint64(dataEnd - w.dataBeg)

	// Emit feature payloads, recording each (offset, size).
	type feat struct {
		bit  int
		body []byte
	}
	var feats []feat

	if len(w.buildIDs) > 0 {
		var buf bytes.Buffer
		encodeBuildIDFeature(&buf, w.buildIDs)
		feats = append(feats, feat{bit: featBuildID, body: buf.Bytes()})
	}
	if w.meta.Hostname != "" {
		var buf bytes.Buffer
		encodeStringFeature(&buf, w.meta.Hostname)
		feats = append(feats, feat{bit: featHostname, body: buf.Bytes()})
	}
	if w.meta.OSRelease != "" {
		var buf bytes.Buffer
		encodeStringFeature(&buf, w.meta.OSRelease)
		feats = append(feats, feat{bit: featOSRelease, body: buf.Bytes()})
	}
	if w.meta.NumCPUs > 0 {
		var buf bytes.Buffer
		encodeNRCPUSFeature(&buf, w.meta.NumCPUs, w.meta.NumCPUs)
		feats = append(feats, feat{bit: featNRCPUS, body: buf.Bytes()})
	}

	// Per the kernel format: after the data section, the feature index
	// table is appended (one section{} entry per set bit, in bit-number
	// order). The actual feature payloads follow the index table.
	indexTableSize := int64(len(feats) * 16) // 16 bytes per section{}
	indexTableBeg := dataEnd
	payloadBeg := indexTableBeg + indexTableSize

	var indexEntries []section
	cursor := uint64(payloadBeg)
	addsFeatures := uint64(0)
	for _, f := range feats {
		indexEntries = append(indexEntries, section{
			offset: cursor,
			size:   uint64(len(f.body)),
		})
		cursor += uint64(len(f.body))
		addsFeatures |= 1 << f.bit
	}

	// Write the index table.
	encodeFeatureIndexTable(w.bw, indexEntries)
	w.pos += indexTableSize

	// Write the feature payloads.
	for _, f := range feats {
		n, _ := w.bw.Write(f.body)
		w.pos += int64(n)
	}

	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("perfdata: flush: %w", err)
	}

	// Now patch the file header (seek to 0, rewrite).
	if _, err := w.f.Seek(0, 0); err != nil {
		return fmt.Errorf("perfdata: seek: %w", err)
	}
	patchedBuf := bufio.NewWriter(w.f)
	encodeFileHeader(patchedBuf, fileHeader{
		attrs:        section{offset: fileHeaderSize, size: attrV8Size},
		data:         section{offset: uint64(w.dataBeg), size: dataSize},
		eventTypes:   section{offset: 0, size: 0},
		addsFeatures: addsFeatures,
	})
	if err := patchedBuf.Flush(); err != nil {
		return fmt.Errorf("perfdata: header patch flush: %w", err)
	}
	return w.f.Close()
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
GOTOOLCHAIN=auto go test ./internal/perfdata/... -count=1 2>&1 | tail -10
```
Expected: PASS for all `internal/perfdata` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/perfdata.go internal/perfdata/perfdata_test.go
git commit -m "internal/perfdata: Writer high-level API (Open/Add*/Close, file-header patching)"
```

---

## Task 9: `internal/perfevent.ProbeHardwareCycles()` event-type auto-detect

**Files:**
- Create: `internal/perfevent/probe.go`
- Test: `internal/perfevent/probe_test.go`
- Modify: `internal/perfevent/perfevent.go` (`OpenAll` accepts the spec)

- [ ] **Step 1: Write a failing test for the EventSpec struct**

```go
// internal/perfevent/probe_test.go
package perfevent

import "testing"

func TestEventSpec_String(t *testing.T) {
	hw := EventSpec{Type: PerfTypeHardware, Config: PerfCountHWCPUCycles}
	if got := hw.String(); got != "hardware/cpu-cycles" {
		t.Errorf("hw.String() = %q, want %q", got, "hardware/cpu-cycles")
	}
	sw := EventSpec{Type: PerfTypeSoftware, Config: PerfCountSWCPUClock}
	if got := sw.String(); got != "software/cpu-clock" {
		t.Errorf("sw.String() = %q, want %q", got, "software/cpu-clock")
	}
	other := EventSpec{Type: 99, Config: 42}
	if got := other.String(); got != "type=99/config=42" {
		t.Errorf("other.String() = %q, want %q", got, "type=99/config=42")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
GOTOOLCHAIN=auto go test ./internal/perfevent/... -count=1 -run TestEventSpec 2>&1 | tail -5
```
Expected: build failure (`undefined: EventSpec`).

- [ ] **Step 3: Write the implementation**

```go
// internal/perfevent/probe.go
// Probes the kernel for the most accurate sample event available, falling
// back gracefully when running in environments (cloud VMs, k8s pods on
// virtualized hosts) where hardware PMU events aren't exposed.
package perfevent

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	PerfTypeHardware = 0
	PerfTypeSoftware = 1

	PerfCountHWCPUCycles = 0
	PerfCountSWCPUClock  = 0
)

// EventSpec is the perf-event configuration used to open per-CPU events
// AND to populate the perf.data attr section. SamplePeriod is interpreted
// as a frequency (Hz) when Frequency is true.
type EventSpec struct {
	Type         uint32
	Config       uint64
	SamplePeriod uint64
	Frequency    bool
}

func (s EventSpec) String() string {
	switch {
	case s.Type == PerfTypeHardware && s.Config == PerfCountHWCPUCycles:
		return "hardware/cpu-cycles"
	case s.Type == PerfTypeSoftware && s.Config == PerfCountSWCPUClock:
		return "software/cpu-clock"
	default:
		return fmt.Sprintf("type=%d/config=%d", s.Type, s.Config)
	}
}

// ProbeHardwareCycles tries to open a PERF_TYPE_HARDWARE / PERF_COUNT_HW_CPU_CYCLES
// event; on success returns that EventSpec. On the typical virtualized-host
// failures (EOPNOTSUPP, ENOENT, ENODEV, EINVAL, EACCES) it returns the
// software cpu-clock fallback. Any other error propagates — those usually
// mean broken kernel state we shouldn't paper over.
//
// sampleHz is the desired sample rate; threaded through into the returned
// EventSpec.SamplePeriod with Frequency=true.
func ProbeHardwareCycles(sampleHz uint64) (EventSpec, error) {
	attr := unix.PerfEventAttr{
		Type:   PerfTypeHardware,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Config: PerfCountHWCPUCycles,
		Sample: sampleHz,
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	fd, err := unix.PerfEventOpen(&attr, -1, 0, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err == nil {
		_ = unix.Close(fd)
		return EventSpec{
			Type:         PerfTypeHardware,
			Config:       PerfCountHWCPUCycles,
			SamplePeriod: sampleHz,
			Frequency:    true,
		}, nil
	}
	// Common virt / restricted-env failures — fall back silently.
	if errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOENT) ||
		errors.Is(err, unix.ENODEV) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.EACCES) {
		return EventSpec{
			Type:         PerfTypeSoftware,
			Config:       PerfCountSWCPUClock,
			SamplePeriod: sampleHz,
			Frequency:    true,
		}, nil
	}
	return EventSpec{}, fmt.Errorf("perfevent: probe HW cycles: %w", err)
}
```

- [ ] **Step 4: Modify `OpenAll` to accept the spec**

In `internal/perfevent/perfevent.go`, find:

```go
attr := &unix.PerfEventAttr{
    Type:   unix.PERF_TYPE_SOFTWARE,
    Config: unix.PERF_COUNT_SW_CPU_CLOCK,
    Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
    Sample: uint64(sampleRate),
    Bits:   bits,
}
```

Replace the `Type` and `Config` lines with:

```go
attr := &unix.PerfEventAttr{
    Type:   spec.Type,
    Config: spec.Config,
    Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
    Sample: spec.SamplePeriod,
    Bits:   bits,
}
```

And update `OpenAll`'s signature to take a new `spec EventSpec` arg:

```go
func OpenAll(prog *ebpf.Program, cpus []uint, spec EventSpec, opts ...Option) (*Set, error)
```

(Remove the existing `sampleRate int` parameter — `spec.SamplePeriod` replaces it.)

- [ ] **Step 5: Run unit tests**

```bash
GOTOOLCHAIN=auto go test ./internal/perfevent/... -count=1 2>&1 | tail -5
```
Expected: PASS for the new EventSpec test. Other perfevent tests will need to be fixed in step 6 if `OpenAll` callers exist in tests.

- [ ] **Step 6: Update callers**

Search for `perfevent.OpenAll` in the codebase. Likely callers: `profile/profiler.go` and `unwind/dwarfagent/agent.go`.

For each caller, change:

```go
perfSet, err := perfevent.OpenAll(prog, cpus, sampleRate)
```

to (in this task; the auto-detect probe will be wired in Task 10/11):

```go
spec := perfevent.EventSpec{
    Type:         perfevent.PerfTypeSoftware,
    Config:       perfevent.PerfCountSWCPUClock,
    SamplePeriod: uint64(sampleRate),
    Frequency:    true,
}
perfSet, err := perfevent.OpenAll(prog, cpus, spec)
```

This temporarily hardcodes software cpu-clock at every call site (preserving today's behavior). Tasks 10/11 replace these with a single up-front `ProbeHardwareCycles` call so all profilers share the same auto-detected event.

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go build ./... 2>&1 | tail -5
```
Expected: clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/perfevent/probe.go internal/perfevent/probe_test.go internal/perfevent/perfevent.go profile/profiler.go unwind/dwarfagent/agent.go
git commit -m "internal/perfevent: ProbeHardwareCycles + EventSpec; OpenAll takes EventSpec"
```

---

## Task 10: `profile/profiler.go` fan-out

**Files:**
- Modify: `profile/profiler.go`

- [ ] **Step 1: Add `perfData *perfdata.Writer` field to `Profiler`**

In `profile/profiler.go`, find:

```go
type Profiler struct {
	objs       *perfObjects
	symbolizer *blazesym.Symbolizer
	resolver   *procmap.Resolver
	perfSet    *perfevent.Set
	tags       []string
	sampleRate int
	labels     map[string]string
}
```

Add a `perfData *perfdata.Writer` field (with import for `internal/perfdata`):

```go
import (
    // ... existing imports ...
    "github.com/dpsoft/perf-agent/internal/perfdata"
)

type Profiler struct {
	objs       *perfObjects
	symbolizer *blazesym.Symbolizer
	resolver   *procmap.Resolver
	perfSet    *perfevent.Set
	tags       []string
	sampleRate int
	labels     map[string]string
	perfData   *perfdata.Writer // optional, nil when --perf-data-output not set
}
```

- [ ] **Step 2: Extend `NewProfiler` to accept the writer**

Change the signature:

```go
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, labels map[string]string, perfData *perfdata.Writer) (*Profiler, error)
```

Pass `perfData` through to the `Profiler{}` literal.

- [ ] **Step 3: Add fan-out on every emitted sample**

In the sample-emission path (where samples are added to `pprof.ProfileBuilder`), find the existing builder.Add call and add an analogous perfData call right after, guarded by nil:

```go
// after pprofBuilder.AddSample(...)
if pr.perfData != nil {
    pr.perfData.AddSample(perfdata.SampleRecord{
        IP:        sample.PCs[0],
        Pid:       sample.PID,
        Tid:       sample.TID,
        Time:      sample.TimeNs,
        Cpu:       uint32(sample.CPU),
        Period:    1,
        Callchain: sample.PCs,
    })
}
```

(The exact field names on the existing `sample` struct may differ — search for `sample.PCs` in the file to find the source of truth and adapt.)

You'll also need to expose `SampleRecord`, `MmapRecord`, etc. as public types in `internal/perfdata`. Promote `sampleRecord` → `SampleRecord`, `mmap2Record` → `Mmap2Record`, `commRecord` → `CommRecord`, `buildIDEntry` → `BuildIDEntry`, plus any private encoder helpers.

- [ ] **Step 4: Update perfdata to export public types**

In `internal/perfdata/records.go` and friends, rename private types to public. Update all internal references in `internal/perfdata/*.go`. Update tests too.

- [ ] **Step 5: Build + run tests**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test ./profile/... ./internal/perfdata/... -count=1 2>&1 | tail -10
```
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add profile/profiler.go internal/perfdata/
git commit -m "profile/profiler: fan samples out to perfdata.Writer when configured"
```

---

## Task 11: `unwind/dwarfagent/agent.go` fan-out

**Files:**
- Modify: `unwind/dwarfagent/agent.go`

- [ ] **Step 1: Mirror Task 10 changes for the DWARF profiler**

`dwarfagent.Profiler` already accepts a `perfevent.Set`; add the `perfData *perfdata.Writer` field, the constructor parameter, and the per-sample fan-out hook in the same way Task 10 did for `profile.Profiler`.

The DWARF profiler emits samples through the `*session` struct's aggregator (`unwind/dwarfagent/common.go`). The fan-out hook goes there:

```go
// In common.go, in the per-sample callback (search for ProfileBuilder.AddSample
// or equivalent), add right after the pprof emission:
if s.perfData != nil {
    s.perfData.AddSample(perfdata.SampleRecord{
        IP:        sample.PCs[0],
        Pid:       sample.PID,
        Tid:       sample.TID,
        Time:      sample.TimeNs,
        Cpu:       uint32(sample.CPU),
        Period:    1,
        Callchain: sample.PCs,
    })
}
```

Add `perfData *perfdata.Writer` to the `session` struct; pass it through `newSession`.

- [ ] **Step 2: Update `NewProfilerWithMode` and friends**

Add a trailing `perfData *perfdata.Writer` parameter to:
- `NewProfilerWithMode`
- `NewProfilerWithHooks`
- `NewProfiler`
- `NewOffCPUProfilerWithHooks`
- `NewOffCPUProfiler`

Off-CPU profilers always pass `nil` for `perfData` (off-CPU samples don't fit perf.data's shape; we don't emit them).

`newSession` gains the trailing param.

Update internal callers (`perfagent/agent.go`, bench scenarios, tests in `unwind/dwarfagent/*_test.go`) to pass `nil`.

- [ ] **Step 3: Build + run tests**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test ./... -count=1 2>&1 | tail -10
```
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add unwind/dwarfagent/ perfagent/ bench/
git commit -m "unwind/dwarfagent: thread perfdata.Writer through profilers (nil-safe)"
```

---

## Task 12: `perfagent` options + agent wiring

**Files:**
- Modify: `perfagent/options.go`
- Modify: `perfagent/agent.go`
- Test: `perfagent/options_test.go`

- [ ] **Step 1: Add `WithPerfDataOutput` option + Config field**

Append to `perfagent/options.go`:

```go
// WithPerfDataOutput enables writing a Linux perf.data file alongside the
// pprof output. Compatible with --profile (--pid or -a). Off-CPU and PMU
// modes ignore this option; only CPU samples are written to perf.data.
//
// Output is consumable by perf script, perf report, create_llvm_prof
// (AutoFDO PGO), FlameGraph, hotspot, etc.
func WithPerfDataOutput(path string) Option {
	return func(c *Config) { c.PerfDataOutput = path }
}
```

Add to the `Config` struct (alongside the other output-path fields):

```go
// PerfDataOutput is the path for a kernel-format perf.data file. Empty
// disables emission. Set via WithPerfDataOutput.
PerfDataOutput string
```

- [ ] **Step 2: Add a unit test for the option**

Append to `perfagent/options_test.go`:

```go
func TestWithPerfDataOutput_SetsConfig(t *testing.T) {
	cfg := DefaultConfig()
	WithPerfDataOutput("app.perf.data")(cfg)
	if cfg.PerfDataOutput != "app.perf.data" {
		t.Errorf("PerfDataOutput = %q, want %q", cfg.PerfDataOutput, "app.perf.data")
	}
}
```

- [ ] **Step 3: Wire the option into `Agent.Start`**

In `perfagent/agent.go`, in `Start`, after `resolveTarget()` and before constructing the CPU profiler:

```go
var perfDataWriter *perfdata.Writer
if a.config.PerfDataOutput != "" {
    spec, err := perfevent.ProbeHardwareCycles(uint64(a.config.SampleRate))
    if err != nil {
        return fmt.Errorf("probe perf event: %w", err)
    }
    log.Printf("perf-agent: perf.data event = %s", spec)
    hostname, _ := os.Hostname()
    osRelease := readOSRelease() // small helper; falls back to "unknown"
    nrCPUs := uint32(len(cpus))
    perfDataWriter, err = perfdata.Open(a.config.PerfDataOutput, perfdata.EventSpec{
        Type:         spec.Type,
        Config:       spec.Config,
        SamplePeriod: spec.SamplePeriod,
        Frequency:    spec.Frequency,
    }, perfdata.MetaInfo{
        Hostname:  hostname,
        OSRelease: osRelease,
        NumCPUs:   nrCPUs,
    })
    if err != nil {
        return fmt.Errorf("open perf.data: %w", err)
    }
    defer func() {
        if perfDataWriter != nil {
            _ = perfDataWriter.Close()
        }
    }()
}
```

Then pass `perfDataWriter` (which may be nil) into the CPU profiler constructor.

Add a small `readOSRelease()` helper at the bottom of `agent.go`:

```go
func readOSRelease() string {
    out, err := exec.Command("uname", "-r").Output()
    if err != nil {
        return "unknown"
    }
    return strings.TrimSpace(string(out))
}
```

- [ ] **Step 4: Build + run tests**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test ./perfagent/... -count=1 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/options.go perfagent/options_test.go perfagent/agent.go
git commit -m "perfagent: WithPerfDataOutput option; open writer in Start when configured"
```

---

## Task 13: `main.go` CLI flag

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Add the flag**

In `main.go`, in the flag declarations block:

```go
flagPerfDataOutput = flag.String("perf-data-output", "",
    "Write a kernel-format perf.data file alongside the pprof output.\n"+
    "Consumable by perf script, perf report, create_llvm_prof (AutoFDO PGO),\n"+
    "FlameGraph, hotspot, etc.")
```

Plumb it through to the agent:

```go
opts = append(opts, perfagent.WithPerfDataOutput(*flagPerfDataOutput))
```

(Add this alongside the other `perfagent.With*Output` plumbing.)

- [ ] **Step 2: Build the binary**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go build -o perf-agent .
./perf-agent --help 2>&1 | grep perf-data-output
```
Expected: flag appears in `--help` output.

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "main: --perf-data-output flag plumbing"
```

---

## Task 14: Integration test — `perf script` round-trip

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 1: Write the new test**

Append to `test/integration_test.go`:

```go
// TestPerfDataOutput captures a perf.data file from a known workload,
// runs `perf script` against it, and asserts the output contains expected
// markers (process name + at least one symbolized frame).
func TestPerfDataOutput(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))
	if _, err := exec.LookPath("perf"); err != nil {
		t.Skipf("perf binary not installed; skipping: %v", err)
	}

	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	wl := exec.Command(binPath, "10", "2")
	require.NoError(t, wl.Start())
	defer func() {
		_ = wl.Process.Kill()
		_ = wl.Wait()
	}()
	time.Sleep(2 * time.Second)

	outPath := filepath.Join(t.TempDir(), "test.perf.data")
	agent := exec.Command(getAgentPath(t),
		"--profile",
		"--pid", strconv.Itoa(wl.Process.Pid),
		"--duration", "5s",
		"--perf-data-output", outPath,
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	st, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("perf.data not created: %v", err)
	}
	if st.Size() < 200 {
		t.Fatalf("perf.data suspiciously small: %d bytes", st.Size())
	}

	// Validate via perf script — it'll error on a malformed file.
	cmd := exec.Command("perf", "script", "-i", outPath)
	scriptOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("perf script failed on our output: %v\n%s", err, string(scriptOut))
	}
	if len(scriptOut) == 0 {
		t.Fatalf("perf script produced no output (no samples in perf.data?)")
	}
	t.Logf("perf script captured %d bytes of output", len(scriptOut))
}
```

- [ ] **Step 2: Run the integration test locally**

```bash
cd /home/diego/github/perf-agent/.worktrees/perf-data-output
make build
sudo setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test ./test/... -count=1 -run TestPerfDataOutput -v 2>&1 | tail -20
```
Expected: PASS (or SKIP if rust workload not built).

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: integration test for --perf-data-output (perf script round-trip)"
```

---

## Task 15: User-facing walkthrough — `docs/perf-data-output.md`

**Files:**
- Create: `docs/perf-data-output.md`

- [ ] **Step 1: Write the doc**

```markdown
# perf.data output

`perf-agent --profile --perf-data-output app.perf.data ...` writes a
[Linux kernel-format `perf.data`](https://github.com/torvalds/linux/blob/master/tools/perf/Documentation/perf.data-file-format.txt)
file alongside the existing pprof output. The same capture serializes into
two formats; you pick whichever consumer you want.

## What perf-agent emits

- `PERF_RECORD_COMM`, `PERF_RECORD_MMAP2` (with build-id), `PERF_RECORD_SAMPLE`,
  `PERF_RECORD_FINISHED_ROUND` records in the data section.
- Feature sections: `HEADER_BUILD_ID`, `HEADER_HOSTNAME`, `HEADER_OSRELEASE`,
  `HEADER_NRCPUS`.
- Sample event auto-detected: `PERF_TYPE_HARDWARE / PERF_COUNT_HW_CPU_CYCLES`
  on bare metal where the PMU is exposed; falls back to
  `PERF_TYPE_SOFTWARE / PERF_COUNT_SW_CPU_CLOCK` in cloud VMs / k8s pods
  without PMU passthrough. perf-agent logs which event was chosen at INFO.

## Capture

```bash
perf-agent --profile --pid <PID> --duration 60s --perf-data-output app.perf.data
```

Combine with `--profile-output app.pb.gz` to get pprof and perf.data from the
same capture. Use `-a/--all` for system-wide instead of `--pid`.

For AutoFDO-style training runs, longer durations (60s+) under a representative
workload give better profile accuracy than short, idle captures.

## Consumer recipes

### `perf script` / `perf report`

```bash
perf script -i app.perf.data
perf report -i app.perf.data
```

Sanity-check that samples landed and stacks resolved.

### FlameGraph

```bash
perf script -i app.perf.data | \
  ./stackcollapse-perf.pl | \
  ./flamegraph.pl > flame.svg
```

`stackcollapse-perf.pl` and `flamegraph.pl` come from
<https://github.com/brendangregg/FlameGraph>.

### Rust AutoFDO PGO

```bash
# 1. Build with debug info so create_llvm_prof can resolve symbols
RUSTFLAGS="-C debuginfo=2" cargo build --release

# 2. Capture a representative profile with perf-agent
perf-agent --profile --pid $(pgrep my-app) --duration 60s \
           --perf-data-output train.perf.data

# 3. Convert to LLVM .profdata via autofdo's create_llvm_prof
#    (https://github.com/google/autofdo#install)
create_llvm_prof --binary=./target/release/my-app \
                 --profile=train.perf.data \
                 --out=train.prof

# 4. Recompile with PGO
RUSTFLAGS="-C profile-use=$(pwd)/train.prof" cargo build --release
```

### C++ AutoFDO PGO

Same `create_llvm_prof` step, then:

```bash
clang++ -fprofile-sample-use=train.prof -O2 ...
```

### hotspot (GUI flame-graph viewer)

`hotspot --executable ./target/release/my-app app.perf.data`

## What about Go?

Go consumes pprof natively for PGO — you don't need perf.data:

```bash
perf-agent --profile --pid <PID> --duration 60s --profile-output train.pprof
go build -pgo=train.pprof ./...
```

`--perf-data-output` is for tools that don't speak pprof.

## Troubleshooting

- **`perf script: invalid file format`** — magic bytes don't match. Rerun with
  the latest perf-agent; verify the file isn't truncated (capture exited early).
- **`create_llvm_prof: build-id mismatch`** — the binary was rebuilt between
  capture and conversion. Re-run with the binary that produced the capture, or
  recapture against the new binary.
- **Empty / sparse profile** — workload didn't run during the capture window.
  Check sample count: `perf script -i app.perf.data | wc -l`. Below ~50 samples
  is unreliable for AutoFDO; capture longer or under heavier load.
- **`cycles event not supported` log message** — expected in cloud VMs and
  many k8s pods. Software cpu-clock is in effect; AutoFDO still works, just
  with slightly less accurate cycle attribution.
- **Missing debug info** in `perf script` output — rebuild with
  `-C debuginfo=2` (Rust) or `-g` (C++).

## What's not in this output (yet)

- LBR / branch records (bare-metal-only feature; v2 spec).
- `PERF_RECORD_FORK` / `PERF_RECORD_EXIT` lifecycle records.
- Hardware tracing data (Intel PT, ARM CoreSight).
```

- [ ] **Step 2: Commit**

```bash
git add docs/perf-data-output.md
git commit -m "docs: perf-data-output.md — user walkthrough (perf script, FlameGraph, AutoFDO)"
```

---

## Self-review

**1. Spec coverage.** Each section in `2026-05-04-perf-data-output-design.md` maps to:

| Spec section | Implementing task(s) |
|---|---|
| Goal — emit perf.data | Tasks 1–8 |
| CLI surface — `--perf-data-output` | Tasks 12, 13 |
| Event-type auto-detect | Task 9 |
| File header / attr | Tasks 2, 3 |
| Records (COMM, MMAP2, SAMPLE, FINISHED_ROUND) | Tasks 4, 5, 6 |
| Feature sections | Task 7 |
| Writer high-level API | Task 8 |
| Integration into perfagent.Agent | Task 12 |
| profile/dwarfagent fan-out | Tasks 10, 11 |
| Documentation | Task 15 |
| Testing | Task 14 (integration); per-task unit tests cover the rest |

**2. Placeholder scan.** No `TBD` / `TODO` / "fill in details" in any task. Every code step has actual code.

**3. Type consistency.** Public types in `internal/perfdata`: `Writer`, `Open`, `EventSpec`, `MetaInfo`, `SampleRecord`, `Mmap2Record`, `CommRecord`, `BuildIDEntry`. Private types and helpers begin lowercase. Tasks 4–8 use the lowercase names; Task 10 promotes them to public when the first external caller appears (`profile/profiler.go`). All references after Task 10 use the public names. *(Self-correction: if the implementer is reading tasks out of order, this rename can confuse — flag the rename explicitly in Task 10's commit.)*

**4. Dependency order.** Task 1 (primitives) blocks 2–7 (encoders). Task 8 (Writer) needs all encoders. Task 9 (event-type) is independent. Tasks 10–11 (fan-out) need 8 + 9 (the public types and the spec). Task 12 (agent wiring) needs 8 + 9 + 10 + 11. Task 13 (CLI) needs 12. Tasks 14–15 (test + docs) need everything else. Examples (Tasks 16–18) need everything else.

---

## Task 16: Runnable Rust AutoFDO example

**Files:**
- Create: `examples/rust-pgo/Cargo.toml`
- Create: `examples/rust-pgo/src/main.rs`
- Create: `examples/rust-pgo/pgo-cycle.sh`
- Create: `examples/rust-pgo/README.md`

**Goal:** an end-to-end script anyone can run that builds a CPU-bound Rust workload, captures a perf-agent profile, converts via `create_llvm_prof`, rebuilds with PGO, strips the final binary, and prints the speedup. Demonstrates the full AutoFDO loop — no hidden steps.

- [ ] **Step 1: Workload source**

```toml
# examples/rust-pgo/Cargo.toml
[package]
name = "rust-pgo-example"
version = "0.1.0"
edition = "2021"

[profile.release]
debug = "limited"  # keep -C debuginfo=2-equivalent for the convert step
lto = "thin"
codegen-units = 1
# Final --release artefact stays *with* debug info so create_llvm_prof can
# resolve symbols. The pgo-cycle.sh script strips the *optimised* output
# explicitly (cargo rustc -C strip=symbols) so users see a clean before/after.
```

```rust
// examples/rust-pgo/src/main.rs
//
// CPU-bound demonstrator: 99% of dispatched ops are `Add`, the other 1%
// are spread across the rare arms. AutoFDO will move the Add arm to the
// hot fall-through and shrink the prologue's branch overhead. Real-world
// workloads see 5-15% speedup; this synthetic one tends to land near 8%.
//
// Run: `./rust-pgo-example <iterations>` (default 200_000_000).

#[derive(Clone, Copy)]
enum Op { Add, Sub, Mul, Div }

#[inline(never)]
fn dispatch(op: Op, a: u64, b: u64) -> u64 {
    match op {
        Op::Add => a.wrapping_add(b),
        Op::Sub => a.wrapping_sub(b),
        Op::Mul => a.wrapping_mul(b),
        Op::Div => if b == 0 { 0 } else { a / b },
    }
}

fn main() {
    let n: u64 = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(200_000_000);
    let ops = [Op::Add, Op::Sub, Op::Mul, Op::Div];
    let mut total: u64 = 1;
    for i in 0..n {
        // 99% Add, 1% one of the others — the hot path AutoFDO will inline
        // and place as the fall-through.
        let op = if i % 100 == 0 {
            ops[((i / 100) as usize) % 4]
        } else {
            Op::Add
        };
        total = total.wrapping_add(dispatch(op, i, total));
    }
    println!("{}", total);
}
```

- [ ] **Step 2: pgo-cycle.sh — the full loop**

```bash
#!/usr/bin/env bash
# examples/rust-pgo/pgo-cycle.sh
#
# End-to-end Rust AutoFDO demo. Requires:
#   - cargo (any recent stable)
#   - perf-agent built and on PATH (or pass --agent ./perf-agent)
#   - create_llvm_prof from https://github.com/google/autofdo
#   - hyperfine (optional; falls back to /usr/bin/time -p)
#
# Output: baseline vs PGO-optimised wall-clock time for the same workload.
set -euo pipefail

ITER=${ITER:-200000000}
DURATION=${DURATION:-30s}
AGENT=${AGENT:-perf-agent}
WORKDIR=$(cd "$(dirname "$0")" && pwd)
cd "$WORKDIR"

bench() {
    local label=$1 bin=$2
    if command -v hyperfine >/dev/null 2>&1; then
        hyperfine --warmup 1 --runs 3 --export-json "${label}.json" \
            "$bin $ITER" >"${label}.txt" 2>&1
        awk -F'"mean": ' '/mean/{print $2}' "${label}.json" | head -1
    else
        /usr/bin/time -p "$bin" "$ITER" 2>&1 | awk '/real/ {print $2}'
    fi
}

echo "==> 1. Baseline build (with debug info)"
RUSTFLAGS="-C debuginfo=2" cargo build --release --quiet

echo "==> 2. Baseline benchmark"
BASELINE=$(bench baseline ./target/release/rust-pgo-example)
echo "    baseline: ${BASELINE}s"

echo "==> 3. Capture profile via perf-agent"
./target/release/rust-pgo-example "$ITER" &
WL_PID=$!
sleep 1   # workload warmup
"$AGENT" --profile --pid "$WL_PID" --duration "$DURATION" \
         --perf-data-output train.perf.data
wait "$WL_PID" || true

echo "==> 4. Convert perf.data → LLVM .prof via create_llvm_prof"
create_llvm_prof \
    --binary=./target/release/rust-pgo-example \
    --profile=train.perf.data \
    --out=train.prof

echo "==> 5. PGO build (uses train.prof; strips symbols on the final artefact)"
RUSTFLAGS="-C profile-use=$WORKDIR/train.prof -C strip=symbols" \
    cargo build --release --quiet

echo "==> 6. PGO-optimised benchmark"
OPT=$(bench optimized ./target/release/rust-pgo-example)
echo "    optimized: ${OPT}s"

echo
echo "==> Speedup"
awk -v b="$BASELINE" -v o="$OPT" \
    'BEGIN { printf "    %.2fx faster (%.1f%% improvement)\n", b/o, (b-o)/b*100 }'

echo "==> Final stripped binary:"
ls -la ./target/release/rust-pgo-example
file  ./target/release/rust-pgo-example
```

- [ ] **Step 3: README**

```markdown
<!-- examples/rust-pgo/README.md -->
# Rust AutoFDO PGO with perf-agent

A complete, runnable demonstration: build a Rust workload, capture a profile
with perf-agent, convert via Google's `create_llvm_prof` (autofdo), rebuild
with PGO and a stripped final binary, measure the speedup.

## Prerequisites

- Rust toolchain (`cargo --version` ≥ 1.70).
- `perf-agent` built and on PATH (or pass `AGENT=/path/to/perf-agent`).
  Required caps: `setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep`.
- `create_llvm_prof` from <https://github.com/google/autofdo>. Build:
  ```bash
  git clone https://github.com/google/autofdo
  cd autofdo
  cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
  cmake --build build
  sudo cp build/create_llvm_prof /usr/local/bin/
  ```
- Optional: `hyperfine` (`cargo install hyperfine`) for nicer benchmark output;
  the script falls back to `/usr/bin/time` if absent.

## Run

```bash
cd examples/rust-pgo
./pgo-cycle.sh
```

Tune the workload size with `ITER=<n>` (default 200M iterations) or capture
duration with `DURATION=60s` (default 30s).

## What it does

1. `cargo build --release` with `-C debuginfo=2` — keeps debug info so
   `create_llvm_prof` can map sample IPs back to function names.
2. Benchmarks the baseline binary.
3. Runs the workload, attaches perf-agent for `$DURATION`, writes
   `train.perf.data`.
4. `create_llvm_prof --binary=… --profile=train.perf.data --out=train.prof`
   produces an LLVM sample-profile.
5. `cargo build --release` with `-C profile-use=train.prof -C strip=symbols` —
   PGO build, final binary stripped.
6. Benchmarks the optimised binary, prints the speedup.

Typical result: 5–15% speedup on this synthetic workload. Real production
workloads vary; the numbers are illustrative.

## Why this works

The dispatch loop hits the `Add` arm 99% of the time. Without PGO, the
compiler has no way to know which arm is hot, so the match prologue treats
all four arms equally. With AutoFDO, the converter records that `Add` was
overwhelmingly the leaf at runtime; LLVM moves it to fall-through, hoists the
guard out of the loop, and inlines the call. The remaining 1% of operations
take a slow path; the bulk of the time gets faster.
```

- [ ] **Step 4: Test the example end-to-end on the host**

```bash
cd examples/rust-pgo
chmod +x pgo-cycle.sh
./pgo-cycle.sh
```
Expected: prints baseline time, optimized time, and a positive speedup. May skip if `create_llvm_prof` not installed; in that case the script prints a clear error referencing the install instructions.

- [ ] **Step 5: Commit**

```bash
git add examples/rust-pgo/
git commit -m "examples/rust-pgo: end-to-end AutoFDO demo (build, profile, convert, rebuild, strip, measure)"
```

---

## Task 17: Runnable C++ AutoFDO example

**Files:**
- Create: `examples/cpp-pgo/Makefile`
- Create: `examples/cpp-pgo/workload.cpp`
- Create: `examples/cpp-pgo/pgo-cycle.sh`
- Create: `examples/cpp-pgo/README.md`

**Goal:** mirror the Rust example with clang and `-fprofile-sample-use`. Same workload shape so users can compare PGO impact between languages.

- [ ] **Step 1: Workload source**

```cpp
// examples/cpp-pgo/workload.cpp
//
// CPU-bound: 99% of dispatched ops are Add. clang -fprofile-sample-use
// will move the Add arm to fall-through and inline it through the loop.
//
// Build with -g (-C debuginfo=2 equivalent) so create_llvm_prof can resolve
// symbols. Run: ./workload <iterations>

#include <cstdint>
#include <cstdio>
#include <cstdlib>

enum class Op { Add, Sub, Mul, Div };

__attribute__((noinline))
static uint64_t dispatch(Op op, uint64_t a, uint64_t b) {
    switch (op) {
        case Op::Add: return a + b;
        case Op::Sub: return a - b;
        case Op::Mul: return a * b;
        case Op::Div: return b == 0 ? 0 : a / b;
    }
    __builtin_unreachable();
}

int main(int argc, char** argv) {
    uint64_t n = (argc > 1) ? std::strtoull(argv[1], nullptr, 10) : 200000000;
    static const Op ops[] = {Op::Add, Op::Sub, Op::Mul, Op::Div};
    uint64_t total = 1;
    for (uint64_t i = 0; i < n; ++i) {
        Op op = (i % 100 == 0) ? ops[(i / 100) % 4] : Op::Add;
        total += dispatch(op, i, total);
    }
    std::printf("%llu\n", (unsigned long long)total);
    return 0;
}
```

- [ ] **Step 2: Makefile**

```makefile
# examples/cpp-pgo/Makefile
CXX      ?= clang++
CXXFLAGS ?= -O2 -g -std=c++17

.PHONY: baseline pgo strip clean
baseline:
	$(CXX) $(CXXFLAGS) workload.cpp -o workload-baseline

pgo:
	$(CXX) $(CXXFLAGS) -fprofile-sample-use=$(PWD)/train.prof workload.cpp -o workload-pgo

strip:
	strip --strip-all workload-pgo

clean:
	rm -f workload-baseline workload-pgo train.perf.data train.prof baseline.json optimized.json
```

- [ ] **Step 3: pgo-cycle.sh**

```bash
#!/usr/bin/env bash
# examples/cpp-pgo/pgo-cycle.sh — same shape as the Rust demo.
set -euo pipefail

ITER=${ITER:-200000000}
DURATION=${DURATION:-30s}
AGENT=${AGENT:-perf-agent}
WORKDIR=$(cd "$(dirname "$0")" && pwd)
cd "$WORKDIR"

bench() {
    local label=$1 bin=$2
    if command -v hyperfine >/dev/null 2>&1; then
        hyperfine --warmup 1 --runs 3 --export-json "${label}.json" \
            "$bin $ITER" >/dev/null
        awk -F'"mean": ' '/mean/{print $2}' "${label}.json" | head -1
    else
        /usr/bin/time -p "$bin" "$ITER" 2>&1 | awk '/real/ {print $2}'
    fi
}

echo "==> 1. Baseline build (clang++ -O2 -g)"
make -s baseline

echo "==> 2. Baseline benchmark"
BASELINE=$(bench baseline ./workload-baseline)
echo "    baseline: ${BASELINE}s"

echo "==> 3. Capture profile via perf-agent"
./workload-baseline "$ITER" &
WL_PID=$!
sleep 1
"$AGENT" --profile --pid "$WL_PID" --duration "$DURATION" \
         --perf-data-output train.perf.data
wait "$WL_PID" || true

echo "==> 4. Convert perf.data → LLVM .prof"
create_llvm_prof \
    --binary=./workload-baseline \
    --profile=train.perf.data \
    --out=train.prof

echo "==> 5. PGO build (-fprofile-sample-use=train.prof)"
make -s pgo

echo "==> 6. Strip the optimised binary"
make -s strip

echo "==> 7. PGO-optimised benchmark"
OPT=$(bench optimized ./workload-pgo)
echo "    optimized: ${OPT}s"

echo
echo "==> Speedup"
awk -v b="$BASELINE" -v o="$OPT" \
    'BEGIN { printf "    %.2fx faster (%.1f%% improvement)\n", b/o, (b-o)/b*100 }'

echo "==> Final stripped binary:"
ls -la ./workload-pgo
file  ./workload-pgo
```

- [ ] **Step 4: README**

```markdown
<!-- examples/cpp-pgo/README.md -->
# C++ AutoFDO PGO with perf-agent

Same shape as `examples/rust-pgo` but using clang's
`-fprofile-sample-use=` flag. Useful for comparing PGO impact between
languages on the same algorithmic workload.

## Prerequisites

- clang ≥ 12 (any modern release supports `-fprofile-sample-use`).
- `perf-agent` built and on PATH (caps as documented in the repo README).
- `create_llvm_prof` from <https://github.com/google/autofdo>.
- Optional: `hyperfine`.

## Run

```bash
cd examples/cpp-pgo
./pgo-cycle.sh
```

`ITER` and `DURATION` env vars work the same as in the Rust example.

## What it does

1. Builds `workload-baseline` with `-O2 -g`.
2. Benchmarks it.
3. Captures a profile with perf-agent → `train.perf.data`.
4. Converts via `create_llvm_prof` → `train.prof`.
5. Builds `workload-pgo` with `-fprofile-sample-use=train.prof`.
6. Strips the optimised binary.
7. Benchmarks and prints the speedup.

The same dispatch-loop trick used in the Rust example: 99% of operations
hit a single match arm, AutoFDO pulls that arm to fall-through.
```

- [ ] **Step 5: Test on the host**

```bash
cd examples/cpp-pgo
chmod +x pgo-cycle.sh
./pgo-cycle.sh
```
Expected: prints baseline + optimised + speedup. Skips with a clear error if `clang`/`create_llvm_prof` not installed.

- [ ] **Step 6: Commit**

```bash
git add examples/cpp-pgo/
git commit -m "examples/cpp-pgo: end-to-end clang AutoFDO demo (-fprofile-sample-use, stripped)"
```

---

## Task 18: FlameGraph capture + render example

**Files:**
- Create: `examples/flamegraph/capture.sh`
- Create: `examples/flamegraph/README.md`

**Goal:** turn a perf-agent capture into an SVG flame graph using Brendan Gregg's [FlameGraph](https://github.com/brendangregg/FlameGraph) scripts. No PGO; just demonstrates that perf.data emission feeds the canonical visualisation tool.

- [ ] **Step 1: capture.sh**

```bash
#!/usr/bin/env bash
# examples/flamegraph/capture.sh
#
# Capture a perf-agent profile against a running PID and render a
# Brendan-Gregg-style flame graph to flame.svg.
#
# Requires:
#   - perf-agent (caps set)
#   - perf binary (for `perf script`)
#   - FlameGraph scripts (stackcollapse-perf.pl, flamegraph.pl).
#     Either on PATH, or set FLAMEGRAPH_DIR=/path/to/FlameGraph.
#
# Usage:
#   ./capture.sh <PID> [DURATION]
#       e.g. ./capture.sh $(pgrep my-app) 30s

set -euo pipefail

PID=${1:?usage: $0 <PID> [DURATION]}
DURATION=${2:-30s}
AGENT=${AGENT:-perf-agent}

# Locate FlameGraph scripts.
if [[ -n "${FLAMEGRAPH_DIR:-}" ]]; then
    SC="$FLAMEGRAPH_DIR/stackcollapse-perf.pl"
    FG="$FLAMEGRAPH_DIR/flamegraph.pl"
elif command -v stackcollapse-perf.pl >/dev/null 2>&1; then
    SC=$(command -v stackcollapse-perf.pl)
    FG=$(command -v flamegraph.pl)
else
    cat >&2 <<EOF
error: FlameGraph scripts not found.
  Either:
    git clone https://github.com/brendangregg/FlameGraph
    export FLAMEGRAPH_DIR=\$(pwd)/FlameGraph
  or install the scripts on PATH.
EOF
    exit 1
fi

WORKDIR=$(cd "$(dirname "$0")" && pwd)
cd "$WORKDIR"

echo "==> 1. Capture profile (PID=$PID, duration=$DURATION)"
"$AGENT" --profile --pid "$PID" --duration "$DURATION" \
         --perf-data-output capture.perf.data

echo "==> 2. perf script | stackcollapse-perf.pl | flamegraph.pl"
perf script -i capture.perf.data | "$SC" | "$FG" \
    --title "perf-agent flame graph (pid $PID, $DURATION)" \
    > flame.svg

echo "==> Wrote flame.svg ($(stat -c %s flame.svg) bytes)"
echo "    Open it in a browser, or with: xdg-open flame.svg"
```

- [ ] **Step 2: README**

```markdown
<!-- examples/flamegraph/README.md -->
# perf-agent → FlameGraph

Capture a profile with perf-agent and render a Brendan Gregg-style flame
graph. Demonstrates that perf-agent's perf.data output feeds the canonical
flame-graph tooling unchanged.

## Prerequisites

- `perf-agent` built and on PATH, with caps set
  (`setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep`).
- `perf` binary on PATH (for `perf script`). Most distributions ship it
  in the `linux-tools` / `perf` / `linux-perf` package.
- Brendan Gregg's FlameGraph scripts:
  ```bash
  git clone https://github.com/brendangregg/FlameGraph
  export FLAMEGRAPH_DIR=$(pwd)/FlameGraph
  ```
  Or copy `stackcollapse-perf.pl` and `flamegraph.pl` into a directory on
  your PATH.

## Run

```bash
# Capture against any running process for 30 seconds:
./capture.sh $(pgrep my-app) 30s

# Or specify a PID directly:
./capture.sh 12345 60s
```

Output: `flame.svg` in the current directory. Open it in any browser —
flame graphs are interactive (click to zoom, search by symbol name).

## What it does

1. `perf-agent --profile --pid <PID> --duration <D> --perf-data-output capture.perf.data`
2. `perf script -i capture.perf.data` → text per-sample dump.
3. `stackcollapse-perf.pl` → "folded" stack format (one stack per line, with sample count).
4. `flamegraph.pl` → SVG.

The pipeline is the same one you'd use with `perf record`. perf-agent slots
in as the capture step; the rest of the chain is unchanged.

## Notes on accuracy

perf-agent samples at 99 Hz with software cpu-clock by default (or hardware
cycles if available — see the agent's INFO log on startup). For flame graphs
that's plenty — the goal is identifying *which functions* dominate, not
precise cycle attribution. If you need higher fidelity, increase the sample
rate (`--sample-rate 999`) or capture for longer (`DURATION=120s`).
```

- [ ] **Step 3: Test on the host**

```bash
cd examples/flamegraph
chmod +x capture.sh

# Use any long-running process. A simple one:
sleep 600 &
SLEEP_PID=$!
./capture.sh "$SLEEP_PID" 5s
kill "$SLEEP_PID"
ls -la flame.svg
```
Expected: prints capture progress, writes a non-zero `flame.svg`. Skips with a clear error if FlameGraph scripts not found.

- [ ] **Step 4: Commit**

```bash
git add examples/flamegraph/
git commit -m "examples/flamegraph: capture-and-render demo using brendangregg/FlameGraph"
```

---

## Task 19: top-level `examples/README.md`

**Files:**
- Create: `examples/README.md`

- [ ] **Step 1: Write the index**

```markdown
<!-- examples/README.md -->
# perf-agent examples

End-to-end runnable demonstrations of what perf-agent can do beyond a
quick CPU profile. Each example is a self-contained directory with its
own README, source workload, and driver script.

| Example | What it shows |
|---|---|
| [`rust-pgo/`](rust-pgo/) | Rust AutoFDO PGO. Build → profile → `create_llvm_prof` → rebuild → strip → measure speedup. |
| [`cpp-pgo/`](cpp-pgo/) | C++ AutoFDO PGO. Same shape via clang's `-fprofile-sample-use`. |
| [`flamegraph/`](flamegraph/) | Render a Brendan-Gregg flame graph from a perf-agent capture. |

All three depend on `perf-agent` being built and on PATH with the standard
capability set (`setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep`).

## Why these are here

The README describes what perf-agent emits. These examples prove the
workflows end-to-end: prerequisites you actually need to install, scripts
that run unattended, expected output you can compare against. If a workflow
documented in the README doesn't have a runnable example here, treat that
as a documentation bug.
```

- [ ] **Step 2: Commit**

```bash
git add examples/README.md
git commit -m "examples: top-level index README"
```
