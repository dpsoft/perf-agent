# Task 1 — `internal/cubin`, a pure-Go cubin line-table reader

Branch `feat/cubin-line-table`. One commit. Plan:
`docs/superpowers/plans/2026-08-25-gpu-pc-sampling.md`, Task 1.

**Cannot verify:** nothing in this task needed hardware, and nothing about it was
verified on hardware. This machine has `CapEff: 0` and no GPU. The plan states
"Must be measured on the RTX 3090 afterwards: **nothing.** This task is complete
offline," and that held — but two things are worth naming as untested rather than
tested:

- **The fixtures are `sm_86`, CUDA 13.3, and `extern "C"`.** Every claim below is a
  claim about cubins that compiler emits for that architecture. A different `-arch`,
  a different toolkit major version, C++-mangled kernel names, separate compilation
  (`-rdc=true`), or a fatbin rather than a bare cubin have not been read.
- **Nothing here has seen a `pcOffset` produced by CUPTI.** The reader resolves
  function-relative offsets because the line table's addresses are function-relative
  (§3 below proves that from the bytes). That CUPTI's `CUpti_PCSamplingPCData.pcOffset`
  is measured from the same origin is the plan's premise, not this task's finding, and
  Task 6 is where it gets measured.

The plan's stated mechanism worked exactly as written. No improvisation was needed.

---

## 1. The API as built

```go
func Parse(b []byte) (*Cubin, error)
func (c *Cubin) Functions() []Function
func (c *Cubin) Resolve(fn string, pcOffset uint64) (file string, line uint32, ok bool)
func (c *Cubin) HasLineInfo() bool

type Function struct {
    Name        string  // symbol name, mangled as the compiler left it
    SymIndex    int     // index in .symtab, counting the null symbol at 0
    Section     string  // ".text.<Name>"
    Size        uint64  // code size, from the symbol
    HasLineInfo bool    // a .debug_line sequence is bound to this function
}
```

The four entry points the plan specifies are exactly as specified. `Resolve` takes the
function by `Name`. Three additions, each with a reason:

**`(*Cubin).LineInfoErr() error`** — required by the constraint that `ok == false` be
decomposable. See §2.

**`Function.SymIndex`** — the plan's finding 2 turns on whether CUPTI's `functionIndex`
is "the function's unique symbol index in the module" in the `.symtab` sense. Task 6
measures that; carrying the index costs nothing and means Task 6 does not have to
re-derive it. **It is exposed, not asserted:** nothing here claims `functionIndex ==
SymIndex`.

**`ErrPanic`** — `Parse` recovers panics from `debug/elf` / `debug/dwarf` and returns
them as errors, because cubin bytes arrive from a profiled process across a trust
boundary and the caller's guarantee is that a bad cubin is *counted*, never fatal.
The sentinel exists so the fuzz target can still fail on a panic. A blanket recover
that made panics indistinguishable from ordinary parse errors would hide from the
fuzzer precisely the bugs it is there to find.

No cgo. Imports are `bytes`, `debug/dwarf`, `debug/elf`, `encoding/binary`, `errors`,
`fmt`, `sort`. No `nvdisasm`, no `cuobjdump`, no libcupti. No SASS decoding.

## 2. `ok == false` is decomposable — the constraint, and how it is met

The plan requires that a caller distinguish "no line table at all" from "line table
present, this PC not in it", because Task 4 builds a four-valued `gpu_src_status` on
them. Reading the bytes turned up a **third** state the two-valued split does not
cover, and collapsing it into either one would misdirect the reader:

| `HasLineInfo()` | `LineInfoErr()` | means | the reader's action |
| --- | --- | --- | --- |
| `false` | `nil` | no `.debug_line` — built without `-lineinfo` | rebuild with `-lineinfo` |
| `true` | `nil`, `Resolve` false | table present, does not cover this PC | nothing; the compiler emitted no line here |
| `true` | non-nil | `.debug_line` present but **damaged** | rebuild will not help; the bytes are wrong |

`HasLineInfo()` deliberately reports the **section's presence**, not the table's
usability. Had it reported usability, a damaged table would read as `no-lineinfo` and
the operator would be told to add a compiler flag they already have. `LineInfoErr()` is
what separates them.

A fourth case is decomposable through `Functions()`: a module can carry line info for
some functions and not others, so `Function.HasLineInfo` distinguishes "this module has
no table" from "this module has a table but not for this function". `Resolve` on an
unknown function name also returns false, and `Functions()` is what tells the two apart.

**Recommendation for Task 4** (not implemented here — the plan says the store is the
single place the enum is decided): map the damaged-table case to `no-module`, alongside
`ModulesUnparseable`, not to `no-lineinfo`. We hold bytes we cannot use, which is the
same actionable fact for the reader as holding nothing — and `no-lineinfo` would be an
active lie about the build flags.

**A `Parse` error is not the same as a missing line table.** `Parse` fails only when the
module is unusable as a whole: not an ELF, `e_machine != EM_CUDA`, not 64-bit
little-endian, or no `.symtab`. A `.debug_line` that is absent or damaged is *not* a
`Parse` error, because the function list survives it and the function list is what names
the kernel a PC sample landed in. Losing kernel names over a build-flag choice would
have made every no-`-lineinfo` module report as `no-module`.

## 3. The `.rel.debug_line` finding — the answer is "one sequence per function", and the relocation is **not** safely ignorable

The plan says to assert this by reading, not assume it. Reading it changed the design.

**What the bytes say.** In `two_kernels_lineinfo.cubin`, `.debug_line` holds **one
line-program sequence per function, each beginning at address 0**. It is not one
relocated sequence spanning the module. Each sequence's `end_sequence` address equals
its function symbol's `st_size` exactly (384 = `0x180` for both kernels; 2432 = `0x980`
for `unrolledSum`). **So the addresses are function-relative**, which is what a
`pcOffset` is, and in the arithmetic sense the plan meant, the relocation *is* the
identity: every relocation targets an all-zero 8-byte operand, and every device function
symbol has `st_value == 0`, so applying the relocations literally writes zero over zero.

**But the relocation is still load-bearing, for a different reason than the plan
anticipated.** Nothing *inside* the line program says which function a sequence
describes. Both kernels in the two-kernel fixture are 384 bytes and both sequences run
`0x000`–`0x180`, so the sequences are byte-for-byte indistinguishable by address. The
only record of the binding is `.rel.debug_line`: one relocation per sequence, against
that sequence's own `FUNC` symbol, pointed at the 8-byte operand of the sequence's
opening `DW_LNE_set_address`.

**And the obvious shortcut is wrong.** In `two_kernels_lineinfo.cubin` the relocation
entries appear in the **reverse** of the sequences' byte order within `.debug_line`:

```
.rel.debug_line[0]  r_offset = 0xb0  ->  scale
.rel.debug_line[1]  r_offset = 0x82  ->  offset      <- 0x82 is the FIRST sequence
```

A reader that paired sequence *i* with relocation *i* — a natural thing to write, and
the thing that "the relocation is the identity, so ignore it" invites — would have
attributed every one of `scale`'s source lines to `offset` and vice versa. It would not
have errored. It would have produced confident, exact-looking, wrong source attribution,
which is the failure mode this project's constraints exist to prevent.

**Consequence for the resolver.** Rather than applying the relocations literally (which
leaves every sequence overlapping at 0 and indistinguishable), `relocSequences` rewrites
each sequence's `DW_LNE_set_address` operand — in a private copy of the section — to a
**distinct synthetic base**, `(i+1) << 40`. The line table then reads back with each
sequence in its own 1 TiB address window, and the window index recovers the binding from
the relocation rather than from position. Addresses are made function-relative again by
subtracting the base.

The rewrite is guarded rather than trusted. Each relocation must land at an offset whose
preceding three bytes are exactly `00 09 02` (`DW_LNE_set_address`), whose 8-byte operand
is currently zero, and which is inside the section. If any of that fails the **whole
table is refused** into `LineInfoErr` rather than partially rewritten. The 1 TiB window
is not a magic number that could silently wrap: a line entry landing outside every window
is also a refusal.

`TestRelDebugLineBindsSequencesToFunctions` asserts all of the above straight from the
ELF bytes — the prologue, the zero operand, `st_value == 0`, each sequence starting at
`pcOffset` 0, each ending at exactly `st_size` — and asserts the reversed relocation
order in the two-kernel fixture explicitly, so if a future toolkit emits them in byte
order the test says so instead of quietly passing.

**Mutation-checked.** Two deliberate defects were introduced to confirm the tests can
fail: swapping the base assignment, and binding every sequence to the first function.
Both were caught by `TestTwoKernelsResolveIndependently` and
`TestRelDebugLineBindsSequencesToFunctions`. A test that cannot fail is not a test.

## 4. The measured PC-to-line collapse ratio

`unrolled_lineinfo.cubin`, `unrolledSum`, a `#pragma unroll` over 64 iterations:

**152 distinct instruction PCs → 5 distinct source lines = 30.4×.**

Comfortably above the plan's ≥ 4× threshold. The threshold was not adjusted.

**Distinct PCs are counted at instruction granularity, not at line-table row
granularity, and the difference is the whole point.** The plan's cardinality table
defines `gpu_pc` as "one per distinct *sampled instruction*", and a PC sample can land
on any instruction, not only on an address where a line-table row begins. The compiler
emits a **single row** spanning the entire unrolled body — `0x050` through `0x820`, 125
instructions, all line 9 — which is exactly the collapse the claim is about. Counted at
row granularity the same fixture gives 7 rows / 5 lines = **1.4×**, which would have
failed the threshold while describing nothing real. Both numbers are reported here so
the choice is visible rather than buried.

The instruction count uses a 16-byte stride: on `sm_70` and later every SASS instruction
is a 128-bit bundle with no separate control word. **This is not SASS decoding** — no
instruction is interpreted, only counted — and it lives in the test, never in the
library, which is stride-agnostic and resolves any `pcOffset`. `TestInstructionStride`
asserts the assumption against the fixtures (every row address and every function size
is a multiple of 16), so if a future `-arch` broke it the tests would say so rather than
measuring a ratio over PCs that cannot occur.

Collapse across all fixtures, at instruction granularity:

| fixture / function | size | insn PCs | distinct lines | ratio |
| --- | --- | --- | --- | --- |
| `single_lineinfo` / `addOne` | 384 | 24 | 7 | 3.4× |
| `two_kernels_lineinfo` / `scale` | 384 | 24 | 6 | 4.0× |
| `two_kernels_lineinfo` / `offset` | 384 | 24 | 7 | 3.4× |
| `unrolled_lineinfo` / `unrolledSum` | 2432 | 152 | 5 | **30.4×** |

The plan's own planning-time figure ("384 bytes of SASS = 24 instructions … collapsing
to 5 distinct source lines") is 4.8× on a differently-shaped source; the trivial kernels
here land at 3.4–4.0×, so **the ≥ 4× claim holds for unrolled kernels and does not hold
for trivial ones.** That is consistent with the plan's own wording — "kernels with
unrolled loops collapse much harder" — but it means the collapse argument for label
cardinality rests on real kernels being loop-shaped, not on a floor that every kernel
meets.

## 5. Fixtures and how to rebuild them

Four cubins and three `.cu` sources under `internal/cubin/testdata/`, with
`testdata/README.md` carrying the commands and the reason for each. CUDA 13.3
(`V13.3.73`), `-arch=sm_86`. **`nvcc -cubin` compiles; it does not need a GPU**, which
is why this task is fully offline.

```sh
NVCC=/usr/local/cuda-13.3/bin/nvcc
BUILD=/tmp/perf-agent-cubin-fixtures

rm -rf "$BUILD" && mkdir -p "$BUILD"
cp single.cu two_kernels.cu unrolled.cu "$BUILD"/
cd "$BUILD"

$NVCC -arch=sm_86 -lineinfo -cubin -o single_lineinfo.cubin      single.cu
$NVCC -arch=sm_86            -cubin -o single_nolineinfo.cubin   single.cu
$NVCC -arch=sm_86 -lineinfo -cubin -o two_kernels_lineinfo.cubin two_kernels.cu
$NVCC -arch=sm_86 -lineinfo -cubin -o unrolled_lineinfo.cubin    unrolled.cu
```

Built from a fixed path because the directory recorded in the DWARF line table is
`nvcc`'s working directory. `nvcc` absolutizes the source path however it is spelled,
and `-ffile-prefix-map` does not reach the device compiler, so the recorded directory
cannot be made relative. The tests therefore compare `filepath.Base` of the recorded
file, which is stable wherever the fixtures are rebuilt. The **file name** and the
**line numbers** are asserted exactly.

**`-lineinfo` detection confirmed:** `single.cu` built with `-lineinfo` has
`.debug_line` (117 bytes) and `.debug_frame`; the same source built without it has
`.debug_frame` and **no** `.debug_line`. Presence of a non-empty `.debug_line` is the
signal, exactly as the plan states.

## 6. Tests

`go test ./internal/cubin/ -count=1` — all pass.

- `TestSingleLineinfoExactPCToLine` — the exact `(pcOffset → line)` table for `addOne`:
  all eight row boundaries `(0x00,5) (0x10,6) (0x40,7) (0x60,8) (0xa0,10) (0xb0,9)
  (0xc0,10) (0xd0,12)`, plus ten interior PCs confirming a row's location holds until
  the next row, plus the `end_sequence` at `0x180 == st_size` after which nothing
  resolves. Note lines 10, 9, 10 at `0xa0/0xb0/0xc0`: the line table is **not
  monotonic** in line number, so a resolver may not assume ordering.
- `TestNoLineinfo` — `HasLineInfo() == false`, `LineInfoErr() == nil`, and `Resolve`
  returns `ok == false` at **every** instruction offset across the function and past its
  end. The function list, name, size and section survive.
- `TestTwoKernelsResolveIndependently` — both kernels resolve, with exact lines. Since
  both occupy the identical PC range, the test additionally sweeps every instruction of
  both and asserts `scale` never yields a line ≥ 13 and `offset` never yields one ≤ 11 —
  the two kernels' line ranges are disjoint, so a mis-binding cannot hide.
- `TestRelDebugLineBindsSequencesToFunctions` — §3, asserted from the ELF bytes.
- `TestUnrolledCollapseRatio` — §4, `≥ 4×`, and logs the measured ratio.
- `TestInstructionStride` — the 16-byte assumption §4 rests on.
- `TestDamagedLineTableIsNotReportedAsAbsent` — the third state of §2 is reachable and
  distinguishable: corrupt the `DW_LNE_set_address` the relocation points at, and get
  `HasLineInfo() == true` with `LineInfoErr() != nil`, the kernel still nameable, and
  nothing resolving.
- `TestLineTableRefusesRowsPastTheFunction` — the §7 fuzz finding, pinned.
- `TestDuplicateFunctionNamesAreRefused` — `Resolve` is keyed by function name, so two
  `FUNC` symbols sharing a name would make a lookup ambiguous and answer with whichever
  function's rows landed in the map. `Parse` refuses rather than picks: returning one
  kernel's source lines for another kernel's PC is the same failure §3 is about, and it
  would be no better arriving by this route. None of the fixtures has duplicates; the
  test builds one by pointing a symbol's `st_name` at another's string.
- `TestParseRejectsNonCubin`, `TestParseDoesNotRetainInput` (the transport hands over an
  `mmap` that may be unmapped once `Parse` returns), `TestFunctionsIsACopy`.

## 7. Fuzzing — and the defect it found

`FuzzParse` seeds with all four fixtures, eight truncation fractions of each plus a
one-byte truncation, byte flips at `e_machine` / `e_shoff` / `e_shnum` and other
structural offsets, and flips every 337 bytes through each fixture. It asserts: no
panic (via the `ErrPanic` sentinel, so the recover does not blind the fuzzer); never
both an error and a result; function count and total row count each bounded by the input
length (allocation proportional to input); every row bound to a real function of that
cubin and inside its address range; `Resolve` answering for no function when
`HasLineInfo()` is false; and no data returned alongside `ok == false`.

**Run with `-fuzz`, not seed-corpus only.** First run, `-fuzztime 150s`: **it found a
real defect.**

```
fuzz_test.go:101: unrolledSum: row at 0x3da04031 is past the function's 2432 bytes
```

A corrupted line program can walk its address arbitrarily far past the end of the
function its sequence is bound to. `Resolve`'s own range check would have masked this —
the bogus PC never resolves — so it would not have produced a wrong answer today. But it
meant the reader was **accepting a table it then had to defend against at every lookup**,
and rows filed under a function that the line program was demonstrably not describing.
A table that needs defending at lookup time is a table that should not have been
accepted.

Fixed in `buildLineTable`: a line entry beyond its function's `st_size` refuses the whole
table into `LineInfoErr`, consistent with every other structural refusal in §3. Every
fixture ends its sequence at exactly `st_size`, so no legitimate cubin is affected. The
minimized failing input is committed as a regression seed at
`internal/cubin/testdata/fuzz/FuzzParse/a9fc420c4bbee140`, and
`TestLineTableRefusesRowsPastTheFunction` pins the behaviour deterministically without
depending on the corpus file.

Re-run after the fix at `-fuzztime 300s`: no further findings (704k+ execs, 241 corpus
entries). A third run at `-fuzztime 120s` after the duplicate-name guard: no findings.

The corpus is not committed beyond the one regression seed — Go keeps generated corpus in
the build cache, so a fresh checkout fuzzes from the seeds in `FuzzParse` itself, which is
why those seeds enumerate truncations and structural byte flips explicitly rather than
relying on a cached corpus being present.

## 8. Verification run

```
go build ./...                              ok
go vet ./...                                ok
go test ./internal/cubin/ -count=1 -v       ok  (all tests above)
go test ./... -count=1                      ok  (whole repo, no regressions)
golangci-lint run --timeout=5m              0 issues
go test -fuzz FuzzParse -fuzztime 150s      1 finding, fixed
go test -fuzz FuzzParse -fuzztime 300s      no findings
go test -fuzz FuzzParse -fuzztime 120s      no findings (after the duplicate-name guard)
```

Nothing outside `internal/cubin/` was touched. Nothing consumes the package yet, so
there was no wiring to do.
