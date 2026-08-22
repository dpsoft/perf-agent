# Issue #44 — `WALKER_FLAG_UNWIND_TERMINATED` conflated success with failure

Branch `fix/unwind-terminator-flags`, worktree `.worktrees/unwind-terminator`.

## 1. The split

`bpf/unwind_common.h`:

```c
#define WALKER_FLAG_FP_TERMINATED    0x01
#define WALKER_FLAG_DWARF_USED       0x02
#define WALKER_FLAG_CFI_MISS         0x04
#define WALKER_FLAG_RA_UNDEFINED     0x08   // was WALKER_FLAG_UNWIND_TERMINATED
#define WALKER_FLAG_FP_EXHAUSTED     0x10   // new
```

Exactly which code path sets each, both inside `walk_step`:

| flag | site | condition | meaning |
|---|---|---|---|
| `WALKER_FLAG_RA_UNDEFINED` (0x08) | `bpf/unwind_common.h:577`, DWARF path, `else if (e.ra_type == RA_TYPE_UNDEFINED)` | the CFI of the frame being unwound gives no return address | **success** — the frame is the outermost of the chain (glibc marks `_start` and thread entry points this way) |
| `WALKER_FLAG_FP_EXHAUSTED` (0x10) | `bpf/unwind_common.h:628`, FP path, `if (ctx->fp == 0)` | arrived at an FP_SAFE/FALLBACK frame with a zero frame pointer | **failure** — the DWARF step out of the FP_LESS frame below gave no location for `%rbp` and zeroed it; the caller of this frame is real and is missing |

They are mutually exclusive by construction: each arm `return 1`s the moment it sets its flag, so a walk ends exactly once.

The comment at the old `:594` claimed the second recorded "the same fact `saved_fp == 0` records one step later". That is false and has been corrected in place: `saved_fp == 0` is a genuine chain root read out of live user memory, while `ctx->fp == 0` here is the walker's own previous DWARF step having zeroed the register. The corrected comment says so and names #45 as the root cause.

Consumer side (`gpuprobe/consumer.go`):

- `walkerFlagsTerminated = walkerFlagFPTerminated | walkerFlagRAUndefined` — `walkerFlagFPExhausted` deliberately excluded.
- `Stats.StackWalkReachedRoot` — the good outcome (RA undefined).
- `Stats.StackWalkFPExhausted` — the bad outcome, a **subset of `StackWalkAbandoned`** with a named cause (the same relationship `StacksWalkedCFIMiss` has).
- The classification switch checks `walkerFlagFPExhausted` **before** `len(ips) == maxWalkFrames`, so a walk that lost its frame pointer on frame 127 is filed as abandoned, not as "ran out of budget".

When #45 is fixed, the measurement is walks migrating from `StackWalkFPExhausted` to `StackWalkReachedRoot`, with `StackWalkAbandoned` falling to zero.

## 2. Evidence for which arm fires in practice

Issue #44's claim — "all 452 DWARF walks ended at `main` via the `ctx->fp == 0` arm" — came from a report, not a measurement. The GPU is not reachable from this environment, but the *mechanism* is entirely decided by data that is readable with no capabilities at all: `walk_step`'s choice at each frame depends only on (a) the classification mode of the frame it is on and (b) the `fp_type` of the frame it stepped out of. Neither depends on memory contents; the arm is reached before any read.

Measured with `unwind/ehcompile` against the gate's own producer, `shim/perfagent-gpu-fpless` (built to reproduce the CUDA stack shape), plus the system glibc:

```
$ go run /tmp/i44/probe/main.go shim/perfagent-gpu-fpless \
      perfagent_stub_run perfagent_fpless_bridge perfagent_fpless_caller main _start
== shim/perfagent-gpu-fpless: 66 entries, 67 classes, 141 syms
perfagent_stub_run       pc=0x40175b mode=FP_SAFE  cfa=FP+16 fp=OFFSET_CFA(-16) ra=OFFSET_CFA(-8)
perfagent_fpless_bridge  pc=0x400954 mode=FP_LESS  cfa=SP+32 fp=UNDEFINED (+0)  ra=OFFSET_CFA(-8)
perfagent_fpless_caller  pc=0x400981 mode=FP_LESS  cfa=SP+32 fp=UNDEFINED (+0)  ra=OFFSET_CFA(-8)
main                     pc=0x4007c6 mode=FP_SAFE  cfa=FP+16 fp=OFFSET_CFA(-16) ra=OFFSET_CFA(-8)
_start                   pc=0x400863 mode=FP_LESS  cfa=SP+8  fp=UNDEFINED (+0)  ra=UNDEFINED (+0)

$ go run /tmp/i44/probe/main.go /lib64/libc.so.6 \
      __libc_start_main __libc_start_call_main start_thread
__libc_start_main        pc=0x37b8   mode=FP_SAFE  cfa=FP+16 fp=OFFSET_CFA(-16) ra=OFFSET_CFA(-8)
__libc_start_call_main   pc=0x3687   mode=FP_SAFE  cfa=FP+16 fp=OFFSET_CFA(-16) ra=OFFSET_CFA(-8)
start_thread             pc=0x72b50  mode=FP_SAFE  cfa=FP+16 fp=OFFSET_CFA(-16) ra=OFFSET_CFA(-8)
```

Reading that as a walk:

1. `perfagent_fpless_caller` is `FP_LESS` with `fp_type = UNDEFINED` (it never touches `%rbp`, so its CFI carries no rule for it, and `ehcompile` reads "no rule" as UNDEFINED — issue #45). `walk_step` therefore sets `new_fp = 0`.
2. The next frame is `main`, classified `FP_SAFE`. `walk_step` takes the FP path and the very first thing it does is `if (ctx->fp == 0)`. **The `ctx->fp == 0` arm fires**, at `main`, exactly as the issue reports.
3. The `ra_type == UNDEFINED` arm is not merely unfired — it is *unreachable*. The only frame on this stack whose CFI marks it outermost is `_start` (`ra=UNDEFINED`, confirmed above), and reaching it needs three more FP_SAFE frames (`__libc_start_call_main`, `__libc_start_main`, `_start`) that a walk with a zeroed frame pointer cannot traverse.

**The claim is confirmed**, and the fix as specified is the right one. Two caveats stated plainly:

- This reproduces the *mechanism* on a local x86-64 Fedora binary and on the gate's own producer. It is not the RTX 3090 run; the number 452 is not independently verified, and nothing here confirms `libcuda`'s CFI specifically.
- The confirmation is a derivation from real CFI, not an observed flag value from a loaded program. `CapEff: 0` in this environment — no BPF program could be loaded, attached, or run.

Pinned as `TestTheReportedTerminationArmIsTheOneTheCFIForces` (`gpuprobe/gate_test.go`), which runs unprivileged and needs only `make` and the toolchain.

## 3. Before/after disassembly of the two shipped programs

`bpf/unwind_common.h` is shared with `perf_dwarf.bpf.c` and `offcpu_dwarf.bpf.c`, both shipped.

**Confirmed directly, not assumed:**

- `bpf/perf_dwarf.bpf.c:107` and `bpf/offcpu_dwarf.bpf.c:122` are the only reads of `walker_flags` in either program, and both mask with `WALKER_FLAG_DWARF_USED` (bit 1) alone:
  ```
  $ grep -rn "walker_flags" bpf/perf_dwarf.bpf.c bpf/offcpu_dwarf.bpf.c
  bpf/perf_dwarf.bpf.c:84:    rec->hdr.walker_flags = 0;
  bpf/perf_dwarf.bpf.c:107:   rec->hdr.mode = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED) ...
  bpf/offcpu_dwarf.bpf.c:104: rec->hdr.walker_flags = 0;
  bpf/offcpu_dwarf.bpf.c:122: rec->hdr.mode    = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED) ...
  ```
- No Go code reads the field. `profile/*_bpfel.go` carry only the generated `WalkerFlags uint8` struct member. The only decoder is `unwind/dwarfagent/sample.go:72`, and grepping every `.go` file in the repo for `WalkerFlags` finds it referenced *nowhere* outside `sample.go` and its own `sample_test.go`. Nothing branches on it.

**Reproducibility baseline first.** `go generate ./profile/... ./gpuprobe/...` on the unmodified tree reproduced all six committed objects byte-for-byte (identical sha256), so any post-change difference is attributable to the change alone.

**After regenerating**, `llvm-objdump -d --no-show-raw-insn`, all six objects (x86 and arm64):

```
$ diff -u /tmp/i44/base/perf_dwarf_x86_bpfel.dis /tmp/i44/after/perf_dwarf_x86_bpfel.dis
@@ -193,7 +193,7 @@
      196:	goto +0x5 <walk_step+0x650>
      197:	r1 = *(u64 *)(r6 + 0x20)
      198:	w2 = *(u8 *)(r1 + 0x1a)
-     199:	r2 |= 0x8
+     199:	r2 |= 0x10
      200:	*(u8 *)(r1 + 0x1a) = w2
      201:	r0 = 0x1
      202:	exit
```

Identical single-instruction diff, at the same index 199, in `offcpu_dwarf_x86_bpfel`, `perf_dwarf_arm64_bpfel`, `offcpu_dwarf_arm64_bpfel`, `gpuusdt_x86_bpfel` and `gpuusdt_arm64_bpfel`. That instruction is the FP-exhausted arm's `walker_flags |=`, and it is inside `walk_step` (the containing symbol at that offset is `<walk_step>`; instruction count and every other offset are unchanged).

Section-level byte comparison against the committed objects:

```
perf_dwarf_x86_bpfel     .text: DIFFERS (1 bytes)   perf_event:          IDENTICAL (736 bytes)
offcpu_dwarf_x86_bpfel   .text: DIFFERS (1 bytes)   tp_btf/sched_switch: IDENTICAL (1240 bytes)
perf_dwarf_arm64_bpfel   .text: DIFFERS (1 bytes)   perf_event:          IDENTICAL (736 bytes)
offcpu_dwarf_arm64_bpfel .text: DIFFERS (1 bytes)   tp_btf/sched_switch: IDENTICAL (1240 bytes)
gpuusdt_x86_bpfel        .text: DIFFERS (1 bytes)   uprobe.multi:        IDENTICAL (4224 bytes)
gpuusdt_arm64_bpfel      .text: DIFFERS (1 bytes)   uprobe.multi:        IDENTICAL (4224 bytes)
```

The differing byte, located exactly:

```
$ llvm-objcopy --dump-section=.text=b.bin /tmp/i44/base/perf_dwarf_x86_bpfel.o /dev/null
$ llvm-objcopy --dump-section=.text=a.bin profile/perf_dwarf_x86_bpfel.o /dev/null
$ cmp -l b.bin a.bin
1597  10  20            # (octal) 8 -> 16, the only differing byte
$ stat -c%s a.bin
3304                    # 413 instructions; byte offset 1596 = insn 199, imm low byte
```

`.text` holds the out-of-line `walk_step`; the entry-point program sections (`perf_event`, `tp_btf/sched_switch`, `uprobe.multi`) are byte-identical in every object. **No captured frames change** — the only altered value is the constant ORed into a metadata byte. The two shipped profilers still compute `sample_header.mode` from bit 1, which this change does not touch.

The `.o` files shrink by 8-16 bytes. That is `.BTF`/`.BTF.ext` line-info, which encodes source line numbers and source text, and the comment edits moved lines. `llvm-readelf -SsWr` shows only file offsets and BTF section sizes moving; symbol tables and relocations are unchanged.

## 4. The three bounds constants — re-derived

`llvm-objdump -d gpuprobe/gpuusdt_x86_bpfel.o`, after regeneration, in `gpu_usdt_batch`:

```
      50:	b7 02 00 00 28 0c 00 00	r2 = 0xc28      ; reserve = 3112, arg to call 0x83 (bpf_ringbuf_reserve)
      56:	b7 01 00 00 00 0c 00 00	r1 = 0xc00      ; clamp = 3072
      58:	b7 07 00 00 00 0c 00 00	r7 = 0xc00      ;   (the clamped payload length)
      72:	07 01 00 00 28 00 00 00	r1 += 0x28      ; payload written at +40, past the batch header,
                                                        ; then call 0x70 (bpf_probe_read_user)
```

- reserve **3112** = `0xc28`
- payload offset **+40** = `0x28`
- clamp **3072** = `0xc00`

All three unchanged; the batch header stays 40 bytes with `stack_id` at offset 32.

Also unchanged: `SEC("uprobe.multi")` (`bpf/gpu_usdt.bpf.c:463`), the explicit `p.KernelVersion` (`gpuprobe/consumer.go:946`), `ex.UprobeMulti(...)` (`gpuprobe/consumer.go:1000`). `bpf/gpu_usdt.bpf.c` is not in the diff at all. Capabilities are untouched — no `CAP_SYS_ADMIN` introduced.

## 5. Tests

New, and each fails against the pre-change behaviour:

| test | file | fails before because |
|---|---|---|
| `TestAWalkThatRanOutOfFramePointerIsCountedAbandoned` | `gpuprobe/consumer_test.go` | `StackWalkAbandoned` read 0 for a walk stopped mid-stack |
| `TestReachedRootAndFPExhaustedAreDisjoint` | `gpuprobe/consumer_test.go` | one bit could not tell the two outcomes apart |
| `TestAFullLengthWalkThatRanOutOfFramePointerIsAbandonedNotTruncated` | `gpuprobe/consumer_test.go` | a 127-frame FP-exhausted walk was filed under "ran out of budget" |
| `TestTheTwoTerminationArmsSetDifferentFlagsInWalkStep` | `gpuprobe/consumer_test.go` | parses `walk_step` in the header; both arms named `WALKER_FLAG_UNWIND_TERMINATED` |
| `TestWalkerFlagsMirrorTheBPFHeader` (extended) | `gpuprobe/consumer_test.go` | header exported the old single macro |
| `TestTheReportedTerminationArmIsTheOneTheCFIForces` | `gpuprobe/gate_test.go` | new; measures §2's claim from real CFI |

Failure proof, running the new tests against the old conflated semantics (`walkerFlagsTerminated` widened to include `walkerFlagFPExhausted`, FP-exhausted case removed from the switch):

```
--- FAIL: TestAWalkThatRanOutOfFramePointerIsCountedAbandoned (0.00s)
        Error:  Not equal: expected: 0x1  actual: 0x0
--- FAIL: TestReachedRootAndFPExhaustedAreDisjoint (0.00s)
        Error:  Not equal: expected: 0x1  actual: 0x0
--- FAIL: TestAFullLengthWalkThatRanOutOfFramePointerIsAbandonedNotTruncated (0.00s)
        Error:  Not equal: expected: 0x1  actual: 0x0
FAIL
```

And against the old header (`git show HEAD:bpf/unwind_common.h`):

```
--- FAIL: TestWalkerFlagsMirrorTheBPFHeader (0.00s)
        - "WALKER_FLAG_FP_EXHAUSTED": 16   - "WALKER_FLAG_RA_UNDEFINED": 8
        + "WALKER_FLAG_UNWIND_TERMINATED": 8
--- FAIL: TestTheTwoTerminationArmsSetDifferentFlagsInWalkStep (0.00s)
        Error:  expected: "WALKER_FLAG_RA_UNDEFINED"  actual: "WALKER_FLAG_UNWIND_TERMINATED"
        Error:  expected: "WALKER_FLAG_FP_EXHAUSTED"  actual: "WALKER_FLAG_UNWIND_TERMINATED"
        Error:  Should not be: "WALKER_FLAG_UNWIND_TERMINATED"
```

### The privileged gate changed meaning, deliberately

`TestStubDrivesThePipelineToPprofWithoutAGPU` (the Phase 4b gate) asserted `StackWalkAbandoned == 0`. Under the corrected semantics that assertion **would now fail**, because on this producer every DWARF walk really does end by losing the frame pointer — §2 derives that from the binary's own CFI. Asserting zero would be asserting the bug. The gate now asserts instead that abandonment is *fully explained*:

```go
assert.Zero(t, stats.StacksWalkedCFIMiss, ...)
assert.Equal(t, stats.StackWalkFPExhausted, stats.StackWalkAbandoned, ...)
assert.Equal(t, stats.StacksWalkedDWARF,   stats.StackWalkFPExhausted, ...)
assert.Zero(t, stats.StackWalkReachedRoot, "... when #45 IS fixed this is the assertion to invert")
```

That is the instrument: no unexplained abandonment, no CFI misses, and every DWARF walk accounted for by the one known cause. Fixing #45 moves the counts to `StackWalkReachedRoot` and inverts the last two lines.

### Incidental fix, needed by the new test

`requireBuilt` (`gpuprobe/gate_test.go`) hard-coded `make perfagent-gpu-stub` regardless of the path it was given, so `TestTheProducersBridgeFramesAreFPLessInTheCFI` — and the new test — would fail on a clean tree with the `perfagent-gpu-fpless` binary absent. It now builds `filepath.Base(path)`.

## 6. Commands and output

```
$ go generate ./profile/... ./gpuprobe/...          # on the UNMODIFIED tree
# all six objects reproduced byte-for-byte (sha256 identical to committed)

$ go build ./... && go vet ./...
BUILD+VET OK

$ go test ./gpuprobe/ ./gpu/ ./internal/... ./unwind/... -count=1
ok  github.com/dpsoft/perf-agent/gpuprobe             0.078s
ok  github.com/dpsoft/perf-agent/gpu                  3.340s
ok  github.com/dpsoft/perf-agent/internal/bpfstack    0.003s
ok  github.com/dpsoft/perf-agent/internal/gpuabi      0.005s
ok  github.com/dpsoft/perf-agent/internal/k8slabels   0.005s
ok  github.com/dpsoft/perf-agent/internal/nspid       0.005s
ok  github.com/dpsoft/perf-agent/internal/perfdata    0.161s
ok  github.com/dpsoft/perf-agent/internal/perfevent   0.003s
ok  github.com/dpsoft/perf-agent/internal/usdt        0.095s
ok  github.com/dpsoft/perf-agent/unwind/dwarfagent    0.006s
ok  github.com/dpsoft/perf-agent/unwind/ehcompile     0.013s
ok  github.com/dpsoft/perf-agent/unwind/ehmaps        0.112s
ok  github.com/dpsoft/perf-agent/unwind/fpwalker      0.003s
?   github.com/dpsoft/perf-agent/unwind/perfreader    [no test files]
ok  github.com/dpsoft/perf-agent/unwind/procmap       0.003s

$ go test ./gpuprobe/ -race -count=1
ok  github.com/dpsoft/perf-agent/gpuprobe             1.172s

$ make -C shim && make -C shim test
drain_test OK / sampler_test OK / probe_args_test: ok / usdt_abi_test (silent pass)

$ make -C shim check-fpless
perfagent_fpless_bridge: no frame pointer, %rbp untouched
perfagent_fpless_caller: no frame pointer, %rbp untouched
check-fpless: OK

$ gofmt -l gpu gpuprobe shim internal unwind
internal/perfdata/attr.go
internal/perfdata/encode_test.go
internal/perfdata/header.go
internal/perfdata/records_test.go
unwind/dwarfagent/miss_drainer.go
unwind/dwarfagent/miss_drainer_test.go
unwind/ehmaps/scan_enroll_test.go
unwind/procmap/addressmapper_test.go
# All eight are PRE-EXISTING on untouched HEAD (verified by stashing this
# change and re-running). They are the Go 1.19+ comment-reflow rule, in files
# this change does not touch. `gofmt -l gpuprobe/ bpf/` is empty.

$ ~/go/bin/golangci-lint run --timeout=5m
0 issues.

$ CGO_LDFLAGS="-L/tmp/bzstatic -lblazesym_c" go test -c ./gpuprobe/ -o /home/diego/gpuprobe.test
$ readelf -d /home/diego/gpuprobe.test | grep -c blazesym
0
```

## 7. What could not be verified

- **The gate itself never ran.** `CapEff: 0` in this environment; no BPF program was loaded, attached, or executed. The verifier has not seen the regenerated objects. The single changed instruction is a widened immediate on an existing `|=` into an already-verified byte store, which does not alter register types, bounds or reachability, but that is reasoning about the verifier rather than a verifier run. `/home/diego/gpuprobe.test` is built and blazesym-static for a privileged run.
- **`TestStubDrivesThePipelineToPprofWithoutAGPU` (the Phase 4b gate)'s rewritten assertions are untested against a live run.** The predicted equalities (`abandoned == fp-exhausted == dwarf`, `reached-root == 0`) follow from the CFI derivation in §2, and they are the values the gate should print. If a live run disagrees, the derivation — not the flag split — is what needs revisiting.
- **The RTX 3090 numbers are unverified.** "452 walks" and the `libcuda` frame shape come from the issue's report. What is verified is that the mechanism producing that shape is real, reproducible locally, and forced by the CFI on the gate's own producer.
- **arm64 was not executed.** The arm64 objects were regenerated and disassembled (identical one-instruction diff) but nothing arm64 ran.
- Issue **#45 itself is untouched.** `ehcompile` still emits `FPTypeUndefined` for "no rule for `%rbp`". This change only makes the resulting truncation countable.

## Controller note — live gate run (2026-08-21)

Predictions confirmed on hardware, unprivileged derivation vindicated:

    walk shape: dwarf=62 fp-only=1 no-tables=1 cfi-miss=0 truncated=0
                abandoned=62 fp-exhausted=62 reached-root=0 registered=1 binaries=6
    main: mode=FP_SAFE, reached with ctx->fp == 0 -> WALKER_FLAG_FP_EXHAUSTED

abandoned == fp-exhausted == dwarf == 62, reached-root == 0, exactly as derived.

THIS IS THE BASELINE FOR #45. reached-root=0 means not one walk reaches a genuine
root — every DWARF walk is cut short by a zeroed FP, which is precisely the ehcompile
UNDEFINED-vs-unchanged defect. Before the flag split this was invisible: both outcomes
set the same bit and the walk reported as complete.

#45's success criterion is therefore concrete and measurable: walks must migrate from
fp-exhausted toward reached-root. A #45 fix that leaves reached-root at 0 has not worked,
whatever else it changes.
