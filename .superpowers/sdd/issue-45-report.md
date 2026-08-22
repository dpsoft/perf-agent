# Issue #45 — "no rule for `%rbp`" meant UNDEFINED where the psABI says unchanged

Branch `fix/ehcompile-fp-unchanged`, worktree `.worktrees/ehframe-fp-unchanged`, cut
from `fix/unwind-terminator-flags` (#44) so #44's counters are the instrument.

---

## 1. The three changes

### 1.1 "No rule for the frame-pointer register" is SAME VALUE, and `DW_CFA_restore` reverts to the CIE

`unwind/ehcompile/interpreter.go`.

Three edits, not one:

| edit | what |
|---|---|
| `archDefaultFPRule()` / `archDefaultRARule()` | named the interpreter's pre-CFI state: **FP = same-value, RA = undefined**. `newInterpreter` seeds both. |
| `sealInitialRules()` + `initFPRule` / `initRARule` | snapshot of the rules after the CIE's initial instructions have run. `ehcompile.go` calls it between the CIE run and the FDE run. |
| `restoreRegInitial()` | `DW_CFA_restore` / `DW_CFA_restore_extended` now revert to that snapshot instead of hard-coding `ruleUndefined`. |

**Why same-value is psABI-correct.** `%rbp` (x86-64) and `x29` (arm64) are
callee-saved: the x86-64 psABI §3.2.1 Fig. 3.4 and AAPCS64 §6.1.1 both oblige the
callee to preserve them across a call. DWARF 5 §6.4.1 deliberately leaves the
initial rule for a register "unspecified"/architecture-defined so the ABI can say
this. libgcc's `unwind-dw2.c` implements exactly that: every column starts
`REG_UNSAVED`, and `uw_update_context_1` simply does not touch an unsaved column —
i.e. same-value, not destroyed. A frame whose CFI never mentions the register is a
frame that never touched it, so the caller's value is still live in it.

**Why the restore fix is not optional, and why it is a second active fix rather
than a tidy-up.** DWARF 5 §6.4.2.3 defines `DW_CFA_restore` as reverting to *"the
rule assigned it by the initial instructions of the CIE"* — not to an architectural
default, and the two differ whenever the CIE says anything.

`DW_CFA_restore r6` (`%rbp`) is common: 1983 sites in libcuda, 654 in libstdc++,
286 in libc, 40 / 29 / 2 in the rest of the corpus. The **old code turned every one
of them into `FPTypeUndefined`**, exactly as it did for a register never mentioned
at all. Attributed precisely (§4.2), that is **3325 of the 61,513 changed rows
(5.4%)** and 37,480 changed code bytes — rows that a fix touching only the initial
default would not have moved.

The return-address half is the same defect and is worse where it lands. Every
x86-64 CIE says `DW_CFA_offset r16, -8`, so the old code turned `DW_CFA_restore r16`
into `RATypeUndefined` — a synthesised *"this frame is outermost"* marker in the
middle of a function, which `walk_step` would honour by setting
`WALKER_FLAG_RA_UNDEFINED` and stopping the walk **while reporting success**. A bare
default flip leaves that intact. This half is latent rather than observed: no
producer in the surveyed corpus emits `DW_CFA_restore r16` (§4.3). It is pinned by a
test either way.

**The bogus-frame risk, which is real but small — see §8 for the full limitation.**
The census (§4.3) finds `DW_CFA_undefined` applied to `%rbp` **zero times** across
~200k CFI rows in six binaries; the opcode appears only on `r16`. So reading "no
rule" as unchanged never contradicts an explicit producer claim — the claim is never
made. But "no rule" is an *inference* about the producer, not a statement by it, and
a scan of all 20,976 no-rule FDEs in the corpus finds **three functions where the
inference is wrong**, all hand-written assembly in glibc. §8 names them, measures the
incidence, and states what the fix does to the failure mode there. It does not
overturn the change; it is a known limitation and it is not buried.

### 1.2 The RA column keeps its current meaning

`archDefaultRARule()` is `ruleUndefined`, which is DWARF 5 §6.4.1's own initial rule
for the return-address column and the marker glibc uses for `_start` and thread
entry points. #44's `WALKER_FLAG_RA_UNDEFINED` is how a walk recognises a genuine
root, and making the column same-value by default would delete that signal.

**RA behaviour did change in one respect, and it is a bug fix, not a semantic
change:** `DW_CFA_restore` on the RA column now reverts to the CIE's rule
(`OffsetCFA(-8)`) rather than fabricating `Undefined`. Measured effect on the
corpus: **zero rows** (§4.3) — no producer emits it. Pinned by
`TestInterpret_RestoreOfTheRAColumnRevertsToTheCIERule`.

### 1.3 `walk_step`: the end-of-FP-chain guard keeps the return address it already read

`bpf/unwind_common.h`.

Before, the FP path ended like this:

```c
if (bpf_probe_read_user(&saved_fp, ..., ctx->fp) != 0) return 1;
if (bpf_probe_read_user(&ret_addr, ..., ctx->fp + 8) != 0) return 1;
if (saved_fp == 0) { flags |= WALKER_FLAG_FP_TERMINATED; return 1; }
if (saved_fp <= ctx->fp) return 1;
```

Both stops throw away `ret_addr` — a value read successfully out of a *different
slot* of the same frame, whose validity depends only on `ctx->fp` naming a real
frame, which is the same precondition as reading `saved_fp` at all. Note also that
`saved_fp == 0` is a **special case of** `saved_fp <= ctx->fp` (`ctx->fp` is
non-zero, checked above), so the two are one guard split in two. They are now
merged and neither discards the frame:

- `saved_fp == 0` — the x86-64 psABI's outermost-frame marker (`_start` does
  `xorl %ebp, %ebp`; the clone child does the same). `WALKER_FLAG_FP_TERMINATED`,
  then the caller's PC is handed to the **next iteration** rather than appended
  here, so the loop head records it *and the unwind tables get to classify it*.
  That is the only way a walk can reach a frame whose CFI declares the root and end
  with `WALKER_FLAG_RA_UNDEFINED`.
- `saved_fp <= ctx->fp`, non-zero — a frame pointer that does not increase: a
  corrupt or hand-rolled frame. New bit `WALKER_FLAG_FP_NONMONOTONIC` (0x20),
  classified as a failure like `FP_EXHAUSTED`. The return address is appended
  inline and the walk stops — the frame it came from is already suspect, so it is
  not used to classify a further PC.

The step past the root is bounded to **exactly one** by `fp_chain_ended(ctx)`,
which reads `WALKER_FLAG_FP_TERMINATED` (set at that one site and nowhere else) and
is tested in the two places that can be reached with `ctx->fp == 0`:

- FP path, `ctx->fp == 0`: return without setting `FP_EXHAUSTED`. Without this the
  fix would relabel every *complete* frame-pointer walk a failure — the shipped
  profilers' common case.
- DWARF path, `ra_type == RA_TYPE_OFFSET_CFA`: return before the read. The CFI had
  its chance to declare the root and did not, so the two sources disagree; believe
  the FP chain, which was read out of the running stack, rather than manufacture a
  caller from a CFA derived past the psABI's own root marker.

**That second guard needed a flag of its own, and not having one was a defect this
commit would otherwise have introduced.** `WALKER_FLAG_FP_TERMINATED` is already set
when it fires, so a bare `return 1` there files the walk as a clean termination and
**no counter moves** — a counter reading green in a case that may well be a
truncation, which is the exact defect class #44 exists to remove. On the Phase 4b
gate `assert.Equal(dwarf, reached-root)` would have caught it; for `perf_dwarf` and
`offcpu_dwarf` nothing would. So, following #44's pattern:

```c
#define WALKER_FLAG_ROOT_DISAGREEMENT 0x40
```

with `Stats.StackWalkRootDisagreement` beside it, and the classification switch
tests it **before** `walkerFlagsTerminated` — otherwise termination would swallow
it, since the two bits always arrive together. It is counted in
`StackWalkAbandoned`, like `StackWalkFPExhausted` and `StackWalkFPNonMonotonic`: an
ending the two sources disagree about is *unconfirmed*, and an unconfirmed ending
must not be filed as a success. It reads zero on the gate's producer, where
`_start`'s CFI declares the root and the two sources agree.

Using the reported bit as the state, rather than a new `struct walk_ctx` field,
keeps that struct at 40 bytes — which is why the three drivers' entry-point program
sections come out **byte-identical** (§4.1). With a field they did not.

**Scope note, stated plainly.** #45 and the brief both point at the
`saved_fp <= fp` arm specifically. Fixing only that arm is *not sufficient for the
pass condition* and I did not stop there — see §5.2 for the derivation. The
`saved_fp == 0` arm is the one that fires on a main-thread stack, it discards the
same already-read return address, and stopping there is what makes
`reached-root` unreachable. Both arms are the same guard; both are fixed.

---

## 2. What the walk looks like now, on the gate's own producer

Frames captured per DWARF walk, before and after, on `shim/perfagent-gpu-fpless`:

```
before (4)                          after (7)
  perfagent_stub_run        0x4019b8   perfagent_stub_run        0x4019b8
  perfagent_fpless_bridge   0x40095b   perfagent_fpless_bridge   0x40095b
  perfagent_fpless_caller   0x400988   perfagent_fpless_caller   0x400988
  main                      0x40077a   main                      0x40077a
  <FP_EXHAUSTED>                       __libc_start_call_main    0x7ffff78f3681
                                       __libc_start_main_impl    0x7ffff78f3798
                                       _start                    0x400875
                                       <FP_TERMINATED|RA_UNDEFINED>
```

---

## 3. Evidence (b): the gdb cross-check

`/usr/bin/gdb`, breakpoint at the sampled-launch USDT probe site (`0x4019b8`, inside
`perfagent_stub_run`; the probe's semaphore is armed from gdb so the probe path is
the one taken). ASLR off, as gdb runs it.

```
$ gdb -batch -nx -ex 'set backtrace past-main on' -ex 'set backtrace past-entry on' \
      -ex 'break main' -ex 'run 16 1000 8 0' \
      -ex 'set *(unsigned short*)0x4060ec = 1' -ex 'break *0x4019b8' -ex continue -ex bt \
      --args ./perfagent-gpu-fpless 16 1000 8 0

#0  gpu_launch_sampled_v1_emit (...) at stub/stub.cc:35        <- inlined, same PC as #1
#1  perfagent_stub_run (...) at stub/stub.cc:117
#2  0x000000000040095b in perfagent_fpless_bridge (...)
#3  0x0000000000400988 in perfagent_fpless_caller (...)
#4  0x000000000040077a in main (...)
#5  0x00007ffff78f3681 in __libc_start_call_main () from /lib64/libc.so.6
#6  0x00007ffff78f3798 in __libc_start_main_impl () from /lib64/libc.so.6
#7  0x0000000000400875 in _start ()
```

Seven distinct PCs. Now the same walk, executed by hand against **real memory**
(every value below printed by gdb at that breakpoint) using **our compiled tables**:

| step | frame | our classification / rule | value read | gdb says |
|---|---|---|---|---|
| 0 | `pc=0x4019b8` | FP_SAFE (`cfa=FP+16`) → FP path, `fp=0x7fffffffd560` | `*(fp)=0x7fffffffd5e0`, `*(fp+8)=0x40095b` | #2 = `0x40095b` ✓ |
| 1 | `pc=0x40095b` | FP_LESS, `cfa=SP+32`, `ra=CFA-8`, **`fp=SAME_VALUE`** | `sp=0x7fffffffd570`, `cfa=0x7fffffffd590`, `*(cfa-8)=0x400988` | #3 = `0x400988` ✓ |
| 2 | `pc=0x400988` | FP_LESS, `cfa=SP+32`, `ra=CFA-8`, **`fp=SAME_VALUE`** | `cfa=0x7fffffffd5b0`, `*(cfa-8)=0x40077a` | #4 = `0x40077a` ✓ |
| 3 | `pc=0x40077a` (`main`) | FP_SAFE → FP path, `fp=0x7fffffffd5e0` **(survives only because of step 1-2)** | `saved_fp=0x7fffffffd690`, `ret=0x7ffff78f3681` | #5 ✓ |
| 4 | `pc=libc+0x3681` | FP_SAFE (`cfa=FP+16`, row `0x3604+267`) | `saved_fp=0x7fffffffd6f0`, `ret=0x7ffff78f3798` | #6 ✓ |
| 5 | `pc=libc+0x3798` | FP_SAFE (`cfa=FP+16`, row `0x3718+328`) | `saved_fp=0`, `ret=0x400875` | #7 ✓ |
| 6 | `pc=0x400875` (`_start`) | FP_LESS, row `0x400854+34`, **`ra=UNDEFINED`** | — | gdb stops here too |

Under the old tables step 1 sets `new_fp = 0` and step 3 hits `ctx->fp == 0`:
`WALKER_FLAG_FP_EXHAUSTED`, four frames, gdb's #5-#7 missing. Under the old
`walk_step`, step 5's `saved_fp == 0` discarded `ret=0x400875` and `_start` was lost
even with the tables fixed.

**gdb's unwinder does not disagree with the fix anywhere.** It reaches the same
seven PCs and stops at the same frame, and for the same reason we now do: `_start`'s
CFI declares no return address. (gdb needs `backtrace past-main on` to print #5-#7
at all; that is a display default, not an unwinder disagreement — it walks them
either way, as `bt` with the setting shows.)

---

## 4. Evidence (a) and (c)

### 4.1 Shared-header safety: exactly what changed in the six objects

`bpf/unwind_common.h` is shared with `perf_dwarf.bpf.c` and `offcpu_dwarf.bpf.c`,
both shipped. Unlike #44 this change **is intended to alter their output**, so the
claim proved here is *what* changed, not that nothing did.

**Reproducibility baseline first.** With `bpf/unwind_common.h` reverted to HEAD,
`go generate ./profile/... ./gpuprobe/...` reproduced all six committed objects
byte-for-byte:

```
IDENTICAL perf_dwarf_x86_bpfel.o     IDENTICAL offcpu_dwarf_x86_bpfel.o
IDENTICAL perf_dwarf_arm64_bpfel.o   IDENTICAL offcpu_dwarf_arm64_bpfel.o
IDENTICAL gpuusdt_x86_bpfel.o        IDENTICAL gpuusdt_arm64_bpfel.o
```

so every difference below is attributable to this change alone.

**Section-level, after regeneration:**

```
perf_dwarf_x86_bpfel     .text: 3304 -> 3632   perf_event:          IDENTICAL (736 bytes)
perf_dwarf_arm64_bpfel   .text: 3304 -> 3632   perf_event:          IDENTICAL (736 bytes)
offcpu_dwarf_x86_bpfel   .text: 3304 -> 3632   tp_btf/sched_switch: IDENTICAL (1240 bytes)
offcpu_dwarf_arm64_bpfel .text: 3304 -> 3632   tp_btf/sched_switch: IDENTICAL (1240 bytes)
gpuusdt_x86_bpfel        .text: 3304 -> 3632   uprobe.multi:        IDENTICAL (4224 bytes)
gpuusdt_arm64_bpfel      .text: 3304 -> 3632   uprobe.multi:        IDENTICAL (4224 bytes)
```

**Every entry-point program section is byte-identical.** All the change is in
`.text`, which holds two out-of-line functions:

```
walk_step          0x0000  3416 bytes   (was 3088 = 386 insns; now 427 insns, +41)
mapping_scan_step  0x0d58   216 bytes   byte-identical (verified by slice compare)
```

and the `.text` section is byte-identical across all six objects
(`sha256` of the six extracted sections collapses to one value), so one enumeration
covers all six.

**Enumerating the +36 instructions.** A literal line-by-line diff is not the honest
unit: the compiler re-allocated registers and re-ordered blocks across the whole
function, so ~487 disassembly lines differ while doing the same work. The
differences are therefore enumerated by *invariant* and by *new basic block*.

Invariants that did **not** move — each one would be a finding if it had:

```
helper-call census      base == after, exactly:
    call 0x1  ×12 (bpf_map_lookup_elem)   call 0x70 ×4 (bpf_probe_read_user)
    call 0x2  ×2  (bpf_map_update_elem)   call 0x83 ×2 (bpf_ringbuf_reserve)
    call 0x5  ×2  (bpf_ktime_get_ns)      call 0x84 ×2 (bpf_ringbuf_submit)
    call 0xb5 ×1  (bpf_loop/…)
  -> the walker reads no more user memory and touches no more maps than before.

flag-OR sites           base: |=0x1 |=0x2 |=0x4 |=0x8 |=0x10               (5)
                        after:|=0x1 |=0x2 |=0x4 |=0x8 |=0x10 |=0x20 |=0x40  (7)
  -> exactly two new ones, WALKER_FLAG_FP_NONMONOTONIC (0x20) and
     WALKER_FLAG_ROOT_DISAGREEMENT (0x40). Every pre-existing flag still has
     exactly one site.

array-write bound       `if rN > 0x7e goto` : base 1, after 2
  -> the loop head's MAX_FRAMES check, plus the one guarding the new inline
     write in the non-monotonic arm. No unbounded write was introduced.
```

New blocks, read out of the after-listing (`llvm-objdump -d`, `perf_dwarf_x86`;
identical in all six):

```
; --- fp_chain_ended() in the FP path: `if (ctx->fp == 0)`
 210: r0 = 0x1
 211: r1 = *(u64 *)(r6 + 0x20)        ; ctx->rec
 212: w2 = *(u8 *)(r1 + 0x1a)         ; walker_flags
 213: r3 = r2
 214: r3 &= 0x1                       ; WALKER_FLAG_FP_TERMINATED
 215: if r3 != 0x0 goto +0x2          ; already past the root -> plain return 1
 216: r2 |= 0x10                      ; else WALKER_FLAG_FP_EXHAUSTED
 217: *(u8 *)(r1 + 0x1a) = w2
 218: exit

; --- saved_fp == 0: carry the caller's PC forward instead of dropping it
 219: r2 += 0x10                      ; ctx->fp + 16
 220: *(u64 *)(r6 + 0x10) = r2        ; ctx->sp
 221: r1 = *(u64 *)(r10 - 0x8)        ; ret_addr
 222: *(u64 *)(r6 + 0x8) = r3         ; ctx->fp = saved_fp (== 0)
 223: *(u64 *)(r6 + 0x0) = r1         ; ctx->pc = ret_addr
 224: r0 = 0x0                        ; return 0  (continue)
 225: goto -0x8

; --- saved_fp <= ctx->fp, non-zero
 226: r4 |= 0x20                      ; WALKER_FLAG_FP_NONMONOTONIC
 227: *(u8 *)(r1 + 0x1a) = w4
 228: r2 = *(u64 *)(r10 - 0x8)        ; ret_addr
 229: if r2 == 0x0 goto -0xc          ; nothing to record -> return 1
 230: w3 = *(u32 *)(r6 + 0x1c)        ; ctx->n_pcs
 231: if r3 > 0x7e goto -0xe          ; bound to MAX_FRAMES
 232: r4 = r3
 233: r4 += 0x1
 234: *(u32 *)(r6 + 0x1c) = w4
 235: r3 <<= 0x3
 236: r1 += r3
 237: *(u64 *)(r1 + 0x28) = r2        ; rec->pcs[n_pcs++] = ret_addr
 238: goto -0x15                      ; return 1

; --- fp_chain_ended() in the DWARF path, guarding the RA_TYPE_OFFSET_CFA read,
;     and raising WALKER_FLAG_ROOT_DISAGREEMENT rather than stopping silently
 372: r5 = r4                         ; walker_flags
 373: r5 &= 0x1                       ; WALKER_FLAG_FP_TERMINATED
 374: if r5 == 0x0 goto +0x7          ; not past the root -> do the normal RA read
 375: r4 |= 0x40                      ; WALKER_FLAG_ROOT_DISAGREEMENT
 376: *(u8 *)(r3 + 0x1a) = w4
 377: goto -0xa0                      ; return 1
 378: r1 = *(u64 *)(r5 + 0x20)        ; (the RA_TYPE_UNDEFINED arm, unchanged)
 379: w2 = *(u8 *)(r1 + 0x1a)
 380: r2 |= 0x8                       ; WALKER_FLAG_RA_UNDEFINED
```

Everything else in the diff is register renaming (`r7`↔`r6`, `r8`↔`r9`), stack-slot
renumbering, branch-displacement adjustment, and one relocation target
(`r2 = 0xc10 ll` → `r2 = 0xd30 ll`, the moved address of the byte-identical
`mapping_scan_step`).

**Bounds re-derived** from `llvm-objdump -d gpuprobe/gpuusdt_x86_bpfel.o`, at the
same instruction indices #44 recorded:

```
 50: r2 = 0xc28      reserve  = 3112
 56: r1 = 0xc00      clamp    = 3072
 58: r7 = 0xc00
 72: r1 += 0x28      payload at +40
```

Unchanged. `bpf/gpu_usdt.bpf.c` is not in the diff; `SEC("uprobe.multi")`, the
explicit `p.KernelVersion` and `ex.UprobeMulti(...)` are untouched; no capability
change.

The `.o` files grow ~1500 bytes each: 328 of `.text` and the rest `.BTF` /
`.BTF.ext`, which encode source line numbers and source text and therefore move
whenever comments and lines move. `llvm-readelf -SsWr` shows nothing else moving
beyond file offsets.

### 4.2 Table-level before/after

`ehcompile.Compile` at HEAD~ (the `.worktrees/unwind-terminator` checkout) vs at
HEAD, dumping every row and comparing them **row-aligned by PC** (row counts are
identical in every binary, so the alignment is exact — no coalescing changed):

| binary | rows | rows changed | share | code bytes | bytes changed | share |
|---|---:|---:|---:|---:|---:|---:|
| `/usr/lib64/libcuda.so.1` | 135805 | 42732 | 31.5% | 19,060,942 | 2,305,009 | **12.1%** |
| `/lib64/libc.so.6` | 13721 | 4186 | 30.5% | 1,447,228 | 305,283 | **21.1%** |
| `/lib64/libstdc++.so.6` | 26481 | 13949 | 52.7% | 1,456,986 | 384,381 | **26.4%** |
| `/lib64/ld-linux-x86-64.so.2` | 1411 | 398 | 28.2% | 165,041 | 30,114 | **18.2%** |
| `/bin/ls` | 720 | 215 | 29.9% | 94,465 | 11,044 | **11.7%** |
| `shim/perfagent-gpu-fpless` | 66 | 33 | 50.0% | 6,807 | 385 | **5.7%** |

**Which CFI construct is responsible**, measured by instrumenting the interpreter to
record whether the frame-pointer rule in force at each emitted row came from a
`DW_CFA_restore` or from the register never having been mentioned (throwaway patch,
not committed; the two columns sum exactly to the changed-row count in every
binary):

| binary | rows from `DW_CFA_restore r6` | bytes | rows never mentioning `%rbp` | bytes |
|---|---:|---:|---:|---:|
| libcuda.so.1 | 1983 | 19,471 | 40,749 | 2,285,538 |
| libc.so.6 | 287 | 4,600 | 3,899 | 299,496 |
| libstdc++.so.6 | 984 | 12,696 | 12,965 | 307,344 |
| ld-linux | 40 | 406 | 358 | 29,660 |
| /bin/ls | 29 | 274 | 186 | 10,770 |
| perfagent-gpu-fpless | 2 | 33 | 31 | 352 |
| **total** | **3325 (5.4%)** | **37,480** | **58,188 (94.6%)** | **2,933,160** |

(The row counts exceed the opcode counts in §4.3 — 984 vs 654 in libstdc++ — because
a restored rule stays in force across the `advance_loc`s that follow it.)

**Every changed row changed the same way and only that way:**

```
fp=UNDEFINED+0  =>  fp=SAME_VALUE+0
```

Zero rows changed `CFAType`, `CFAOffset`, `RAType`, `RAOffset` or `FPOffset`; zero
`Classification` rows changed (FP_SAFE / FP_LESS / FALLBACK are all identical);
zero rows moved in either direction other than `UNDEFINED → SAME_VALUE`. The two
golden fixtures show the same thing at file level — five `"FPType": 0 → 2` lines
each in `hello.golden` and `hello_arm64.golden`, and nothing else.

**Comparison against the pre-work readelf counts, and why the row shares differ.**
The pre-work evidence reported 12.3% (libc), 27.5% (libstdc++), 15.9% (libcuda) as a
share of **readelf rows**; our shares of **our rows** are 30.5% / 52.7% / 31.5%.
That is not a discrepancy in the fix, it is a different denominator: readelf emits a
row per `advance_loc`, our compiler coalesces adjacent identical rows, so our rows
are coarser and skew toward the long function bodies where "no rule for `%rbp`" is
most likely. **Code bytes** is the apples-to-apples measure, and it matches:

| binary | ours (bytes changed) | readelf (bytes with no rule for `%rbp`) |
|---|---:|---:|
| libcuda.so.1 | 12.1% | 12.1% |
| libc.so.6 | 21.1% | 20.7% |
| libstdc++.so.6 | 26.4% | 27.3% |
| ld-linux | 18.2% | 19.8% |
| /bin/ls | 11.7% | 13.2% |
| perfagent-gpu-fpless | 5.7% | 11.3% |

The readelf side had to be re-measured to get this: `--debug-dump=frames-interp`
prints **no table at all** for an FDE whose program adds nothing to the CIE, and
those bytes (273,116 of libc's) are precisely "no rule for `%rbp`". It also omits
the `rbp` column entirely for FDEs that never mention the register. Counting only
literal `u` cells undercounts.

The residual gaps are ranges our compiler classifies FALLBACK and emits no CFI row
for at all (an expression-based CFA), which is why our totals are slightly smaller —
`perfagent-gpu-fpless`'s 5.7% vs 11.3% is one such FDE (`DW_CFA_def_cfa: r5 (rdi)`).

**Byte-exact cross-tabulation** — for every byte inside a changed row, what readelf
says about `%rbp` there:

```
libc.so.6      no-table 273116   u 26639   no-col 5456   [+72 bytes: parse artifact*]
libcuda.so.1   no-table 2114735  no-col 111083   u 79191
libstdc++.so.6 no-col 237935     no-table 76771  u 69675
fpless         no-table 180      no-col 128      u 77
```

`no-table` / `no-col` / `u` are readelf's three spellings of "no rule". **No changed
row overlaps a range where `%rbp` has a real rule.**

*The 72-byte residue is a column-alignment artifact in the cross-check script:
readelf prints `r9 (r9)` with an embedded space. Checked by hand — libc's three
`DW_CFA_register: r6 in r9` sites are at FDEs `0x19c50..0x19d12`,
`0x19d40..0x19d96`, `0x100f20..0x101030`, and in each the register rule begins at
the FDE's *second* row, where the CFA also becomes `rdi+0`; our compiler classifies
those ranges FALLBACK and emits no CFI row for them. Our changed rows stop exactly
at that boundary (`0x19c50 + 0xa4 = 0x19cf4`, `0x19d40 + 0x38 = 0x19d78`).

### 4.3 Opcode census — which CFI construct is responsible

`readelf --debug-dump=frames`, raw opcode stream:

| binary | `DW_CFA_restore: r6` | `DW_CFA_restore: r16` | `DW_CFA_undefined: r6` | `DW_CFA_undefined: r16` | `DW_CFA_register: r6` |
|---|---:|---:|---:|---:|---:|
| libcuda.so.1 | 1983 | **0** | **0** | 0 | 0 |
| libc.so.6 | 286 | **0** | **0** | 2 | 3 |
| libstdc++.so.6 | 654 | **0** | **0** | 0 | 0 |
| ld-linux | 40 | **0** | **0** | 0 | 1 |
| /bin/ls | 29 | **0** | **0** | 1 | 0 |
| perfagent-gpu-fpless | 2 | **0** | **0** | 1 | 0 |

Reading it:

- **The dominant construct is the absence of one.** Almost every changed byte is a
  range where no opcode ever mentions `%rbp`. That is change 1.1's default.
- **`DW_CFA_restore r6` is an active second fix**, not a tidy-up: 1983 / 654 / 286 /
  40 / 29 / 2 sites, every one of which the old code turned into `FPTypeUndefined`,
  accounting for **3325 changed rows (5.4% of the total)**. It now resolves through
  the CIE snapshot. In this corpus every CIE happens to be silent about `%rbp`, so
  the snapshot's *value* equals the arch default — but the old code did not use the
  arch default, it hard-coded UNDEFINED, and
  `TestInterpret_RestoreRevertsToTheCIERuleNotTheArchDefault` fails against a bare
  default flip too (demonstrated, §6).
- **`DW_CFA_restore r16` is never emitted**, which is why zero RA rows changed. The
  fix to that path is latent, and it is a real defect: the old code would have
  fabricated a mid-function "outermost frame".
- **`DW_CFA_undefined` on `%rbp`: zero everywhere**, extending the pre-work finding
  from five binaries to six and to the gate's own producer. The reading this change
  adopts cannot contradict an explicit producer claim, because the claim is never
  made.

---

## 5. Evidence (d): existing test expectations, and the pass condition

### 5.1 Every changed assertion

| test | was pinning | new expectation psABI-correct? |
|---|---|---|
| `TestInterpret_CompressedRestore` (`interpreter_test.go:367`, `FPTypeUndefined`) | the *simplification* `restoreRegInitial` admitted to in its own comment. Its CIE has no initial instructions and nothing in its program says `%rbp` is dead, so "restore ⇒ undefined" was asserting the bug. | **Yes.** `FPTypeSameValue`: restore reverts to the CIE's rule, which here is "no rule", which under the psABI is unchanged. |
| `TestCompile_GoldenFile_x86` / `_arm64` | a byte-for-byte snapshot of the compiler's output on `hello`. It pins *everything*, so it moves whenever anything moves; it was not asserting anything about `%rbp` specifically. | **Yes**, and the diff is the proof: five `"FPType": 0 → 2` lines per file, no other field, no classification, no PC range. |
| `TestTheReportedTerminationArmIsTheOneTheCFIForces` (`gate_test.go:766`) | #44's derivation that **the broken** mechanism forces the `ctx->fp == 0` arm — `require.Equal(FPTypeUndefined, caller.FPType)` with the message *"the FP-less frame preserves a frame pointer after all"*. It was a correct description of the defect, deliberately written to be inverted by #45. | **Yes.** Replaced by `TestTheCFIForcesTheWalkToReachTheRoot`, the same method applied to the fixed mechanism, walking the whole chain to `_start`. |
| `TestWalkerFlagsMirrorTheBPFHeader` | that the Go mirror of `WALKER_FLAG_*` matches the header exactly. | **Yes** — it did its job: it caught the new bit. Extended with `WALKER_FLAG_FP_NONMONOTONIC = 0x20`. |
| `TestStubDrivesThePipelineToPprofWithoutAGPU` (the privileged gate) | `assert.Zero(StackWalkReachedRoot, "... when #45 IS fixed this is the assertion to invert")`, plus `abandoned == fp-exhausted == dwarf`. | **Yes** — inverted exactly as its own comment instructed. See §5.3. |

**No test was pinning genuinely correct behaviour.** The one that came closest is
`TestTheReportedTerminationArmIsTheOneTheCFIForces`: it was *true* and it was
*correct about the code as it stood*. It described a defect, and #44 wrote it so
that #45 would have to rewrite it.

`TestTheTwoTerminationArmsSetDifferentFlagsInWalkStep` was left untouched and still
passes: the RA-undefined arm still sets `WALKER_FLAG_RA_UNDEFINED` and the
`ctx->fp == 0` arm still sets `WALKER_FLAG_FP_EXHAUSTED`.

### 5.2 Predicted gate numbers, derived from the CFI before any live run

The #44 baseline on the RTX 3090:

```
dwarf=62 fp-only=1 no-tables=1 cfi-miss=0 truncated=0
abandoned=62 fp-exhausted=62 reached-root=0
```

**Prediction after this change:**

```
walk shape: dwarf=62 fp-only=1 no-tables=1 cfi-miss=0 truncated=0 abandoned=0
            fp-exhausted=0 nonmonotonic=0 root-disagree=0 reached-root=62
            registered=1 binaries=6
```

Derivation, entirely from the compiled CFI of the producer and of the system libc
(and cross-checked against gdb in §3):

1. `perfagent_fpless_{bridge,caller}` are FP_LESS with `ra=OffsetCFA(-8)` and, now,
   `fp=SAME_VALUE`. `walk_step` crosses both by CFI and leaves `ctx->fp` alone.
   `WALKER_FLAG_DWARF_USED` is set — that is what makes a walk count as `dwarf`.
2. `main` is FP_SAFE (`cfa=FP+16`) and is now reached **with** a frame pointer, so
   the FP path continues instead of hitting `ctx->fp == 0`. **`fp-exhausted` → 0**,
   and with it `abandoned` → 0 (it had no other contributor: `cfi-miss` was already
   0 and `truncated` was already 0).
3. `__libc_start_call_main` (libc `0x3604+267`) and `__libc_start_main_impl`
   (libc `0x3718+328`) are both FP_SAFE — measured, not assumed — so two more FP
   steps.
4. `__libc_start_main_impl`'s saved-FP slot is **zero** (`_start` does
   `xorl %ebp, %ebp`, confirmed in the producer's own disassembly and read live in
   gdb). `WALKER_FLAG_FP_TERMINATED`, and the return address beside it —
   `0x400875` — is now carried forward instead of dropped.
5. `0x400875` falls in `_start`'s CFI row `0x400854+34`, which is FP_LESS with
   `ra=UNDEFINED`. → **`WALKER_FLAG_RA_UNDEFINED`, `reached-root`.**
   **`reached-root` = the number of DWARF walks.** The two sources agree here, so
   `WALKER_FLAG_ROOT_DISAGREEMENT` cannot fire and `root-disagree` is 0. (An
   independent check by the reviewer: `_start`'s `DW_CFA_undefined: r16` is the
   only one in the whole executable, and its row covers the post-`call` PC the
   walk actually arrives with.)
6. The FP-only capture(s) — the ones taken before the tables land — follow the same
   FP chain to the same zero saved FP, but with no tables `_start` classifies as
   FP_SAFE by default and the `fp_chain_ended` guard stops the walk cleanly. They
   set `FP_TERMINATED` only, so they are complete and not abandoned, and they do
   **not** contribute to `reached-root`.

The invariants, which is what the gate asserts (the 62/1 split itself is timing
dependent — it is how many launches were sampled before the CFI compile finished —
so it is not asserted, exactly as before):

```
StackWalkReachedRoot      == StacksWalkedDWARF   and > 0
StackWalkFPExhausted      == 0
StackWalkFPNonMonotonic   == 0
StackWalkRootDisagreement == 0
StackWalkAbandoned        == 0
StacksWalkedCFIMiss       == 0
StacksWalkedDWARF + StacksWalkedFPOnly == 63     (unchanged)
```

Also expected, though not asserted: each DWARF-walked stack is now **7 frames deep
instead of 4**, and the three new frames symbolize as `__libc_start_call_main`,
`__libc_start_main_impl` and `_start`.

### 5.3 The inverted gate assertion, and why it cannot pass vacuously

```go
assert.Zero(t, stats.StacksWalkedCFIMiss, ...)
assert.Zero(t, stats.StackWalkFPExhausted, ...)
assert.Zero(t, stats.StackWalkFPNonMonotonic, ...)
assert.Zero(t, stats.StackWalkRootDisagreement, ...)
assert.Zero(t, stats.StackWalkAbandoned, ...)
assert.Positive(t, stats.StackWalkReachedRoot,
    "not one walk reached a frame whose CFI marks it outermost ... if it still reads zero, the fix did not take")
assert.Equal(t, stats.StacksWalkedDWARF, stats.StackWalkReachedRoot, ...)
```

`assert.Equal` alone would pass on a run where both counters were zero — the exact
vacuity #44 warned about — so `assert.Positive` sits in front of it, and
`assert.Positive(t, stats.StacksWalkedDWARF)` (unchanged, further up the gate)
independently refuses a run in which no walk used CFI at all.

---

## 6. Tests, and proof each one is falsifiable

New / rewritten:

| test | file | fails before because |
|---|---|---|
| `TestInterpret_ArchDefaultsAreFPSameValueAndRAUndefined` | `unwind/ehcompile/interpreter_test.go` | the FP default was UNDEFINED (both archs) |
| `TestInterpret_CompressedRestore` (rewritten) | same | restore reset to UNDEFINED |
| `TestInterpret_RestoreRevertsToTheCIERuleNotTheArchDefault` | same | pins the case a **bare default flip** gets wrong |
| `TestInterpret_RestoreOfTheRAColumnRevertsToTheCIERule` | same | pins the latent RA-column half of the same bug |
| `TestTheCFIForcesTheWalkToReachTheRoot` (replaces `TestTheReportedTerminationArmIsTheOneTheCFIForces`) | `gpuprobe/gate_test.go` | the bridge frames compiled to `FPTypeUndefined` |
| `TestWalkStepStepsPastTheFramePointerRoot` | same | the two saved-FP arms were separate bare `return 1`s |
| `TestAWalkMayReachTheRootByBothTheFPChainAndTheCFI` | `gpuprobe/consumer_test.go` | documents the newly-possible `FP_TERMINATED` + `RA_UNDEFINED` pair (see note) |
| `TestAWalkWhoseRootTheCFIContradictsIsNotCountedASuccess` | same | the disagreement had no bit; the walk read as a clean `FP_TERMINATED` |
| `TestAWalkStoppedByANonMonotonicFramePointerIsCountedAbandoned` | same | the cause had no bit and no counter |
| `TestAFullLengthNonMonotonicWalkIsAbandonedNotTruncated` | same | a 127-frame non-monotonic walk filed as "ran out of budget" |
| `TestWalkerFlagsMirrorTheBPFHeader` (extended) | same | header exported a bit the mirror did not |

Failure proofs, each run by reverting one thing:

```
$ # variant A: "bare default flip" — restore reverts to the arch default, not the CIE
--- FAIL: TestInterpret_RestoreRevertsToTheCIERuleNotTheArchDefault
--- FAIL: TestInterpret_RestoreOfTheRAColumnRevertsToTheCIERule

$ # variant B: archDefaultFPRule() back to ruleUndefined
--- FAIL: TestCompile_GoldenFile_x86
--- FAIL: TestCompile_GoldenFile_arm64
--- FAIL: TestInterpret_CompressedRestore
--- FAIL: TestInterpret_ArchDefaultsAreFPSameValueAndRAUndefined/x86_64
--- FAIL: TestInterpret_ArchDefaultsAreFPSameValueAndRAUndefined/arm64
--- FAIL: TestTheCFIForcesTheWalkToReachTheRoot
        perfagent_fpless_bridge: the CFI carries no rule for %rbp ...
        perfagent_fpless_caller: the CFI carries no rule for %rbp ...

$ # variant C: bpf/unwind_common.h reverted to HEAD
--- FAIL: TestWalkStepStepsPastTheFramePointerRoot
        "-1" is not positive
        the saved-FP guard is gone or reshaped; the zero and non-monotonic cases must share it

$ # variant D: consumer.go classification reverted (no non-monotonic bit)
--- FAIL: TestAWalkStoppedByANonMonotonicFramePointerIsCountedAbandoned
--- FAIL: TestAFullLengthNonMonotonicWalkIsAbandonedNotTruncated

$ # variant E: the DWARF-path fp_chain_ended guard reverted to a silent `return 1`
--- FAIL: TestWalkStepStepsPastTheFramePointerRoot
        the FP-chain-says-root / CFI-says-caller disagreement stops the walk silently

$ # variant F: the classification switch tests walkerFlagsTerminated FIRST
--- FAIL: TestAWalkWhoseRootTheCFIContradictsIsNotCountedASuccess
        an ending the frame-pointer chain and the unwind tables disagree about
        must not be filed as a success just because FP_TERMINATED is set
```

`TestAWalkMayReachTheRootByBothTheFPChainAndTheCFI` is the one new test that also
passes against the old consumer code, and that is stated rather than hidden: it
asserts a *classification* that was already right for an input that could not
previously occur. Its value is that the classification switch's comment used to
claim the two bits were mutually exclusive; they no longer are, and the test now
pins the behaviour the comment used to justify.

---

## 7. Commands and output

```
$ go generate ./profile/... ./gpuprobe/...        # on the UNMODIFIED tree
  all six objects reproduced byte-for-byte

$ go build ./... && go vet ./...
BUILD+VET OK

$ go test ./unwind/... ./gpuprobe/ ./gpu/ ./internal/... -count=1
ok  github.com/dpsoft/perf-agent/unwind/dwarfagent   ok  .../unwind/ehcompile
ok  github.com/dpsoft/perf-agent/unwind/ehmaps       ok  .../unwind/fpwalker
ok  github.com/dpsoft/perf-agent/unwind/procmap      ok  .../gpuprobe
ok  github.com/dpsoft/perf-agent/gpu                 ok  .../internal/{bpfstack,gpuabi,k8slabels,nspid,perfdata,perfevent,usdt}

$ go test ./gpuprobe/ -race -count=1
ok  github.com/dpsoft/perf-agent/gpuprobe   1.153s

$ make -C shim && make -C shim test
batch/clock/drain/usdt_abi/sampler/probe_args: all OK

$ make -C shim check-fpless
perfagent_fpless_bridge: no frame pointer, %rbp untouched
perfagent_fpless_caller: no frame pointer, %rbp untouched
check-fpless: OK

$ ~/go/bin/golangci-lint run --timeout=5m
0 issues.

$ gofmt -l unwind/ehcompile gpuprobe bpf
(empty)
```

---

## 8. Known limitation: three functions where "no rule" really does mean clobbered

The change rests on an *inference* — a frame whose CFI never mentions the
frame-pointer register did not touch it — and hand-written assembly can break it.
Every no-rule FDE in the corpus was scanned for instructions that write `%rbp`
other than a matched `push`/`pop` (which is a save/restore, not a clobber):

| binary | FDEs with no rule for `%rbp` | of those, FDEs that really clobber it |
|---|---:|---:|
| **libcuda.so.1** | 15,646 | **0** |
| libc.so.6 | 1,583 | **3** |
| libstdc++.so.6 | 3,562 | 0 |
| ld-linux-x86-64.so.2 | 106 | 0 |
| /bin/ls | 64 | 0 |
| perfagent-gpu-fpless | 15 | 0 |
| **total** | **20,976** | **3 (0.014%)** |

All three are hand-written assembly in glibc, and all three have an FDE that is
entirely `DW_CFA_nop`:

```
0x2f330..0x2f53c  __swapcontext      0x2f3ee: mov 0x78(%rdx),%rbp   <- reloads %rbp
                                              from the target ucontext
0x2fc80..0x2fd6b  __mpn_addmul_1     0x2fc85: push %rbp
                                     0x2fcac: lea (%rdx),%rbp       <- scratch
                                     0x2fd0c: adc $0x0,%rbp
0x31910..0x319fb  __mpn_submul_1     same shape as __mpn_addmul_1
```

**This does not overturn the change, and what it changes is the failure mode, not
the outcome.**

- For the two `__mpn_` routines the walk was already lost in those ranges before
  this commit: the all-nop FDE leaves the CFA rule at the CIE's `rsp+8` even after
  two pushes, so the CFA — and therefore the return address — is wrong there
  regardless of what the `%rbp` rule says.
- For `__swapcontext` the CFA *is* correct (it pushes nothing), so before this
  commit a walk crossing it stepped out correctly and then died loudly one frame
  later; now it steps out correctly and carries a `%rbp` belonging to a different
  context.

The observable difference in all three: what used to be `new_fp = 0` and a **counted**
`WALKER_FLAG_FP_EXHAUSTED` one frame later is now a garbage `%rbp` propagating. Most
often that faults on the next `bpf_probe_read_user` (an unflagged stop, counted in
`StackWalkAbandoned` via the default arm); sometimes it lands in
`WALKER_FLAG_FP_NONMONOTONIC`; and with small probability it yields a **plausible
bogus frame that nothing catches**. That last case is the honest cost of the change,
and it is paid against 12-27% of every shipped library's code no longer truncating.

Recorded in `bpf/unwind_common.h` at the `FP_TYPE_SAME_VALUE` arm, so the next reader
of that code finds it rather than the reassuring half of the story.

Worth noting where the risk is *not*: **libcuda.so.1, the library #45 is actually
about, has zero such functions in 15,646 no-rule FDEs.**

## 9. Two things recorded, not fixed

**`make generate` is not idempotent on the committed tree, and it is not this
change's doing.** Running `go generate ./offcpu/...` on an otherwise clean tree
rewrites `offcpu/offcpu_x86_bpfel.o`: 14 bytes, same file size (37,136), **all inside
`.BTF`** — `.text` and the `tp_btf/sched_switch` program section are byte-identical.
It is BTF type-ID renumbering; the regenerated x86 object comes out byte-identical to
the committed arm64 one, because `bpf/offcpu.bpf.c` compiles to the same bytecode for
both targets and the second `bpf2go` invocation settles the type IDs. `bpf/offcpu.bpf.c`
does not include `unwind_common.h` (grep: 0 hits), so nothing here can reach it.
**The file is reverted and is not in this commit** — if it shows up in a later diff it
is this pre-existing drift, not fallout from #45.

The six objects this change *does* touch reproduce exactly: regenerating twice in a
row leaves the tree clean.

**`walk_step` now has one more thing a future edit could quietly break.** The
`fp_chain_ended` predicate reads `WALKER_FLAG_FP_TERMINATED` instead of carrying its
own state — deliberately, because a `struct walk_ctx` field grows the struct past 40
bytes and changes all three drivers' entry code (measured: it does, +16 bytes per
program section). The cost is that setting that bit anywhere else would silently
change control flow. `TestWalkStepStepsPastTheFramePointerRoot` asserts the predicate
appears exactly twice; nothing asserts the bit has exactly one write site.

## 10. What could NOT be verified here

- **The gate never ran.** `CapEff: 0` in this environment: no BPF program was
  loaded, attached, or executed, and the verifier has not seen the regenerated
  objects. The predicted numbers in §5.2 are derived from CFI and from a gdb-read
  live stack, not observed from a walk. **The live run on the RTX 3090 machine is
  outstanding.**
- **Verifier risk, named rather than waved away.** The new code adds two branches on
  a byte already loaded from `ctx->rec->hdr.walker_flags`, one bounded array write
  (`ctx->n_pcs < MAX_FRAMES`, the same pattern as the existing loop head), and a
  `return 0` on a path that previously returned 1. It adds **no new helper calls**
  and **no new user-memory reads** (§4.1's call census is identical). `walk_step`
  grew 386 → 422 instructions, well inside any complexity limit. That is reasoning
  about the verifier, not a verifier run.
- **The prediction `reached-root == dwarf` is a derivation.** If the live run
  disagrees, the frame-by-frame chain in §5.2 — not the psABI argument — is what
  needs revisiting, and the gate's failure message prints every counter needed to
  see where it broke.
- **arm64 was not executed.** The arm64 objects were regenerated and their `.text`
  is byte-identical to x86's, but nothing arm64 ran. The arm64 half of change 1.1
  (x29 callee-saved under AAPCS64) is asserted by unit test on synthetic CFI and by
  the arm64 golden file, not by a run.
- **`libcuda.so.1`'s effect is measured at the table level only.** 2,305,009 code
  bytes of libcuda change from "FP destroyed" to "FP preserved", but no CUDA process
  was walked here.
- **The RTX 3090 numbers from #43/#44 remain second-hand.** What is verified is that
  the mechanism producing them is real, reproducible locally, forced by the CFI, and
  now fixed.

## Controller note — live runs (2026-08-22)

Both predictions confirmed on hardware, under `cap_bpf,cap_perfmon,cap_checkpoint_restore`,
no `cap_sys_admin`. The BPF programs load and verify; the verifier-risk note above is closed.

### Gate, on the FP-less producer — exactly as derived

    walk shape: dwarf=62 fp-only=1 no-tables=1 cfi-miss=0 truncated=0 abandoned=0
                fp-exhausted=0 nonmonotonic=0 root-disagree=0 reached-root=62
                registered=1 binaries=6

Against #44's baseline (`abandoned=62 fp-exhausted=62 reached-root=0`), every walk
migrated from fp-exhausted to reached-root. That was #45's stated success criterion.

### Real CUDA, RTX 3090 — the result that matters

`StackWalkReachedRoot == StacksWalkedDWARF` in **every** run (257, 289, 293, 294, 301,
302, 310 across seven runs), with `Abandoned=0`, `FPExhausted=0`, `FPNonMonotonic=0`,
`RootDisagreement=0`, `Truncated=0`. 100% of DWARF walks through libcuda now terminate
at a root the CFI declares.

### What the baseline runs revealed

An A/B against `main` (#44) was run to check a suspected coverage regression. It found
no regression — but it exposed something the counters had been hiding:

    base (main, #44): StackWalkReachedRoot:0  StackWalkFPExhausted:0  StackWalkAbandoned:0
                      StacksWalkedDWARF:303 / 327 / 336

All three termination counters read zero while 300+ DWARF walks completed. Those walks
were not reaching a root, and were not being counted as failing either: with `fp` zeroed
by the UNDEFINED defect, the `bpf_probe_read_user` at `ctx->fp` faults, `walk_step`
returns 1, and **no flag is set on any path**. Every DWARF walk through libcuda was
ending silently, mid-stack, and reporting as healthy.

#44 made the FP-exhausted case visible on the gate's producer. On real libcuda the
failure never reached that arm — it died one step earlier, in a read fault with no
flag. This is the sixth instance of the project's recurring defect shape: a counter
that reads green precisely when things are worst.

### Coverage: no regression, but a pre-existing gap worth its own issue

Six interleaved A/B pairs, `StacksWalkedDWARF` out of 500 sampled launches:

| | runs | mean |
|---|---|---|
| base (`main`, #44) | 267, 291, 304, 332, 330, 324 | 308 |
| new (#45) | 307, 288, 303, 309, 311, 317 | 306 |

Indistinguishable. An earlier non-interleaved block appeared to show #45 ~7% lower;
that was an ordering artifact (the base runs warmed up across the block). Two mechanisms
were ruled out directly: compiled table size is **identical** before and after
(libcuda 135805 entries both), as is compile time (78ms vs 73ms).

What both arms share is that ~40% of sampled stacks (`StacksWalkedNoTables` ≈ 200/500)
are walked before their CFI tables exist, are FP-walked instead, and are then correctly
refused by the #39 attribution guard (`StacksProfilerOnly == StacksWalkedNoTables`
exactly, in every run). The Phase 4b report recorded 48/500 (9.6%) for this; that number
is **not reproducible on this machine today** on either arm, so it should not be treated
as a baseline. Filed separately — the fix is eager registration at attach or an
MmapWatcher, deferred by Phase 4b Task 3.
