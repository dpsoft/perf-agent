# Task 2 — ABI additions: the stall-reason map and the sampling window

Branch `feat/abi-stall-map-window`, one commit on top of `origin/main` (`1681e2bc`).

Two new probes, neither mutating a frozen record, plus `gpu_config_v1` finished
(it had a C struct and `SizeConfig = 24` and nothing else). `KIND_MAX` goes
8 → 16, and `kindMax` in `gpuprobe/consumer.go` moves with it in the same
commit.

---

## The records as built

### `gpu_stall_reason_map_v1` — 136 bytes

```c
#define GPU_STALL_NAME_MAX 128          // CUPTI_STALL_REASON_STRING_SIZE
struct gpu_stall_reason_map_v1 {
    uint32_t index;
    uint16_t name_len;
    uint8_t  truncated;
    uint8_t  _pad;
    char     name[GPU_STALL_NAME_MAX];
};
```

| field | offset | size | asserted in |
|---|---|---|---|
| `index` | 0 | 4 | `usdt_abi.h` `GPU_STATIC_ASSERT`, `usdt_abi_test.c` byte check |
| `name_len` | 4 | 2 | both |
| `truncated` | 6 | 1 | both |
| `_pad` | 7 | 1 | — (implicit) |
| `name` | 8 | 128 | both, plus `sizeof(...->name) == GPU_STALL_NAME_MAX` |
| **total** | | **136** | both, plus `TestGoSizesMatchTheCStructs` |

One-shot, replayed on late attach. Fixed-size by the ABI's rules; `name_len`
is authoritative, and `truncated` is flagged per record rather than hidden —
the same discipline `gpu_kernel_name_v1` uses.

### `gpu_sampling_window_v1` — 24 bytes

```c
#define GPU_SAMPLING_MODE_CONTINUOUS        1
#define GPU_SAMPLING_MODE_KERNEL_SERIALIZED 2
struct gpu_sampling_window_v1 {
    uint64_t start_ns;
    uint64_t end_ns;
    uint8_t  mode;
    uint8_t  _pad[7];
};
```

| field | offset | size |
|---|---|---|
| `start_ns` | 0 | 8 |
| `end_ns` | 8 | 8 |
| `mode` | 16 | 1 |
| `_pad` | 17 | 7 |
| **total** | | **24** |

`end_ns == 0` is the encoded "open when the producer stopped reporting" case
that the plan's "target exits mid-burst" condition requires. It is decoded as
`SamplingWindow.Open()`, and `DecodeSamplingWindow` deliberately does *not*
treat it as an inversion. An actual inversion (`end < start`, both non-zero)
is rejected with a distinct `ErrWindowInverted`, because a negative duration
reaching the serialization disclosure would mark an arbitrary set of
executions perturbed.

`mode` zero is not a mode: it means the producer left the field unset, which
must stay distinguishable from a declared continuous burst. Pinned by
`TestSamplingModesAreNonZeroAndDistinct`.

### `gpu_config_v1` — 24 bytes, unchanged, now reachable

The struct is untouched. Added: `gpuabi.Config`, `DecodeConfig`,
`KIND_CONFIG = 9`, `REC_CONFIG 24`, the `cookieFor` arm, the `decodeBatch`
arm, and offset assertions in the header for the first time (offsets 0 / 8 /
12 / 16), since Go now decodes it by hard-coded offset. **No behaviour** —
`sampling_factor` / `sm_count` / `clock_hz` are decoded and carried, and
nothing consumes them yet. That is Task 6.

### Kinds

| kind | value | record | `BATCH_CAP` |
|---|---|---|---|
| `KIND_STALL_MAP` | 7 | 136 | **22** |
| `KIND_SAMPLING_WINDOW` | 8 | 24 | **64** (record-count capped) |
| `KIND_CONFIG` | 9 | 24 | **64** (record-count capped) |
| `KIND_MAX` | **16** (was 8) | — | — |

`BATCH_CAP` values re-derived from the macro against `PAYLOAD_BYTES = 3072`:
`3072/136 = 22`, `3072/24 = 128 → clamped to MAX_RECORDS_PER_BATCH = 64`.
Exactly the plan's figures.

`KIND_MAX` is 16, as the plan specifies, rather than the 10 that would just
fit `KIND_CONFIG`: resizing `dropped` and `stacks_missing` is a map-layout
change on both sides at once, so it is done once with headroom. A new
`_Static_assert(KIND_CONFIG < KIND_MAX)` keeps the headroom from being
silently used up — a kind at or past `KIND_MAX` is discarded by `count_drop`
without being counted anywhere.

---

## `MAX_BATCHED_RECORD_BYTES` was NOT raised

It stays at 48. Sizing 136 in would have grown every launch batch's
reservation from 3,072 to 8,704 bytes and cut the 4 MB ring from ~1,300
batches to ~480 — paid on the launch hot path to serve a probe that fires
a few dozen times per process. `BATCH_CAP` gives the stall map its own cap of
22 instead, and the excess is counted as a drop rather than truncated.

`MAX_RECORD_BYTES` also stays at 272 (136 ≤ 272), so
`_Static_assert(MAX_RECORD_BYTES <= PAYLOAD_BYTES)` holds untouched. Asserted
from three sides: the C header
(`sizeof(gpu_stall_reason_map_v1) <= sizeof(gpu_kernel_name_v1)`), the BPF
source's existing static assert, and Go
(`TestNoRecordExceedsTheBPFWorstCase`, which also pins
`MAX_BATCHED_RECORD_BYTES == SizeExec`).

---

## The `kindMax` pinning test, and evidence it was written first

`gpuprobe/kindmax_test.go`, `TestKindMaxPinsTheBPFDropAccountingArrays`.

**Written and run against untouched `origin/main` before `KIND_MAX` was
touched.** At that point the worktree was at `HEAD = 1681e2bc` with
`git status --porcelain` showing exactly one entry — `?? gpuprobe/kindmax_test.go`
— and no modification to `bpf/gpu_usdt.bpf.c`, `gpuprobe/consumer.go`, or any
`.o`:

```
$ git status --porcelain
?? gpuprobe/kindmax_test.go
$ git rev-parse HEAD
1681e2bcf9147f95a2892582dc85f63fea72a66b

$ go test ./gpuprobe/ -run TestKindMaxPinsTheBPFDropAccountingArrays -count=1 -v
=== RUN   TestKindMaxPinsTheBPFDropAccountingArrays
--- PASS: TestKindMaxPinsTheBPFDropAccountingArrays (0.00s)
ok  	github.com/dpsoft/perf-agent/gpuprobe	0.005s
```

It passed against `kindMax = 8` / `KIND_MAX 8` / `dropped.max_entries = 8`.

**And it is not vacuous.** Still on the otherwise-untouched tree, `kindMax`
was bumped to 16 in Go *only* — the exact drift the test exists to catch —
and the test failed on all three of its independent claims:

```
--- FAIL: TestKindMaxPinsTheBPFDropAccountingArrays
    Go kindMax=16 but the embedded BPF object sizes dropped at 8 entries.
    Go kindMax=16 but the embedded BPF object sizes stacks_missing at 8 entries.
    kindMax must mirror KIND_MAX literally  (expected 16, actual 8)
```

`consumer.go` was then restored from backup before any real edit was made.

What the test pins, and why each part earns its place:

1. `kindMax` against the **embedded BPF object's** `dropped.max_entries` and
   `stacks_missing.max_entries` — the bytecode that actually ships, via
   `loadGpuusdt()`. This is the claim that matters: a re-read of the C source
   would only prove two files agree about a number neither compiled, and would
   pass even when `make generate` had not been re-run.
2. `kindMax` against the `#define KIND_MAX` literal in the C source, so the
   two constants cannot drift textually.
3. That `dropped` and `stacks_missing` are still *declared* with
   `max_entries, KIND_MAX`, so a future edit that decouples the map size from
   the constant fails here instead of passing vacuously.
4. That every probe `cookieFor` installs has a kind `< kindMax`. `count_drop`
   discards anything at or past `KIND_MAX` without counting it anywhere, so a
   cookie past the end is loss with no counter at all.

After the change, all four hold at `kindMax = 16`, and the regenerated object
reports `dropped max_entries=16`, `stacks_missing max_entries=16`.

The failure mode this guards is the project's signature defect: a mis-sized
drop array is a counter that cannot go non-zero. Too large on the Go side and
every lookup past the end fails silently, so a drop storm reads as zero drops;
too small and the top kinds are never read at all. Neither end errors.

---

## Re-derived bounds constants

From `llvm-objdump -d gpuprobe/gpuusdt_x86_bpfel.o` **after** `make generate`:

```
67:	b7 02 00 00 28 0c 00 00	r2 = 0xc28      ; bpf_ringbuf_reserve size  = 3112
73:	b7 01 00 00 00 0c 00 00	r1 = 0xc00      ; clamp compare              = 3072
75:	b7 07 00 00 00 0c 00 00	r7 = 0xc00      ; clamped byte count         = 3072
91:	07 01 00 00 28 00 00 00	r1 += 0x28      ; payload offset             = 40
```

| constant | value | hex | status |
|---|---|---|---|
| reservation `sizeof(struct batch_msg)` | 3112 | `0xc28` | **unchanged** |
| payload offset (`batch_hdr` size) | 40 | `0x28` | **unchanged** |
| clamp (`PAYLOAD_BYTES`) | 3072 | `0xc00` | **unchanged** |

Identical to the pre-change object (same values at instructions 50 / 72 / 56
/ 58 before the new `record_size` / `max_records` arms shifted the
instruction numbering). Nothing in this task widened the reservation.

---

## `make generate` is idempotent

Confirmed by hashing after the first run and re-running:

```
$ sha256sum ...  > /tmp/gen1.txt   # after run 1
$ make generate && sha256sum -c /tmp/gen1.txt
gpuprobe/gpuusdt_x86_bpfel.o: OK
gpuprobe/gpuusdt_arm64_bpfel.o: OK
cpu/cpu_x86_bpfel.o: OK
offcpu/offcpu_x86_bpfel.o: OK
profile/perf_x86_bpfel.o: OK
```

The second run produced byte-identical objects for every target.

**Objects committed:** `gpuprobe/gpuusdt_x86_bpfel.o` and
`gpuprobe/gpuusdt_arm64_bpfel.o` only (+768 bytes each — the new
`record_size` / `max_records` arms and the resized map BTF).

**Objects deliberately excluded and reverted:** `cpu/cpu_{x86,arm64}_bpfel.o`,
`offcpu/offcpu_{x86,arm64}_bpfel.o`, `profile/perf_{x86,arm64}_bpfel.o`. Each
regenerated with the *same byte length* but 6 differing bytes (11 for
`offcpu/offcpu_x86_bpfel.o` — the known pre-existing BTF drift). This task
touches only `bpf/gpu_usdt.bpf.c`, so those objects cannot be affected by it;
the drift is against the committed versions, not run-to-run, and carrying it
here would hide an unrelated change inside an ABI commit.

---

## Verification run

| command | result |
|---|---|
| `make generate` (×2) | idempotent, second run left the tree unchanged |
| `make -C shim` | OK |
| `make -C shim test` | OK — `usdt_abi_test: OK`, all 8 suites |
| `make -C shim check-fpless` | `check-fpless: OK` |
| `go build ./... && go vet ./...` | clean |
| `go test ./gpu/ ./gpuprobe/ ./internal/... -count=1` | all `ok` |
| `go test ./gpu/ ./gpuprobe/ -race -count=4` | all `ok` |
| `~/go/bin/golangci-lint run --timeout=5m` | `0 issues.` |

### Tests added

`shim/core/usdt_abi_test.c` — was a bare `main`; the `_Static_assert`s in the
header were the whole test. It now also builds each new record and reads it
back **byte by byte through a little-endian reader**, which is the one thing
`offsetof` cannot prove: that a record written through the C struct lands
where `internal/gpuabi` reads it by hard-coded offset, endianness and padding
included. Covers the stall map (index/name_len/truncated/name, and that bytes
past `name_len` are not part of the name), the sampling window (including
`end_ns == 0` staying representable), and `gpu_config_v1`.

`internal/gpuabi/abisize_test.go` (new) — the cross-language pin. Every
existing size test in this package compares a Go constant to a *literal*,
which proves only that the constant and the test agree. This one parses the
`GPU_STATIC_ASSERT(sizeof(struct X) == N)` lines out of `shim/core/usdt_abi.h`
and the `REC_*` / `GPU_*_MAX` / `GPU_SAMPLING_MODE_*` defines out of
`bpf/gpu_usdt.bpf.c`, and pins all three sources against each other for all
ten records. Mutation-checked: setting `SizeStallReason = 140` fails both
`TestGoSizesMatchTheCStructs` and `TestBPFRecordSizesMatchTheGoSizes`.

`internal/gpuabi/records_test.go` — decoder round-trips for all three records,
including truncation flagging, `ErrWindowInverted`, `Open()` on `end_ns == 0`,
short buffers, and **`name_len > GPU_STALL_NAME_MAX` erroring rather than
slicing out of range** (tested at `MAX+1`, `200` and `0xFFFF`, each wrapped in
`require.NotPanics`, plus the legal boundary at exactly `MAX`).

`gpuprobe/consumer_test.go` — `decodeBatch` round-trips for the three new
kinds including multi-record stride (three stall entries at 136 bytes apiece,
which is where an off-by-one stride shows up), the overlong-`name_len` case
reaching `decodeBatch` without panicking, count-beyond-payload rejection,
`cookieFor` returning 7/8/9, a source-level check that `record_size` and
`max_records` both grew arms for every kind `cookieFor` installs, and
`TestNewKindsAreCarriedUndecodedAndCounted`.

`gpuprobe/batch_size_test.go` — `REC_STALL_MAP`, `REC_SAMPLING_WINDOW` and
`REC_CONFIG` added to the per-kind cap derivation.

### Why the overlong-name check is a panic test, not just an error test

`DecodeStallReason` reads `name_len` from a producer-supplied field and uses
it to slice a fixed 128-byte array. The decode runs on the consumer's ringbuf
drain goroutine, so a slice-out-of-range there does not fail one record — it
takes the consumer down and loses everything still in the ring. The record is
136 bytes on the wire whatever the field claims, so there is never anything
past the array to read, and the only correct outcome is an error at the batch
boundary.

### Scope held

`applyBatch` gained **no** arms. The three new kinds still land in its
`default:` case and are counted as `Stats.Undecoded`, exactly as `kindPC` and
`kindModule` do today — asserted by `TestNewKindsAreCarriedUndecodedAndCounted`
so the later change that drives that counter to zero has something concrete to
flip. Normalizing them is Task 7; PC-sampling behaviour is Task 6. Nothing in
the shim emits either new probe yet.

---

## Cannot verify

**No GPU run was performed and none is claimed.** The implementer has
`CapEff: 0`, so every capability- and hardware-gated test skipped. Nothing in
this task is structural, and the plan says so — but the following are
genuinely unverified here:

- **That any of the three probes ever fires.** No producer emits
  `gpu_stall_reason_map_v1`, `gpu_sampling_window_v1` or `gpu_config_v1`.
  Nothing in this commit has been exercised end to end from a probe site to a
  decoded record; the decoders were driven from hand-built buffers only.
- **That the BPF program still loads and verifies.** The object regenerates,
  its maps carry the expected `max_entries`, and the reservation/clamp
  constants are unchanged in the disassembly — but `BPF_PROG_LOAD` was never
  called. The new `record_size` / `max_records` arms are compile-time
  constants in an if-chain, the same shape as the six existing arms, so no new
  verifier pressure is expected. Expected is not observed.
- **That 38 stall reasons arrive on GA102, or that `GPU_STALL_NAME_MAX = 128`
  is enough for their names.** `truncated` exists precisely because that is a
  producer-side fact this side cannot check. Task 6 measures it.
- **The `end_ns == 0` hard-exit path.** Encoded and decoded correctly here;
  whether a real mid-burst exit actually produces it depends on the adapter's
  `atexit` path, which does not exist yet. Task 10.
- **That `KIND_MAX = 16` costs nothing at load time.** The two arrays grow from
  8 to 16 `__u64` slots — 64 extra bytes each — which is not worth measuring,
  but it has not been measured.

Attach was not run, the gate was not run, and no number in this report came
from hardware.
