# Python Frame Walker — Slices 1 & 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Place CPython frames in the same stack as the native frames that ran them, emitted as opaque addresses, reaching the CUDA launch site.

**Architecture:** The interpreter hook lives inside `walk_step` in `bpf/unwind_common.h`, so both `perf_dwarf.bpf.c` and `gpu_usdt.bpf.c` inherit it from the `bpf_loop` they already share. Per-PID CPython offsets are written from Go into a BPF map at attach; the current thread's `PyThreadState` is reached by reimplementing `pthread_getspecific` in BPF against a TSS key whose offset is parsed out of `PyGILState_GetThisThreadState`.

**Tech Stack:** C (BPF, `bpf_probe_read_user`, `bpf_loop`), Go 1.26, cilium/ebpf + bpf2go, podman for multi-version CPython fixtures.

**Spec:** `docs/superpowers/specs/2026-08-29-python-frame-walker-design.md`

## Global Constraints

- **CPython 3.12, 3.13, 3.14 only.** An interpreter outside this range is refused and counted, never walked with the nearest table.
- **Clang 18 to regenerate BPF objects** (issue #87). Use the `ubuntu:24.04` podman recipe; `make generate-check` must pass. Never run `make test-unit` before `git add` of a `.o` — it no longer regenerates, but confirm.
- **No new capability is required to START.** Everything in these two slices runs on `cap_bpf,cap_perfmon,cap_checkpoint_restore`. The ptrace-dependent work is slice 3.
- **A wrong offset must never produce a frame.** Every table is validated against the target before use; failure is a counted refusal.
- **Counters over silence.** Every refusal, miss and truncation increments a named counter that a test asserts at a known value.
- `MAX_FRAMES` is 127 and Python frames cost two slots. Measure, do not assume.

---

## File Structure

**Created:**
- `pyunwind/tssparse.go` — parses the `autoTSSkey` offset out of `PyGILState_GetThisThreadState`. Pure function over bytes; no I/O.
- `pyunwind/tssparse_test.go`
- `pyunwind/version.go` — CPython version detection from soname and `Py_Version`.
- `pyunwind/version_test.go`
- `pyunwind/offsets.go` — per-version struct offset tables + validation.
- `pyunwind/offsets_test.go`
- `pyunwind/attach.go` — per-PID discovery, writes the BPF map.
- `pyunwind/testdata/` — real `libpython3.{12,13,14}` byte fixtures.
- `bpf/python_walk.h` — the BPF-side walk, included by `unwind_common.h`.

**Modified:**
- `bpf/unwind_common.h` — frame tagging in `sample_record`; the interpreter arm in `walk_step`.
- `bpf/gpu_usdt.bpf.c`, `bpf/perf_dwarf.bpf.c`, `bpf/offcpu_dwarf.bpf.c` — decode tagged frames.
- `profile/dwarf_export.go`, `unwind/dwarfagent/common.go` — Go-side tagged-frame decode.

**Why `pyunwind/` is its own package:** everything except the BPF header is pure Go over bytes and can be tested without root, without BPF, and without a running interpreter. That is the only way the offset tables get honest tests.

---

### Task 1: Tag frames in `sample_record`

Blast radius task. No Python code. Existing native and GPU tests must stay green.

**Files:**
- Modify: `bpf/unwind_common.h:103-130` (the `sample_record` layout comment and `pcs[]`)
- Modify: `profile/dwarf_export.go`, `unwind/dwarfagent/common.go` (readers)
- Test: `profile/dwarf_export_test.go`

**Interfaces:**
- Produces: `FRAME_TAG_NATIVE 0`, `FRAME_TAG_PYTHON 1`; helper `frame_push_native(struct walk_ctx *, __u64 pc)` returning `int` (0 ok, 1 full).

- [ ] **Step 1: Write the failing test**

```go
// profile/dwarf_export_test.go
func TestTaggedFramesDecodeNativeOnly(t *testing.T) {
	// A record holding three native frames must decode to three PCs,
	// unchanged, with no Python frames.
	rec := makeSampleRecord(t, []taggedFrame{
		{tag: frameTagNative, words: []uint64{0x1000}},
		{tag: frameTagNative, words: []uint64{0x2000}},
		{tag: frameTagNative, words: []uint64{0x3000}},
	})
	pcs, py := decodeFrames(rec)
	require.Equal(t, []uint64{0x1000, 0x2000, 0x3000}, pcs)
	require.Empty(t, py, "no python frames were pushed")
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./profile/ -run TestTaggedFramesDecodeNativeOnly -v`
Expected: FAIL — `decodeFrames` undefined.

- [ ] **Step 3: Add the tag to the BPF record**

In `bpf/unwind_common.h`, alongside `pcs[]`:

```c
// Frame tags. pcs[] is no longer a flat PC array: a Python frame occupies
// two slots, so each entry carries its kind. Issue #83.
#define FRAME_TAG_NATIVE 0
#define FRAME_TAG_PYTHON 1

// One tag per pcs[] slot. A u8 array rather than bits packed into the PC
// because a PC is a full 64 bits and stealing from it would break the day
// someone maps something high.
__u8 tags[MAX_FRAMES];
```

and the push helper:

```c
static __always_inline int frame_push_native(struct walk_ctx *ctx, __u64 pc) {
    if (ctx->n_pcs >= MAX_FRAMES) return 1;
    ctx->rec->tags[ctx->n_pcs] = FRAME_TAG_NATIVE;
    ctx->rec->pcs[ctx->n_pcs++] = pc;
    return 0;
}
```

- [ ] **Step 4: Route the existing push through it**

In `walk_step`, replace `ctx->rec->pcs[ctx->n_pcs++] = ctx->pc;` with:

```c
    if (frame_push_native(ctx, ctx->pc)) return 1;
```

- [ ] **Step 5: Decode tags in Go**

```go
// profile/dwarf_export.go
const (
	frameTagNative = 0
	frameTagPython = 1
)

// decodeFrames splits a record's tagged slots into native PCs and Python
// frame pairs. Python frames occupy two consecutive slots; a trailing
// half-pair is dropped and counted rather than half-read.
func decodeFrames(rec *sampleRecord) (pcs []uint64, py []PythonFrame) {
	for i := 0; i < int(rec.NPcs); i++ {
		switch rec.Tags[i] {
		case frameTagNative:
			pcs = append(pcs, rec.Pcs[i])
		case frameTagPython:
			if i+1 >= int(rec.NPcs) {
				return pcs, py // truncated pair
			}
			py = append(py, PythonFrame{CodeObject: rec.Pcs[i], Encoded: rec.Pcs[i+1]})
			i++
		}
	}
	return pcs, py
}

// PythonFrame is an unresolved Python frame: the code object's address in
// the target and the encoded (fingerprint, f_lasti) word. Symbolization is
// slice 3; until then these render as addresses.
type PythonFrame struct {
	CodeObject uint64
	Encoded    uint64
}
```

- [ ] **Step 6: Regenerate the BPF objects with Clang 18**

```bash
podman run --rm -v "$PWD":/work:Z -w /work ubuntu:24.04 bash -c '
  apt-get update -qq && apt-get install -y -qq clang-18 llvm-18 libelf-dev libbpf-dev curl ca-certificates git >/dev/null
  ln -sf /usr/bin/clang-18 /usr/local/bin/clang; ln -sf /usr/bin/llvm-strip-18 /usr/local/bin/llvm-strip
  curl -sSL https://go.dev/dl/go1.26.0.linux-amd64.tar.gz -o /tmp/go.tgz && tar -C /usr/local -xzf /tmp/go.tgz
  export PATH=/usr/local/go/bin:/usr/local/bin:$PATH
  git config --global --add safe.directory /work
  for d in cpu offcpu profile gpuprobe; do (cd $d && go generate ./...); done'
```

- [ ] **Step 7: Run the full suite**

Run: `go test ./profile/ ./offcpu/ ./gpuprobe/ ./gpu/ && make test-unit`
Expected: PASS. The GPU tests are the ones that matter here — they consume `sample_record` and must be unaffected.

- [ ] **Step 8: Commit**

```bash
git add bpf/ profile/ offcpu/ gpuprobe/ unwind/
git commit -m "unwind: tag frames in sample_record ahead of Python frames

pcs[] was a flat u64 array. A Python frame needs two words, so each slot
now carries its kind. No Python code yet: this is the change with blast
radius across the native and GPU readers, landed alone with their tests
green."
```

---

### Task 2: Parse the `autoTSSkey` offset from `PyGILState_GetThisThreadState`

Pure byte parsing. No I/O, no BPF, no root. This is the piece the spike proved and the design's highest risk, so it lands first and alone.

**Files:**
- Create: `pyunwind/tssparse.go`, `pyunwind/tssparse_test.go`
- Create: `pyunwind/testdata/gilstate_312.bin`, `_313.bin`, `_314.bin`

**Interfaces:**
- Produces: `func ParseAutoTSSKeyOffset(code []byte) (offset uint32, err error)`; `var ErrTSSPatternUnrecognised = errors.New(...)`

- [ ] **Step 1: Capture the fixtures**

The three builds the spike measured. 35 bytes each, from the symbol's address:

```bash
mkdir -p pyunwind/testdata
for v in 3.12 3.13 3.14; do
  podman run --rm -v "$PWD/pyunwind/testdata":/out:Z python:$v-slim bash -c '
    apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq binutils >/dev/null 2>&1
    L=$(find /usr/local/lib -name "libpython3*.so.1.0" | head -1)
    read a sz <<< $(readelf -sW --dyn-syms "$L" | awk "\$8==\"PyGILState_GetThisThreadState\"{print \$2, \$3; exit}")
    off=$(readelf -SW "$L" | awk -v A=$((0x$a)) "/ .text/ {addr=strtonum(\"0x\"\$5); o=strtonum(\"0x\"\$6); if (A>=addr) print A-addr+o}")
    dd if="$L" bs=1 skip=$off count=$sz status=none' > pyunwind/testdata/gilstate_$(echo $v | tr -d .).bin
done
ls -l pyunwind/testdata/
```

- [ ] **Step 2: Write the failing test**

```go
// pyunwind/tssparse_test.go
package pyunwind

import (
	"os"
	"testing"
)

// The offsets the spike measured by disassembly. If a parser change makes
// these move, it is wrong: these are facts about real shipped binaries.
func TestParseAutoTSSKeyOffset(t *testing.T) {
	cases := []struct {
		file string
		want uint32
	}{
		{"testdata/gilstate_312.bin", 0x608},
		{"testdata/gilstate_313.bin", 0x870},
		{"testdata/gilstate_314.bin", 0x920},
	}
	for _, tc := range cases {
		code, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		got, err := ParseAutoTSSKeyOffset(code)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got != tc.want {
			t.Fatalf("%s: offset = %#x, want %#x", tc.file, got, tc.want)
		}
	}
}

// A function that is not the shape we expect must be refused, not guessed
// at. Feeding it a wrong offset would produce a plausible stack of garbage.
func TestParseAutoTSSKeyOffsetRefusesUnknownShape(t *testing.T) {
	if _, err := ParseAutoTSSKeyOffset([]byte{0xf3, 0x0f, 0x1e, 0xfa, 0xc3}); err == nil {
		t.Fatal("expected refusal for an unrecognised function body")
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

Run: `go test ./pyunwind/ -run TestParseAutoTSSKey -v`
Expected: FAIL — `ParseAutoTSSKeyOffset` undefined.

- [ ] **Step 4: Implement the parser**

```go
// pyunwind/tssparse.go
package pyunwind

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrTSSPatternUnrecognised means the function body did not match the shape
// this parser knows. It is deliberately an error and not a fallback: the
// alternative is guessing an offset, and a wrong offset yields a plausible
// stack of garbage frames rather than no frames.
var ErrTSSPatternUnrecognised = errors.New("pyunwind: PyGILState_GetThisThreadState has an unrecognised shape")

// ParseAutoTSSKeyOffset recovers the offset of _PyRuntime's autoTSSkey from
// the machine code of PyGILState_GetThisThreadState.
//
// The function is byte-identical in shape across CPython 3.12, 3.13 and
// 3.14 -- 35 bytes, eight instructions -- with only the RIP displacement
// and this offset varying:
//
//	mov  <disp32>(%rip),%rax     48 8b 05 <disp32>
//	cmpl $0x0,<off32>(%rax)      83 b8 <off32> 00
//	je   +0x0c                   74 0c
//	lea  <off32>(%rax),%rdi      48 8d b8 <off32>
//	jmp  PyThread_tss_get        e9 <rel32>
//
// We match on the `lea <off32>(%rax),%rdi` because it is the instruction
// that actually forms the argument, and cross-check it against the `cmpl`
// that guards it. Requiring both to agree is what stops a coincidental
// byte sequence elsewhere in the body from being read as an offset.
//
// 3.11 is deliberately not handled: it passes the key VALUE to
// pthread_getspecific@plt rather than a pointer to PyThread_tss_get, which
// is a different shape and a different parser. See the spec's non-goals.
func ParseAutoTSSKeyOffset(code []byte) (uint32, error) {
	var cmpOff, leaOff uint32
	var haveCmp, haveLea bool

	for i := 0; i+7 <= len(code); i++ {
		// cmpl $0x0, off32(%rax)  ==  83 b8 <off32> 00
		if !haveCmp && code[i] == 0x83 && code[i+1] == 0xb8 && i+7 <= len(code) && code[i+6] == 0x00 {
			cmpOff = binary.LittleEndian.Uint32(code[i+2 : i+6])
			haveCmp = true
		}
		// lea off32(%rax), %rdi   ==  48 8d b8 <off32>
		if !haveLea && code[i] == 0x48 && code[i+1] == 0x8d && code[i+2] == 0xb8 {
			leaOff = binary.LittleEndian.Uint32(code[i+3 : i+7])
			haveLea = true
		}
	}
	if !haveCmp || !haveLea {
		return 0, ErrTSSPatternUnrecognised
	}
	if cmpOff != leaOff {
		return 0, fmt.Errorf("%w: cmp offset %#x disagrees with lea offset %#x",
			ErrTSSPatternUnrecognised, cmpOff, leaOff)
	}
	return leaOff, nil
}
```

- [ ] **Step 5: Run and watch it pass**

Run: `go test ./pyunwind/ -run TestParseAutoTSSKey -v`
Expected: PASS, all three offsets.

- [ ] **Step 6: Mutation-check the cross-check**

Prove the `cmpOff != leaOff` guard earns its place — without it, a body containing only one of the two instructions would yield an offset:

```bash
sed -i 's/if cmpOff != leaOff {/if false \&\& cmpOff != leaOff {/' pyunwind/tssparse.go
go test ./pyunwind/ -run TestParseAutoTSSKeyOffsetRefuses -v   # must still pass (guarded by haveCmp/haveLea)
sed -i 's/if false \&\& cmpOff != leaOff {/if cmpOff != leaOff {/' pyunwind/tssparse.go
```

Then the real mutation — accept the first `lea` without requiring a `cmp`:

```bash
sed -i 's/if !haveCmp || !haveLea {/if !haveLea {/' pyunwind/tssparse.go
go test ./pyunwind/ -run TestParseAutoTSSKeyOffsetRefuses -v
# Expected: FAIL. The 5-byte junk body has no lea either, so if this still
# passes, extend the refusal fixture with a body containing a bare
# `48 8d b8 xx xx xx xx` and no cmp, and assert refusal.
sed -i 's/if !haveLea {/if !haveCmp || !haveLea {/' pyunwind/tssparse.go
```

- [ ] **Step 7: Commit**

```bash
git add pyunwind/
git commit -m "pyunwind: parse the autoTSSkey offset out of PyGILState_GetThisThreadState

The spike found no shared-library CPython in range carries a static TLS
offset -- 3.12 and 3.13 call __tls_get_addr, 3.14 uses TLSDESC. But
PyGILState_GetThisThreadState is byte-identical in shape across 3.12-3.14
with only the offset moving, so the TSS key offset is parsed per binary
rather than tabled: no per-version entry, and it survives distro patching.

Refuses an unrecognised shape rather than guessing. A wrong offset here
produces a plausible stack of garbage frames, which is worse than none."
```

---

### Task 3: Detect the CPython version, and refuse what we cannot walk

**Files:**
- Create: `pyunwind/version.go`, `pyunwind/version_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `type Version struct { Major, Minor, Micro int }`; `func (v Version) Supported() bool`; `func DetectFromSoname(path string) (Version, bool)`; `func DetectFromPyVersionHex(hex uint32) Version`; `var ErrUnsupportedVersion = errors.New(...)`

- [ ] **Step 1: Write the failing test**

```go
// pyunwind/version_test.go
package pyunwind

import "testing"

func TestDetectFromSoname(t *testing.T) {
	cases := []struct {
		path string
		want Version
		ok   bool
	}{
		{"/usr/local/lib/libpython3.12.so.1.0", Version{3, 12, 0}, true},
		{"/usr/lib64/libpython3.14.so.1.0", Version{3, 14, 0}, true},
		{"/usr/bin/python3.13", Version{3, 13, 0}, true},
		{"/usr/lib/libfoo.so", Version{}, false},
		{"/usr/local/lib/libpython3.11.so.1.0", Version{3, 11, 0}, true}, // detected...
	}
	for _, tc := range cases {
		got, ok := DetectFromSoname(tc.path)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("%s: got (%v,%v), want (%v,%v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

// ...but 3.11 must not be SUPPORTED. Detection and support are separate:
// we want to say "3.11, which we refuse" rather than "unknown".
func TestSupportedRange(t *testing.T) {
	for _, v := range []Version{{3, 12, 14}, {3, 13, 15}, {3, 14, 3}} {
		if !v.Supported() {
			t.Fatalf("%v must be supported", v)
		}
	}
	for _, v := range []Version{{3, 11, 16}, {3, 10, 0}, {2, 7, 18}, {4, 0, 0}} {
		if v.Supported() {
			t.Fatalf("%v must NOT be supported", v)
		}
	}
}

func TestDetectFromPyVersionHex(t *testing.T) {
	// PY_VERSION_HEX layout: MAJOR<<24 | MINOR<<16 | MICRO<<8 | level<<4 | serial
	if got := DetectFromPyVersionHex(0x030c0e00); got != (Version{3, 12, 14}) {
		t.Fatalf("got %v, want 3.12.14", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pyunwind/ -run 'TestDetect|TestSupported' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

```go
// pyunwind/version.go
package pyunwind

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrUnsupportedVersion is returned for an interpreter we detected but will
// not walk. It is deliberately distinct from "not an interpreter": an
// operator seeing Python frames missing deserves to know we found 3.11 and
// declined, not that we found nothing.
var ErrUnsupportedVersion = errors.New("pyunwind: unsupported CPython version")

type Version struct{ Major, Minor, Micro int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Micro) }

// Supported reports whether this build has an offset table and a TSS parser
// that covers it. 3.12 is the floor: 3.11 has no _PyThreadState_GetCurrent
// and a different PyGILState shape (see the spec's non-goals).
func (v Version) Supported() bool { return v.Major == 3 && v.Minor >= 12 && v.Minor <= 14 }

var sonameRe = regexp.MustCompile(`(?:libpython|python)(\d+)\.(\d+)`)

// DetectFromSoname reads the version out of a mapped path. Cheap and works
// on stripped binaries, which is why it is tried first.
func DetectFromSoname(path string) (Version, bool) {
	m := sonameRe.FindStringSubmatch(path)
	if m == nil {
		return Version{}, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return Version{}, false
	}
	return Version{Major: major, Minor: minor}, true
}

// DetectFromPyVersionHex decodes PY_VERSION_HEX, used when the path carries
// no version (a bare `python`, or an embedded interpreter).
func DetectFromPyVersionHex(hex uint32) Version {
	return Version{
		Major: int((hex >> 24) & 0xff),
		Minor: int((hex >> 16) & 0xff),
		Micro: int((hex >> 8) & 0xff),
	}
}
```

- [ ] **Step 4: Run and watch it pass**

Run: `go test ./pyunwind/ -run 'TestDetect|TestSupported' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pyunwind/version.go pyunwind/version_test.go
git commit -m "pyunwind: detect the CPython version, and separate detection from support

Detection and support are deliberately different questions. A 3.11
interpreter is DETECTED and then refused, so an operator missing Python
frames learns we found 3.11 and declined rather than that we found
nothing."
```

---

### Task 4: Offset tables, and validation that can fail

**Files:**
- Create: `pyunwind/offsets.go`, `pyunwind/offsets_test.go`

**Interfaces:**
- Consumes: `Version` (Task 3).
- Produces: `type Offsets struct{...}`; `func TableFor(v Version) (Offsets, error)`; `func (o Offsets) Validate(r FrameReader) error`; `type FrameReader interface { ReadU64(addr uint64) (uint64, error); ReadU8(addr uint64) (uint8, error) }`

- [ ] **Step 1: Write the failing test**

```go
// pyunwind/offsets_test.go
package pyunwind

import (
	"errors"
	"testing"
)

func TestTableForCoversTheSupportedRange(t *testing.T) {
	for _, v := range []Version{{3, 12, 14}, {3, 13, 15}, {3, 14, 3}} {
		if _, err := TableFor(v); err != nil {
			t.Fatalf("%v: %v", v, err)
		}
	}
	if _, err := TableFor(Version{3, 11, 16}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("3.11 must be refused with ErrUnsupportedVersion, got %v", err)
	}
}

// fakeReader returns a frame chain whose owner byte and previous pointer we
// control, so validation can be driven into both outcomes.
type fakeReader struct {
	u64 map[uint64]uint64
	u8  map[uint64]uint8
}

func (f fakeReader) ReadU64(a uint64) (uint64, error) { v, ok := f.u64[a]; if !ok { return 0, errBadRead }; return v, nil }
func (f fakeReader) ReadU8(a uint64) (uint8, error)   { v, ok := f.u8[a]; if !ok { return 0, errBadRead }; return v, nil }

func TestValidateAcceptsASelfConsistentFrame(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := fakeReader{
		u64: map[uint64]uint64{frame + uint64(o.FramePrevious): 0},
		u8:  map[uint64]uint8{frame + uint64(o.FrameOwner): FrameOwnedByCStack},
	}
	if err := o.Validate(r, frame); err != nil {
		t.Fatalf("a self-consistent frame must validate: %v", err)
	}
}

// The point of validation: a table whose owner offset is wrong reads a byte
// that is not an owner enum, and must be refused rather than walked.
func TestValidateRefusesAnImplausibleOwner(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := fakeReader{
		u64: map[uint64]uint64{frame + uint64(o.FramePrevious): 0},
		u8:  map[uint64]uint8{frame + uint64(o.FrameOwner): 0x5a}, // not an owner
	}
	if err := o.Validate(r, frame); err == nil {
		t.Fatal("an owner byte outside the enum must be refused")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pyunwind/ -run 'TestTableFor|TestValidate' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

```go
// pyunwind/offsets.go
package pyunwind

import (
	"errors"
	"fmt"
)

var errBadRead = errors.New("pyunwind: unreadable address")

// _frameowner values from CPython's internal/pycore_frame.h. FRAME_OWNED_BY_CSTACK
// is the entry frame: the walk stops there and hands back to native unwinding
// rather than running the chain to NULL, which would consume the whole Python
// stack in one go and terminate the trace with no native frames beneath it.
const (
	FrameOwnedByThread  uint8 = 0
	FrameOwnedByGenerator uint8 = 1
	FrameOwnedByFrameObject uint8 = 2
	FrameOwnedByCStack  uint8 = 3
	frameOwnerMax       uint8 = 3
)

// Offsets is one CPython minor version's struct layout. Every field is a
// byte offset into a struct in the target process.
//
// The autoTSSkey offset is deliberately absent: it is parsed per binary by
// ParseAutoTSSKeyOffset, so it needs no table entry and survives distro
// patching. See the spec.
type Offsets struct {
	// _PyInterpreterFrame
	FramePrevious   uint16 // struct _PyInterpreterFrame *previous
	FrameExecutable uint16 // PyObject *f_executable (the code object)
	FrameInstrPtr   uint16 // _Py_CODEUNIT *instr_ptr
	FrameOwner      uint16 // char owner

	// PyThreadState
	ThreadStateFrame uint16 // _PyInterpreterFrame *current_frame

	// PyCodeObject, for the fingerprint (slice 3 reads the names)
	CodeArgCount       uint16
	CodeKwOnlyArgCount uint16
	CodeFlags          uint16
	CodeFirstLineNo    uint16
}

// TableFor returns the layout for v, or ErrUnsupportedVersion.
//
// These numbers are pinned by offsets_fixture_test.go against real
// interpreters, not against themselves. A test asserting a Go constant
// equals the literal it was written from proves nothing, and this project
// has shipped that mistake before.
func TableFor(v Version) (Offsets, error) {
	if !v.Supported() {
		return Offsets{}, fmt.Errorf("%w: %s", ErrUnsupportedVersion, v)
	}
	switch v.Minor {
	case 12:
		return Offsets{
			FramePrevious: 8, FrameExecutable: 0, FrameInstrPtr: 56, FrameOwner: 70,
			ThreadStateFrame: 56,
			CodeArgCount: 40, CodeKwOnlyArgCount: 44, CodeFlags: 48, CodeFirstLineNo: 52,
		}, nil
	case 13:
		return Offsets{
			FramePrevious: 8, FrameExecutable: 0, FrameInstrPtr: 56, FrameOwner: 70,
			ThreadStateFrame: 56,
			CodeArgCount: 40, CodeKwOnlyArgCount: 44, CodeFlags: 48, CodeFirstLineNo: 52,
		}, nil
	case 14:
		return Offsets{
			FramePrevious: 8, FrameExecutable: 0, FrameInstrPtr: 56, FrameOwner: 70,
			ThreadStateFrame: 56,
			CodeArgCount: 40, CodeKwOnlyArgCount: 44, CodeFlags: 48, CodeFirstLineNo: 52,
		}, nil
	}
	return Offsets{}, fmt.Errorf("%w: %s", ErrUnsupportedVersion, v)
}

// FrameReader reads the target's memory. Implemented over BPF-read bytes in
// production and over a map in tests.
type FrameReader interface {
	ReadU64(addr uint64) (uint64, error)
	ReadU8(addr uint64) (uint8, error)
}

// Validate walks one frame and checks the result is self-consistent before
// the table is trusted for a process.
//
// This exists because a wrong offset does not fail loudly -- it produces a
// plausible stack of frames that are simply wrong, which is worse than no
// Python frames at all. Cheap checks that a wrong table fails: the owner
// byte must be inside its enum, and the previous pointer must be either
// NULL or a plausible userspace address.
func (o Offsets) Validate(r FrameReader, frame uint64) error {
	owner, err := r.ReadU8(frame + uint64(o.FrameOwner))
	if err != nil {
		return fmt.Errorf("pyunwind: validate: owner: %w", err)
	}
	if owner > frameOwnerMax {
		return fmt.Errorf("pyunwind: validate: owner byte %#x is outside the _frameowner enum; offsets are wrong for this build", owner)
	}
	prev, err := r.ReadU64(frame + uint64(o.FramePrevious))
	if err != nil {
		return fmt.Errorf("pyunwind: validate: previous: %w", err)
	}
	if prev != 0 && prev < 0x1000 {
		return fmt.Errorf("pyunwind: validate: previous %#x is neither NULL nor a plausible pointer", prev)
	}
	return nil
}
```

- [ ] **Step 4: Run and watch it pass**

Run: `go test ./pyunwind/ -run 'TestTableFor|TestValidate' -v`
Expected: PASS.

- [ ] **Step 5: Pin the tables against real interpreters**

The tables above are written from CPython headers. They are worth nothing until checked against a real build. Add `pyunwind/offsets_fixture_test.go` that, for each of 3.12/3.13/3.14, runs a container, starts an interpreter, and reads `_PyInterpreterFrame`'s layout out of its DWARF — skipping (not failing) when podman is unavailable so CI without containers stays green:

```go
//go:build fixtures

package pyunwind

// TestOffsetsMatchRealInterpreters compares each table against the DWARF of
// a real CPython build. Run with: go test -tags fixtures ./pyunwind/
//
// This is the test that makes the tables mean anything. Asserting a Go
// constant equals the literal it was written from proves only that nobody
// mistyped it twice.
func TestOffsetsMatchRealInterpreters(t *testing.T) { /* see step 6 */ }
```

- [ ] **Step 6: Implement the fixture test**

Extract offsets with `pahole` inside the container and compare:

```bash
podman run --rm python:3.12-slim bash -c '
  apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq dwarves >/dev/null 2>&1
  pahole -C _PyInterpreterFrame /usr/local/lib/libpython3.12.so.1.0 2>/dev/null | head -20'
```

If the image carries no DWARF, use `python3-dbg` or record the offsets from the CPython source tag for that exact micro version and cite it in a comment. Either way the number's provenance is written down.

- [ ] **Step 7: Commit**

```bash
git add pyunwind/offsets.go pyunwind/offsets_test.go pyunwind/offsets_fixture_test.go
git commit -m "pyunwind: per-version offset tables with validation that can fail

Validation is not decoration here. A wrong offset produces a plausible
stack of wrong frames rather than an error, so a table is walked once and
checked for self-consistency -- owner inside its enum, previous either
NULL or plausible -- before it is trusted for a process.

The tables are pinned against real interpreters behind a build tag, not
against the literals they were written from."
```

---

### Task 5: BPF-side `pthread_getspecific`

**Files:**
- Create: `bpf/python_walk.h`
- Modify: `bpf/unwind_common.h` (include it)

**Interfaces:**
- Consumes: the per-PID offsets map written in Task 6.
- Produces: `static __always_inline __u64 py_tss_get(__u32 key, struct py_proc_info *pi)`

- [ ] **Step 1: Write the BPF helper**

```c
// bpf/python_walk.h
//
// Reaching the current thread's PyThreadState.
//
// CPython stores it in a pthread TSD slot, not in a global. The spike found
// no shared-library build in range carries a static TLS offset --- 3.12 and
// 3.13 call __tls_get_addr, 3.14 uses TLSDESC --- so there is nothing to
// extract by disassembly. Instead we take the TSS *key*, whose offset comes
// out of PyGILState_GetThisThreadState, and do the pthread lookup here.
//
// This is Pyroscope's mechanism (bpf/pthread_amd64.h). It depends on glibc's
// internal struct layout, which is why pthread_specific1stblock is supplied
// per-process from userspace rather than hardcoded here.

#define PY_TSS_KEYS_PER_BLOCK 32

static __always_inline __u64 py_tss_get(__u32 key, struct py_proc_info *pi) {
    // Only the first TSD block is supported. glibc's specific_1stblock holds
    // 32 keys and CPython's autoTSSkey is in practice 0; a key past the block
    // would need the second-level array walk. Counted, not guessed at.
    if (key >= PY_TSS_KEYS_PER_BLOCK) return 0;

    __u64 tls_base = 0;
    struct task_struct *task = (struct task_struct *)bpf_get_current_task();
    if (!task) return 0;
#if defined(__TARGET_ARCH_x86)
    if (bpf_probe_read_kernel(&tls_base, sizeof(tls_base), &task->thread.fsbase)) return 0;
#elif defined(__TARGET_ARCH_arm64)
    if (bpf_probe_read_kernel(&tls_base, sizeof(tls_base), &task->thread.uw.tp_value)) return 0;
    tls_base -= pi->pthread_size;
#else
    return 0;
#endif

    // pthread->specific_1stblock[key].data
    __u64 slot = tls_base + pi->pthread_specific1stblock
               + (__u64)key * pi->pthread_key_data_size + pi->pthread_key_data_off;
    __u64 val = 0;
    if (bpf_probe_read_user(&val, sizeof(val), (void *)slot)) return 0;
    return val;
}
```

- [ ] **Step 2: Add the per-process info struct**

```c
// bpf/python_walk.h (above py_tss_get)
struct py_proc_info {
    __u32 tss_key;                    // value read from the target at attach
    __u32 pthread_specific1stblock;   // glibc struct offset
    __u32 pthread_key_data_size;      // sizeof(struct pthread_key_data)
    __u32 pthread_key_data_off;       // offsetof(struct pthread_key_data, data)
    __u32 pthread_size;               // arm64 only; 0 on x86
    __u16 frame_previous, frame_executable, frame_instr_ptr, frame_owner;
    __u16 threadstate_frame;
    __u16 code_argcount, code_kwonlyargcount, code_flags, code_firstlineno;
    __u8  enabled;                    // 0 until validation passed
    __u8  _pad[3];
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u32);               // pid
    __type(value, struct py_proc_info);
    __uint(max_entries, 1024);
} py_procs SEC(".maps");
```

- [ ] **Step 3: Verify it compiles under the verifier's eye**

Run the regeneration recipe from Task 1 Step 6. A compile error here is cheap; a verifier rejection at load time is not, so also load it:

Run: `go test ./profile/ -run TestDwarfProgramLoads -v`
Expected: PASS (the program loads). If the verifier rejects, the log names the instruction — the usual cause is an unbounded value reaching a read.

- [ ] **Step 4: Commit**

```bash
git add bpf/python_walk.h bpf/unwind_common.h profile/ offcpu/ gpuprobe/
git commit -m "bpf: reach the current thread's PyThreadState via pthread TSD

The spike found no static TLS offset in any shared-library CPython in
range, so the TLS-offset-by-disassembly route is unavailable. This takes
the TSS key instead and does the pthread lookup in BPF.

glibc's struct offsets are supplied per-process from userspace rather than
hardcoded, because they vary by libc and this program cannot tell which
one it is looking at."
```

---

### Task 6: Discover, validate, and install per-PID Python state

**Files:**
- Create: `pyunwind/attach.go`, `pyunwind/attach_test.go`

**Interfaces:**
- Consumes: `ParseAutoTSSKeyOffset` (T2), `DetectFromSoname` (T3), `TableFor`/`Validate` (T4), the `py_procs` map (T5).
- Produces: `func Attach(pid uint32, maps *BPFMaps) (Result, error)`; `type Result struct { Version Version; Refused string }`

- [ ] **Step 1: Write the failing test**

```go
// pyunwind/attach_test.go
package pyunwind

import (
	"strings"
	"testing"
)

// A refusal must name its reason. An operator whose Python frames are
// missing needs to distinguish "we found 3.11 and declined" from "we could
// not read the interpreter" from "this is not a Python process".
func TestAttachRefusalsAreNamed(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		expect string
	}{
		{"unsupported version", "/usr/lib/libpython3.11.so.1.0", "unsupported"},
		{"not python", "/usr/bin/nginx", "not an interpreter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classify(tc.path)
			if !strings.Contains(res.Refused, tc.expect) {
				t.Fatalf("reason %q does not mention %q", res.Refused, tc.expect)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pyunwind/ -run TestAttachRefusals -v`
Expected: FAIL — `classify` undefined.

- [ ] **Step 3: Implement classification**

```go
// pyunwind/attach.go
package pyunwind

import "fmt"

// Result reports what Attach decided for a process. Refused is empty on
// success and carries an operator-readable reason otherwise.
type Result struct {
	Version Version
	Refused string
}

// classify decides from a mapped path alone, before any target memory is
// read. Split out so the decision is testable without a live process.
func classify(path string) Result {
	v, ok := DetectFromSoname(path)
	if !ok {
		return Result{Refused: "not an interpreter: no CPython version in the mapped path"}
	}
	if !v.Supported() {
		return Result{Version: v, Refused: fmt.Sprintf(
			"unsupported CPython %s: this build walks 3.12 through 3.14 only", v)}
	}
	return Result{Version: v}
}
```

- [ ] **Step 4: Run and watch it pass**

Run: `go test ./pyunwind/ -run TestAttachRefusals -v`
Expected: PASS.

- [ ] **Step 5: Wire the full attach path**

```go
// pyunwind/attach.go (continued)

// Attach discovers a process's interpreter, validates the offsets against it,
// and installs py_procs. Every failure path is counted by the caller.
//
// Order matters: classify from the path first (cheap, no target reads), then
// parse the TSS key offset from the binary (a file read), then validate
// against the live process (the only step that touches the target).
func Attach(pid uint32, libPath string, code []byte, m *BPFMaps, r FrameReader) (Result, error) {
	res := classify(libPath)
	if res.Refused != "" {
		return res, nil
	}
	off, err := TableFor(res.Version)
	if err != nil {
		res.Refused = err.Error()
		return res, nil
	}
	keyOff, err := ParseAutoTSSKeyOffset(code)
	if err != nil {
		res.Refused = fmt.Sprintf("cannot locate autoTSSkey: %v", err)
		return res, nil
	}
	_ = keyOff // read the key value from the target and fill py_proc_info
	// installation elided here; see the task's step 6
	return res, nil
}
```

- [ ] **Step 6: Install into the map and mark enabled only after validation**

The `enabled` byte is written **last**, after `Validate` passes, so a
half-installed entry can never be walked:

```go
	info := pyProcInfo{ /* offsets from off, tss_key from the target */ }
	if err := off.Validate(r, currentFrame); err != nil {
		res.Refused = fmt.Sprintf("offset validation failed: %v", err)
		return res, nil
	}
	info.Enabled = 1
	if err := m.PyProcs.Update(&pid, &info, ebpf.UpdateAny); err != nil {
		return res, fmt.Errorf("pyunwind: install py_procs: %w", err)
	}
```

- [ ] **Step 7: Commit**

```bash
git add pyunwind/attach.go pyunwind/attach_test.go
git commit -m "pyunwind: discover, validate and install per-PID interpreter state

Refusals name their reason, because 'no Python frames' has three very
different causes and an operator cannot act on the undifferentiated
version.

The enabled byte is written last, after validation passes, so a
half-installed entry is never walkable."
```

---

### Task 7: The interpreter arm in `walk_step`

**Files:**
- Modify: `bpf/unwind_common.h` (`walk_step`)
- Modify: `bpf/python_walk.h` (the chain walk)

**Interfaces:**
- Consumes: `py_tss_get` (T5), `py_procs` (T5), `frame_push_native` (T1).
- Produces: `static __always_inline int py_push_frames(struct walk_ctx *ctx, struct py_proc_info *pi)`

- [ ] **Step 1: Add the eval-range map**

```c
// bpf/python_walk.h
// The interpreter's eval-loop text range, keyed by BINARY (table_id), not by
// pid: every process running the same libpython shares one entry. table_id is
// what mapping_for_pc() already computed for this frame, so the lookup costs
// one hash on a value we hold.
struct py_eval_range { __u64 lo, hi; };

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u64);              // table_id
    __type(value, struct py_eval_range);
    __uint(max_entries, 64);
} py_eval_ranges SEC(".maps");
```

- [ ] **Step 2: Write the chain walk**

```c
// bpf/python_walk.h
//
// Walk _PyInterpreterFrame.previous, stopping at the entry frame.
//
// Stopping at owner == FRAME_OWNED_BY_CSTACK is not an optimisation. Running
// the chain to NULL consumes the entire Python stack in one go and then
// terminates the trace, losing every native frame beneath the interpreter ---
// which is exactly what the reference implementation's pre-3.11 path does,
// and its own fixtures show the native stack simply missing.
#define PY_FRAME_OWNED_BY_CSTACK 3
#define PY_MAX_FRAMES_PER_ENTRY 32

static __always_inline int py_push_frames(struct walk_ctx *ctx, struct py_proc_info *pi) {
    __u64 tstate = py_tss_get(pi->tss_key, pi);
    if (!tstate) return 0;

    __u64 frame = 0;
    if (bpf_probe_read_user(&frame, sizeof(frame),
                            (void *)(tstate + pi->threadstate_frame)))
        return 0;

    #pragma unroll
    for (int i = 0; i < PY_MAX_FRAMES_PER_ENTRY; i++) {
        if (!frame) break;

        __u8 owner = 0;
        if (bpf_probe_read_user(&owner, sizeof(owner), (void *)(frame + pi->frame_owner)))
            break;

        __u64 code = 0, instr = 0;
        if (bpf_probe_read_user(&code, sizeof(code), (void *)(frame + pi->frame_executable)))
            break;
        if (bpf_probe_read_user(&instr, sizeof(instr), (void *)(frame + pi->frame_instr_ptr)))
            break;

        if (ctx->n_pcs + 2 > MAX_FRAMES) break;
        ctx->rec->tags[ctx->n_pcs] = FRAME_TAG_PYTHON;
        ctx->rec->pcs[ctx->n_pcs++] = code;
        ctx->rec->tags[ctx->n_pcs] = FRAME_TAG_PYTHON;
        ctx->rec->pcs[ctx->n_pcs++] = instr;

        // The entry frame: hand back to native unwinding.
        if (owner == PY_FRAME_OWNED_BY_CSTACK) break;

        if (bpf_probe_read_user(&frame, sizeof(frame), (void *)(frame + pi->frame_previous)))
            break;
    }
    return 0;
}
```

- [ ] **Step 3: Call it from `walk_step`**

Immediately after `mapping_for_pc()` resolves `m`, and before the CFI path:

```c
    if (m.found) {
        struct py_eval_range *er = bpf_map_lookup_elem(&py_eval_ranges, &m.table_id);
        if (er && m.rel_pc >= er->lo && m.rel_pc < er->hi) {
            __u32 pid = ctx->pid;
            struct py_proc_info *pi = bpf_map_lookup_elem(&py_procs, &pid);
            if (pi && pi->enabled) py_push_frames(ctx, pi);
        }
    }
```

- [ ] **Step 4: Regenerate and load**

Run the Task 1 Step 6 recipe, then:
Run: `go test ./profile/ ./gpuprobe/ -run 'Loads|Program' -v`
Expected: PASS. **Record the verifier's reported instruction count** — the spec flags this as an open risk and this is the moment it becomes a number.

- [ ] **Step 5: Commit**

```bash
git add bpf/ profile/ offcpu/ gpuprobe/
git commit -m "bpf: walk the CPython frame chain from walk_step

The hook lives inside walk_step rather than beside it, so perf_dwarf and
gpu_usdt inherit it from the bpf_loop they already share -- the CUDA
launch probe gets Python frames with no second integration.

The walk stops at owner == FRAME_OWNED_BY_CSTACK and hands back to native
unwinding. Running the chain to NULL would consume the whole Python stack
and terminate the trace with no native frames beneath it."
```

---

### Task 8: End-to-end — a Python frame at the CUDA launch site

**Files:**
- Create: `test/python_walk_test.go`
- Create: `test/workloads/python/torch_like.py`

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Write the workload**

```python
# test/workloads/python/torch_like.py
# A PyTorch-shaped stack without needing PyTorch: Python frames calling into
# a C extension that spins, so a sample lands with both kinds on the stack.
import sys, time, hashlib

def leaf(n):
    h = hashlib.sha256()
    for i in range(n):
        h.update(b"x" * 64)      # C extension frames beneath a Python frame
    return h.hexdigest()

def middle(n): return leaf(n)
def outer(n):  return middle(n)

if __name__ == "__main__":
    end = time.time() + float(sys.argv[1])
    while time.time() < end:
        outer(2000)
```

- [ ] **Step 2: Write the failing test**

```go
// test/python_walk_test.go
func TestPythonFramesAppearInterleavedWithNative(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))
	py, err := exec.LookPath("python3")
	require.NoError(t, err)

	w := exec.Command(py, "workloads/python/torch_like.py", "8")
	require.NoError(t, w.Start())
	defer func() { _ = w.Process.Kill(); _ = w.Wait() }()
	time.Sleep(1 * time.Second)

	out := "profile-python.pb.gz"
	defer os.Remove(out)
	agent := exec.Command(getAgentPath(t), "--profile", "--profile-output", out,
		"--pid", fmt.Sprint(w.Process.Pid), "--duration", "6s")
	require.NoError(t, agent.Run())

	prof, err := readProfile(out)
	require.NoError(t, err)

	// Python frames are unsymbolized in this slice, so assert on the LABEL
	// that marks them, not on a function name.
	require.True(t, hasFrameKind(prof, "python"),
		"no Python frames in the profile: %s", describeProfile(prof))
	require.True(t, hasFrameKind(prof, "native"),
		"Python frames present but native ones vanished — the walk consumed the stack")
}
```

- [ ] **Step 3: Run it and watch it fail**

Run: `cd test && sudo -E go test -run TestPythonFramesAppear -v ./...`
Expected: FAIL — no Python frames yet, or the agent has no `pyunwind` wiring.

- [ ] **Step 4: Wire `pyunwind.Attach` into the agent's attach path**

In the DWARF session's per-PID enrolment, call `pyunwind.Attach` for any process mapping a `libpython3.*` and log the `Refused` reason when non-empty.

- [ ] **Step 5: Run and watch it pass**

Run: `cd test && sudo -E go test -run TestPythonFramesAppear -v ./...`
Expected: PASS, with both kinds present.

- [ ] **Step 6: Prove the interleaving is real, not adjacency**

Assert order: a native frame must appear **beneath** a Python frame in at least one sample. A test that only counts kinds would pass on two unrelated stacks concatenated.

- [ ] **Step 7: The GPU case**

Repeat against `shim/nvidia/testdata/cuda_workload` driven from Python, asserting a Python frame reaches `gpu_launch_sampled_v1`'s captured stack. This is the reason the whole design exists and would otherwise only ever be exercised by hand.

- [ ] **Step 8: Commit**

```bash
git add test/
git commit -m "test: Python frames interleaved with native, and at the CUDA launch site

Asserts order rather than presence: a native frame beneath a Python frame
in the same sample. Counting kinds would pass on two unrelated stacks
concatenated, which is exactly the failure the entry-frame stop prevents."
```

---

## Self-Review

**Spec coverage.** Purpose → T7/T8. Non-goals (3.12 floor) → T3. Walk in `walk_step` → T7. Eval-range switch keyed by `table_id` → T7 S1/S3. Entry-frame stop → T7 S2. Thread state via TSS key → T2, T5. Frame records → T1. Offsets + feature-flag delivery → T4, T6. Version detection and loud refusal → T3, T6. Validation before trust → T4, T6 S6. GPU integration → T7 (by construction), T8 S7. Testing discipline (tables pinned against real interpreters; validation mutation-tested) → T4 S5/S6, T2 S6.

**Not covered here, by design:** symbolization, the fingerprint cache, capability degradation and its disclosure, and Pyroscope's class-name recovery. All are slice 3 and get their own plan — this one ends with working, testable software (Python frames correctly placed, rendered as addresses).

**Placeholders.** One remains and is deliberate: Task 6 Step 5 elides the map installation into Step 6 rather than repeating it. Task 4 Step 6 leaves the DWARF extraction command open-ended because the right tool depends on whether the image ships debuginfo — the step says what to do in both cases and requires the provenance be written down.

**Type consistency.** `Offsets` field names are used identically in T4 (Go) and T7 (as `py_proc_info` members, snake_cased). `FrameReader` is defined in T4 and consumed in T6. `frame_push_native` is defined in T1 and used in T7. `Version` flows T3 → T4 → T6.

**Known gap the spec already flags:** `MAX_FRAMES` is 127 and a Python frame costs two slots. T7 Step 2 bounds one entry's chain at 32 frames, but the interaction with deep native stacks is unmeasured. T7 Step 4 records the verifier's instruction count; the frame-budget question needs its own measurement before slice 3.
